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

package scaler

// 魔法数字可配化（11.03 评审意见③）的回归测试：
// 缩容步长、迟滞带、错误率扩容比例都从 CRD 读取，不再硬编码 20%/10%/20%。
// 另含 Apply 的冲突重试测试（评审意见②在伸缩路径的对称修复）。

import (
	"context"
	"fmt"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	appsv1alpha1 "github.com/weirong-ui/kubestasis/api/v1alpha1"
)

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

func TestDecide_CustomScaleDownStep(t *testing.T) {
	// scaleDownStepPercent=50：10 副本一次最多缩到 5（默认 20% 时只能缩到 8）。
	// 同时收窄迟滞带，让断言只体现步长一个变量。
	app := newApp(10, 1, 20, 0, appsv1alpha1.MetricTarget{Type: appsv1alpha1.MetricQPS, Target: "100"})
	app.Spec.Scaling.ScaleDownStepPercent = 50
	app.Spec.Scaling.HysteresisPercent = 1
	d := NewEngine().WithMetricsSource(fakeSource{vals: map[appsv1alpha1.MetricType]fakeVal{
		appsv1alpha1.MetricQPS: {1, true},
	}}).Decide(context.Background(), app, func() time.Time { return frozen })
	if d.Action != ActionScaleDown || d.ToReplicas != 5 {
		t.Fatalf("with 50%% step, want ScaleDown to 5, got %v -> %d", d.Action, d.ToReplicas)
	}
}

func TestDecide_CustomHysteresis(t *testing.T) {
	// hysteresisPercent=50：current=10 时带宽为 5，desired=14 与 10 差 4 → 不动作
	app := newApp(10, 1, 20, 0, appsv1alpha1.MetricTarget{Type: appsv1alpha1.MetricQPS, Target: "100"})
	app.Spec.Scaling.HysteresisPercent = 50
	d := NewEngine().WithMetricsSource(fakeSource{vals: map[appsv1alpha1.MetricType]fakeVal{
		appsv1alpha1.MetricQPS: {14, true},
	}}).Decide(context.Background(), app, func() time.Time { return frozen })
	if d.Action != ActionNone {
		t.Fatalf("diff 4 within 50%% band of 10, want None, got %v -> %d", d.Action, d.ToReplicas)
	}
}

func TestPrometheusSource_ErrorRateStepConfigurable(t *testing.T) {
	// errorRateScaleUpPercent=50：current=4 时错误率超阈值应扩 +2（默认 20% 只扩 +1）
	srv := promFake(t, `{"status":"success","data":{"result":[{"value":[1730000000,"0.5"]}]}}`, nil)
	defer srv.Close()

	app := testApp()
	app.Spec.Scaling = &appsv1alpha1.ScalingPolicy{ErrorRateScaleUpPercent: 50}
	got, ok := NewPrometheusSource(srv.URL).DesiredReplicas(context.Background(), app,
		appsv1alpha1.MetricTarget{Type: appsv1alpha1.MetricErrorRate, Target: "0.01"}, 4)
	if !ok || got != 6 {
		t.Fatalf("with 50%% error-rate step, want (6, true), got (%d, %v)", got, ok)
	}
}

func TestApply_RetriesOnConflict(t *testing.T) {
	app := newApp(2, 1, 20, 0)
	calls := 0
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(app).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object,
				patch client.Patch, opts ...client.PatchOption) error {
				if calls == 0 {
					calls++
					return apierrors.NewConflict(
						schema.GroupResource{Group: "apps.sentry.io", Resource: "apps"},
						obj.GetName(), fmt.Errorf("the object has been modified"))
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
		}).Build()

	d := Decision{Action: ActionScaleUp, FromReplicas: 2, ToReplicas: 5}
	if err := NewEngine().Apply(context.Background(), c, app, d); err != nil {
		t.Fatalf("conflict must be absorbed by RetryOnConflict, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 injected conflict, got %d", calls)
	}
	var got appsv1alpha1.App
	if err := c.Get(context.Background(), types.NamespacedName{Name: "sample", Namespace: "default"}, &got); err != nil {
		t.Fatal(err)
	}
	if *got.Spec.Replicas != 5 {
		t.Fatalf("replicas = %d, want 5", *got.Spec.Replicas)
	}
	if *app.Spec.Replicas != 5 {
		t.Fatalf("caller-side app.Spec.Replicas not synced, got %d", *app.Spec.Replicas)
	}
}
