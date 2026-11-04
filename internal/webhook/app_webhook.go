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

// Package webhook 实现 App CRD 的 Admission Webhook（P3）。
//
// 为什么有了 CRD schema 校验还要 Webhook？两层各管一段：
//   - CRD schema：类型、取值范围、enum、默认值——结构层面，声明在类型注解里
//   - Webhook：跨字段约束（min≤max、replicas 落在 [min,max]）、语义规则
//     （禁 latest、target 字符串能被对应指标类型解析）、同一指标类型不重复——
//     这些超出 OpenAPI schema 的表达力
//
// 与 apiserver 的交互顺序（11.03 真集群实测确认，这决定了代码怎么写）：
// 请求 → 解码时应用 CRD schema 默认值/剪枝 → Mutating Webhook（本文件的
// Defaulter）→ Validating Webhook（本文件的 Validator）→ schema 校验 → 持久化。
//
// 关键推论：**CRD 默认值比 Mutating Webhook 先落地**。所以凡是要做跨字段
// 推导的字段（replicas 跟随 minReplicas），就不能在 CRD 上挂默认值——
// 否则 webhook 看到的永远是被抢先填好的值，跨字段逻辑形同虚设。
// Validator 看到的则是默认值已落全的对象，可以信任字段值。
package webhook

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	appsv1alpha1 "github.com/weirong-ui/kubestasis/api/v1alpha1"
)

// 下面的注解由 controller-gen 生成 config/webhook/manifests.yaml。
// path 必须与 controller-runtime 注册时的默认路径一致（/mutate|validate-<group>-<version>-<kind>），
// 否则 WebhookConfiguration 指向的 URL 在 manager 里不存在，所有 App 的增改都会被拒。
//
// +kubebuilder:webhook:path=/mutate-kubestasis-io-v1alpha1-app,mutating=true,failurePolicy=fail,sideEffects=None,groups=kubestasis.io,resources=apps,verbs=create;update,versions=v1alpha1,name=mapp.kubestasis.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-kubestasis-io-v1alpha1-app,mutating=false,failurePolicy=fail,sideEffects=None,groups=kubestasis.io,resources=apps,verbs=create;update,versions=v1alpha1,name=vapp.kubestasis.io,admissionReviewVersions=v1

// AppDefaulter 注入 schema 默认值表达不了的跨字段默认值。
type AppDefaulter struct{}

var _ admission.CustomDefaulter = AppDefaulter{}

// Default 目前只有一条跨字段规则：replicas 未显式给出时跟随 scaling.minReplicas
// （而不是恒为 1）——否则用户写 minReplicas: 3 期望"起步就 3 个"，
// 第一轮却被当 1 个创建，再被伸缩器扩上去，多一次冷启动抖动。
func (AppDefaulter) Default(_ context.Context, obj runtime.Object) error {
	app, ok := obj.(*appsv1alpha1.App)
	if !ok {
		return fmt.Errorf("expected App, got %T", obj)
	}
	if app.Spec.Replicas == nil {
		replicas := int32(1)
		if app.Spec.Scaling != nil && app.Spec.Scaling.MinReplicas > 1 {
			replicas = app.Spec.Scaling.MinReplicas
		}
		app.Spec.Replicas = &replicas
	}
	return nil
}

// AppValidator App 的准入校验。
type AppValidator struct{}

var _ admission.CustomValidator = AppValidator{}

func (AppValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	app, ok := obj.(*appsv1alpha1.App)
	if !ok {
		return nil, fmt.Errorf("expected App, got %T", obj)
	}
	return validateApp(app)
}

func (AppValidator) ValidateUpdate(_ context.Context, _, newObj runtime.Object) (admission.Warnings, error) {
	app, ok := newObj.(*appsv1alpha1.App)
	if !ok {
		return nil, fmt.Errorf("expected App, got %T", newObj)
	}
	return validateApp(app)
}

func (AppValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil // 删除不做拦截：故障时要能一键拆除
}

