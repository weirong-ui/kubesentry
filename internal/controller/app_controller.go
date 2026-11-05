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

package controller

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	appsv1alpha1 "github.com/weirong-ui/kubestasis/api/v1alpha1"
	"github.com/weirong-ui/kubestasis/internal/healer"
	"github.com/weirong-ui/kubestasis/internal/metrics"
	"github.com/weirong-ui/kubestasis/internal/scaler"
)

const (
	appFinalizer = "kubestasis.io/finalizer"

	// 托管资源统一标签，便于 `kubectl get deploy,svc -l app.kubernetes.io/managed-by=kubestasis`.
	labelManagedBy = "app.kubernetes.io/managed-by"
	labelApp       = "app"

	// conditionAvailable 表示期望副本数已全部就绪。
	conditionAvailable = "Available"

	// Event reasons: 统一前缀便于运维检索与简历讲解.
	EventReasonScaleUp    = "KubeStasisScaleUp"
	EventReasonScaleDown  = "KubeStasisScaleDown"
	EventReasonHeal       = "KubeStasisHealAction"
	EventReasonRollback   = "KubeStasisRollback"
	EventReasonCanaryFail = "KubeStasisCanaryFailed"
	EventReasonReconcile  = "KubeStasisReconcile"
	EventReasonError      = "KubeStasisError"
)

// AppReconciler reconciles an App object.
type AppReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Scaler 伸缩决策引擎（多信号 + 冷却 + 迟滞）
	Scaler *scaler.Engine
	// Healer 分级自愈引擎
	Healer *healer.Engine
	// Recorder 用于写 Kubernetes Event。为 nil 时静默跳过，便于纯逻辑单测。
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=kubestasis.io,resources=apps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubestasis.io,resources=apps/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kubestasis.io,resources=apps/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=replicasets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services;events;pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods/status,verbs=get
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

