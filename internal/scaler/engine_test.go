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

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	appsv1alpha1 "github.com/weirong-ui/kubestasis/api/v1alpha1"
)

// fakeSource 固定返回预设值，让 Decide 的每一道闸都可以独立测试。
type fakeSource struct {
	vals map[appsv1alpha1.MetricType]fakeVal
}

type fakeVal struct {
	replicas int32
	ok       bool
}

func (f fakeSource) DesiredReplicas(_ context.Context, _ *appsv1alpha1.App,
	target appsv1alpha1.MetricTarget, _ int32) (int32, bool) {
	v, ok := f.vals[target.Type]
	return v.replicas, ok && v.ok
}

func newApp(replicas, min, max int32, cooldown int, metrics ...appsv1alpha1.MetricTarget) *appsv1alpha1.App {
	return &appsv1alpha1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "sample", Namespace: "default"},
		Spec: appsv1alpha1.AppSpec{
			Replicas: &replicas,
			Scaling: &appsv1alpha1.ScalingPolicy{
				MinReplicas:     min,
				MaxReplicas:     max,
				CooldownSeconds: cooldown,
				Metrics:         metrics,
			},
		},
	}
}

// TestDecide_NoScalingPolicy 省略 spec.scaling 时必须显式拒绝，
// 不能按零值继续（minReplicas=0 会把副本缩没）。
func TestDecide_NoScalingPolicy(t *testing.T) {
	app := &appsv1alpha1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "bare", Namespace: "default"},
		Spec:       appsv1alpha1.AppSpec{Replicas: int32Ptr3(3)},
	}
	e := NewEngine().WithMetricsSource(fakeSource{vals: map[appsv1alpha1.MetricType]fakeVal{
		appsv1alpha1.MetricQPS: {100, true},
	}})
	d := e.Decide(context.Background(), app, func() time.Time { return frozen })
	if d.Action != ActionNone || d.Reason == "" {
		t.Fatalf("want explicit None with reason, got %+v", d)
	}
}

func int32Ptr3(v int32) *int32 { return &v }

// TestAppSpec_OmitsNilPolicies 锁住真实集群里炸出来的序列化 bug：
// Go 的 omitempty 对结构体不生效——值类型的 Scaling/Health 会被序列化成
// 零值对象（strategy=""），被 CRD 的 enum 校验拒绝，导致 finalizer 的
// Update 失败、对象卡死。指针类型 + omitempty 才能真正省略。
func TestAppSpec_OmitsNilPolicies(t *testing.T) {
	app := &appsv1alpha1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "bare", Namespace: "default"},
		Spec:       appsv1alpha1.AppSpec{Image: "nginx:1.27"},
	}
	b, err := json.Marshal(app)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Contains(s, "scaling") {
		t.Errorf("nil Scaling must be omitted from JSON, got %s", s)
	}
	if strings.Contains(s, "health") {
		t.Errorf("nil Health must be omitted from JSON, got %s", s)
	}
}

func qpsTarget() appsv1alpha1.MetricTarget {
	return appsv1alpha1.MetricTarget{Type: appsv1alpha1.MetricQPS, Target: "1000"}
}

var frozen = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

func TestDecide_NoSource_ReturnsNoneExplicitly(t *testing.T) {
	// 无指标源必须显式返回，而不是拿当前副本数冒充信号（修复前的 bug）
	e := NewEngine()
	d := e.Decide(context.Background(), newApp(5, 2, 20, 120, qpsTarget()), func() time.Time { return frozen })
	if d.Action != ActionNone {
		t.Fatalf("want None, got %s", d.Action)
	}
	if d.Reason == "" {
		t.Fatal("reason should explain why (no metrics source), got empty")
	}
}

func TestDecide_HysteresisBand(t *testing.T) {
	tests := []struct {
		name    string
		current int32
		signal  int32 // QPS 信号算出的期望副本
		want    Action
		wantTo  int32
	}{
		{"差值在带内不动作 current=10±1", 10, 11, ActionNone, 10},
		{"差值超带上沿则扩容", 10, 12, ActionScaleUp, 12},
		{"小副本数带宽至少为 1：current=3 信号 4 在带内", 3, 4, ActionNone, 3},
		{"小副本数信号 5 超带扩容", 3, 5, ActionScaleUp, 5},
		{"回落 minReplicas 不受迟滞约束", 3, 2, ActionScaleDown, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := NewEngine().WithMetricsSource(fakeSource{vals: map[appsv1alpha1.MetricType]fakeVal{
				appsv1alpha1.MetricQPS: {tc.signal, true},
			}})
			d := e.Decide(context.Background(), newApp(tc.current, 2, 20, 0, qpsTarget()), func() time.Time { return frozen })
			if d.Action != tc.want || d.ToReplicas != tc.wantTo {
				t.Fatalf("want %s->%d, got %s->%d", tc.want, tc.wantTo, d.Action, d.ToReplicas)
			}
		})
	}
}

