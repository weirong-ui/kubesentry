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
	"fmt"
	"math"
	"time"

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

// Engine 伸缩决策引擎。只做"决策"，不碰集群——这样 Decision 可以纯函数级单元测试.
type Engine struct {
	// nowFunc 注入时钟，测试时可控制时间推进冷却窗口
	nowFunc func() time.Time
}

func NewEngine() *Engine {
	return &Engine{nowFunc: time.Now}
}

// Decide 根据冷却窗口与迟滞规则判断是否伸缩。
// 注意：指标查询（Prometheus）由 healer/scaler 的上层调用方注入，
// 本方法保持纯决策逻辑以便测试；实际指标通过 signals 传入。
func (e *Engine) Decide(app *appsv1alpha1.App, nowFn func() time.Time) Decision {
	e.nowFunc = nowFn
	current := int32(1)
	if app.Spec.Replicas != nil {
		current = *app.Spec.Replicas
	}

	policy := app.Spec.Scaling
	signals := make(map[appsv1alpha1.MetricType]int32, len(policy.Metrics))
	var desired int32 = policy.MinReplicas

	// TODO(P1): 接入 Prometheus 查询后，此处遍历 policy.Metrics，
	// 按各信号计算 desired 并取 max。当前骨架先以 current 为基准。
	for _, m := range policy.Metrics {
		signals[m.Type] = current
		if signals[m.Type] > desired {
			desired = signals[m.Type]
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
	if diff := desired - current; diff <= hysteresis && diff >= -hysteresis {
		return Decision{Action: ActionNone, FromReplicas: current, ToReplicas: current, Signals: signals}
	}

	// 冷却窗口：冷却期内拒绝缩容（扩容放行）
	if e.inCooldown(app) && desired < current {
		return Decision{Action: ActionNone, FromReplicas: current, ToReplicas: current,
			Reason: "scale down blocked by cooldown window", Signals: signals}
	}

	// 缩容渐进：单次最多 -20%
	if desired < current {
		floor := int32(math.Ceil(float64(current) * 0.8))
		if desired < floor {
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

// Apply 将决策落到 Deployment 并回写 Status.LastScalingAt.
// TODO(P1): 实现真实写路径 + 失败重试语义.
func (e *Engine) Apply(_ interface{}, _ interface{}, _ *appsv1alpha1.App, _ interface{}, _ Decision) error {
	return nil
}
