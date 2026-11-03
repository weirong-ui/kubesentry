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

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1alpha1 "github.com/weirong-ui/kubestasis/api/v1alpha1"
)

var fixedNow = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

func newScheme(t *testing.T) *runtime.Scheme {
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

func newEngine() *Engine {
	e := NewEngine()
	e.nowFunc = func() time.Time { return fixedNow }
	return e
}

func newApp(name string, replicas int32) *appsv1alpha1.App {
	return &appsv1alpha1.App{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: appsv1alpha1.AppSpec{
			Image:    "demo:v1",
			Replicas: &replicas,
		},
	}
}

// newPod 构造一个带指定容器状态的托管 Pod。
func newPod(name, appName string, mutate func(*corev1.ContainerStatus)) *corev1.Pod {
	cs := corev1.ContainerStatus{Name: appName}
	if mutate != nil {
		mutate(&cs)
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels:    map[string]string{labelManagedBy: "kubestasis", labelApp: appName},
		},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{cs},
		},
	}
}

func TestInspect_HealthyPods(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(newPod("p1", "demo", nil)).Build()
	report := newEngine().Inspect(context.Background(), c, newApp("demo", 2))
	if report.Unhealthy {
		t.Fatalf("healthy pod should not trigger report, got %v", report.Signals)
	}
}

func TestInspect_CrashLoopSuggestsL2(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(
			newPod("p1", "demo", func(cs *corev1.ContainerStatus) {
				cs.State.Waiting = &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}
				cs.RestartCount = 8
			}),
			newPod("p2", "demo", nil),
		).Build()

	report := newEngine().Inspect(context.Background(), c, newApp("demo", 2))
	if !report.Unhealthy {
		t.Fatal("CrashLoopBackOff must be reported unhealthy")
	}
	if report.SuggestedLevel != L2Isolate {
		t.Fatalf("CrashLoop should suggest L2, got L%d", report.SuggestedLevel+1)
	}
	if len(report.Pods) != 1 || report.Pods[0] != "p1" {
		t.Fatalf("unhealthy pods = %v, want [p1]", report.Pods)
	}
}

func TestInspect_OOMKilledSuggestsL1(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(newPod("p1", "demo", func(cs *corev1.ContainerStatus) {
			cs.LastTerminationState.Terminated = &corev1.ContainerStateTerminated{Reason: "OOMKilled"}
		})).Build()

	report := newEngine().Inspect(context.Background(), c, newApp("demo", 2))
	if !report.Unhealthy {
		t.Fatal("OOMKilled must be reported unhealthy")
	}
	if report.SuggestedLevel != L1ScaleUp {
		t.Fatalf("OOMKilled should suggest L1, got L%d", report.SuggestedLevel+1)
	}
}

func TestInspect_IgnoresUnrelatedAndTerminatingPods(t *testing.T) {
	other := newPod("other", "demo", func(cs *corev1.ContainerStatus) {
		cs.State.Waiting = &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}
	})
	other.Labels[labelApp] = "someone-else" // 别的 App 的 Pod
	terminating := newPod("dying", "demo", func(cs *corev1.ContainerStatus) {
		cs.State.Waiting = &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}
	})
	now := metav1.Now()
	terminating.DeletionTimestamp = &now      // 正在删除，不算故障
	terminating.Finalizers = []string{"hold"} // fake client 要求带 finalizer 才能挂 deletionTimestamp

	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(other, terminating).Build()
	report := newEngine().Inspect(context.Background(), c, newApp("demo", 2))
	if report.Unhealthy {
		t.Fatalf("unrelated/terminating pods must be ignored, got %v", report.Signals)
	}
}

func TestInspect_HighRestartsFallback(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(newPod("p1", "demo", func(cs *corev1.ContainerStatus) {
			cs.RestartCount = 6 // >= RestartThreshold(5)，但没有 CrashLoop/OOM 标记
		})).Build()

	report := newEngine().Inspect(context.Background(), c, newApp("demo", 2))
	if !report.Unhealthy {
		t.Fatal("restart count over threshold must be reported")
	}
}

// ---------- Apply：分级与升级路径 ----------

func newDeploy(name, image string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: name, Image: image}}},
			},
		},
	}
}

