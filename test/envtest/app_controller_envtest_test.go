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

package envtest

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1alpha1 "github.com/weirong-ui/kubestasis/api/v1alpha1"
)

const (
	waitTimeout = 30 * time.Second
)

func int32Ptr(v int32) *int32 { return &v }

func deref32(p *int32) int32 {
	if p == nil {
		return 0
	}
	return *p
}

// requireEnv 环境不可用时 Skip（CI 设 KUBESTASIS_REQUIRE_ENVTEST=1 后不会走到这里）。
func requireEnv(t *testing.T) {
	t.Helper()
	if k8sClient == nil {
		t.Skipf("envtest 二进制不可用，跳过：%v", startErr)
	}
}

// newNamespace 给每个用例独立命名空间，互不污染。
func newNamespace(t *testing.T, prefix string) string {
	t.Helper()
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{GenerateName: prefix + "-"},
	}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), ns)
	})
	return ns.Name
}

func newApp(ns, name string, replicas int32) *appsv1alpha1.App {
	return &appsv1alpha1.App{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1alpha1.AppSpec{
			Image:    "nginx:1.27-alpine",
			Replicas: int32Ptr(replicas),
			Port:     80,
		},
	}
}

// TestCreateApp_ReconcilesChildren 验证创建 App 后的完整闭环：
// watch 触发 → 补 Finalizer → 建 Deployment/Service → ownerRef 指回 App → status 回写。
func TestCreateApp_ReconcilesChildren(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	ns := newNamespace(t, "create")
	key := types.NamespacedName{Name: "web", Namespace: ns}

	if err := k8sClient.Create(ctx, newApp(ns, "web", 2)); err != nil {
		t.Fatalf("create app: %v", err)
	}

	// Deployment 被建出且 spec 与 App 一致
	var deploy appsv1.Deployment
	eventually(t, waitTimeout, "Deployment 出现", func() (bool, error) {
		if err := k8sClient.Get(ctx, key, &deploy); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		return true, nil
	})
	if got := deref32(deploy.Spec.Replicas); got != 2 {
		t.Fatalf("Deployment replicas = %d, want 2", got)
	}
	if img := deploy.Spec.Template.Spec.Containers[0].Image; img != "nginx:1.27-alpine" {
		t.Fatalf("Deployment image = %s", img)
	}

	// ownerReference 必须指回 App——删除回收、漂移 watch 都靠它
	if len(deploy.OwnerReferences) != 1 || deploy.OwnerReferences[0].Kind != "App" ||
		deploy.OwnerReferences[0].Name != "web" {
		t.Fatalf("Deployment ownerReferences = %+v", deploy.OwnerReferences)
	}

	// Service 同步建出，端口跟随 App.spec.port
	var svc corev1.Service
	eventually(t, waitTimeout, "Service 出现", func() (bool, error) {
		if err := k8sClient.Get(ctx, key, &svc); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		return true, nil
	})
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 80 {
		t.Fatalf("Service ports = %+v", svc.Spec.Ports)
	}

	// App 本体：finalizer 补上、status 账本回写（DesiredReplicas）
	var app appsv1alpha1.App
	eventually(t, waitTimeout, "App finalizer + status 回写", func() (bool, error) {
		if err := k8sClient.Get(ctx, key, &app); err != nil {
			return false, err
		}
		hasFinalizer := false
		for _, f := range app.Finalizers {
			if f != "" {
				hasFinalizer = true
			}
		}
		return hasFinalizer && app.Status.DesiredReplicas == 2, nil
	})
}

