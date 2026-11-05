/*
Copyright 2026 weirong-ui

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package scaler

// 缩容稳定窗口的回归测试（2026-11-04 压测 run5 抓到的误缩容）：
// 突发流量刚起来、1m 均值还没抬上去的那一两轮，desired 会短暂低于 current，
// 旧逻辑立刻缩容，13 秒后又得扩回来。窗口要求"连续低于目标够久"才认低谷。

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	appsv1alpha1 "github.com/weirong-ui/kubestasis/api/v1alpha1"
)

// newAppWithWindow 复制 newApp 但显式给稳定窗口，且冷却为 0（隔离变量）。
func newAppWithWindow(replicas int32, windowSeconds int32) *appsv1alpha1.App {
	app := newApp(replicas, 1, 10, 0, qpsTarget())
	app.Spec.Scaling.ScaleDownStabilizationSeconds = &windowSeconds
	return app
}

func withBelowSince(app *appsv1alpha1.App, at time.Time) *appsv1alpha1.App {
	t := metav1.NewTime(at)
	app.Status.BelowTargetSince = &t
	return app
}

func engineWanting(v int32) *Engine {
	return NewEngine().WithMetricsSource(fakeSource{vals: map[appsv1alpha1.MetricType]fakeVal{
		appsv1alpha1.MetricQPS: {v, true},
	}})
}

func TestStabilization_FirstLowSampleStartsClockAndBlocks(t *testing.T) {
	// 第一次看到"低于目标"：只记起点，不缩容。这就是 run5 里 4→3 的那一步。
	app := newAppWithWindow(4, 60)
	d := engineWanting(2).Decide(context.Background(), app, func() time.Time { return frozen })

	if d.Action != ActionNone {
		t.Fatalf("first low sample must not scale down, got %s -> %d", d.Action, d.ToReplicas)
	}
	if d.ReasonCode != ReasonStabilizing {
		t.Fatalf("want reason code %q, got %q", ReasonStabilizing, d.ReasonCode)
	}
	if !d.BelowTarget.Set || d.BelowTarget.Value == nil {
		t.Fatal("decision should propose recording the window start")
	}
	if !d.BelowTarget.Value.Time.Equal(frozen) {
		t.Fatalf("belowTargetSince = %v, want %v", d.BelowTarget.Value.Time, frozen)
	}
	// 纯函数契约：决策不改输入对象，由控制器落盘
	if app.Status.BelowTargetSince != nil {
		t.Fatalf("Decide must not mutate its input, got %v", app.Status.BelowTargetSince)
	}
}

func TestStabilization_BlocksUntilWindowElapsed(t *testing.T) {
	app := withBelowSince(newAppWithWindow(4, 60), frozen.Add(-30*time.Second))

	d := engineWanting(2).Decide(context.Background(), app, func() time.Time { return frozen })

	if d.Action != ActionNone || d.ReasonCode != ReasonStabilizing {
		t.Fatalf("30s into a 60s window must not scale, got %s (%s)", d.Action, d.ReasonCode)
	}
	// 计时起点不能被重置：否则每轮都从 0 开始，永远到不了窗口末尾。
	// 窗口未满时应"不改"，所以建议必须是 Set=false。
	if d.BelowTarget.Set {
		t.Fatalf("window not elapsed should leave the start untouched, got %+v", d.BelowTarget)
	}
	if !app.Status.BelowTargetSince.Time.Equal(frozen.Add(-30 * time.Second)) {
		t.Fatalf("input must stay untouched, got %v", app.Status.BelowTargetSince.Time)
	}
}

func TestStabilization_AllowsScaleDownAfterWindow(t *testing.T) {
	app := withBelowSince(newAppWithWindow(4, 60), frozen.Add(-61*time.Second))

	d := engineWanting(2).Decide(context.Background(), app, func() time.Time { return frozen })

	if d.Action != ActionScaleDown {
		t.Fatalf("61s into a 60s window should scale down, got %s (%s)", d.Action, d.ReasonCode)
	}
}

func TestStabilization_SignalRecoveryResetsClock(t *testing.T) {
	// 需求回到当前副本以上（突发真的来了）：低谷不成立，计时清零，
	// 下一轮即使又低于目标，也要重新等满一个窗口。
	app := withBelowSince(newAppWithWindow(4, 60), frozen.Add(-600*time.Second))

	d := engineWanting(6).Decide(context.Background(), app, func() time.Time { return frozen })

	if d.Action != ActionScaleUp {
		t.Fatalf("want ScaleUp, got %s (%s)", d.Action, d.ReasonCode)
	}
	if !d.BelowTarget.Set || d.BelowTarget.Value != nil {
		t.Fatalf("recovery must propose clearing the start, got %+v", d.BelowTarget)
	}
	if app.Status.BelowTargetSince == nil {
		t.Fatal("Decide must not mutate its input")
	}
}

func TestStabilization_ZeroDisablesWindow(t *testing.T) {
	// 显式 0：恢复旧行为（冷却与迟滞仍然生效）。这里冷却=0、带内不触发，
	// 所以首个低采样就该直接缩容。
	app := newAppWithWindow(4, 0)
	d := engineWanting(2).Decide(context.Background(), app, func() time.Time { return frozen })

	if d.Action != ActionScaleDown {
		t.Fatalf("window=0 should scale down immediately, got %s (%s)", d.Action, d.ReasonCode)
	}
}

func TestStabilization_HysteresisBandDoesNotStartClock(t *testing.T) {
	// 迟滞带内（差 1，带 10%×4=1）说明供需基本匹配，不是低谷：
	// 不该留下计时起点，否则下次真低谷会"白捡"一段计时。
	app := withBelowSince(newAppWithWindow(4, 60), frozen.Add(-600*time.Second))
	d := engineWanting(3).Decide(context.Background(), app, func() time.Time { return frozen })

	if d.Action != ActionNone || d.ReasonCode != ReasonHysteresis {
		t.Fatalf("want None/hysteresis, got %s (%s)", d.Action, d.ReasonCode)
	}
	if !d.BelowTarget.Set || d.BelowTarget.Value != nil {
		t.Fatalf("band-internal decision must propose clearing the start, got %+v", d.BelowTarget)
	}
}

// 纯函数契约（2026-11-05 外部评审指出 Decide 既改 Engine.nowFunc 又改
// app.Status，与"纯函数可测"的设计自述矛盾，此处把契约钉成测试）：
// 同一份输入重复决策，不能改动输入对象。
func TestDecide_DoesNotMutateInput(t *testing.T) {
	app := withBelowSince(newAppWithWindow(4, 60), frozen.Add(-30*time.Second))
	before := app.DeepCopy()

	engineWanting(2).Decide(context.Background(), app, func() time.Time { return frozen })

	if !reflect.DeepEqual(app, before) {
		t.Fatalf("Decide mutated its input:\n before: %+v\n  after: %+v", before, app)
	}
}

// Engine 不再持有可变时钟字段：同一实例并发决策不应有共享状态
// （`go test -race` 下验证）。
func TestDecide_ConcurrentSafe(t *testing.T) {
	e := engineWanting(2)
	app := newAppWithWindow(4, 60)
	now := func() time.Time { return frozen }

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = e.Decide(context.Background(), app, now)
		}()
	}
	wg.Wait()
}

// 每条返回路径都必须带 ReasonCode：空字符串会变成 metrics 里一个叫 reason="" 的
// 脏 label，而且是"看起来有数据、实际不可聚合"的那种脏。
func TestDecide_ReasonCodeAlwaysSet(t *testing.T) {
	ctx := context.Background()
	now := func() time.Time { return frozen }

	t.Run("no policy", func(t *testing.T) {
		app := &appsv1alpha1.App{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "default"}}
		if d := engineWanting(3).Decide(ctx, app, now); d.ReasonCode != ReasonNoPolicy {
			t.Fatalf("got %q", d.ReasonCode)
		}
	})
	t.Run("no metrics source", func(t *testing.T) {
		d := NewEngine().Decide(ctx, newApp(2, 1, 10, 0, qpsTarget()), now)
		if d.ReasonCode != ReasonNoMetricsSource {
			t.Fatalf("got %q", d.ReasonCode)
		}
	})
	t.Run("within bounds", func(t *testing.T) {
		d := engineWanting(2).Decide(ctx, newAppWithWindow(2, 60), now)
		if d.ReasonCode != ReasonWithinBounds {
			t.Fatalf("got %q", d.ReasonCode)
		}
	})
	t.Run("scale up", func(t *testing.T) {
		d := engineWanting(6).Decide(ctx, newAppWithWindow(2, 60), now)
		if d.ReasonCode != ReasonScaleUp {
			t.Fatalf("got %q", d.ReasonCode)
		}
	})
	t.Run("scale down", func(t *testing.T) {
		d := engineWanting(1).Decide(ctx, newAppWithWindow(4, 0), now)
		if d.ReasonCode != ReasonScaleDown {
			t.Fatalf("got %q", d.ReasonCode)
		}
	})
	t.Run("cooldown", func(t *testing.T) {
		app := newApp(4, 1, 10, 120, qpsTarget())
		zero := int32(0)
		app.Spec.Scaling.ScaleDownStabilizationSeconds = &zero
		last := metav1.NewTime(frozen.Add(-10 * time.Second))
		app.Status.LastScalingAt = &last
		d := engineWanting(2).Decide(ctx, app, now)
		if d.ReasonCode != ReasonCooldown {
			t.Fatalf("got %q", d.ReasonCode)
		}
	})
}
