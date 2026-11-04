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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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

// ---------- spec.resources ----------

func TestValidate_ResourcesRequestsExceedLimits(t *testing.T) {
	app := validApp()
	app.Spec.Resources = &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
	}
	_, err := validateApp(app)
	if err == nil || !strings.Contains(err.Error(), "不能大于 limits") {
		t.Fatalf("requests>limits must be rejected, got %v", err)
	}
}

func TestValidate_ResourcesCPURequestWithoutLimitWarns(t *testing.T) {
	app := validApp()
	app.Spec.Resources = &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
	}
	warns, err := validateApp(app)
	if err != nil {
		t.Fatalf("request-only resources must pass with warning, got %v", err)
	}
	if len(warns) == 0 || !strings.Contains(strings.Join(warns, ";"), "cpu limit") {
		t.Fatalf("expected cpu-limit warning, got %v", warns)
	}
}

func TestValidate_ResourcesValidPasses(t *testing.T) {
	app := validApp()
	app.Spec.Resources = &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("250m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
	}
	if warns, err := validateApp(app); err != nil {
		t.Fatalf("valid resources rejected: %v (warns %v)", err, warns)
	}
}

func TestValidate_RejectsReservedEnvName(t *testing.T) {
	app := validApp()
	app.Spec.Env = []corev1.EnvVar{{Name: "APP_NAME", Value: "hijacked"}}
	_, err := validateApp(app)
	if err == nil || !strings.Contains(err.Error(), "保留变量") {
		t.Fatalf("reserved env must be rejected, got %v", err)
	}
}

func TestValidate_RejectsDuplicateEnv(t *testing.T) {
	app := validApp()
	app.Spec.Env = []corev1.EnvVar{{Name: "FOO", Value: "1"}, {Name: "FOO", Value: "2"}}
	_, err := validateApp(app)
	if err == nil || !strings.Contains(err.Error(), "重复") {
		t.Fatalf("duplicate env must be rejected, got %v", err)
	}
}

func TestValidate_UserEnvPasses(t *testing.T) {
	app := validApp()
	app.Spec.Env = []corev1.EnvVar{{Name: "CPU_SPIN_MS", Value: "8"}}
	if _, err := validateApp(app); err != nil {
		t.Fatalf("user env rejected: %v", err)
	}
}

// ---------- 探针校验 ----------

// HTTPGet 的 path 必须以 "/" 开头：不拦的话运行期探针永远失败，
// 表现为 Pod 一直 NotReady，但用户看不出是配置写错了。
func TestValidate_HTTPProbePathMustBeAbsolute(t *testing.T) {
	app := validApp()
	app.Spec.Readiness = &appsv1alpha1.ProbeConfig{
		Type: appsv1alpha1.ProbeHTTPGet,
		Path: "healthz",
	}
	_, err := validateApp(app)
	if err == nil || !strings.Contains(err.Error(), "必须以 \"/\" 开头") {
		t.Fatalf("want absolute-path error, got %v", err)
	}
}

// TCPSocket 配了 path 是明显笔误（会被静默忽略）——只警告，不阻断。
func TestValidate_TCPProbeWithPathWarns(t *testing.T) {
	app := validApp()
	app.Spec.Liveness = &appsv1alpha1.ProbeConfig{Path: "/healthz"}
	warns, err := validateApp(app)
	if err != nil {
		t.Fatalf("should not block: %v", err)
	}
	found := false
	for _, w := range warns {
		if strings.Contains(w, "liveness.path") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a warning about ignored liveness.path, got %v", warns)
	}
}

// 合法探针配置必须放行（别把默认用法也拦了）。
func TestValidate_ValidProbesPass(t *testing.T) {
	app := validApp()
	app.Spec.Readiness = &appsv1alpha1.ProbeConfig{
		Type: appsv1alpha1.ProbeHTTPGet, Path: "/readyz", InitialDelaySeconds: 5,
	}
	app.Spec.Liveness = &appsv1alpha1.ProbeConfig{Type: appsv1alpha1.ProbeTCPSocket}
	if _, err := validateApp(app); err != nil {
		t.Fatalf("valid probes should pass: %v", err)
	}
}