func TestDecide_CooldownBlocksScaleDownOnly(t *testing.T) {
	// 冷却起点 60s 前，窗口 120s：仍在冷却
	scaledAt := metav1.NewTime(frozen.Add(-60 * time.Second))

	tests := []struct {
		name    string
		signal  int32
		want    Action
		wantTo  int32
		wantMsg string
	}{
		{"冷却期内拒绝缩容", 2, ActionNone, 10, "cooldown"},
		{"冷却期内扩容紧急放行", 18, ActionScaleUp, 18, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			app := newApp(10, 2, 20, 120, qpsTarget())
			app.Status.LastScalingAt = &scaledAt
			e := NewEngine().WithMetricsSource(fakeSource{vals: map[appsv1alpha1.MetricType]fakeVal{
				appsv1alpha1.MetricQPS: {tc.signal, true},
			}})
			d := e.Decide(context.Background(), app, func() time.Time { return frozen })
			if d.Action != tc.want || d.ToReplicas != tc.wantTo {
				t.Fatalf("want %s->%d, got %s->%d", tc.want, tc.wantTo, d.Action, d.ToReplicas)
			}
			if tc.wantMsg != "" && !contains(d.Reason, tc.wantMsg) {
				t.Fatalf("reason %q should mention %q", d.Reason, tc.wantMsg)
			}
		})
	}

	t.Run("冷却窗口过期后允许缩容", func(t *testing.T) {
		app := newApp(10, 2, 20, 120, qpsTarget())
		expired := metav1.NewTime(frozen.Add(-180 * time.Second))
		app.Status.LastScalingAt = &expired
		e := NewEngine().WithMetricsSource(fakeSource{vals: map[appsv1alpha1.MetricType]fakeVal{
			appsv1alpha1.MetricQPS: {2, true},
		}})
		d := e.Decide(context.Background(), app, func() time.Time { return frozen })
		// 缩容渐进：10 最多一次缩 2 个 → 8
		if d.Action != ActionScaleDown || d.ToReplicas != 8 {
			t.Fatalf("want ScaleDown->8, got %s->%d", d.Action, d.ToReplicas)
		}
	})
}

// TestDecide_ShrinkIsGradual 验证缩容渐进的 ceil 陷阱：
// 错误写法 ceil(current*0.8) 在 current=2/3/4 时会被 ceil 拉回原值，永远缩不动。
func TestDecide_ShrinkIsGradual(t *testing.T) {
	tests := []struct {
		current int32
		wantTo  int32
	}{
		{2, 1},  // ceil(2*0.2)=1 → 2-1=1
		{3, 2},  // ceil(3*0.2)=1 → 3-1=2
		{4, 3},  // ceil(4*0.2)=1 → 4-1=3
		{5, 4},  // ceil(5*0.2)=1 → 5-1=4
		{10, 8}, // ceil(10*0.2)=2 → 10-2=8
	}
	for _, tc := range tests {
		e := NewEngine().WithMetricsSource(fakeSource{vals: map[appsv1alpha1.MetricType]fakeVal{
			appsv1alpha1.MetricQPS: {1, true}, // 信号指向 min
		}})
		d := e.Decide(context.Background(), newApp(tc.current, 1, 20, 0, qpsTarget()), func() time.Time { return frozen })
		if d.Action != ActionScaleDown {
			t.Fatalf("current=%d: want ScaleDown, got %s", tc.current, d.Action)
		}
		if d.ToReplicas != tc.wantTo {
			t.Fatalf("current=%d: want shrink to %d, got %d", tc.current, tc.wantTo, d.ToReplicas)
		}
	}
}

func TestDecide_ClampsToMinMax(t *testing.T) {
	t.Run("扩容不超上限", func(t *testing.T) {
		e := NewEngine().WithMetricsSource(fakeSource{vals: map[appsv1alpha1.MetricType]fakeVal{
			appsv1alpha1.MetricQPS: {100, true},
		}})
		d := e.Decide(context.Background(), newApp(10, 2, 20, 0, qpsTarget()), func() time.Time { return frozen })
		if d.ToReplicas != 20 {
			t.Fatalf("want clamped to 20, got %d", d.ToReplicas)
		}
	})
	t.Run("全信号无数据逐步缩回 min（不卡死也不瞬间到底）", func(t *testing.T) {
		e := NewEngine().WithMetricsSource(fakeSource{vals: map[appsv1alpha1.MetricType]fakeVal{
			appsv1alpha1.MetricQPS: {0, false}, // 无数据：跳过，不是 0
		}})
		d := e.Decide(context.Background(), newApp(6, 2, 20, 0, qpsTarget()), func() time.Time { return frozen })
		if d.Action != ActionScaleDown || d.ToReplicas != 4 {
			t.Fatalf("want ScaleDown 6->4 (gradual toward min=2), got %s->%d", d.Action, d.ToReplicas)
		}
	})
}

func TestDecide_MultiSignalTakesMax(t *testing.T) {
	// QPS 说 4，错误率说要扩到 12 → 取最紧急者 12
	e := NewEngine().WithMetricsSource(fakeSource{vals: map[appsv1alpha1.MetricType]fakeVal{
		appsv1alpha1.MetricQPS:       {4, true},
		appsv1alpha1.MetricErrorRate: {12, true},
	}})
	app := newApp(10, 2, 20, 0,
		qpsTarget(),
		appsv1alpha1.MetricTarget{Type: appsv1alpha1.MetricErrorRate, Target: "0.01"})
	d := e.Decide(context.Background(), app, func() time.Time { return frozen })
	if d.ToReplicas != 12 {
		t.Fatalf("want max(4,12)=12, got %d", d.ToReplicas)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
