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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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

	// Event reasons: 统一前缀便于运维检索与简历讲解.
	EventReasonScaleUp    = "KubeSentryScaleUp"
	EventReasonScaleDown  = "KubeSentryScaleDown"
	EventReasonHeal       = "KubeSentryHealAction"
	EventReasonRollback   = "KubeSentryRollback"
	EventReasonCanaryFail = "KubeSentryCanaryFailed"
)

// AppReconciler reconciles an App object.
type AppReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Scaler 伸缩决策引擎（多信号 + 冷却 + 迟滞）
	Scaler *scaler.Engine
	// Healer 分级自愈引擎
	Healer *healer.Engine
}

// +kubebuilder:rbac:groups=apps.sentry.io,resources=apps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.sentry.io,resources=apps/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.sentry.io,resources=apps/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;events;pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods/status,verbs=get

// Reconcile 主调和循环：
//
//	1. 处理删除（Finalizer）
//	2. Ensure Deployment 与 spec 一致
//	3. 金丝雀观察窗口检查（失败则回滚，直接结束本轮）
//	4. 健康检查：异常 → Healer；正常 → Scaler
//	5. 更新 Status
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
			// TODO(P0): 导出自愈审计记录后再放行删除
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

	// ---------- 2. Ensure Deployment ----------
	deploy, opErr := r.ensureDeployment(ctx, &app)
	if opErr != nil {
		return ctrl.Result{}, opErr
	}

	// ---------- 3. 金丝雀观察窗口 ----------
	if app.Spec.Health.Canary.Enabled && r.Healer.InCanaryWindow(&app) {
		failed := r.Healer.CheckCanary(ctx, &app)
		if failed {
			r.emitEvent(&app, EventReasonCanaryFail,
				"error rate exceeded threshold during canary window, rolling back")
			if err := r.Healer.Rollback(ctx, r.Client, &app, deploy); err != nil {
				return ctrl.Result{}, fmt.Errorf("canary rollback: %w", err)
			}
			return r.patchStatus(ctx, &app, appsv1alpha1.PhaseRollingBack, "canary rollback")
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// ---------- 4. 健康 → 自愈 / 正常 → 伸缩 ----------
	healthReport := r.Healer.Inspect(ctx, &app)
	if healthReport.Unhealthy {
		action, err := r.Healer.Apply(ctx, r.Client, &app, deploy, healthReport)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("heal: %w", err)
		}
		if action != nil {
			r.emitEvent(&app, EventReasonHeal, action.Describe())
		}
		return r.patchStatus(ctx, &app, appsv1alpha1.PhaseHealing, healthReport.Summary())
	}

	decision := r.Scaler.Decide(&app, time.Now)
	if decision.Action != scaler.ActionNone {
		if err := r.Scaler.Apply(ctx, r.Client, &app, deploy, decision); err != nil {
			return ctrl.Result{}, fmt.Errorf("scale: %w", err)
		}
		reason := EventReasonScaleUp
		if decision.Action == scaler.ActionScaleDown {
			reason = EventReasonScaleDown
		}
		r.emitEvent(&app, reason, decision.Describe())
		log.Info("scaled", "from", decision.FromReplicas, "to", decision.ToReplicas)
	}

	// ---------- 5. 更新 Status ----------
	return r.patchStatus(ctx, &app, appsv1alpha1.PhaseStable, "")
}

// ensureDeployment 按 App spec 创建或更新 Deployment（幂等的 CreateOrUpdate）。
// 安全上下文等默认值由 Defaulting Webhook 注入；此处只负责业务字段对齐。
func (r *AppReconciler) ensureDeployment(ctx context.Context, app *appsv1alpha1.App) (*appsv1.Deployment, error) {
	want := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: app.Name, Namespace: app.Namespace},
	}
	replicas := int32(1)
	if app.Spec.Replicas != nil {
		replicas = *app.Spec.Replicas
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, want, func() error {
		want.Spec.Replicas = &replicas
		want.Spec.Selector = &metav1.LabelSelector{
			MatchLabels: map[string]string{"app.kubernetes.io/managed-by": "kubesentry", "app": app.Name},
		}
		want.Spec.Template.Labels = want.Spec.Selector.MatchLabels
		want.Spec.Template.Spec.Containers = []corev1.Container{{
			Name:  app.Name,
			Image: app.Spec.Image,
			Ports: []corev1.ContainerPort{{ContainerPort: app.Spec.Port}},
		}}
		return controllerutil.SetControllerReference(app, want, r.Scheme)
	})
	if err != nil {
		return nil, fmt.Errorf("ensure deployment: %w", err)
	}
	return want, nil
}

func (r *AppReconciler) patchStatus(ctx context.Context, app *appsv1alpha1.App,
	phase appsv1alpha1.AppPhase, healSummary string) (ctrl.Result, error) {

	patch := client.MergeFrom(app.DeepCopy())
	app.Status.Phase = phase
	app.Status.CurrentReplicas = derefOr(app.Spec.Replicas, 1)
	if healSummary != "" {
		app.Status.LastHealAction = healSummary
	}
	if err := r.Status().Patch(ctx, app, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch status: %w", err)
	}
	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil // 周期性巡检兜底
}

func (r *AppReconciler) emitEvent(app *appsv1alpha1.App, reason, msg string) {
	// TODO(P0): r.Recorder.Event(app, corev1.EventTypeNormal, reason, msg)
	_ = reason
	_ = msg
}

func derefOr(p *int32, def int32) int32 {
	if p == nil {
		return def
	}
	return *p
}

// SetupWithManager sets up the controller with the Manager.
func (r *AppReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.App{}).
		Owns(&appsv1.Deployment{}). // Deployment 变化也会触发 Reconcile，加速收敛
		Complete(r)
}
