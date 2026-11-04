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

// 探针生成与缩容稳定窗口状态回写的回归测试。
//
// 探针这块补的是一个语义漏洞：托管 Pod 之前完全没有探针，容器一启动就被标 Ready，
// Service 会把流量打给还没开始监听的实例，status.currentReplicas（取自 readyReplicas）
// 也跟着虚高 —— 于是 "Available=True" 是乐观信号，不是事实。

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1alpha1 "github.com/weirong-ui/kubestasis/api/v1alpha1"
)

func probeContainer(t *testing.T, app *appsv1alpha1.App) corev1.Container {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(app).Build()
	deploy, err := newTestReconciler(c).ensureDeployment(context.Background(), app)
	if err != nil {
		t.Fatalf("ensureDeployment: %v", err)
	}
	if len(deploy.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("want exactly one container, got %d", len(deploy.Spec.Template.Spec.Containers))
	}
	return deploy.Spec.Template.Spec.Containers[0]
}

// 省略 spec.readiness 时仍然注入 TCP 就绪探针，端口跟随 spec.port。
func TestEnsureDeployment_DefaultReadinessProbe(t *testing.T) {
	app := newTestApp("sample", 2) // Port 80，未声明任何探针
	c := probeContainer(t, app)

	if c.ReadinessProbe == nil {
		t.Fatal("readiness probe must be injected even when spec.readiness is omitted")
	}
	if c.ReadinessProbe.TCPSocket == nil {
		t.Fatalf("default probe should be TCPSocket, got %+v", c.ReadinessProbe)
	}
	if got := c.ReadinessProbe.TCPSocket.Port.IntValue(); got != 80 {
		t.Errorf("probe port = %d, want spec.port 80", got)
	}
	if c.ReadinessProbe.InitialDelaySeconds != 3 || c.ReadinessProbe.PeriodSeconds != 10 ||
		c.ReadinessProbe.TimeoutSeconds != 2 || c.ReadinessProbe.FailureThreshold != 3 {
		t.Errorf("unexpected default timings: %+v", c.ReadinessProbe)
	}
}

// 存活探针默认**不**注入：配错的代价是反复重启健康实例。
func TestEnsureDeployment_NoLivenessProbeByDefault(t *testing.T) {
	if c := probeContainer(t, newTestApp("sample", 1)); c.LivenessProbe != nil {
		t.Fatalf("liveness probe must not be injected unless declared, got %+v", c.LivenessProbe)
	}
}

// 显式声明 HTTPGet 时按 path 生成，未填的字段用该探针的默认值。
func TestEnsureDeployment_HTTPProbeHonorsPathAndTimings(t *testing.T) {
	app := newTestApp("sample", 1)
	app.Spec.Readiness = &appsv1alpha1.ProbeConfig{
		Type:                appsv1alpha1.ProbeHTTPGet,
		Path:                "/healthz",
		InitialDelaySeconds: 5,
	}
	app.Spec.Liveness = &appsv1alpha1.ProbeConfig{InitialDelaySeconds: 30}

	c := probeContainer(t, app)

	if c.ReadinessProbe.HTTPGet == nil || c.ReadinessProbe.HTTPGet.Path != "/healthz" {
		t.Fatalf("readiness should be HTTPGet /healthz, got %+v", c.ReadinessProbe)
	}
	if c.ReadinessProbe.InitialDelaySeconds != 5 {
		t.Errorf("explicit initialDelaySeconds ignored: %+v", c.ReadinessProbe)
	}
	if c.ReadinessProbe.PeriodSeconds != 10 {
		t.Errorf("unspecified period should fall back to 10, got %d", c.ReadinessProbe.PeriodSeconds)
	}
	if c.LivenessProbe == nil || c.LivenessProbe.TCPSocket == nil {
		t.Fatalf("declared liveness should default to TCPSocket, got %+v", c.LivenessProbe)
	}
	if c.LivenessProbe.InitialDelaySeconds != 30 || c.LivenessProbe.PeriodSeconds != 20 {
		t.Errorf("liveness timings wrong: %+v", c.LivenessProbe)
	}
}

// HTTPGet 但没填 path：用 "/" 兜底，而不是生成一个空路径（会被 apiserver 拒绝）。
func TestEnsureDeployment_HTTPProbeDefaultsPath(t *testing.T) {
	app := newTestApp("sample", 1)
	app.Spec.Readiness = &appsv1alpha1.ProbeConfig{Type: appsv1alpha1.ProbeHTTPGet}

	if got := probeContainer(t, app).ReadinessProbe.HTTPGet.Path; got != "/" {
		t.Fatalf("empty path should default to \"/\", got %q", got)
	}
}

// 稳定窗口的计时起点必须能落盘、也能被清空（JSON null 而不是"字段省略"）。
func TestPatchBelowTargetSince_SetAndClear(t *testing.T) {
	ctx := context.Background()
	app := newTestApp("sample", 2)
	// 需要 status 子资源才能单独 patch status
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(&appsv1alpha1.App{}).
		WithObjects(app).
		Build()
	r := newTestReconciler(c)

	start := metav1.NewTime(time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC))
	app.Status.BelowTargetSince = &start
	if err := r.patchBelowTargetSince(ctx, app); err != nil {
		t.Fatalf("set: %v", err)
	}

	var got appsv1alpha1.App
	if err := c.Get(ctx, types.NamespacedName{Name: "sample", Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.BelowTargetSince == nil || !got.Status.BelowTargetSince.Time.Equal(start.Time) {
		t.Fatalf("belowTargetSince not persisted: %+v", got.Status.BelowTargetSince)
	}

	got.Status.BelowTargetSince = nil
	if err := r.patchBelowTargetSince(ctx, &got); err != nil {
		t.Fatalf("clear: %v", err)
	}

	var cleared appsv1alpha1.App
	if err := c.Get(ctx, types.NamespacedName{Name: "sample", Namespace: "default"}, &cleared); err != nil {
		t.Fatal(err)
	}
	if cleared.Status.BelowTargetSince != nil {
		t.Fatalf("belowTargetSince should be cleared, got %v", cleared.Status.BelowTargetSince.Time)
	}
}
