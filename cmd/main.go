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

// main 入口：启动 KubeStasis Manager（承载 Controller 与 Webhook）。
package main

import (
	"flag"
	"os"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	appsv1alpha1 "github.com/weirong-ui/kubestasis/api/v1alpha1"
	"github.com/weirong-ui/kubestasis/internal/controller"
	"github.com/weirong-ui/kubestasis/internal/healer"
	"github.com/weirong-ui/kubestasis/internal/scaler"
	"github.com/weirong-ui/kubestasis/internal/webhook"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(appsv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var promURL string
	var promWindow string
	var healCooldown time.Duration
	var enableWebhook bool
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Ensuring only one active instance.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	// Prometheus 地址：不配则伸缩引擎休眠（Decide 返回"无指标源"），其余功能不受影响。
	// 集群内常见值：http://prometheus-kube-prometheus-prometheus.monitoring.svc.cluster.local:9090
	flag.StringVar(&promURL, "prometheus-url", os.Getenv("PROMETHEUS_URL"),
		"Prometheus Query API endpoint; scaling stays dormant when empty.")
	flag.StringVar(&promWindow, "prometheus-window", "5m",
		"PromQL rate window for scaling/canary queries (demo 用 1m 反应更快).")
	flag.DurationVar(&healCooldown, "heal-cooldown", 3*time.Minute,
		"Observation period between graduated self-heal actions; same fault within this window escalates the level.")
	// Webhook 默认关闭：它需要 TLS 证书（/tmp/k8s-webhook-server/serving-certs），
	// 裸 `make deploy` 不装证书，开了会 CrashLoop。用 make deploy-webhook 一键
	// 部署带 Webhook 的形态（overlay + hack/webhook-certs.sh 自签证书注入）。
	flag.BoolVar(&enableWebhook, "enable-webhook", false,
		"Enable admission webhooks (requires TLS certs; use make deploy-webhook).")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: probeAddr,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "kubestasis.sentry.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	scaleEngine := scaler.NewEngine()
	healEngine := healer.NewEngine()
	healEngine.HealCooldown = healCooldown
	if promURL != "" {
		promSource := scaler.NewPrometheusSource(promURL)
		promSource.Window = promWindow
		scaleEngine.WithMetricsSource(promSource)
		// 同一个源同时给金丝雀回滚提供错误率（5xx 占比）
		healEngine.ErrSource = promSource
		setupLog.Info("scaling & canary enabled", "prometheus", promURL, "window", promWindow)
	} else {
		setupLog.Info("scaling dormant: no --prometheus-url configured")
	}

	if err := (&controller.AppReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Scaler:   scaleEngine,
		Healer:   healEngine,
		Recorder: mgr.GetEventRecorderFor("kubestasis"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "App")
		os.Exit(1)
	}

	if enableWebhook {
		if err := ctrl.NewWebhookManagedBy(mgr).
			For(&appsv1alpha1.App{}).
			WithValidator(webhook.AppValidator{}).
			WithDefaulter(webhook.AppDefaulter{}).
			Complete(); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "App")
			os.Exit(1)
		}
		setupLog.Info("admission webhook enabled", "port", 9443)
	}

	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting KubeStasis manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