// validateApp 全量校验规则。每一条都对应一个真实踩过/可预见的坑，注释里注明动机。
// 返回的 warnings 不会阻断请求，但会出现在 kubectl 输出里——用于"能跑但很可能不是
// 你想要"的场景（特权端口）。
func validateApp(app *appsv1alpha1.App) (admission.Warnings, error) {
	var errs []string
	var warns admission.Warnings

	// 1. 镜像必须钉住确切 tag：latest 与裸镜像名（无 tag 默认 latest）都会让
	//    L3 回滚语义失效——"回滚到上一个镜像"要求镜像是离散可枚举的版本。
	if err := validateImage(app.Spec.Image); err != nil {
		errs = append(errs, err.Error())
	}

	// 2. 特权端口只警告不拒绝：容器内非 root 进程 bind <1024 需要
	//    CAP_NET_BIND_SERVICE，配错了表现为启动失败，但不一定是错误（应用可能以 root 跑）
	if app.Spec.Port > 0 && app.Spec.Port < 1024 {
		warns = append(warns, fmt.Sprintf("spec.port=%d 是特权端口（<1024），容器内非 root 进程将无法绑定", app.Spec.Port))
	}

	if s := app.Spec.Scaling; s != nil {
		// 3. 跨字段：min 不能大于 max（schema 只能各自约束范围，管不了相互关系）
		if s.MinReplicas > s.MaxReplicas {
			errs = append(errs, fmt.Sprintf("spec.scaling.minReplicas(%d) 不能大于 maxReplicas(%d)",
				s.MinReplicas, s.MaxReplicas))
		}
		// 4. 初始副本落在 [min, max] 内：落在外面首轮 Reconcile 就被伸缩器"纠正"，
		//    用户的 spec 与集群实际状态立刻分叉，排障时非常迷惑
		if app.Spec.Replicas != nil {
			r := *app.Spec.Replicas
			if r < s.MinReplicas || r > s.MaxReplicas {
				errs = append(errs, fmt.Sprintf("spec.replicas(%d) 必须落在 [%d, %d] 区间内",
					r, s.MinReplicas, s.MaxReplicas))
			}
		}
		// 5. 指标 target 必须能被对应类型解析，且同一类型不重复
		//    （重复类型在"取 max"语义下没有定义，属于配置笔误）
		seen := map[appsv1alpha1.MetricType]bool{}
		for i, m := range s.Metrics {
			if seen[m.Type] {
				errs = append(errs, fmt.Sprintf("spec.scaling.metrics[%d]: 指标类型 %s 重复", i, m.Type))
				continue
			}
			seen[m.Type] = true
			if err := validateMetricTarget(m); err != nil {
				errs = append(errs, fmt.Sprintf("spec.scaling.metrics[%d] (%s): %v", i, m.Type, err))
			}
		}
	}

	if h := app.Spec.Health; h != nil {
		// 6. 金丝雀阈值：必须能解析成 (0,1) 的小数。解析失败时运行期会静默回退
		//    到 0.01（见 healer.CheckCanary），配置错了用户却不知道——准入期拦下
		if h.Canary.Enabled {
			if v, err := strconv.ParseFloat(h.Canary.ErrorRateThreshold, 64); err != nil || v <= 0 || v >= 1 {
				errs = append(errs, fmt.Sprintf("spec.health.canary.errorRateThreshold=%q 必须是 (0,1) 内的小数，如 \"0.01\"",
					h.Canary.ErrorRateThreshold))
			}
			// 7. 观察窗口太短会误回滚：指标采集+rate 窗口本身就有延迟
			if h.Canary.ObservationSeconds > 0 && h.Canary.ObservationSeconds < 30 {
				errs = append(errs, fmt.Sprintf("spec.health.canary.observationSeconds=%d 太短（<30s），指标还没采到就会误判",
					h.Canary.ObservationSeconds))
			}
		}
	}

	if r := app.Spec.Resources; r != nil {
		// 8. requests 不得超过 limits：配置错了 Pod 直接被 apiserver 拒绝，
		//    但在 App 上只是"一直 Provisioning"，用户看不出根因
		for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			req, hasReq := r.Requests[name]
			lim, hasLim := r.Limits[name]
			if hasReq && hasLim && req.Cmp(lim) > 0 {
				errs = append(errs, fmt.Sprintf("spec.resources.requests.%s(%s) 不能大于 limits(%s)",
					name, req.String(), lim.String()))
			}
		}
		// 9. 声明了 CPU request 却没有 limit：BestEffort 式的"软声明"在
		//    节点压力下最容易被驱逐，且单 Pod 能吃满节点——只警告，不阻断
		if _, hasReq := r.Requests[corev1.ResourceCPU]; hasReq {
			if _, hasLim := r.Limits[corev1.ResourceCPU]; !hasLim {
				warns = append(warns, "spec.resources 声明了 cpu request 但没有 cpu limit：节点压力下该 Pod 可能吃满节点 CPU，也更容易被驱逐")
			}
		}
	}

	// 10. env 的保留变量与重名：APP_NAME/POD_NAMESPACE 由控制器强制注入，
	//     用户声明会被覆盖（清单看着生效、实际不是），必须准入期拦下
	envSeen := map[string]bool{}
	for i, e := range app.Spec.Env {
		if e.Name == "APP_NAME" || e.Name == "POD_NAMESPACE" {
			errs = append(errs, fmt.Sprintf("spec.env[%d]: %q 是保留变量（由控制器注入，用于指标标签对齐）", i, e.Name))
			continue
		}
		if e.Name == "" {
			errs = append(errs, fmt.Sprintf("spec.env[%d]: name 不能为空", i))
			continue
		}
		if envSeen[e.Name] {
			errs = append(errs, fmt.Sprintf("spec.env[%d]: 变量 %q 重复", i, e.Name))
		}
		envSeen[e.Name] = true
	}

	if len(errs) > 0 {
		return warns, fmt.Errorf("App %s/%s 未通过准入校验: %s", app.Namespace, app.Name, strings.Join(errs, "; "))
	}
	return warns, nil
}

