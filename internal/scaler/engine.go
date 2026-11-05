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
//  2. 冷却窗口：冷却期内拒绝缩容，扩容可紧急放行
//  3. 缩容稳定窗口：信号要连续低于目标够久才缩，防止"突发刚起、均值没抬上来"误缩
//  4. 迟滞带：只挡缩容，扩容直通（对称迟滞会吃掉突发时的扩容）
//  5. 缩容渐进：每次最多 -20%；扩容一步到位（不对称策略）
package scaler

import (
	"context"
	"fmt"
	"math"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1alpha1 "github.com/weirong-ui/kubestasis/api/v1alpha1"
)

type Action string

const (
	ActionNone      Action = "None"
	ActionScaleUp   Action = "ScaleUp"
	ActionScaleDown Action = "ScaleDown"
)

// 决策原因码：**固定枚举**，专门给 metrics 当 label 用。
// 自由文本的 Reason 会带具体秒数/副本数（"blocked by stabilization window (17s/60s)"），
// 直接当 label 就是基数炸弹；两者一个给人看（Events/日志），一个给机器聚合。
const (
	ReasonNoPolicy        = "no_policy"
	ReasonNoMetricsSource = "no_metrics_source"
	ReasonHysteresis      = "hysteresis"
	ReasonStabilizing     = "stabilizing"
	ReasonCooldown        = "cooldown"
	ReasonScaleUp         = "scale_up"
	ReasonScaleDown       = "scale_down"
	ReasonWithinBounds    = "within_bounds"
)

// Decision 一次伸缩决策的完整上下文，用于 Event 留痕与单元测试断言.
type Decision struct {
	Action       Action
	FromReplicas int32
	ToReplicas   int32
	Reason       string
	// ReasonCode 固定的原因枚举（见上面的常量），供 metrics 使用
	ReasonCode string
	// Signals 各信号独立计算出的期望副本数，方便调试和可视化
	Signals map[appsv1alpha1.MetricType]int32
	// BelowTarget 对缩容稳定窗口计时起点的写入建议。
	//
	// 存在的原因：Decide 曾直接改 app.Status.BelowTargetSince，这让一个号称
	// "纯函数可测"的决策函数带了输入副作用——外部代码评审指出过（2026-11-05）。
	// 现在决策只产出建议，由控制器落盘，Decide 不碰输入对象。
	BelowTarget BelowTargetUpdate
}

// BelowTargetUpdate 对 status.belowTargetSince 的写入建议，三态：
//   - Set=false：不改（本次决策与稳定窗口无关）
//   - Set=true, Value=nil：清零（需求回到目标以上，或落在迟滞带内）
//   - Set=true, Value=T：置为计时起点（首次出现缩容候选）
//
// 三态是必须的：nil 在 JSON merge patch 里表示"写 null（清零）"，
// 字段省略才表示"不改"，两者不能用一个 nil 指针表达。
type BelowTargetUpdate struct {
	Set   bool
	Value *metav1.Time
}

// clearedBelowTarget 是"清零"的简写，语义见 BelowTargetUpdate。
func clearedBelowTarget() BelowTargetUpdate { return BelowTargetUpdate{Set: true} }

func (d Decision) Describe() string {
	return fmt.Sprintf("%s: %d -> %d (%s)", d.Action, d.FromReplicas, d.ToReplicas, d.Reason)
}

// MetricsSource 指标来源（P1 由 Prometheus Query API 实现）。
// 未注入时 Decide 会明确返回"无指标源"，而不是拿当前副本数冒充信号——
// 后者会让伸缩"看起来在工作"，实际永远不触发，是最难查的一类 bug。
type MetricsSource interface {
	// DesiredReplicas 返回该指标下应存在的副本数；无数据或查询失败时 ok=false。
	//
	// ok=false 的语义是"我暂时不知道"，调用方必须跳过该信号，
	// **绝不能当成 0 处理** —— 否则 Prometheus 一挂就会把副本缩到 min。
	DesiredReplicas(ctx context.Context, app *appsv1alpha1.App,
		target appsv1alpha1.MetricTarget, current int32) (replicas int32, ok bool)
}

