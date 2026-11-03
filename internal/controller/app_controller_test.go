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
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1alpha1 "github.com/weirong-ui/kubestasis/api/v1alpha1"
	"github.com/weirong-ui/kubestasis/internal/healer"
	"github.com/weirong-ui/kubestasis/internal/scaler"
)

func reconcileReq(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "default"}}
}

func scalerNoopDecision() scaler.Decision {
	return scaler.Decision{Action: scaler.ActionNone}
}

func newTestReconciler(c client.Client) *AppReconciler {
	return &AppReconciler{
		Client:   c,
		Scheme:   c.Scheme(),
		Scaler:   scaler.NewEngine(),
		Healer:   healer.NewEngine(),
		Recorder: nil, // 单测无 recorder，emitEvent 应静默跳过而不是 panic
	}
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := appsv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func int32Ptr(v int32) *int32 { return &v }

func newTestApp(name string, replicas int32) *appsv1alpha1.App {
	return &appsv1alpha1.App{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Generation: 1},
		Spec: appsv1alpha1.AppSpec{
			Image:    "nginx:1.27-alpine",
			Replicas: int32Ptr(replicas),
			Port:     80,
		},
	}
}

// TestUpdateStatus_ReadsRealReplicas 锁住 P0 的核心修复：
// status.currentReplicas 必须取自 Deployment.status.readyReplicas，
// 而不是 spec.replicas —— 后者会让 kubectl get app 在 Pod 全挂时仍然显示"满编"。
func TestUpdateStatus_ReadsRealReplicas(t *testing.T) {
	ctx := context.Background()
	app := newTestApp("sample", 3)

	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&appsv1alpha1.App{}).
		WithObjects(app).
		Build()

	deploy := &appsv1.Deployment{Status: appsv1.DeploymentStatus{
		ReadyReplicas:   2, // 只有 2 个就绪，spec 想要 3
		UpdatedReplicas: 2,
	}}

	r := newTestReconciler(c)
	if _, err := r.updateStatus(ctx, app, deploy, statusPatch{phase: appsv1alpha1.PhaseStable, decision: scalerNoopDecision()}); err != nil {
		t.Fatalf("updateStatus: %v", err)
	}

	var got appsv1alpha1.App
	if err := c.Get(ctx, types.NamespacedName{Name: "sample", Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}

	if got.Status.CurrentReplicas != 2 {
		t.Errorf("currentReplicas = %d, want 2 (real ready), not spec value", got.Status.CurrentReplicas)
	}
	if got.Status.DesiredReplicas != 3 {
		t.Errorf("desiredReplicas = %d, want 3", got.Status.DesiredReplicas)
	}
	if got.Status.ObservedGeneration != 1 {
		t.Errorf("observedGeneration = %d, want 1", got.Status.ObservedGeneration)
	}
	if cond := findCondition(got.Status.Conditions, "Available"); cond == nil || cond.Status != metav1.ConditionFalse {
		t.Errorf("Available should be False when 2/3 ready, got %+v", cond)
	}
	if cond := findCondition(got.Status.Conditions, "Progressing"); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("Progressing should be True when not all replicas updated, got %+v", cond)
	}
}

// TestUpdateStatus_AvailableWhenAllReady 完全就绪时 Available=True。
func TestUpdateStatus_AvailableWhenAllReady(t *testing.T) {
	ctx := context.Background()
	app := newTestApp("sample", 2)

	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&appsv1alpha1.App{}).
		WithObjects(app).
		Build()

	deploy := &appsv1.Deployment{Status: appsv1.DeploymentStatus{
		ReadyReplicas:   2,
		UpdatedReplicas: 2,
	}}

	r := newTestReconciler(c)
	if _, err := r.updateStatus(ctx, app, deploy, statusPatch{phase: appsv1alpha1.PhaseStable, decision: scalerNoopDecision()}); err != nil {
		t.Fatal(err)
	}

	var got appsv1alpha1.App
	if err := c.Get(ctx, types.NamespacedName{Name: "sample", Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}
	if cond := findCondition(got.Status.Conditions, "Available"); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("Available should be True when 2/2 ready, got %+v", cond)
	}
	if cond := findCondition(got.Status.Conditions, "Progressing"); cond == nil || cond.Status != metav1.ConditionFalse {
		t.Errorf("Progressing should be False when all up to date, got %+v", cond)
	}
}