// validateImage 强制"显式非 latest tag"。
func validateImage(image string) error {
	// 取最后一个 / 之后的部分看有没有 tag（镜像名里可带 port，如 registry:5000/x:tag，
	// 所以要按最后一个 / 切段再找 :）
	last := image
	if i := strings.LastIndex(image, "/"); i >= 0 {
		last = image[i+1:]
	}
	if !strings.Contains(last, ":") {
		return fmt.Errorf("spec.image %q 没有显式 tag（等价于 :latest），回滚与审计都要求版本可枚举", image)
	}
	if strings.HasSuffix(last, ":latest") {
		return fmt.Errorf("spec.image %q 禁止使用 :latest——它让 L3 回滚失去意义", image)
	}
	return nil
}

// validateMetricTarget 按指标类型校验 target 字符串可解析。
// 规则与 scaler.parseTarget 保持一致（那里是运行期解析，这里是准入期拦截）。
func validateMetricTarget(m appsv1alpha1.MetricTarget) error {
	switch m.Type {
	case appsv1alpha1.MetricQPS, appsv1alpha1.MetricQueueDepth:
		if v, err := strconv.ParseFloat(m.Target, 64); err != nil || v <= 0 {
			return fmt.Errorf("target %q 必须是正数（QPS=每 Pod 每秒请求数 / QueueDepth=每 Pod 排队请求数）", m.Target)
		}
	case appsv1alpha1.MetricErrorRate, appsv1alpha1.MetricKVCacheUsage:
		if v, err := strconv.ParseFloat(m.Target, 64); err != nil || v <= 0 || v >= 1 {
			return fmt.Errorf("target %q 必须是 (0,1) 内的小数", m.Target)
		}
	case appsv1alpha1.MetricP99Latency, appsv1alpha1.MetricTTFT:
		// 裸数字按秒，或 time.ParseDuration 可解析（"200ms"/"1s"）
		if v, err := strconv.ParseFloat(m.Target, 64); err == nil && v > 0 {
			return nil
		}
		if d, err := time.ParseDuration(m.Target); err != nil || d <= 0 {
			return fmt.Errorf("target %q 必须是正秒数或合法 duration（如 \"200ms\"）", m.Target)
		}
	default:
		return fmt.Errorf("未知指标类型 %q", m.Type)
	}
	return nil
}
