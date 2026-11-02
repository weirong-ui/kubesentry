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
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	appsv1alpha1 "github.com/weirong-ui/kubesentry/api/v1alpha1"
	"github.com/weirong-ui/kubesentry/internal/healer"
	"github.com/weirong-ui/kubesentry/internal/scaler"
)

const (
	appFinalizer = "apps.sentry.io/finalizer"

	// 托管资源统一标签，便于 `kubectl get deploy,svc -l app.kubernetes.io/managed-by=kubesentry`.
	labelManagedBy = "app.kubernetes.io/managed-by"
	labelApp       = "app"

	// conditionAvailable 表示期望副本数已全部就绪。
	conditionAvailable = "Available"

	// Event reasons: 统一前缀便于运维检索与简历讲解.
	EventReasonScaleUp    = "KubeSentryScaleUp"
	EventReasonScaleDown  = "KubeSentryScaleDown"
	EventReasonHeal       = "KubeSentryHealAction"
	EventReasonRollback   = "KubeSentryRollback"
	EventReasonCanaryFail = "KubeSentryCanaryFailed"
	EventReasonReconcile  = "KubeSentryReconcile"
	EventReasonError      = "KubeSentryError"
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

// +kubebuilder:rbac:groups=apps.sentry.io,resources=apps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.sentry.io,resources=apps/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.sentry.io,resources=apps/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
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
			// TODO(P2): 导出自愈审计记录后再放行删除
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
		r.emitEvent(&app, corev1.EventTypeWarning, EventReasonError, "ensure deployment: "+opErr.Error())
		return ctrl.Result{}, opErr
	}
	if _, opErr := r.ensureService(ctx, &app); opErr != nil {
		r.emitEvent(&app, corev1.EventTypeWarning, EventReasonError, "ensure service: "+opErr.Error())
		return ctrl.Result{}, opErr
	}

	// ---------- 3. 金丝雀观察窗口 ----------
	if app.Spec.Health.Canary.Enabled && r.Healer.InCanaryWindow(&app) {
		failed := r.Healer.CheckCanary(ctx, &app)
		if failed {
			r.emitEvent(&app, corev1.EventTypeWarning, EventReasonCanaryFail,
				"error rate exceeded threshold during canary window, rolling back")
			if err := r.Healer.Rollback(ctx, r.Client, &app, deploy); err != nil {
				return ctrl.Result{}, fmt.Errorf("canary rollback: %w", err)
			}
			return r.updateStatus(ctx, &app, deploy, appsv1alpha1.PhaseRollingBack, "canary rollback", scaler.Decision{})
		}
		return r.updateStatus(ctx, &app, deploy, appsv1alpha1.PhaseStable, "", scaler.Decision{})
	}

	// ---------- 4. 健康 → 自愈 / 正常 → 伸缩 ----------
	healthReport := r.Healer.Inspect(ctx, &app)
	if healthReport.Unhealthy {
		action, err := r.Healer.Apply(ctx, r.Client, &app, deploy, healthReport)
		if err != nil {
			r.emitEvent(&app, corev1.EventTypeWarning, EventReasonError, "heal: "+err.Error())
			return ctrl.Result{}, fmt.Errorf("heal: %w", err)
		}
		if action != nil {
			r.emitEvent(&app, corev1.EventTypeNormal, EventReasonHeal, action.Describe())
		}
		return r.updateStatus(ctx, &app, deploy, appsv1alpha1.PhaseHealing, healthReport.Summary(), scaler.Decision{})
	}

	decision := r.Scaler.Decide(&app, time.Now)
	if decision.Action != scaler.ActionNone {
		if err := r.Scaler.Apply(ctx, r.Client, &app, decision); err != nil {
			r.emitEvent(&app, corev1.EventTypeWarning, EventReasonError, "scale: "+err.Error())
			return ctrl.Result{}, fmt.Errorf("scale: %w", err)
		}
		reason := EventReasonScaleUp
		if decision.Action == scaler.ActionScaleDown {
			reason = EventReasonScaleDown
		}
		r.emitEvent(&app, corev1.EventTypeNormal, reason, decision.Describe())
		log.Info("scaled", "from", decision.FromReplicas, "to", decision.ToReplicas)
	}

	// ---------- 5. 回写 Status ----------
	return r.updateStatus(ctx, &app, deploy, appsv1alpha1.PhaseStable, "", decision)
}

// ensureDeployment 按 App spec 创建或更新 Deployment（幂等的 CreateOrUpdate）。
// 安全上下文等默认值由 Defaulting Webhook 注入；此处只负责业务字段对齐。
func (r *AppReconciler) ensureDeployment(ctx context.Context, app *appsv1alpha1.App) (*appsv1.Deployment, error) {
	want := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: app.Name, Namespace: app.Namespace},
	}
	replicas := desiredReplicas(app)

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, want, func() error {
		// 标签要打在 Deployment 本身上，否则 README 里的
		// `kubectl get deploy -l app.kubernetes.io/managed-by=kubesentry` 查不到
		want.Labels = selectorLabels(app)
		want.Spec.Replicas = &replicas
		want.Spec.Selector = &metav1.LabelSelector{MatchLabels: selectorLabels(app)}
		want.Spec.Template.Labels = selectorLabels(app)
		want.Spec.Template.Spec.Containers = []corev1.Container{{
			Name:  app.Name,
			Image: app.Spec.Image,
			Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: appPort(app), Protocol: corev1.ProtocolTCP}},
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

// updateStatus 用观测到的真实状态回写 status，并维护 Conditions。
// 关键：副本数取自 Deployment.status，而不是 spec —— 否则 status 永远是期望值，
// `kubectl get app` 与实际脱节，排障时会误导。
func (r *AppReconciler) updateStatus(ctx context.Context, app *appsv1alpha1.App,
	deploy *appsv1.Deployment, phase appsv1alpha1.AppPhase,
	healSummary string, decision scaler.Decision) (ctrl.Result, error) {

	patch := client.MergeFrom(app.DeepCopy())

	desired := desiredReplicas(app)
	ready := deploy.Status.ReadyReplicas
	wasAvailable := meta.IsStatusConditionTrue(app.Status.Conditions, conditionAvailable)

	app.Status.Phase = phase
	app.Status.ObservedGeneration = app.Generation
	app.Status.CurrentReplicas = ready
	app.Status.DesiredReplicas = desired

	if healSummary != "" {
		app.Status.LastHealAction = healSummary
	}
	// 伸缩动作落地后更新冷却窗口起点（决策发生在本次 Reconcile 内）
	if decision.Action != scaler.ActionNone {
		now := metav1.Now()
		app.Status.LastScalingAt = &now
	}
	// 已收敛时清掉历史自愈动作，避免 status 里永远挂着一条过期信息
	if phase == appsv1alpha1.PhaseStable && ready >= desired && app.Status.LastHealAction != "" {
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

func selectorLabels(app *appsv1alpha1.App) map[string]string {
	return map[string]string{labelManagedBy: "kubesentry", labelApp: app.Name}
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

// SetupWithManager sets up the controller with the Manager.
func (r *AppReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.App{}).
		Owns(&appsv1.Deployment{}). // Deployment 变化也会触发 Reconcile，加速收敛
		Owns(&corev1.Service{}).    // Service 被误删时自动重建
		Complete(r)
}
