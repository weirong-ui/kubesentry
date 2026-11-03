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

// Package healer 实现分级故障自愈引擎。
//
// 设计原则（对应掘金文章 04《给 K8s 加一个"自动值班员"》）：
//   - 分级动作：L1 扩容 → L2 摘除不健康副本 → L3 回滚到上一个版本
//   - 每次 Reconcile 只执行一级动作，动作之间必须留观察期（HealCooldown），
//     避免"扩容+回滚"同时发生破坏事故现场
//   - 升级路径由 App.Status.LastHealLevel/LastHealAt 驱动：冷却期内同一故障
//     再次出现才升级，而不是一口气冲到 L3
//   - 所有动作通过 Kubernetes Event 审计留痕
package healer

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1alpha1 "github.com/weirong-ui/kubesentry/api/v1alpha1"
)

// Level 自愈动作等级.
type Level int

const (
	L1ScaleUp  Level = iota // 缓解流量/资源型问题
	L2Isolate               // 删除不健康 Pod，让 Deployment 重建新实例
	L3Rollback              // 回退到上一个健康版本
)

// 托管 Pod 的标签（与 controller.selectorLabels 保持一致）。
const (
	labelManagedBy = "app.kubernetes.io/managed-by"
	labelApp       = "app"

	// revisionAnnotation 是 Deployment controller 写在 ReplicaSet 上的修订号。
	revisionAnnotation = "deployment.kubernetes.io/revision"
)

// ErrorRateSource 错误率查询源（金丝雀窗口用）。由 scaler.PrometheusSource 实现，
// 未注入时 CheckCanary 恒为 false —— 没有数据就不动作，绝不能猜。
type ErrorRateSource interface {
	// ErrorRate 返回当前 5xx 错误率（0~1）；无数据时 ok=false。
	ErrorRate(ctx context.Context, app *appsv1alpha1.App) (rate float64, ok bool)
}

// Report 一次健康检查的结果.
type Report struct {
	Unhealthy bool
	// Signals 命中的故障信号，如 "CrashLoopBackOff(pod/order-abc, restarts=8)"
	Signals []string
	// Pods 不健康 Pod 的名字，L2 动作的对象
	Pods []string
	// SuggestedLevel 建议的自愈等级
	SuggestedLevel Level
}

func (r Report) Summary() string {
	if !r.Unhealthy {
		return ""
	}
	return fmt.Sprintf("unhealthy: %v, suggested level: L%d", r.Signals, r.SuggestedLevel+1)
}

// Action 已执行的自愈动作，用于 Event 留痕与 status 回写.
type Action struct {
	Level     Level
	Name      string
	Detail    string
	Timestamp time.Time
}

func (a Action) Describe() string {
	return fmt.Sprintf("L%d %s: %s", a.Level+1, a.Name, a.Detail)
}

// Engine 分级自愈引擎。Inspect 只读集群，Apply 才写.
type Engine struct {
	// ErrSource 错误率查询源，nil 时金丝雀检查恒为"通过"（无数据不动作）
	ErrSource ErrorRateSource

	// RestartThreshold 单容器重启次数达到该值即视为异常信号（兜底，
	// 主要识别路径是 waiting.reason / lastState.terminated.reason）
	RestartThreshold int32

	// HealCooldown 同一 App 两次自愈动作之间的观察期：动作需要时间生效，
	// 冷却期内同一故障再次出现才升级到下一级
	HealCooldown time.Duration

	// nowFunc 注入时钟，测试时可控制冷却窗口
	nowFunc func() time.Time
}

// NewEngine 构造自愈引擎。默认值面向生产：重启阈值 5、自愈观察期 3 分钟。
func NewEngine() *Engine {
	return &Engine{
		RestartThreshold: 5,
		HealCooldown:     3 * time.Minute,
		nowFunc:          time.Now,
	}
}