// TestEnsureDeployment_SetsOwnerAndLabels 验证生成的 Deployment：
// ownerReference（级联删除 + Owns() 触发）与托管标签（README 的 -l 查询）。
func TestEnsureDeployment_SetsOwnerAndLabels(t *testing.T) {
	ctx := context.Background()
	app := newTestApp("sample", 2)

	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(app).
		Build()

	r := newTestReconciler(c)
	deploy, err := r.ensureDeployment(ctx, app)
	if err != nil {
		t.Fatalf("ensureDeployment: %v", err)
	}

	if len(deploy.OwnerReferences) != 1 || deploy.OwnerReferences[0].Kind != "App" {
		t.Errorf("ownerReference not set: %+v", deploy.OwnerReferences)
	}
	if deploy.Labels[labelManagedBy] != "kubestasis" {
		t.Errorf("deployment missing managed-by label: %v", deploy.Labels)
	}
	if deploy.Spec.Template.Labels[labelManagedBy] != "kubestasis" {
		t.Errorf("pod template missing managed-by label: %v", deploy.Spec.Template.Labels)
	}
	if *deploy.Spec.Replicas != 2 {
		t.Errorf("replicas = %d, want 2", *deploy.Spec.Replicas)
	}
}

// TestEnsureService_SelectorMatchesPodTemplate Service 不通的头号原因：
// svc.spec.selector 必须和 deployment 的 pod template labels 完全一致。
func TestEnsureService_SelectorMatchesPodTemplate(t *testing.T) {
	ctx := context.Background()
	app := newTestApp("sample", 2)

	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(app).
		Build()

	r := newTestReconciler(c)
	deploy, err := r.ensureDeployment(ctx, app)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := r.ensureService(ctx, app)
	if err != nil {
		t.Fatal(err)
	}

	for k, v := range svc.Spec.Selector {
		if deploy.Spec.Template.Labels[k] != v {
			t.Errorf("selector %s=%s not in pod labels %v", k, v, deploy.Spec.Template.Labels)
		}
	}
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("service type = %s, want ClusterIP", svc.Spec.Type)
	}
	if svc.Spec.Ports[0].Port != 80 || svc.Spec.Ports[0].TargetPort.IntVal != 80 {
		t.Errorf("port mapping wrong: %+v", svc.Spec.Ports)
	}
}

// TestReconcile_FinalizerAddedAndRemoved Finalizer 时序：先加后摘，且删除时放行。
func TestReconcile_FinalizerLifecycle(t *testing.T) {
	ctx := context.Background()

	newClient := func(t *testing.T, app *appsv1alpha1.App) client.Client {
		return fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			WithStatusSubresource(&appsv1alpha1.App{}).
			WithObjects(app).
			Build()
	}

	t.Run("首次 Reconcile 加上 finalizer", func(t *testing.T) {
		c := newClient(t, newTestApp("sample", 1))
		r := newTestReconciler(c)
		if _, err := r.Reconcile(ctx, reconcileReq("sample")); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		var got appsv1alpha1.App
		if err := c.Get(ctx, types.NamespacedName{Name: "sample", Namespace: "default"}, &got); err != nil {
			t.Fatal(err)
		}
		if !containsFinalizer(&got) {
			t.Fatal("finalizer should be added on first reconcile")
		}
	})

	t.Run("带 deletionTimestamp 的对象被放行（finalizer 摘除后对象真正删除）", func(t *testing.T) {
		app := newTestApp("dying", 1)
		now := metav1.Now()
		app.DeletionTimestamp = &now
		app.Finalizers = []string{appFinalizer}

		c := newClient(t, app)
		r := newTestReconciler(c)
		if _, err := r.Reconcile(ctx, reconcileReq("dying")); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		// finalizer 摘除后对象应被真正删除（fake client 会模拟完整的删除语义）
		var got appsv1alpha1.App
		err := c.Get(ctx, types.NamespacedName{Name: "dying", Namespace: "default"}, &got)
		if !apierrors.IsNotFound(err) {
			t.Fatalf("object should be gone after finalizer removal, got err=%v", err)
		}
	})
}

func TestAppPortFallback(t *testing.T) {
	// 旧对象的 port 可能为 0（CRD default 只对新对象生效），直接建 Service 会被拒
	app := newTestApp("old", 1)
	app.Spec.Port = 0
	if got := appPort(app); got != 8080 {
		t.Errorf("appPort fallback = %d, want 8080", got)
	}
}

func TestDesiredReplicas(t *testing.T) {
	app := newTestApp("sample", 4)
	if got := desiredReplicas(app); got != 4 {
		t.Errorf("desiredReplicas = %d, want 4", got)
	}
	noReplicas := newTestApp("sample", 0)
	noReplicas.Spec.Replicas = nil
	if got := desiredReplicas(noReplicas); got != 1 {
		t.Errorf("desiredReplicas default = %d, want 1", got)
	}
}

func findCondition(conds []metav1.Condition, typ string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == typ {
			return &conds[i]
		}
	}
	return nil
}

func containsFinalizer(app *appsv1alpha1.App) bool {
	for _, f := range app.Finalizers {
		if f == appFinalizer {
			return true
		}
	}
	return false
}
