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
// +groupName=kubestasis.io
package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MetricType 多信号伸缩的指标类型.
// 通用信号：QPS / P99Latency / ErrorRate。
// 推理场景信号（vLLM 风格的 AI 推理服务）：QueueDepth / TTFT / KVCacheUsage。
// +kubebuilder:validation:Enum=QPS;P99Latency;ErrorRate;QueueDepth;TTFT;KVCacheUsage
type MetricType string

const (
	MetricQPS          MetricType = "QPS"          // 每 Pod 每秒请求数
	MetricP99Latency   MetricType = "P99Latency"   // P99 延迟
	MetricErrorRate    MetricType = "ErrorRate"    // 5xx 错误率
	MetricQueueDepth   MetricType = "QueueDepth"   // 推理队列深度（等待中的请求数/Pod）
	MetricTTFT         MetricType = "TTFT"         // 首 token 延迟（Time To First Token）
	MetricKVCacheUsage MetricType = "KVCacheUsage" // KV cache 占用率（0~1）
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
	//   QPS:          "1000"   (每 Pod 每秒请求数)
	//   P99Latency:   "200ms"
	//   ErrorRate:    "0.01"   (1%)
	//   QueueDepth:   "5"      (每 Pod 排队请求数)
	//   TTFT:         "1s"     (首 token 延迟，同 P99Latency 格式)
	//   KVCacheUsage: "0.85"   (KV cache 占用率上限)
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

	// ScaleDownStepPercent 缩容单步最大比例（当前副本数的百分比，至少缩 1 个）。
	// 扩容不受此限：扩容是应急动作，一步到位；缩容必须渐进，避免震荡。
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=20
	// +optional
	ScaleDownStepPercent int `json:"scaleDownStepPercent,omitempty"`

	// HysteresisPercent 迟滞带：期望副本与当前副本差值 ≤ max(1, current×p%) 时不动作。
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=50
	// +kubebuilder:default=10
	// +optional
	HysteresisPercent int `json:"hysteresisPercent,omitempty"`

	// ErrorRateScaleUpPercent 错误率超阈值时的单次扩容比例。
	// 错误率无法换算成副本数，只能按比例吸收；连续超阈值会在多轮 Reconcile 里持续扩。
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=20
	// +optional
	ErrorRateScaleUpPercent int `json:"errorRateScaleUpPercent,omitempty"`
}

// ScaleDownStep 返回缩容单步比例；0（旧对象未落默认值）时回退到 20。
func (p *ScalingPolicy) ScaleDownStep() int {
	if p == nil || p.ScaleDownStepPercent <= 0 {
		return 20
	}
	return p.ScaleDownStepPercent
}

// Hysteresis 返回迟滞带比例；0 时回退到 10。
func (p *ScalingPolicy) Hysteresis() int {
	if p == nil || p.HysteresisPercent <= 0 {
		return 10
	}
	return p.HysteresisPercent
}

// ErrorRateStep 返回错误率扩容比例；0 时回退到 20。
func (p *ScalingPolicy) ErrorRateStep() int {
	if p == nil || p.ErrorRateScaleUpPercent <= 0 {
		return 20
	}
	return p.ErrorRateScaleUpPercent
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

	// ScaleUpPercent L1 自愈扩容比例（当前副本数的百分比，至少 +1），
	// 受 scaling.maxReplicas 钳制。
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=20
	// +optional
	ScaleUpPercent int `json:"scaleUpPercent,omitempty"`

	// RestartThreshold 单容器重启次数达到该值即视为异常信号（兜底识别路径，
	// 主要识别仍靠 waiting.reason / lastState.terminated.reason）。
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=5
	// +optional
	RestartThreshold int32 `json:"restartThreshold,omitempty"`

	// +optional
	Canary CanaryPolicy `json:"canary,omitempty"`
}

// L1ScaleUpPercent 返回 L1 自愈扩容比例；0 时回退到 20。
func (p *HealthPolicy) L1ScaleUpPercent() int {
	if p == nil || p.ScaleUpPercent <= 0 {
		return 20
	}
	return p.ScaleUpPercent
}

// PodRestartThreshold 返回高频重启判定阈值；0 时回退到 5。
func (p *HealthPolicy) PodRestartThreshold() int32 {
	if p == nil || p.RestartThreshold <= 0 {
		return 5
	}
	return p.RestartThreshold
}

// AppSpec defines the desired state of App.
type AppSpec struct {
	// Image 容器镜像，禁止 latest（Validating Webhook 强制）.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// Replicas 期望副本数。故意**不**给 CRD 默认值：CRD 默认值在解码阶段
	// （Mutating Webhook 之前）就会落进对象，若此处 default=1，webhook 的
	// "replicas 跟随 scaling.minReplicas" 跨字段默认将永远被抢先填 1 而失效
	// （11.03 真集群实测确认此顺序）。省略时的兜底：webhook 模式由
	// Defaulter 注入（跟随 minReplicas）；无 webhook 模式由控制器按 1 处理。
	// +kubebuilder:validation:Minimum=0
	// +optional
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

	// Resources 容器资源声明，透传到托管 Deployment 的 containers[0].resources。
	// 可省略，但强烈建议声明：省略时 Pod 是 BestEffort QoS，既没有 CPU 上限
	// （单 Pod 可能吃掉整个节点），也无法参与基于利用率的伸缩。
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// Env 应用环境变量，透传到托管 Pod。
	// APP_NAME / POD_NAMESPACE 由控制器强制注入（指标标签约定），因此不允许
	// 在此声明（Webhook 拦截）——否则用户的声明会被静默覆盖，排障时极其费解。
	// 按 name 做 map 合并：kubectl apply 改单个变量不会把整组 env 冲掉。
	// +optional
	// +listType=map
	// +listMapKey=name
	Env []corev1.EnvVar `json:"env,omitempty"`
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

	// LastHealLevel 最近一次自愈动作的等级（0=L1 扩容, 1=L2 摘除, 2=L3 回滚），
	// 升级路径的判断依据：冷却期内故障再现时升到下一级.
	// +optional
	LastHealLevel int `json:"lastHealLevel,omitempty"`

	// LastHealAt 最近一次自愈动作时间，自愈冷却（观察期）的判断依据.
	// +optional
	LastHealAt *metav1.Time `json:"lastHealAt,omitempty"`

	// CurrentImage 上一次 Reconcile 观察到的镜像，用于检测"镜像刚变更".
	// +optional
	CurrentImage string `json:"currentImage,omitempty"`

	// LastImageChangeAt 上一次镜像变更时间，金丝雀观察窗口的起点.
	// +optional
	LastImageChangeAt *metav1.Time `json:"lastImageChangeAt,omitempty"`

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