// TestDeploymentReady_AppBecomesAvailable 模拟 kubelet 上报就绪
// （envtest 没有 kube-controller-manager，Deployment.status 由测试代写），
// 验证 status 子资源链路：Deployment.status → Reconcile → App.status.conditions。
func TestDeploymentReady_AppBecomesAvailable(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	ns := newNamespace(t, "status")
	key := types.NamespacedName{Name: "api", Namespace: ns}

	if err := k8sClient.Create(ctx, newApp(ns, "api", 2)); err != nil {
		t.Fatalf("create app: %v", err)
	}

	var deploy appsv1.Deployment
	eventually(t, waitTimeout, "Deployment 出现", func() (bool, error) {
		if err := k8sClient.Get(ctx, key, &deploy); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		return true, nil
	})

	// 代写 Deployment.status（真实集群里这是 kubelet 干的）
	deploy.Status.Replicas = 2
	deploy.Status.ReadyReplicas = 2
	deploy.Status.AvailableReplicas = 2
	deploy.Status.UpdatedReplicas = 2
	if err := k8sClient.Status().Update(ctx, &deploy); err != nil {
		t.Fatalf("update deployment status: %v", err)
	}

	var app appsv1alpha1.App
	eventually(t, waitTimeout, "App Available=True", func() (bool, error) {
		if err := k8sClient.Get(ctx, key, &app); err != nil {
			return false, err
		}
		return app.Status.CurrentReplicas == 2 && appAvailable(&app), nil
	})
}

// TestDeleteApp_FinalizerReleased 验证删除链路：
// 删 App → finalizer 摘除 → 对象真正消失。
// （envtest 无 kube-controller-manager，GC 不跑，Deployment 残留是预期——
// 真集群的 ownerRef 级联回收由 kind e2e 兜底验证。）
func TestDeleteApp_FinalizerReleased(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	ns := newNamespace(t, "delete")
	key := types.NamespacedName{Name: "gone", Namespace: ns}

	if err := k8sClient.Create(ctx, newApp(ns, "gone", 1)); err != nil {
		t.Fatalf("create app: %v", err)
	}

	// 先等到 finalizer 就位，否则 delete 直接成功、没验到 finalizer 逻辑
	var app appsv1alpha1.App
	eventually(t, waitTimeout, "finalizer 就位", func() (bool, error) {
		if err := k8sClient.Get(ctx, key, &app); err != nil {
			return false, err
		}
		return len(app.Finalizers) > 0, nil
	})

	if err := k8sClient.Delete(ctx, &app); err != nil {
		t.Fatalf("delete app: %v", err)
	}

	eventually(t, waitTimeout, "App 对象消失", func() (bool, error) {
		err := k8sClient.Get(ctx, key, &app)
		return apierrors.IsNotFound(err), nil
	})
}

// TestDrift_DeploymentReplicasCorrected 验证 owned 资源漂移会被拉回：
// 直接改 Deployment.spec.replicas → owned watch 触发 Reconcile → 拉回 spec 声明值。
// 这是 CreateOrUpdate 幂等性的控制循环级回归。
func TestDrift_DeploymentReplicasCorrected(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	ns := newNamespace(t, "drift")
	key := types.NamespacedName{Name: "drifty", Namespace: ns}

	if err := k8sClient.Create(ctx, newApp(ns, "drifty", 3)); err != nil {
		t.Fatalf("create app: %v", err)
	}

	var deploy appsv1.Deployment
	eventually(t, waitTimeout, "Deployment 出现且 replicas=3", func() (bool, error) {
		if err := k8sClient.Get(ctx, key, &deploy); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		return deref32(deploy.Spec.Replicas) == 3, nil
	})

	// 人为制造漂移（真实场景：有人 kubectl scale 绕过 App）
	deploy.Spec.Replicas = int32Ptr(7)
	if err := k8sClient.Update(ctx, &deploy); err != nil {
		t.Fatalf("inject drift: %v", err)
	}

	eventually(t, waitTimeout, "漂移被拉回 3", func() (bool, error) {
		if err := k8sClient.Get(ctx, key, &deploy); err != nil {
			return false, err
		}
		return deref32(deploy.Spec.Replicas) == 3, nil
	})
}