// Reconcile 主调和循环：
//
//  1. 处理删除（Finalizer）
//  2. Ensure Deployment 与 Service 与 spec 一致
//  3. 金丝雀观察窗口检查（失败则回滚，直接结束本轮）
//  4. 健康检查：异常 → Healer；正常 → Scaler
//  5. 回写 Status（真实副本数 + Conditions）
//
// 注意：Reconcile 必须是幂等的、基于最新对象重读的，任何写操作失败都应
// 返回 err 让控制器的指数退避重试，而不是在本地自旋。
func (r *AppReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var app appsv1alpha1.App
	if err := r.Get(ctx, req.NamespacedName, &app); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil // 已删除，无需处理
		}
		return ctrl.Result{}, fmt.Errorf("get app: %w", err)
	}

	// ---------- 1. 删除处理 ----------
	if !app.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&app, appFinalizer) {
			// 增强方向（不在 P0–P4 承诺内）：删除前把自愈审计记录导出到外部存储
			// 时序必须在这里清：CounterVec/GaugeVec 不会自己回收 label 组合，
			// 反复建删同名 App 会留下永远不归零的残留。
			metrics.Forget(app.Namespace, app.Name)
			controllerutil.RemoveFinalizer(&app, appFinalizer)
			if err := r.Update(ctx, &app); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// 首次出现则补 Finalizer
	if !controllerutil.ContainsFinalizer(&app, appFinalizer) {
		controllerutil.AddFinalizer(&app, appFinalizer)
		if err := r.Update(ctx, &app); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	// ---------- 2. Ensure Deployment & Service ----------
	deploy, opErr := r.ensureDeployment(ctx, &app)
	if opErr != nil {
		return r.failOn(ctx, &app, "ensure deployment", opErr)
	}
	if _, opErr := r.ensureService(ctx, &app); opErr != nil {
		return r.failOn(ctx, &app, "ensure service", opErr)
	}

	// ---------- 3. 金丝雀观察窗口 ----------
	if app.Spec.Health != nil && app.Spec.Health.Canary.Enabled && r.Healer.InCanaryWindow(&app) {
		if r.Healer.CheckCanary(ctx, &app) {
			rev, err := r.Healer.Rollback(ctx, r.Client, &app, deploy)
			if err != nil {
				return r.failOn(ctx, &app, "canary rollback", err)
			}
			if rev == 0 {
				// 错误率超阈值，但没有更早的版本可退：只告警，不空转
				r.emitEvent(&app, corev1.EventTypeWarning, EventReasonCanaryFail,
					"error rate exceeded threshold, but no previous revision to roll back to")
				return r.updateStatus(ctx, &app, deploy, statusPatch{phase: appsv1alpha1.PhaseStable})
			}
			r.emitEvent(&app, corev1.EventTypeWarning, EventReasonRollback,
				fmt.Sprintf("canary failed (error rate exceeded %s): rolled back to revision %d",
					app.Spec.Health.Canary.ErrorRateThreshold, rev))
			return r.updateStatus(ctx, &app, deploy, statusPatch{
				phase:            appsv1alpha1.PhaseRollingBack,
				rolledBack:       true,
				rollbackRevision: rev,
			})
		}
		return r.updateStatus(ctx, &app, deploy, statusPatch{phase: appsv1alpha1.PhaseStable})
	}

	// ---------- 4. 健康 → 自愈 / 正常 → 伸缩 ----------
	healthReport := r.Healer.Inspect(ctx, r.Client, &app)
	if healthReport.Unhealthy {
		// observability-only：只留证据不动作，给"先看清楚再动手"的保守场景用
		if app.Spec.Health != nil && app.Spec.Health.Strategy == appsv1alpha1.StrategyObservabilityOnly {
			r.emitEvent(&app, corev1.EventTypeWarning, EventReasonHeal,
				"observability-only, no action taken: "+healthReport.Summary())
			return r.updateStatus(ctx, &app, deploy, statusPatch{
				phase:       appsv1alpha1.PhaseHealing,
				healSummary: healthReport.Summary(),
			})
		}
		action, err := r.Healer.Apply(ctx, r.Client, &app, deploy, healthReport)
		if err != nil {
			return r.failOn(ctx, &app, "heal", err)
		}
		if action != nil {
			r.emitEvent(&app, corev1.EventTypeWarning, EventReasonHeal, action.Describe())
			log.Info("healed", "action", action.Describe())
		} else {
			// 病了但无动作可执行（已到 L3 观察期 / 无版本可退 / 已到副本上限）：
			// 必须留事件，否则"系统病了但 KubeStasis 没动静"会看起来像漏报
			r.emitEvent(&app, corev1.EventTypeWarning, EventReasonHeal,
				"no action available (observing or at limit): "+healthReport.Summary())
		}
		return r.updateStatus(ctx, &app, deploy, statusPatch{
			phase:       appsv1alpha1.PhaseHealing,
			healAction:  action,
			healSummary: healthReport.Summary(),
		})
	}

	// 稳定窗口的计时起点可能被本轮决策改写（首次出现缩容候选时置位、
	// 需求回到目标以上时清零）。Decide 本身不改输入对象，只返回三态建议，
	// 这里落回 status 再单独 patch —— updateStatus 里的 MergeFrom 基线取自
	// 进入它那一刻，早于这里的修改，打不进同一个 patch。
	prevBelowTarget := app.Status.BelowTargetSince
	decision := r.Scaler.Decide(ctx, &app, time.Now)
	if decision.BelowTarget.Set {
		app.Status.BelowTargetSince = decision.BelowTarget.Value
	}

	// 无论动没动都记录：只统计"动过"的决策，就回答不了"为什么一直没扩"
	metrics.Record(&app, decision)

	if !sameTime(prevBelowTarget, app.Status.BelowTargetSince) {
		if err := r.patchBelowTargetSince(ctx, &app); err != nil {
			return r.failOn(ctx, &app, "scale", err)
		}
	}

	if decision.Action != scaler.ActionNone {
		// 冷却起点必须先于 spec 写入落地：spec patch 会立刻触发下一轮 Reconcile，
		// 若那一刻 cache 里的 status 还没带上 LastScalingAt，冷却窗口形同虚设——
		// 压测 run4 抓到 3→2→1 在一个采样窗口内连跳两步（docs/benchmark/）。
		// watch 事件按 resourceVersion 有序送达，先写 status 就能保证后续任何
		// Reconcile 读到的都是"已开冷却"的对象。
		now := metav1.Now()
		app.Status.LastScalingAt = &now
		if err := r.Status().Patch(ctx, &app, client.RawPatch(types.MergePatchType,
			[]byte(`{"status":{"lastScalingAt":"`+now.UTC().Format(time.RFC3339)+`"}}`))); err != nil {
			return r.failOn(ctx, &app, "scale", fmt.Errorf("pre-write lastScalingAt: %w", err))
		}
		if err := r.Scaler.Apply(ctx, r.Client, &app, decision); err != nil {
			return r.failOn(ctx, &app, "scale", err)
		}
		reason := EventReasonScaleUp
		if decision.Action == scaler.ActionScaleDown {
			reason = EventReasonScaleDown
		}
		r.emitEvent(&app, corev1.EventTypeNormal, reason, decision.Describe())
		log.Info("scaled", "from", decision.FromReplicas, "to", decision.ToReplicas)
	}

	// ---------- 5. 回写 Status ----------
	return r.updateStatus(ctx, &app, deploy, statusPatch{phase: appsv1alpha1.PhaseStable, decision: decision})
}

// ensureDeployment 按 App spec 创建或更新 Deployment（幂等的 CreateOrUpdate）。
// 探针在这里生成：就绪探针默认注入，存活探针只在显式配置时注入（原因见 readinessProbe）。
// 安全上下文不在控制器里注入（App 级别），Operator 自身的安全上下文写在 Deployment 清单里。
func (r *AppReconciler) ensureDeployment(ctx context.Context, app *appsv1alpha1.App) (*appsv1.Deployment, error) {
	want := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: app.Name, Namespace: app.Namespace},
	}
	replicas := desiredReplicas(app)

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, want, func() error {
		// 标签要打在 Deployment 本身上，否则 README 里的
		// `kubectl get deploy -l app.kubernetes.io/managed-by=kubestasis` 查不到
		want.Labels = selectorLabels(app)
		want.Spec.Replicas = &replicas
		want.Spec.Selector = &metav1.LabelSelector{MatchLabels: selectorLabels(app)}
		want.Spec.Template.Labels = selectorLabels(app)
		want.Spec.Template.Spec.Containers = []corev1.Container{{
			Name:  app.Name,
			Image: app.Spec.Image,
			Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: appPort(app), Protocol: corev1.ProtocolTCP}},
			// 探针：就绪默认注入（没有它 Pod 一启动就被算 Ready，
			// Service 会把流量打给还没监听的实例）；存活默认不注入
			ReadinessProbe: readinessProbe(app),
			LivenessProbe:  livenessProbe(app),
			// 资源声明透传：省略时保持 nil（BestEffort），声明了就严格照抄，
			// 不在控制器里偷偷加默认值——资源预算必须由使用者显式决定。
			Resources: resourcesOf(app),
			// 身份环境变量必须最后追加且不可被覆盖：PrometheusSource 的 PromQL
			// 依赖 {app}/{namespace} 标签与 App 名、命名空间一致。
			Env: append(userEnv(app),
				corev1.EnvVar{Name: "APP_NAME", Value: app.Name},
				corev1.EnvVar{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
				}},
			),
		}}
		return controllerutil.SetControllerReference(app, want, r.Scheme)
	})
	if err != nil {
		return nil, fmt.Errorf("ensure deployment: %w", err)
	}
	return want, nil
}

