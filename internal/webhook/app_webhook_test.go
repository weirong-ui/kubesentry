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

package webhook

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	appsv1alpha1 "github.com/weirong-ui/kubestasis/api/v1alpha1"
)

func validApp() *appsv1alpha1.App {
	r := int32(2)
	return &appsv1alpha1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "order", Namespace: "prod"},
		Spec: appsv1alpha1.AppSpec{
			Image:    "registry.example.com/order:v1.2.0",
			Replicas: &r,
			Port:     8080,
		},
	}
}

// ---------- Defaulter ----------

func TestDefault_ReplicasFollowsMinReplicas(t *testing.T) {
	app := validApp()
	app.Spec.Replicas = nil
	app.Spec.Scaling = &appsv1alpha1.ScalingPolicy{MinReplicas: 3, MaxReplicas: 10}
	if err := (AppDefaulter{}).Default(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	if app.Spec.Replicas == nil || *app.Spec.Replicas != 3 {
		t.Fatalf("replicas should follow minReplicas=3, got %v", app.Spec.Replicas)
	}
}

func TestDefault_NoScalingDefaultsToOne(t *testing.T) {
	app := validApp()
	app.Spec.Replicas = nil
	if err := (AppDefaulter{}).Default(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	if app.Spec.Replicas == nil || *app.Spec.Replicas != 1 {
		t.Fatalf("replicas should default to 1, got %v", app.Spec.Replicas)
	}
}

func TestDefault_ExplicitReplicasUntouched(t *testing.T) {
	app := validApp() // replicas=2
	if err := (AppDefaulter{}).Default(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	if *app.Spec.Replicas != 2 {
		t.Fatalf("explicit replicas must not be overwritten, got %d", *app.Spec.Replicas)
	}
}

// ---------- Validator ----------

func TestValidate_ValidAppPasses(t *testing.T) {
	app := validApp()
	app.Spec.Scaling = &appsv1alpha1.ScalingPolicy{
		MinReplicas: 1, MaxReplicas: 10,
		Metrics: []appsv1alpha1.MetricTarget{
			{Type: appsv1alpha1.MetricQPS, Target: "1000"},
			{Type: appsv1alpha1.MetricP99Latency, Target: "200ms"},
			{Type: appsv1alpha1.MetricErrorRate, Target: "0.01"},
		},
	}
	app.Spec.Health = &appsv1alpha1.HealthPolicy{
		Strategy: appsv1alpha1.StrategyGraduated,
		Canary:   appsv1alpha1.CanaryPolicy{Enabled: true, ObservationSeconds: 300, ErrorRateThreshold: "0.02"},
	}
	warns, err := (AppValidator{}).ValidateCreate(context.Background(), app)
	if err != nil {
		t.Fatalf("valid app rejected: %v", err)
	}
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
}

func TestValidate_RejectsBadImage(t *testing.T) {
	for _, image := range []string{"nginx:latest", "nginx", "registry.example.com/x"} {
		app := validApp()
		app.Spec.Image = image
		if _, err := (AppValidator{}).ValidateCreate(context.Background(), app); err == nil {
			t.Fatalf("image %q must be rejected", image)
		} else if !strings.Contains(err.Error(), "latest") && !strings.Contains(err.Error(), "tag") {
			t.Fatalf("error should explain the tag rule, got: %v", err)
		}
	}
}

func TestValidate_RejectsMinGreaterThanMax(t *testing.T) {
	app := validApp()
	app.Spec.Scaling = &appsv1alpha1.ScalingPolicy{MinReplicas: 10, MaxReplicas: 3}
	app.Spec.Replicas = nil // 避免连带触发区间校验，专注 min>max
	if _, err := (AppValidator{}).ValidateCreate(context.Background(), app); err == nil {
		t.Fatal("minReplicas > maxReplicas must be rejected")
	}
}

func TestValidate_ReplicasOutsideMinMax(t *testing.T) {
	app := validApp() // replicas=2
	app.Spec.Scaling = &appsv1alpha1.ScalingPolicy{MinReplicas: 5, MaxReplicas: 10}
	if _, err := (AppValidator{}).ValidateCreate(context.Background(), app); err == nil {
		t.Fatal("replicas below minReplicas must be rejected")
	}
}

func TestValidate_RejectsBadMetricTargets(t *testing.T) {
	cases := []appsv1alpha1.MetricTarget{
		{Type: appsv1alpha1.MetricQPS, Target: "abc"},
		{Type: appsv1alpha1.MetricQPS, Target: "-5"},
		{Type: appsv1alpha1.MetricErrorRate, Target: "1.5"},
		{Type: appsv1alpha1.MetricP99Latency, Target: "soon"},
		{Type: appsv1alpha1.MetricP99Latency, Target: "-1s"},
	}
	for _, m := range cases {
		app := validApp()
		app.Spec.Replicas = nil
		app.Spec.Scaling = &appsv1alpha1.ScalingPolicy{
			MinReplicas: 1, MaxReplicas: 5, Metrics: []appsv1alpha1.MetricTarget{m},
		}
		if _, err := (AppValidator{}).ValidateCreate(context.Background(), app); err == nil {
			t.Fatalf("target %+v must be rejected", m)
		}
	}
}

func TestValidate_RejectsDuplicateMetricType(t *testing.T) {
	app := validApp()
	app.Spec.Replicas = nil
	app.Spec.Scaling = &appsv1alpha1.ScalingPolicy{
		MinReplicas: 1, MaxReplicas: 5,
		Metrics: []appsv1alpha1.MetricTarget{
			{Type: appsv1alpha1.MetricQPS, Target: "100"},
			{Type: appsv1alpha1.MetricQPS, Target: "200"},
		},
	}
	if _, err := (AppValidator{}).ValidateCreate(context.Background(), app); err == nil {
		t.Fatal("duplicate metric type must be rejected")
	}
}

func TestValidate_RejectsBadCanaryThreshold(t *testing.T) {
	for _, threshold := range []string{"abc", "0", "1.5", "-0.1"} {
		app := validApp()
		app.Spec.Health = &appsv1alpha1.HealthPolicy{
			Canary: appsv1alpha1.CanaryPolicy{Enabled: true, ObservationSeconds: 300, ErrorRateThreshold: threshold},
		}
		if _, err := (AppValidator{}).ValidateCreate(context.Background(), app); err == nil {
			t.Fatalf("canary threshold %q must be rejected", threshold)
		}
	}
}

func TestValidate_RejectsTooShortObservationWindow(t *testing.T) {
	app := validApp()
	app.Spec.Health = &appsv1alpha1.HealthPolicy{
		Canary: appsv1alpha1.CanaryPolicy{Enabled: true, ObservationSeconds: 10, ErrorRateThreshold: "0.01"},
	}
	if _, err := (AppValidator{}).ValidateCreate(context.Background(), app); err == nil {
		t.Fatal("observationSeconds=10 must be rejected (metrics not yet scraped)")
	}
}

func TestValidate_UpdatePathAlsoChecked(t *testing.T) {
	bad := validApp()
	bad.Spec.Image = "nginx:latest"
	if _, err := (AppValidator{}).ValidateUpdate(context.Background(), validApp(), bad); err == nil {
		t.Fatal("update path must validate the new object")
	}
}

func TestValidate_DeleteAlwaysAllowed(t *testing.T) {
	bad := validApp()
	bad.Spec.Image = "nginx:latest" // 历史遗留对象也要能删
	if _, err := (AppValidator{}).ValidateDelete(context.Background(), bad); err != nil {
		t.Fatalf("delete must never be blocked, got %v", err)
	}
}

func TestValidate_PrivilegedPortWarnsNotRejects(t *testing.T) {
	app := validApp()
	app.Spec.Port = 443
	warns, err := (AppValidator{}).ValidateCreate(context.Background(), app)
	if err != nil {
		t.Fatalf("privileged port should warn, not reject: %v", err)
	}
	if len(warns) != 1 {
		t.Fatalf("expected 1 warning, got %v", warns)
	}
}

// 回归：聚合错误必须列出全部问题，而不是只报第一条——用户改一次就该看到所有错。
func TestValidate_AggregatesAllErrors(t *testing.T) {
	app := validApp()
	app.Spec.Image = "nginx:latest"
	app.Spec.Scaling = &appsv1alpha1.ScalingPolicy{MinReplicas: 10, MaxReplicas: 3}
	_, err := (AppValidator{}).ValidateCreate(context.Background(), app)
	if err == nil {
		t.Fatal("must be rejected")
	}
	if !strings.Contains(err.Error(), "latest") || !strings.Contains(err.Error(), "minReplicas") {
		t.Fatalf("error should aggregate both problems, got: %v", err)
	}
}