func TestApply_L1ScalesUpSpecWithCap(t *testing.T) {
	app := newApp("demo", 5)
	app.Spec.Scaling = &appsv1alpha1.ScalingPolicy{MinReplicas: 1, MaxReplicas: 5} // 已到上限
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(app).Build()

	report := Report{Unhealthy: true, SuggestedLevel: L1ScaleUp, Signals: []string{"OOMKilled(p1)"}}
	action, err := newEngine().Apply(context.Background(), c, app, newDeploy("demo", "demo:v1"), report)
	if err != nil {
		t.Fatal(err)
	}
	if action != nil {
		t.Fatalf("at maxReplicas, L1 must be a no-op, got %v", action)
	}

	// 未到上限时扩 20%（至少 +1），写回的是 App.spec 而非 Deployment
	app2 := newApp("demo2", 5)
	app2.Spec.Scaling = &appsv1alpha1.ScalingPolicy{MinReplicas: 1, MaxReplicas: 10}
	c2 := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(app2).Build()
	action, err = newEngine().Apply(context.Background(), c2, app2, newDeploy("demo2", "demo:v1"), report)
	if err != nil {
		t.Fatal(err)
	}
	if action == nil || action.Level != L1ScaleUp {
		t.Fatalf("want L1 action, got %v", action)
	}
	var got appsv1alpha1.App
	if err := c2.Get(context.Background(), types.NamespacedName{Name: "demo2", Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}
	if *got.Spec.Replicas != 6 {
		t.Fatalf("replicas = %d, want 6 (5 + 20%%)", *got.Spec.Replicas)
	}
}

func TestApply_L2DeletesUnhealthyPods(t *testing.T) {
	app := newApp("demo", 2)
	bad := newPod("bad-1", "demo", nil)
	good := newPod("good-1", "demo", nil)
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(app, bad, good).Build()

	report := Report{Unhealthy: true, SuggestedLevel: L2Isolate, Pods: []string{"bad-1"}}
	action, err := newEngine().Apply(context.Background(), c, app, newDeploy("demo", "demo:v1"), report)
	if err != nil {
		t.Fatal(err)
	}
	if action == nil || action.Level != L2Isolate {
		t.Fatalf("want L2 action, got %v", action)
	}
	var pods corev1.PodList
	if err := c.List(context.Background(), &pods); err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 1 || pods.Items[0].Name != "good-1" {
		t.Fatalf("bad pod should be deleted, remaining: %v", pods.Items)
	}
}

func TestApply_EscalatesWithinCooldown(t *testing.T) {
	app := newApp("demo", 2)
	// 2 分钟前做过 L2，现在又坏了 → 应升级到 L3
	lastHeal := metav1.NewTime(fixedNow.Add(-2 * time.Minute))
	app.Status.LastHealAt = &lastHeal
	app.Status.LastHealLevel = int(L2Isolate)

	deploy := newDeploy("demo", "demo:v2")
	rsOld := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "demo-old", Namespace: "default",
			Labels:      map[string]string{labelApp: "demo"},
			Annotations: map[string]string{revisionAnnotation: "1"},
		},
		Spec: appsv1.ReplicaSetSpec{Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "demo", Image: "demo:v1"}}},
		}},
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(app, deploy, rsOld).Build()

	report := Report{Unhealthy: true, SuggestedLevel: L2Isolate, Pods: []string{"p1"}}
	action, err := newEngine().Apply(context.Background(), c, app, deploy, report)
	if err != nil {
		t.Fatal(err)
	}
	if action == nil || action.Level != L3Rollback {
		t.Fatalf("recurring fault within cooldown must escalate to L3, got %v", action)
	}
	var got appsv1alpha1.App
	if err := c.Get(context.Background(), types.NamespacedName{Name: "demo", Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Image != "demo:v1" {
		t.Fatalf("image = %s, want rolled back to demo:v1", got.Spec.Image)
	}
}

func TestApply_NoEscalationAfterCooldown(t *testing.T) {
	app := newApp("demo", 2)
	// 10 分钟前的 L1，已超过 3 分钟冷却 → 不升级，重新按建议等级来
	lastHeal := metav1.NewTime(fixedNow.Add(-10 * time.Minute))
	app.Status.LastHealAt = &lastHeal
	app.Status.LastHealLevel = int(L1ScaleUp)

	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(app).Build()
	report := Report{Unhealthy: true, SuggestedLevel: L1ScaleUp, Signals: []string{"OOMKilled(p1)"}}
	action, err := newEngine().Apply(context.Background(), c, app, newDeploy("demo", "demo:v1"), report)
	if err != nil {
		t.Fatal(err)
	}
	if action == nil || action.Level != L1ScaleUp {
		t.Fatalf("fault after cooldown should stay at suggested level, got %v", action)
	}
}

func TestApply_StopsAfterL3WithinCooldown(t *testing.T) {
	app := newApp("demo", 2)
	lastHeal := metav1.NewTime(fixedNow.Add(-1 * time.Minute))
	app.Status.LastHealAt = &lastHeal
	app.Status.LastHealLevel = int(L3Rollback) // 已回滚过还在坏

	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(app).Build()
	report := Report{Unhealthy: true, SuggestedLevel: L3Rollback}
	action, err := newEngine().Apply(context.Background(), c, app, newDeploy("demo", "demo:v1"), report)
	if err != nil {
		t.Fatal(err)
	}
	if action != nil {
		t.Fatalf("already rolled back within cooldown: must observe, got %v", action)
	}
}

func TestApply_HealthyIsNoop(t *testing.T) {
	app := newApp("demo", 2)
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(app).Build()
	action, err := newEngine().Apply(context.Background(), c, app, nil, Report{Unhealthy: false})
	if err != nil || action != nil {
		t.Fatalf("healthy report must be no-op, got action=%v err=%v", action, err)
	}
}

// ---------- Rollback ----------

func TestRollback_PicksPreviousDifferentImage(t *testing.T) {
	app := newApp("demo", 2)
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
		mkRS("demo-r2", "2", "demo:v2"),
		mkRS("demo-r3", "3", "demo:v3"), // 当前版本
	).Build()

	rev, err := newEngine().Rollback(context.Background(), c, app, deploy)
	if err != nil {
		t.Fatal(err)
	}
	if rev != 2 {
		t.Fatalf("revision = %d, want 2", rev)
	}
	var got appsv1alpha1.App
	if err := c.Get(context.Background(), types.NamespacedName{Name: "demo", Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Image != "demo:v2" {
		t.Fatalf("image = %s, want demo:v2 (latest different)", got.Spec.Image)
	}
}

func TestRollback_NoHistoryIsNotAnError(t *testing.T) {
	app := newApp("demo", 2)
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(app).Build()
	rev, err := newEngine().Rollback(context.Background(), c, app, newDeploy("demo", "demo:v1"))
	if err != nil || rev != 0 {
		t.Fatalf("no history should return (0, nil), got (%d, %v)", rev, err)
	}
}

// ---------- 金丝雀窗口 ----------

func TestInCanaryWindow(t *testing.T) {
	newCanaryApp := func(ago time.Duration) *appsv1alpha1.App {
		app := newApp("demo", 2)
		app.Spec.Health = &appsv1alpha1.HealthPolicy{
			Strategy: appsv1alpha1.StrategyGraduated,
			Canary:   appsv1alpha1.CanaryPolicy{Enabled: true, ObservationSeconds: 300},
		}
		if ago >= 0 {
			tm := metav1.NewTime(fixedNow.Add(-ago))
			app.Status.LastImageChangeAt = &tm
		}
		return app
	}

	e := newEngine()
	cases := []struct {
		name string
		app  *appsv1alpha1.App
		want bool
	}{
		{"窗口内", newCanaryApp(1 * time.Minute), true},
		{"窗口外", newCanaryApp(10 * time.Minute), false},
		{"从未变更过镜像", newCanaryApp(-1), false},
		{"未启用金丝雀", func() *appsv1alpha1.App {
			app := newCanaryApp(1 * time.Minute)
			app.Spec.Health.Canary.Enabled = false
			return app
		}(), false},
		{"无 health 策略", newApp("demo", 2), false},
	}
	for _, tc := range cases {
		if got := e.InCanaryWindow(tc.app); got != tc.want {
			t.Errorf("%s: InCanaryWindow = %v, want %v", tc.name, got, tc.want)
		}
	}
}

type fakeErrSource struct {
	rate float64
	ok   bool
}

func (f fakeErrSource) ErrorRate(_ context.Context, _ *appsv1alpha1.App) (float64, bool) {
	return f.rate, f.ok
}

func TestCheckCanary(t *testing.T) {
	newApp := func() *appsv1alpha1.App {
		app := newApp("demo", 2)
		app.Spec.Health = &appsv1alpha1.HealthPolicy{
			Canary: appsv1alpha1.CanaryPolicy{Enabled: true, ErrorRateThreshold: "0.01"},
		}
		return app
	}

	cases := []struct {
		name   string
		source ErrorRateSource
		want   bool
	}{
		{"错误率超阈值 → 回滚", fakeErrSource{rate: 0.05, ok: true}, true},
		{"错误率等于阈值 → 不回滚", fakeErrSource{rate: 0.01, ok: true}, false},
		{"错误率正常 → 不回滚", fakeErrSource{rate: 0.001, ok: true}, false},
		{"无数据 → 宁可放过", fakeErrSource{ok: false}, false},
		{"无数据源 → 不动作", nil, false},
	}
	for _, tc := range cases {
		e := newEngine()
		e.ErrSource = tc.source
		if got := e.CheckCanary(context.Background(), newApp()); got != tc.want {
			t.Errorf("%s: CheckCanary = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCheckCanary_BadThresholdFallsBack(t *testing.T) {
	app := newApp("demo", 2)
	app.Spec.Health = &appsv1alpha1.HealthPolicy{
		Canary: appsv1alpha1.CanaryPolicy{Enabled: true, ErrorRateThreshold: "not-a-number"},
	}
	e := newEngine()
	e.ErrSource = fakeErrSource{rate: 0.02, ok: true} // 默认阈值 0.01，0.02 超了
	if !e.CheckCanary(context.Background(), app) {
		t.Fatal("bad threshold string must fall back to 0.01")
	}
}

// ---------- 报告可读性（Event 文案） ----------

func TestReportSummary(t *testing.T) {
	r := Report{Unhealthy: true, Signals: []string{"CrashLoopBackOff(p1/c, restarts=8)"}, SuggestedLevel: L2Isolate}
	want := "unhealthy: [CrashLoopBackOff(p1/c, restarts=8)], suggested level: L2"
	if got := r.Summary(); got != want {
		t.Fatalf("Summary = %q, want %q", got, want)
	}
	if (Report{}).Summary() != "" {
		t.Fatal("healthy report must have empty summary")
	}
}
