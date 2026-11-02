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

// Package scaler 实现多信号伸缩决策引擎。
//
// 设计要点（对应掘金文章 03《为什么 HPA 会震荡》）：
//  1. 多信号取最紧急者：desired = max(ceil(replicas × current/target))
//  2. 冷却窗口：冷却期内拒绝一切缩容，扩容可紧急放行
//  3. 迟滞带：目标与当前差值 ≤ max(1, 10%) 时不动作
//  4. 缩容渐进：每次最多 -20%；扩容一步到位（不对称策略）
package scaler

import (
	"context"
	"fmt"
	"math"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1alpha1 "github.com/weirong-ui/kubesentry/api/v1alpha1"
)

type Action string

const (
	ActionNone      Action = "None"
	ActionScaleUp   Action = "ScaleUp"
	ActionScaleDown Action = "ScaleDown"
)

// Decision 一次伸缩决策的完整上下文，用于 Event 留痕与单元测试断言.
type Decision struct {
	Action       Action
	FromReplicas int32
	ToReplicas   int32
	Reason       string
	// Signals 各信号独立计算出的期望副本数，方便调试和可视化
	Signals map[appsv1alpha1.MetricType]int32
}

func (d Decision) Describe() string {
	return fmt.Sprintf("%s: %d -> %d (%s)", d.Action, d.FromReplicas, d.ToReplicas, d.Reason)
}

// MetricsSource 指标来源（P1 由 Prometheus Query API 实现）。
// 未注入时 Decide 会明确返回"无指标源"，而不是拿当前副本数冒充信号——
// 后者会让伸缩"看起来在工作"，实际永远不触发，是最难查的一类 bug。
type MetricsSource interface {
	// DesiredReplicas 返回该指标下应存在的副本数；无数据时 ok=false。
	DesiredReplicas(target appsv1alpha1.MetricTarget, current int32) (replicas int32, ok bool)
}

// Engine 伸缩决策引擎。只做"决策"，不碰集群——这样 Decision 可以纯函数级单元测试.
type Engine struct {
	// nowFunc 注入时钟，测试时可控制时间推进冷却窗口
	nowFunc func() time.Time
	// source 指标来源，P1 接入 Prometheus
	source MetricsSource
}

func NewEngine() *Engine {
	return &Engine{nowFunc: time.Now}
}

// WithMetricsSource 注入指标来源（P1）。未注入时伸缩能力保持休眠。
func (e *Engine) WithMetricsSource(src MetricsSource) *Engine {
	e.source = src
	return e
}

// Decide 根据冷却窗口与迟滞规则判断是否伸缩。
// 注意：指标查询（Prometheus）由上层注入，本方法只负责决策，便于纯逻辑单测。
func (e *Engine) Decide(app *appsv1alpha1.App, nowFn func() time.Time) Decision {
	e.nowFunc = nowFn
	current := int32(1)
	if app.Spec.Replicas != nil {
		current = *app.Spec.Replicas
	}

	policy := app.Spec.Scaling
	signals := make(map[appsv1alpha1.MetricType]int32, len(policy.Metrics))
	var desired int32 = policy.MinReplicas

	if e.source == nil {
		return Decision{Action: ActionNone, FromReplicas: current, ToReplicas: current,
			Reason: "no metrics source configured (P1)", Signals: signals}
	}

	// 多信号取最紧急者
	for _, m := range policy.Metrics {
		want, ok := e.source.DesiredReplicas(m, current)
		if !ok {
			continue // 该信号暂时没有数据，跳过而不是当成 0
		}
		signals[m.Type] = want
		if want > desired {
			desired = want
		}
	}

	// 约束上下限
	if desired < policy.MinReplicas {
		desired = policy.MinReplicas
	}
	if desired > policy.MaxReplicas {
		desired = policy.MaxReplicas
	}

	// 迟滞带：差值太小不动
	hysteresis := int32(math.Max(1, float64(current)*0.1))
	withinBand := desired-current <= hysteresis && current-desired <= hysteresis
	// 例外：回落到 minReplicas 不受迟滞带约束，否则 3 副本时永远缩不回 2
	backToMin := desired == policy.MinReplicas && current > policy.MinReplicas
	if withinBand && !backToMin {
		return Decision{Action: ActionNone, FromReplicas: current, ToReplicas: current, Signals: signals}
	}

	// 冷却窗口：冷却期内拒绝缩容（扩容放行）
	if e.inCooldown(app) && desired < current {
		return Decision{Action: ActionNone, FromReplicas: current, ToReplicas: current,
			Reason: "scale down blocked by cooldown window", Signals: signals}
	}

	// 缩容渐进：单次最多减 20%，且至少减 1 个。
	// 注意不能写成 ceil(current*0.8)：current=2/3/4 时 ceil 回到原值，永远缩不下去。
	if desired < current {
		step := int32(math.Ceil(float64(current) * 0.2))
		if step < 1 {
			step = 1
		}
		if floor := current - step; desired < floor {
			desired = floor
		}
	}

	action := ActionNone
	reason := "within bounds"
	if desired > current {
		action, reason = ActionScaleUp, "load above targets"
	} else if desired < current {
		action, reason = ActionScaleDown, "load below targets"
	}
	return Decision{Action: action, FromReplicas: current, ToReplicas: desired, Reason: reason, Signals: signals}
}

func (e *Engine) inCooldown(app *appsv1alpha1.App) bool {
	if app.Status.LastScalingAt == nil {
		return false
	}
	cooldown := time.Duration(app.Spec.Scaling.CooldownSeconds) * time.Second
	return e.nowFunc().Sub(app.Status.LastScalingAt.Time) < cooldown
}

// Apply 把决策落回 CR：改 spec.replicas，让 Reconcile 自然收敛到 Deployment。
// 不直接改 Deployment —— 那样会和 ensureDeployment 打架（下一轮又被 spec 覆盖）。
// 这是声明式的关键：控制器只维护"期望状态"，实际副本数交给 Deployment 控制器。
func (e *Engine) Apply(ctx context.Context, c client.Client, app *appsv1alpha1.App, d Decision) error {
	if d.Action == ActionNone {
		return nil
	}
	if d.ToReplicas == d.FromReplicas {
		return nil
	}
	target := d.ToReplicas
	app.Spec.Replicas = &target
	if err := c.Update(ctx, app); err != nil {
		return fmt.Errorf("write back replicas %d->%d: %w", d.FromReplicas, target, err)
	}
	return nil
}
