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

// Package metrics 导出 KubeStasis 自身的决策指标。
//
// 为什么需要它：在此之前，"这次为什么没扩"只能翻 Events（只在有动作时才留痕）
// 或翻日志（按时间翻，无法聚合）。有了 decisions_total{reason}，它就是一次查询：
//
//	# 过去 10 分钟各原因码的决策分布——None 占绝大多数说明集群很稳
//	sum by (action, reason) (rate(kubestasis_scaling_decisions_total[10m]))
//
//	# 某个 App 被稳定窗口拦了多少次（配的窗口是不是过长？）
//	sum by (reason) (increase(kubestasis_scaling_decisions_total{app="bench-ks",action="None"}[1h]))
//
// 指标名的 reason 一律用 scaler.Reason* 这套固定枚举，**不能**用 Decision.Reason
// 的自由文本：后者带秒数和副本数（"blocked by stabilization window (17s/60s)"），
// 每轮一个新 label，几小时就能把 Prometheus 的时序数打爆。
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	appsv1alpha1 "github.com/weirong-ui/kubestasis/api/v1alpha1"
	"github.com/weirong-ui/kubestasis/internal/scaler"
)

var (
	decisionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kubestasis_scaling_decisions_total",
		Help: "伸缩决策次数，按命名空间/App/动作/原因码切分；action=None 的样本回答\"为什么没动\"。",
	}, []string{"namespace", "app", "action", "reason"})

	desiredReplicas = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kubestasis_scaling_desired_replicas",
		Help: "本轮决策算出的期望副本数；没有伸缩动作时等于当前副本数。",
	}, []string{"namespace", "app"})

	currentReplicas = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kubestasis_scaling_current_replicas",
		Help: "决策时刻的 spec.replicas（不是就绪数）；与 desired 的差值说明被哪个窗口挡住了。",
	}, []string{"namespace", "app"})

	signalReplicas = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kubestasis_scaling_signal_replicas",
		Help: "各信号独立算出的期望副本数——最终决策取其中的最大值，可用来看是哪个信号在推动伸缩。",
	}, []string{"namespace", "app", "signal"})
)

func init() {
	metrics.Registry.MustRegister(decisionsTotal, desiredReplicas, currentReplicas, signalReplicas)
}

// Record 在每轮 Reconcile 决策后调用（无论是否真的伸缩）。
//
// 一定要在 Action=None 时也记录：只统计"动过"的决策，就无法回答
// "为什么一直没扩"，而这恰恰是伸缩类项目最常见的问题。
func Record(app *appsv1alpha1.App, d scaler.Decision) {
	decisionsTotal.WithLabelValues(app.Namespace, app.Name, string(d.Action), d.ReasonCode).Inc()
	desiredReplicas.WithLabelValues(app.Namespace, app.Name).Set(float64(d.ToReplicas))
	currentReplicas.WithLabelValues(app.Namespace, app.Name).Set(float64(d.FromReplicas))
	for sig, n := range d.Signals {
		signalReplicas.WithLabelValues(app.Namespace, app.Name, string(sig)).Set(float64(n))
	}
}

// Forget 清理某个 App 的所有时序。App 被删除时必须调用：
// CounterVec/GaugeVec 不会自己回收 label 组合，反复创建删除 App 会留下永久残留。
func Forget(namespace, name string) {
	match := prometheus.Labels{"namespace": namespace, "app": name}
	decisionsTotal.DeletePartialMatch(match)
	desiredReplicas.DeletePartialMatch(match)
	currentReplicas.DeletePartialMatch(match)
	signalReplicas.DeletePartialMatch(match)
}