// Inspect 列出本 App 的 Pod，识别故障模式：
//   - CrashLoopBackOff: containerStatuses[].state.waiting.reason
//   - OOMKilled:        lastState.terminated.reason（当前 terminated 也算）
//   - 重启次数超阈值:    兜底信号，覆盖其他 waiting reason（如 ImagePullBackOff 之外的）
//
// 分级建议的映射逻辑：
//   - OOMKilled / 高频重启 → L1 扩容（吸收压力、分散内存负载）
//   - CrashLoopBackOff     → L2 摘除重建（扩容救不了坏代码/坏配置）
//
// 只读：本函数不写任何对象。
func (e *Engine) Inspect(ctx context.Context, c client.Client, app *appsv1alpha1.App) Report {
	var podList corev1.PodList
	if err := c.List(ctx, &podList,
		client.InNamespace(app.Namespace),
		client.MatchingLabels{labelManagedBy: "kubesentry", labelApp: app.Name},
	); err != nil {
		// 读失败不算"健康"，但也绝不误动作：返回不健康=false，错误交给日志。
		// 自愈动作的前提是证据确凿，读不到证据时正确选择是"不动"。
		return Report{Unhealthy: false}
	}

	report := Report{SuggestedLevel: L1ScaleUp}
	seen := map[string]bool{} // 按 pod+container 去重信号

	for i := range podList.Items {
		pod := &podList.Items[i]
		// 正在删除/已结束的 Pod 不是故障目标
		if !pod.DeletionTimestamp.IsZero() || pod.Status.Phase == corev1.PodSucceeded {
			continue
		}
		podUnhealthy := false
		for _, cs := range pod.Status.ContainerStatuses {
			key := pod.Name + "/" + cs.Name

			if cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff" {
				if !seen[key] {
					report.Signals = append(report.Signals,
						fmt.Sprintf("CrashLoopBackOff(%s, restarts=%d)", key, cs.RestartCount))
					seen[key] = true
				}
				podUnhealthy = true
				// CrashLoop 说明实例本身起不来，扩容只会多几个起不来的实例 → L2
				if report.SuggestedLevel < L2Isolate {
					report.SuggestedLevel = L2Isolate
				}
			}

			term := cs.LastTerminationState.Terminated
			if cs.State.Terminated != nil {
				term = cs.State.Terminated
			}
			if term != nil && term.Reason == "OOMKilled" {
				if !seen[key+"#oom"] {
					report.Signals = append(report.Signals, fmt.Sprintf("OOMKilled(%s)", key))
					seen[key+"#oom"] = true
				}
				podUnhealthy = true // OOM 保持 L1：先扩容分摊
			}

			if cs.RestartCount >= e.RestartThreshold {
				if !seen[key+"#restarts"] {
					report.Signals = append(report.Signals,
						fmt.Sprintf("HighRestarts(%s, restarts=%d)", key, cs.RestartCount))
					seen[key+"#restarts"] = true
				}
				podUnhealthy = true
			}
		}
		if podUnhealthy {
			report.Pods = append(report.Pods, pod.Name)
		}
	}

	report.Unhealthy = len(report.Signals) > 0
	return report
}

// InCanaryWindow 判断当前是否处于金丝雀观察窗口内：
// 以上一次镜像变更时间（status.lastImageChangeAt）+ observationSeconds 计算。
func (e *Engine) InCanaryWindow(app *appsv1alpha1.App) bool {
	if app.Spec.Health == nil || !app.Spec.Health.Canary.Enabled {
		return false
	}
	if app.Status.LastImageChangeAt == nil {
		return false
	}
	obs := app.Spec.Health.Canary.ObservationSeconds
	if obs <= 0 {
		obs = 300
	}
	return e.nowFunc().Sub(app.Status.LastImageChangeAt.Time) < time.Duration(obs)*time.Second
}

// CheckCanary 查询观察窗口内错误率是否超过阈值。
// 无错误率数据源或查不到数据时返回 false：宁可放过，绝不误回滚。
func (e *Engine) CheckCanary(ctx context.Context, app *appsv1alpha1.App) bool {
	if e.ErrSource == nil || app.Spec.Health == nil {
		return false
	}
	threshold := 0.01
	if s := app.Spec.Health.Canary.ErrorRateThreshold; s != "" {
		if v, err := strconv.ParseFloat(s, 64); err == nil && v > 0 {
			threshold = v
		}
	}
	rate, ok := e.ErrSource.ErrorRate(ctx, app)
	return ok && rate > threshold
}

// Apply 执行自愈动作（一次只执行一级），并处理升级逻辑：
// 冷却期内同一故障再次出现 → 升级到下一级；已到 L3 且在冷却期内 → 不动作（观察中）。
func (e *Engine) Apply(ctx context.Context, c client.Client, app *appsv1alpha1.App,
	deploy *appsv1.Deployment, report Report) (*Action, error) {

	if !report.Unhealthy {
		return nil, nil
	}

	level := report.SuggestedLevel
	if app.Status.LastHealAt != nil &&
		e.nowFunc().Sub(app.Status.LastHealAt.Time) < e.HealCooldown &&
		int(level) <= app.Status.LastHealLevel {
		level = Level(app.Status.LastHealLevel + 1) // 上次动作没治好，升级
	}
	if level > L3Rollback {
		// 已回滚过还在坏：给系统留观察时间，不要原地空转
		return nil, nil
	}

	now := e.nowFunc()
	switch level {
	case L1ScaleUp:
		return e.scaleUp(ctx, c, app, now)
	case L2Isolate:
		return e.isolate(ctx, c, app, report.Pods, now)
	default:
		return e.rollbackAction(ctx, c, app, deploy, now)
	}
}