// Engine 伸缩决策引擎。只做"决策"：不碰集群，也不改输入对象——
// 这样 Decision 可以纯函数级单元测试，并发调用也不需要加锁.
type Engine struct {
	// source 指标来源，P1 接入 Prometheus
	source MetricsSource
}

// NewEngine 构造决策引擎。时钟不放在结构体里：每次 Decide 显式传入，
// 既能按调用控制时间，也不会因为共享字段在并发 Reconcile 下互相覆盖。
func NewEngine() *Engine {
	return &Engine{}
}

// WithMetricsSource 注入指标来源（P1）。未注入时伸缩能力保持休眠。
func (e *Engine) WithMetricsSource(src MetricsSource) *Engine {
	e.source = src
	return e
}

// Decide 根据冷却窗口与迟滞规则判断是否伸缩。
// 注意：指标查询（Prometheus）由上层注入，本方法只负责决策，便于纯逻辑单测。
//
// 纯函数约定：不修改 app（包括 status）。对稳定窗口计时起点的写入意图
// 通过 Decision.BelowTarget 返回，由控制器负责落盘——这样同一份输入
// 重复调用结果相同，测试也可以直接断言 Decision 而不是"对象被改了"。
func (e *Engine) Decide(ctx context.Context, app *appsv1alpha1.App, nowFn func() time.Time) Decision {
	now := nowFn()
	current := int32(1)
	if app.Spec.Replicas != nil {
		current = *app.Spec.Replicas
	}

	policy := app.Spec.Scaling
	signals := make(map[appsv1alpha1.MetricType]int32)
	var desired int32
	if policy != nil {
		desired = policy.MinReplicas
	}

	// 没配伸缩策略：明确说明，而不是按零值继续算（minReplicas=0 会把副本缩没）
	if policy == nil {
		return Decision{Action: ActionNone, FromReplicas: current, ToReplicas: current,
			Reason: "no scaling policy configured", ReasonCode: ReasonNoPolicy, Signals: signals}
	}

	if e.source == nil {
		return Decision{Action: ActionNone, FromReplicas: current, ToReplicas: current,
			Reason: "no metrics source configured (P1)", ReasonCode: ReasonNoMetricsSource, Signals: signals}
	}

	// 多信号取最紧急者
	for _, m := range policy.Metrics {
		want, ok := e.source.DesiredReplicas(ctx, app, m, current)
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

	// 迟滞带：**只抑制缩容**，扩容永远直通。
	//
	// 这条不对称是压测逼出来的（2026-11-04，docs/benchmark/）：
	// 对称迟滞带在 current=4、desired=5 时把扩容也挡住了——差 1 落在带内，
	// 于是突发流量下少一个副本（约 62 QPS 承载），2 分钟多出几百个失败请求。
	// 而震荡的根源从来不是"该扩的时候扩了"，而是"刚扩完就缩"。
	// 所以死区只该出现在收缩方向：扩要快（保可用性），缩要稳（保稳定）。
	hysteresis := int32(math.Max(1, float64(current)*float64(policy.Hysteresis())/100))
	// 例外：回落到 minReplicas 不受迟滞带约束，否则 3 副本时永远缩不回 2
	backToMin := desired == policy.MinReplicas && current > policy.MinReplicas
	if desired < current && current-desired <= hysteresis && !backToMin {
		// 带内不动作 = 需求与供给基本匹配，不算"低谷"，稳定窗口的计时清掉
		return Decision{Action: ActionNone, FromReplicas: current, ToReplicas: current,
			Reason: "scale-down within hysteresis band", ReasonCode: ReasonHysteresis,
			Signals: signals, BelowTarget: clearedBelowTarget()}
	}

	// 缩容稳定窗口：需求必须连续低于目标够久，才承认低谷是真的。
	// 计时状态放在 status 里而不是内存里——控制器会重启，内存计时一重启就清零，
	// 恰好在滚动发布时给出一次"窗口已满"的假信号。
	var belowTarget BelowTargetUpdate
	if desired >= current {
		belowTarget = clearedBelowTarget() // 需求回到目标以上：低谷不成立，计时清零
	} else {
		msg, ok, update := e.stabilizeScaleDown(app, now)
		belowTarget = update
		if !ok {
			return Decision{Action: ActionNone, FromReplicas: current, ToReplicas: current,
				Reason: msg, ReasonCode: ReasonStabilizing, Signals: signals,
				BelowTarget: belowTarget}
		}
	}

	// 冷却窗口：冷却期内拒绝缩容（扩容放行）
	if e.inCooldown(app, now) && desired < current {
		return Decision{Action: ActionNone, FromReplicas: current, ToReplicas: current,
			Reason: "scale down blocked by cooldown window", ReasonCode: ReasonCooldown,
			Signals: signals, BelowTarget: belowTarget}
	}

	// 缩容渐进：单次最多减 scaleDownStepPercent%（默认 20%），且至少减 1 个。
	// 注意不能写成 ceil(current*(1-p))：current=2/3/4 时 ceil 回到原值，永远缩不下去。
	if desired < current {
		step := int32(math.Ceil(float64(current) * float64(policy.ScaleDownStep()) / 100))
		if step < 1 {
			step = 1
		}
		if floor := current - step; desired < floor {
			desired = floor
		}
	}

	action := ActionNone
	reason, code := "within bounds", ReasonWithinBounds
	if desired > current {
		action, reason, code = ActionScaleUp, "load above targets", ReasonScaleUp
	} else if desired < current {
		action, reason, code = ActionScaleDown, "load below targets", ReasonScaleDown
	}
	return Decision{Action: action, FromReplicas: current, ToReplicas: desired,
		Reason: reason, ReasonCode: code, Signals: signals, BelowTarget: belowTarget}
}

// stabilizeScaleDown 判断"持续低于目标"是否已经够久，允许缩容。
// 返回 (给人看的原因, 是否放行, 对计时起点的写入建议)。
// 无副作用：不写 app.Status，由调用方把建议带进 Decision、控制器落盘。
func (e *Engine) stabilizeScaleDown(app *appsv1alpha1.App, now time.Time) (string, bool, BelowTargetUpdate) {
	window := time.Duration(app.Spec.Scaling.StabilizationSeconds()) * time.Second
	if window <= 0 {
		return "", true, BelowTargetUpdate{} // 显式关闭，计时无意义
	}
	if app.Status.BelowTargetSince == nil {
		t := metav1.NewTime(now)
		return fmt.Sprintf("scale-down candidate: signals must stay below targets for %s", window),
			false, BelowTargetUpdate{Set: true, Value: &t}
	}
	elapsed := now.Sub(app.Status.BelowTargetSince.Time)
	if elapsed < window {
		return fmt.Sprintf("scale down blocked by stabilization window (%s of %s)",
			elapsed.Truncate(time.Second), window), false, BelowTargetUpdate{}
	}
	return "", true, BelowTargetUpdate{}
}

func (e *Engine) inCooldown(app *appsv1alpha1.App, now time.Time) bool {
	if app.Status.LastScalingAt == nil {
		return false
	}
	cooldown := time.Duration(app.Spec.Scaling.CooldownSeconds) * time.Second
	return now.Sub(app.Status.LastScalingAt.Time) < cooldown
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
	// 冲突重试 + Merge Patch：本轮 Reconcile 之前，status 回写/他方控制器可能
	// 已经动过这个 App，直接 Update 手头这份旧 resourceVersion 会撞
	// "the object has been modified"。真实故障场景下这类竞争更密集，
	// 正确姿势是每次重试都重新 Get 最新版本再 Patch。
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &appsv1alpha1.App{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(app), latest); err != nil {
			return err
		}
		patch := client.MergeFrom(latest.DeepCopy())
		latest.Spec.Replicas = &target
		return c.Patch(ctx, latest, patch)
	}); err != nil {
		return fmt.Errorf("write back replicas %d->%d: %w", d.FromReplicas, target, err)
	}
	// 同步给调用方：本轮 updateStatus 要用新期望值计算 DesiredReplicas
	app.Spec.Replicas = &target
	return nil
}