// ensureService 创建/更新 ClusterIP Service，使 App 真正可访问。
// selector 必须与 Deployment 的 pod template 标签一致，否则 Endpoints 为空。
func (r *AppReconciler) ensureService(ctx context.Context, app *appsv1alpha1.App) (*corev1.Service, error) {
	port := appPort(app)
	want := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: app.Name, Namespace: app.Namespace},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, want, func() error {
		want.Labels = selectorLabels(app)
		// 只在创建时决定类型，避免每轮把用户改过的字段覆盖回去
		if want.CreationTimestamp.IsZero() {
			want.Spec.Type = corev1.ServiceTypeClusterIP
		}
		want.Spec.Selector = selectorLabels(app)
		want.Spec.Ports = []corev1.ServicePort{{
			Name:       "http",
			Protocol:   corev1.ProtocolTCP,
			Port:       port,
			TargetPort: intstr.FromInt32(port),
		}}
		return controllerutil.SetControllerReference(app, want, r.Scheme)
	})
	if err != nil {
		return nil, fmt.Errorf("ensure service: %w", err)
	}
	return want, nil
}

// sameTime 判断两个时间指针是否表示同一时刻（都为 nil 也算相同）。
func sameTime(a, b *metav1.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Time.Equal(b.Time)
}

// patchBelowTargetSince 单独回写缩容稳定窗口的计时起点。
// nil 语义是"计时清零"，用 JSON null 表达——省略字段的 merge patch 是"不改"，
// 这两者在 JSON merge patch 里必须写清楚，否则低谷过去后计时永远不清。
func (r *AppReconciler) patchBelowTargetSince(ctx context.Context, app *appsv1alpha1.App) error {
	body := `{"status":{"belowTargetSince":null}}`
	if app.Status.BelowTargetSince != nil {
		body = `{"status":{"belowTargetSince":"` +
			app.Status.BelowTargetSince.UTC().Format(time.RFC3339) + `"}}`
	}
	if err := r.Status().Patch(ctx, app, client.RawPatch(types.MergePatchType, []byte(body))); err != nil {
		return fmt.Errorf("write belowTargetSince: %w", err)
	}
	return nil
}