// scaleUp L1：在 spec.replicas 基础上扩 20%（至少 +1），受 scaling.maxReplicas 约束。
// 写回 App.spec 而不是直接改 Deployment——声明式：下一轮 Reconcile 自然收敛，
// 审计上也能看到"是 KubeSentry 改的 spec"。
func (e *Engine) scaleUp(ctx context.Context, c client.Client, app *appsv1alpha1.App, now time.Time) (*Action, error) {
	current := int32(1)
	if app.Spec.Replicas != nil {
		current = *app.Spec.Replicas
	}
	step := int32(math.Ceil(float64(current) * 0.2))
	if step < 1 {
		step = 1
	}
	want := current + step
	if app.Spec.Scaling != nil && want > app.Spec.Scaling.MaxReplicas {
		want = app.Spec.Scaling.MaxReplicas
	}
	if want <= current {
		return nil, nil // 已到上限，等升级
	}

	app.Spec.Replicas = &want
	if err := c.Update(ctx, app); err != nil {
		return nil, fmt.Errorf("L1 scale up: %w", err)
	}
	return &Action{
		Level: L1ScaleUp, Name: "ScaleUp",
		Detail:    fmt.Sprintf("replicas %d -> %d (absorb pressure)", current, want),
		Timestamp: now,
	}, nil
}

// isolate L2：删除不健康 Pod。Deployment 会立刻补新实例——等价于"摘除坏副本"，
// 但不需要动 Service selector（打标签摘除的方案在 Pod 被重建后标签会丢，反而不可靠）。
func (e *Engine) isolate(ctx context.Context, c client.Client, app *appsv1alpha1.App,
	pods []string, now time.Time) (*Action, error) {

	if len(pods) == 0 {
		return nil, nil
	}
	deleted := 0
	for _, name := range pods {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: app.Namespace}}
		if err := c.Delete(ctx, pod); err != nil {
			return nil, fmt.Errorf("L2 delete pod %s: %w", name, err)
		}
		deleted++
	}
	return &Action{
		Level: L2Isolate, Name: "Isolate",
		Detail:    fmt.Sprintf("deleted %d unhealthy pod(s): %v", deleted, pods),
		Timestamp: now,
	}, nil
}

// rollbackAction L3：回滚到上一个不同镜像的 ReplicaSet 版本。
func (e *Engine) rollbackAction(ctx context.Context, c client.Client, app *appsv1alpha1.App,
	deploy *appsv1.Deployment, now time.Time) (*Action, error) {

	rev, err := e.Rollback(ctx, c, app, deploy)
	if err != nil {
		return nil, fmt.Errorf("L3 rollback: %w", err)
	}
	if rev == 0 {
		// 没有更早的版本可退：返回 nil 动作，事件里由控制器说明
		return nil, nil
	}
	return &Action{
		Level: L3Rollback, Name: "Rollback",
		Detail:    fmt.Sprintf("image %s -> %s (revision %d)", deployImage(deploy), app.Spec.Image, rev),
		Timestamp: now,
	}, nil
}

// Rollback 执行回滚：读取 ReplicaSet 修订历史，回退到上一个不同镜像的版本。
//
// 返回回滚到的 revision；0 表示没有可回滚的历史版本（不是错误）。
// 实现上改的是 App.Spec.Image 而不是 Deployment——回到 CR 层面让 Reconcile
// 自然收敛，这是声明式 API 的优雅之处：回滚和正常发布走同一条路径。
func (e *Engine) Rollback(ctx context.Context, c client.Client, app *appsv1alpha1.App,
	deploy *appsv1.Deployment) (int64, error) {

	var rsList appsv1.ReplicaSetList
	if err := c.List(ctx, &rsList,
		client.InNamespace(app.Namespace),
		client.MatchingLabels{labelApp: app.Name},
	); err != nil {
		return 0, fmt.Errorf("list replicasets: %w", err)
	}

	currentImage := deployImage(deploy)
	type candidate struct {
		revision int64
		image    string
	}
	var best *candidate
	for i := range rsList.Items {
		rs := &rsList.Items[i]
		rev, err := strconv.ParseInt(rs.Annotations[revisionAnnotation], 10, 64)
		if err != nil || len(rs.Spec.Template.Spec.Containers) == 0 {
			continue
		}
		img := rs.Spec.Template.Spec.Containers[0].Image
		if img == currentImage {
			continue // 当前版本不算回滚目标
		}
		if best == nil || rev > best.revision {
			best = &candidate{revision: rev, image: img}
		}
	}
	if best == nil {
		return 0, nil
	}

	app.Spec.Image = best.image
	if err := c.Update(ctx, app); err != nil {
		return 0, fmt.Errorf("update app image: %w", err)
	}
	return best.revision, nil
}

func deployImage(deploy *appsv1.Deployment) string {
	if deploy == nil || len(deploy.Spec.Template.Spec.Containers) == 0 {
		return ""
	}
	return deploy.Spec.Template.Spec.Containers[0].Image
}
