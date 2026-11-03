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

package healer

// 本文件覆盖两类回归：
//  1. 写路径的冲突自愈：故障现场 status 回写与 spec 写入天然并发，
//     "the object has been modified" 必须被 RetryOnConflict 吸收，而不是
//     冒泡成 KubeStasisError 事件（11.03 真集群撞过一次 L3 回滚冲突）。
//  2. 魔法数字可配：L1 扩容比例、高频重启阈值从 CRD 读取，不再硬编码 20%/5。

import (
	"context"
	"fmt"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	appsv1alpha1 "github.com/weirong-ui/kubestasis/api/v1alpha1"
)

// conflictOnce 返回一个拦截器：对 App 的第一次 Patch 返回 conflict，之后放行，
// 模拟"status 回写刚改过 resourceVersion"的真实竞争。
func conflictOnce() (interceptor.Funcs, *int) {
	calls := new(int)
	return interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object,
			patch client.Patch, opts ...client.PatchOption) error {
			if _, isApp := obj.(*appsv1alpha1.App); isApp && *calls == 0 {
				*calls++
				return apierrors.NewConflict(
					schema.GroupResource{Group: "apps.sentry.io", Resource: "apps"},
					obj.GetName(), fmt.Errorf("the object has been modified"))
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}, calls
}