// readinessProbe 生成就绪探针。**即使 spec.readiness 省略也注入**：
// 没有就绪探针时，Pod 一启动就被标记 Ready，Service 会把流量打给还没开始监听
// 的实例，status.currentReplicas（取自 readyReplicas）也会虚高——
// 于是 "Available=True" 变成一个乐观信号。默认用 TCPSocket 而不是 HTTPGet：
// TCP 探针只要求端口在监听，对任何框架都成立；HTTPGet 猜错路径会永远不 Ready。
func readinessProbe(app *appsv1alpha1.App) *corev1.Probe {
	cfg := app.Spec.Readiness
	if cfg == nil {
		cfg = &appsv1alpha1.ProbeConfig{}
	}
	return buildProbe(appPort(app), cfg, 3, 10, 2, 3)
}

// livenessProbe 只在显式配置时注入。存活探针配错的代价是"应用没死却被反复重启"
// （探针依赖的依赖抖一下，健康实例就被 kubelet 打死），就绪探针配错的代价只是
// 晚一点接流量。所以一个默认开、一个默认关，不对称是刻意的。
func livenessProbe(app *appsv1alpha1.App) *corev1.Probe {
	if app.Spec.Liveness == nil {
		return nil
	}
	return buildProbe(appPort(app), app.Spec.Liveness, 15, 20, 3, 3)
}

// buildProbe 把 CRD 里的探针配置翻译成 corev1.Probe，0 值用各探针的默认值。
func buildProbe(port int32, cfg *appsv1alpha1.ProbeConfig,
	defInitial, defPeriod, defTimeout, defFailures int32) *corev1.Probe {

	p := &corev1.Probe{
		InitialDelaySeconds: pickPositive(cfg.InitialDelaySeconds, defInitial),
		PeriodSeconds:       pickPositive(cfg.PeriodSeconds, defPeriod),
		TimeoutSeconds:      pickPositive(cfg.TimeoutSeconds, defTimeout),
		FailureThreshold:    pickPositive(cfg.FailureThreshold, defFailures),
		SuccessThreshold:    1,
	}
	if cfg.Type == appsv1alpha1.ProbeHTTPGet {
		path := cfg.Path
		if path == "" {
			path = "/"
		}
		p.HTTPGet = &corev1.HTTPGetAction{Path: path, Port: intstr.FromInt32(port)}
		return p
	}
	p.TCPSocket = &corev1.TCPSocketAction{Port: intstr.FromInt32(port)}
	return p
}

func pickPositive(v, def int32) int32 {
	if v > 0 {
		return v
	}
	return def
}

