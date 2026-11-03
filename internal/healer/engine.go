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
//   - 分级动作：L1 扩容 → L2 降级（摘除不健康副本）→ L3 回滚
//   - 每次 Reconcile 只执行一级动作，动作之间必须留观察期，
//     避免"扩容+回滚"同时发生破坏事故现场
//   - 所有动作通过 Kubernetes Event 审计留痕
package healer

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1alpha1 "github.com/weirong-ui/kubesentry/api/v1alpha1"
)

// Level 自愈动作等级.
type Level int

const (
	L1ScaleUp  Level = iota // 缓解流量型问题
	L2Isolate               // 摘除不健康副本，切断坏实例流量
	L3Rollback              // 回退到上一个健康版本
)

// Report 一次健康检查的结果.
type Report struct {
	Unhealthy bool
	// Signals 命中的故障信号，如 "CrashLoopBackOff(pod/order-abc, restarts=8)"
	Signals []string
	// SuggestedLevel 建议的自愈等级
	SuggestedLevel Level
}

func (r Report) Summary() string {
	if !r.Unhealthy {
		return ""
	}
	return fmt.Sprintf("unhealthy: %v, suggested level: L%d", r.Signals, r.SuggestedLevel+1)
}

// Action 已执行的自愈动作，用于 Event 留痕.
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
	// promClient TODO(P2): 注入 Prometheus 查询客户端，用于错误率/P99 信号
}

// NewEngine 构造自愈引擎。P2 会把 Prometheus 客户端注入进来。
func NewEngine() *Engine {
	return &Engine{}
}

// Inspect 采集 Pod 状态与应用指标，识别故障模式：
//   - CrashLoopBackOff: restartCount 超阈值
//   - OOMKilled: lastState.terminated.reason == "OOMKilled"
//   - 5xx 飙升:   Prometheus 错误率查询（P2）
func (e *Engine) Inspect(ctx context.Context, app *appsv1alpha1.App) Report {
	// TODO(P2): List Pods（ownerReference 关联本 App 的 Deployment），
	// 遍历 ContainerStatuses 判断 restartCount / LastTerminationState.
	return Report{Unhealthy: false}
}

// InCanaryWindow 判断当前是否处于金丝雀观察窗口内.
func (e *Engine) InCanaryWindow(app *appsv1alpha1.App) bool {
	if app.Spec.Health == nil || !app.Spec.Health.Canary.Enabled {
		return false
	}
	// TODO(P2): 基于上一次镜像变更时间 + ObservationSeconds 判断窗口
	return false
}

// CheckCanary 查询观察窗口内错误率是否超过阈值.
func (e *Engine) CheckCanary(_ context.Context, _ *appsv1alpha1.App) bool {
	// TODO(P2): Prometheus 查询 rate(http_requests_total{status=~"5.."}[5m])
	return false
}

// Apply 执行建议等级的自愈动作（一次只执行一级）.
func (e *Engine) Apply(_ context.Context, _ client.Client, _ *appsv1alpha1.App,
	_ *appsv1.Deployment, report Report) (*Action, error) {
	if !report.Unhealthy {
		return nil, nil
	}
	// TODO(P2): 按 report.SuggestedLevel 分发到
	//   L1: deployment.Spec.Replicas++（受 maxReplicas 约束）
	//   L2: 给不健康 Pod 打标签并让 Service selector 摘除
	//   L3: rollout undo（通过修订 annotation 找上一版本镜像）
	return nil, nil
}

// Rollback 执行回滚：读取 ReplicaSet 修订历史，回退到上一个健康版本的镜像.
func (e *Engine) Rollback(_ context.Context, _ client.Client, _ *appsv1alpha1.App,
	_ *appsv1.Deployment) error {
	// TODO(P2):
	//  1. List ReplicaSets（owner = Deployment），按 revision annotation 排序
	//  2. 取上一个非当前版本的 RS 的镜像
	//  3. 更新 App.Spec.Image（回到 CR 层面，让 Reconcile 自然收敛——声明式的优雅之处）
	return fmt.Errorf("rollback: not implemented (P2)")
}
