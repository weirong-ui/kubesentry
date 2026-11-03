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

// Package v1alpha1 contains API Schema definitions for the apps v1alpha1 API group.
// +kubebuilder:object:generate=true
// +groupName=apps.sentry.io
package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MetricType 多信号伸缩的指标类型.
// +kubebuilder:validation:Enum=QPS;P99Latency;ErrorRate
type MetricType string

const (
	MetricQPS        MetricType = "QPS"
	MetricP99Latency MetricType = "P99Latency"
	MetricErrorRate  MetricType = "ErrorRate"
)

// HealthStrategy 自愈策略.
// +kubebuilder:validation:Enum=graduated;observability-only
type HealthStrategy string

const (
	StrategyGraduated         HealthStrategy = "graduated"          // 分级自愈：扩容→降级→回滚
	StrategyObservabilityOnly HealthStrategy = "observability-only" // 只告警不动作
)

// AppPhase App 当前生命周期阶段.
// +kubebuilder:validation:Enum=Provisioning;Stable;Healing;RollingBack
type AppPhase string

const (
	PhaseProvisioning AppPhase = "Provisioning"
	PhaseStable       AppPhase = "Stable"
	PhaseHealing      AppPhase = "Healing"
	PhaseRollingBack  AppPhase = "RollingBack"
)

// MetricTarget 描述一个伸缩信号及其目标值.
type MetricTarget struct {
	// Type 指标类型：QPS / P99Latency / ErrorRate
	Type MetricType `json:"type"`

	// Target 目标值，格式随 Type 而定：
	//   QPS:        "1000"   (每 Pod 每秒请求数)
	//   P99Latency: "200ms"
	//   ErrorRate:  "0.01"   (1%)
	// +kubebuilder:validation:MinLength=1
	Target string `json:"target"`
}

// ScalingPolicy 伸缩策略：多信号 + 冷却窗口 + 迟滞带.
type ScalingPolicy struct {
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	MinReplicas int32 `json:"minReplicas"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=3
	MaxReplicas int32 `json:"maxReplicas"`

	// CooldownSeconds 冷却窗口：窗口内的缩容请求一律拒绝（扩容可紧急放行）.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=120
	CooldownSeconds int `json:"cooldownSeconds"`

	// Metrics 伸缩信号列表，决策时取各信号计算结果的最大值.
	// 可省略：未配置指标时伸缩引擎保持休眠（P1 接入 Prometheus 后才有意义）.
	// +optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=5
	Metrics []MetricTarget `json:"metrics,omitempty"`
}

// CanaryPolicy 金丝雀发布观察策略.
type CanaryPolicy struct {
	// +kubebuilder:default=false
	Enabled bool `json:"enabled"`

	// ObservationSeconds 新版本发布后的观察窗口时长.
	// +kubebuilder:default=300
	ObservationSeconds int `json:"observationSeconds"`

	// ErrorRateThreshold 观察窗口内错误率超过该阈值则自动回滚，如 "0.01".
	// +kubebuilder:default="0.01"
	ErrorRateThreshold string `json:"errorRateThreshold"`
}

// HealthPolicy 健康与自愈策略.
type HealthPolicy struct {
	// Strategy graduated: 分级自愈; observability-only: 只记录事件不动作.
	// +kubebuilder:default=graduated
	Strategy HealthStrategy `json:"strategy"`

	// +optional
	Canary CanaryPolicy `json:"canary,omitempty"`
}

// AppSpec defines the desired state of App.
type AppSpec struct {
	// Image 容器镜像，禁止 latest（Validating Webhook 强制）.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	Replicas *int32 `json:"replicas,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=8080
	Port int32 `json:"port"`

	// Scaling 伸缩策略。可省略：省略时不做伸缩决策。
	// 必须是指针：omitempty 对结构体不生效，值类型会把省略的字段
	// 序列化成零值对象（minReplicas=0 等），被 CRD 校验拒绝。
	// +optional
	Scaling *ScalingPolicy `json:"scaling,omitempty"`

	// Health 健康与自愈策略。可省略：省略时不做自愈。指针原因同上。
	// +optional
	Health *HealthPolicy `json:"health,omitempty"`
}

// AppStatus defines the observed state of App.
type AppStatus struct {
	// Phase 当前阶段.
	// +kubebuilder:default=Provisioning
	Phase AppPhase `json:"phase"`

	// ObservedGeneration 本状态对应的 spec 版本，用于判断状态是否已过期.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// CurrentReplicas **就绪副本数**，取自 Deployment.status.readyReplicas。
	// 注意：这是观测值，不是 spec.replicas；两者不一致时才说明还在收敛。
	// +optional
	CurrentReplicas int32 `json:"currentReplicas,omitempty"`

	// DesiredReplicas spec 声明的期望副本数，方便与 CurrentReplicas 对比排障.
	// +optional
	DesiredReplicas int32 `json:"desiredReplicas,omitempty"`

	// LastScalingAt 上一次伸缩动作时间，冷却窗口的判断依据.
	// +optional
	LastScalingAt *metav1.Time `json:"lastScalingAt,omitempty"`

	// LastHealAction 最近一次自愈动作描述.
	// +optional
	LastHealAction string `json:"lastHealAction,omitempty"`

	// RollbackRevision 若发生过回滚，记录回滚到的版本.
	// +optional
	RollbackRevision int64 `json:"rollbackRevision,omitempty"`

	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=app
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.currentReplicas`
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.status.desiredReplicas`
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.spec.image`

// App is the Schema for the apps API.
type App struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AppSpec   `json:"spec,omitempty"`
	Status AppStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AppList contains a list of App.
type AppList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []App `json:"items"`
}

func init() {
	SchemeBuilder.Register(&App{}, &AppList{})
}

// Ensure corev1 is referenced so deepcopy picks up Pod-related types if extended.
var _ = corev1.ObjectReference{}