// statusPatch 一轮 Reconcile 需要落进 status 的信息。
// 用结构体而不是一长串位置参数：回滚/自愈/伸缩三路径的调用点才不会传错位。
type statusPatch struct {
	phase appsv1alpha1.AppPhase

	// 自愈路径
	healAction  *healer.Action // 本轮实际执行的动作（nil = 观察中/无可执行）
	healSummary string         // Inspect 的原始结论，动作被跳过时也有据可查

	// 伸缩路径
	decision scaler.Decision

	// 金丝雀回滚路径
	rolledBack       bool
	rollbackRevision int64
}

// updateStatus 用观测到的真实状态回写 status，并维护 Conditions。
// 关键：副本数取自 Deployment.status，而不是 spec —— 否则 status 永远是期望值，
// `kubectl get app` 与实际脱节，排障时会误导。
func (r *AppReconciler) updateStatus(ctx context.Context, app *appsv1alpha1.App,
	deploy *appsv1.Deployment, sp statusPatch) (ctrl.Result, error) {

	patch := client.MergeFrom(app.DeepCopy())

	desired := desiredReplicas(app)
	ready := deploy.Status.ReadyReplicas
	wasAvailable := meta.IsStatusConditionTrue(app.Status.Conditions, conditionAvailable)

	app.Status.Phase = sp.phase
	app.Status.ObservedGeneration = app.Generation
	app.Status.CurrentReplicas = ready
	app.Status.DesiredReplicas = desired

	// 金丝雀窗口的账本：镜像变更打开窗口，回滚关闭窗口。
	// 回滚路径必须先标记（否则 spec.Image 变了会被误判成"新发布"，窗口重开 → 回滚死循环）。
	if sp.rolledBack {
		app.Status.CurrentImage = app.Spec.Image
		app.Status.LastImageChangeAt = nil
		app.Status.RollbackRevision = sp.rollbackRevision
	} else if app.Status.CurrentImage != app.Spec.Image {
		app.Status.CurrentImage = app.Spec.Image
		now := metav1.Now()
		app.Status.LastImageChangeAt = &now
	}

	// 自愈账本：记录动作等级与时间，供升级路径（冷却期内复发 → 升级）判断
	if sp.healAction != nil {
		app.Status.LastHealAction = sp.healAction.Describe()
		app.Status.LastHealLevel = int(sp.healAction.Level)
		now := metav1.Now()
		app.Status.LastHealAt = &now
	} else if sp.healSummary != "" {
		app.Status.LastHealAction = sp.healSummary
	}

	// 伸缩动作落地后更新冷却窗口起点（决策发生在本次 Reconcile 内）
	if sp.decision.Action != scaler.ActionNone {
		now := metav1.Now()
		app.Status.LastScalingAt = &now
	}
	// 已收敛时清掉历史自愈动作，避免 status 里永远挂着一条过期信息
	if sp.phase == appsv1alpha1.PhaseStable && ready >= desired && app.Status.LastHealAction != "" {
		app.Status.LastHealAction = ""
	}

	setConditions(app, deploy, desired)

	if err := r.Status().Patch(ctx, app, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch status: %w", err)
	}

	// 首次变为可用时留一条 Event：demo 里 `kubectl get events` 才有东西可看
	if !wasAvailable && meta.IsStatusConditionTrue(app.Status.Conditions, conditionAvailable) {
		r.emitEvent(app, corev1.EventTypeNormal, EventReasonReconcile,
			fmt.Sprintf("app available: %d/%d replicas ready", ready, desired))
	}
	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil // 周期性巡检兜底
}