func TestApply_L1RetriesOnConflict(t *testing.T) {
	app := newApp("demo", 5)
	app.Spec.Scaling = &appsv1alpha1.ScalingPolicy{MinReplicas: 1, MaxReplicas: 10}
	intercept, calls := conflictOnce()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(app).
		WithInterceptorFuncs(intercept).Build()

	report := Report{Unhealthy: true, SuggestedLevel: L1ScaleUp, Signals: []string{"OOMKilled(p1)"}}
	action, err := newEngine().Apply(context.Background(), c, app, newDeploy("demo", "demo:v1"), report)
	if err != nil {
		t.Fatalf("conflict must be absorbed by RetryOnConflict, got %v", err)
	}
	if action == nil || action.Level != L1ScaleUp {
		t.Fatalf("want L1 action, got %v", action)
	}
	if *calls != 1 {
		t.Fatalf("expected exactly 1 injected conflict, got %d", *calls)
	}
	var got appsv1alpha1.App
	if err := c.Get(context.Background(), types.NamespacedName{Name: "demo", Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}
	if *got.Spec.Replicas != 6 {
		t.Fatalf("replicas = %d, want 6", *got.Spec.Replicas)
	}
}

func TestRollback_RetriesOnConflict(t *testing.T) {
	app := newApp("demo", 2)
	app.Spec.Image = "demo:v2" // 当前运行 v2，回滚目标是 RS 历史里的 v1
	deploy := newDeploy("demo", "demo:v2")
	rsOld := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "demo-r1", Namespace: "default",
			Labels:      map[string]string{labelApp: "demo"},
			Annotations: map[string]string{revisionAnnotation: "1"},
		},
		Spec: appsv1.ReplicaSetSpec{Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "demo", Image: "demo:v1"}}},
		}},
	}
	intercept, calls := conflictOnce()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(app, rsOld).
		WithInterceptorFuncs(intercept).Build()

	rev, err := newEngine().Rollback(context.Background(), c, app, deploy)
	if err != nil {
		t.Fatalf("conflict must be absorbed, got %v", err)
	}
	if rev != 1 {
		t.Fatalf("revision = %d, want 1", rev)
	}
	if *calls != 1 {
		t.Fatalf("expected exactly 1 injected conflict, got %d", *calls)
	}
	var got appsv1alpha1.App
	if err := c.Get(context.Background(), types.NamespacedName{Name: "demo", Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Image != "demo:v1" {
		t.Fatalf("image = %s, want demo:v1", got.Spec.Image)
	}
}

func TestApply_L1UsesConfiguredPercent(t *testing.T) {
	app := newApp("demo", 4)
	app.Spec.Health = &appsv1alpha1.HealthPolicy{ScaleUpPercent: 50}
	app.Spec.Scaling = &appsv1alpha1.ScalingPolicy{MinReplicas: 1, MaxReplicas: 20}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(app).Build()

	report := Report{Unhealthy: true, SuggestedLevel: L1ScaleUp, Signals: []string{"OOMKilled(p1)"}}
	action, err := newEngine().Apply(context.Background(), c, app, newDeploy("demo", "demo:v1"), report)
	if err != nil {
		t.Fatal(err)
	}
	if action == nil {
		t.Fatal("want L1 action")
	}
	var got appsv1alpha1.App
	if err := c.Get(context.Background(), types.NamespacedName{Name: "demo", Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}
	if *got.Spec.Replicas != 6 {
		t.Fatalf("replicas = %d, want 6 (4 + 50%%)", *got.Spec.Replicas)
	}
}

func TestInspect_AppLevelRestartThreshold(t *testing.T) {
	app := newApp("demo", 1)
	app.Spec.Health = &appsv1alpha1.HealthPolicy{RestartThreshold: 2} // 引擎默认 5，CR 收紧到 2
	// restartCount=3：低于引擎默认值 5，但达到 CR 的 2
	pod := newPod("p1", "demo", func(cs *corev1.ContainerStatus) { cs.RestartCount = 3 })
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(app, pod).Build()

	report := newEngine().Inspect(context.Background(), c, app)
	if !report.Unhealthy {
		t.Fatal("restartCount=3 with CR threshold=2 must be flagged")
	}

	// 对照组：不配 CR 阈值时，restartCount=3 < 引擎默认 5，不报
	app2 := newApp("demo2", 1)
	pod2 := newPod("p2", "demo2", func(cs *corev1.ContainerStatus) { cs.RestartCount = 3 })
	c2 := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(app2, pod2).Build()
	if report := newEngine().Inspect(context.Background(), c2, app2); report.Unhealthy {
		t.Fatal("restartCount=3 below engine default 5 must NOT be flagged")
	}
}

// ---------- 乒乓回滚回归（2026-11-03 kind CI 翻车实录） ----------

// 场景还原：回滚到 v1 成功后，旧 RS 的 crash Pod 还没清完，下一轮 Inspect 仍
// 不健康。若"上一个不同镜像"不加排除，刚滚走的 v2-crash 会被当成回滚目标
// 滚回来（v2-crash→v1→v2-crash 乒乓）。
func TestRollback_ExcludesBadImages_NoPingPong(t *testing.T) {
	app := newApp("demo", 2)
	app.Spec.Image = "demo:v1" // 已经滚回 v1
	deploy := newDeploy("demo", "demo:v1")
	mkRS := func(name, rev, image string) *appsv1.ReplicaSet {
		return &appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: "default",
				Labels:      map[string]string{labelApp: "demo"},
				Annotations: map[string]string{revisionAnnotation: rev},
			},
			Spec: appsv1.ReplicaSetSpec{Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "demo", Image: image}}},
			}},
		}
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(app,
		mkRS("demo-r1", "1", "demo:v1"),
		mkRS("demo-r2", "2", "demo:v2-crash"), // 事故现场，绝不能滚回去
	).Build()

	rev, err := newEngine().Rollback(context.Background(), c, app, deploy, "demo:v2-crash")
	if err != nil {
		t.Fatal(err)
	}
	if rev != 0 {
		t.Fatalf("bad image must be excluded; want rev 0 (no target), got %d", rev)
	}
	var got appsv1alpha1.App
	if err := c.Get(context.Background(), types.NamespacedName{Name: "demo", Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Image != "demo:v1" {
		t.Fatalf("image ping-ponged to %s", got.Spec.Image)
	}
}

// 排除坏镜像后应选更早的好版本，而不是"没有目标"。
func TestRollback_SkipsBadImagePicksOlder(t *testing.T) {
	app := newApp("demo", 2)
	app.Spec.Image = "demo:v3"
	deploy := newDeploy("demo", "demo:v3")
	mkRS := func(name, rev, image string) *appsv1.ReplicaSet {
		return &appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: "default",
				Labels:      map[string]string{labelApp: "demo"},
				Annotations: map[string]string{revisionAnnotation: rev},
			},
			Spec: appsv1.ReplicaSetSpec{Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "demo", Image: image}}},
			}},
		}
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(app,
		mkRS("demo-r1", "1", "demo:v1"),
		mkRS("demo-r2", "2", "demo:v2-bad"), // v2 也是坏的
		mkRS("demo-r3", "3", "demo:v3"),     // 当前
	).Build()

	rev, err := newEngine().Rollback(context.Background(), c, app, deploy, "demo:v2-bad")
	if err != nil {
		t.Fatal(err)
	}
	if rev != 1 {
		t.Fatalf("want rev 1 (skip bad v2), got %d", rev)
	}
}

func TestInspect_RecordsBadImages(t *testing.T) {
	app := newApp("demo", 1)
	bad := newPod("bad-1", "demo", func(cs *corev1.ContainerStatus) {
		cs.State.Waiting = &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}
	})
	bad.Spec.Containers = []corev1.Container{{Name: "demo", Image: "demo:v2-crash"}}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(app, bad).Build()

	report := newEngine().Inspect(context.Background(), c, app)
	if !report.Unhealthy {
		t.Fatal("crashloop pod must be flagged")
	}
	if len(report.BadImages) != 1 || report.BadImages[0] != "demo:v2-crash" {
		t.Fatalf("BadImages = %v, want [demo:v2-crash]", report.BadImages)
	}
}