// setConditions 维护 Available / Progressing 两个标准 Condition。
// 用 meta.SetStatusCondition 而不是手写 append，才能正确处理 LastTransitionTime 与去重。
func setConditions(app *appsv1alpha1.App, deploy *appsv1.Deployment, desired int32) {
	ready := deploy.Status.ReadyReplicas

	available := desired > 0 && ready >= desired
	availableCond := metav1.Condition{
		Type:               conditionAvailable,
		Status:             metav1.ConditionTrue,
		Reason:             "MinimumReplicasAvailable",
		Message:            fmt.Sprintf("%d/%d replicas ready", ready, desired),
		ObservedGeneration: app.Generation,
	}
	if !available {
		availableCond.Status = metav1.ConditionFalse
		availableCond.Reason = "DeploymentUnavailable"
	}
	meta.SetStatusCondition(&app.Status.Conditions, availableCond)

	progressing := deploy.Status.UpdatedReplicas < desired || ready < desired
	progressingCond := metav1.Condition{
		Type:               "Progressing",
		Status:             metav1.ConditionFalse,
		Reason:             "ReconcileComplete",
		Message:            "all replicas are up to date",
		ObservedGeneration: app.Generation,
	}
	if progressing {
		progressingCond.Status = metav1.ConditionTrue
		progressingCond.Reason = "Reconciling"
		progressingCond.Message = fmt.Sprintf("updated=%d ready=%d desired=%d",
			deploy.Status.UpdatedReplicas, ready, desired)
	}
	meta.SetStatusCondition(&app.Status.Conditions, progressingCond)
}

func (r *AppReconciler) emitEvent(app *appsv1alpha1.App, eventType, reason, msg string) {
	if r.Recorder == nil {
		return // 单测里没有 recorder，直接跳过，不要制造 nil panic
	}
	r.Recorder.Event(app, eventType, reason, msg)
}

// failOn 统一处理写路径错误。
//
// conflict（"the object has been modified"）不是故障：它和 Deployment controller
// 的 status 写入、我们的 status 回写是天然并发的。对它发 Warning 事件会把
// `kubectl get events` 刷成红色噪音（11.03 评审指出观感差），正确做法是
// 静默退避重试——healer/scaler 的写路径内部已用 RetryOnConflict 吸收过一次，
// 漏到这里的冲突说明重试耗尽，交给控制器退避即可。其他错误照旧发 Warning 事件。
func (r *AppReconciler) failOn(ctx context.Context, app *appsv1alpha1.App, op string, err error) (ctrl.Result, error) {
	if apierrors.IsConflict(err) {
		logf.FromContext(ctx).Info("conflict on write, backing off (not an error)", "op", op)
		return ctrl.Result{Requeue: true}, nil
	}
	r.emitEvent(app, corev1.EventTypeWarning, EventReasonError, op+": "+err.Error())
	return ctrl.Result{}, fmt.Errorf("%s: %w", op, err)
}

func selectorLabels(app *appsv1alpha1.App) map[string]string {
	return map[string]string{labelManagedBy: "kubestasis", labelApp: app.Name}
}

func desiredReplicas(app *appsv1alpha1.App) int32 {
	if app.Spec.Replicas != nil {
		return *app.Spec.Replicas
	}
	return 1
}

// appPort 兜底 8080：port 有 default=8080 的 CRD 默认值，但旧对象可能是 0，
// 直接拿 0 建 Service 会被 apiserver 拒绝。
func appPort(app *appsv1alpha1.App) int32 {
	if app.Spec.Port <= 0 {
		return 8080
	}
	return app.Spec.Port
}

// resourcesOf 返回 spec.resources 的深拷贝，避免控制器把指针共享给
// 多个对象的 PodTemplate（后续改动会连带污染缓存里的 App 对象）。
func resourcesOf(app *appsv1alpha1.App) corev1.ResourceRequirements {
	if app.Spec.Resources == nil {
		return corev1.ResourceRequirements{}
	}
	return *app.Spec.Resources.DeepCopy()
}

// userEnv 过滤掉保留变量后返回用户声明的 env 副本。
// 保留变量由控制器强制注入，用户声明同名变量在 Webhook 已拦截；
// 这里再过滤一次，保证绕过 Webhook 创建的旧对象也不会出现重复变量。
func userEnv(app *appsv1alpha1.App) []corev1.EnvVar {
	out := make([]corev1.EnvVar, 0, len(app.Spec.Env))
	for _, e := range app.Spec.Env {
		if e.Name == "APP_NAME" || e.Name == "POD_NAMESPACE" {
			continue
		}
		out = append(out, *e.DeepCopy())
	}
	return out
}

// SetupWithManager sets up the controller with the Manager.
func (r *AppReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.App{}).
		Owns(&appsv1.Deployment{}). // Deployment 变化也会触发 Reconcile，加速收敛
		Owns(&corev1.Service{}).    // Service 被误删时自动重建
		Complete(r)
}
