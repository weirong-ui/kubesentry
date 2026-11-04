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

// Package envtest 是控制循环级集成测试：
// 起一个真实的（进程内）etcd + kube-apiserver，把 AppReconciler 挂进
// controller-runtime Manager，用真实 client 驱动 创建/状态漂移/删除 三条链路。
//
// 与 internal/controller 下 fake client 单测的分工：
//   - fake client 单测：Reconcile 单步逻辑（决策、补丁构造），毫秒级
//   - 本套件：watch → enqueue → Reconcile → 写回 的完整闭环，秒级
//
// 运行前提：KUBEBUILDER_ASSETS 指向 etcd/apiserver 二进制
// （`setup-envtest use 1.31.x -p path` 的输出）。缺二进制时所有用例 Skip，
// 设 KUBESTASIS_REQUIRE_ENVTEST=1 则直接失败（CI 门用，防止静默跳绿）。
package envtest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	appsv1alpha1 "github.com/weirong-ui/kubestasis/api/v1alpha1"
	"github.com/weirong-ui/kubestasis/internal/controller"
	"github.com/weirong-ui/kubestasis/internal/healer"
	"github.com/weirong-ui/kubestasis/internal/scaler"
)

var (
	k8sClient client.Client
	startErr  error
	cancel    context.CancelFunc
)

func TestMain(m *testing.M) {
	logf.SetLogger(zap.New(zap.UseDevMode(true), zap.WriteTo(os.Stderr)))

	// CRD 只装 config/crd/bases——与 CI chart 门同一份"源头产物"，
	// 保证这里验的就是 controller-gen 生成、用户 apply 的那份。
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		// 不打日志到 apiserver 默认输出，保持测试输出干净
		AttachControlPlaneOutput: false,
	}

	cfg, err := env.Start()
	if err != nil {
		startErr = fmt.Errorf("start envtest: %w", err)
		if os.Getenv("KUBESTASIS_REQUIRE_ENVTEST") == "1" {
			fmt.Fprintf(os.Stderr, "FATAL: %v\n", startErr)
			os.Exit(1)
		}
		// 无二进制环境（裸 go test ./...）：全部用例走 Skip，不阻塞本地开发
		os.Exit(m.Run())
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: add client-go scheme: %v\n", err)
		os.Exit(1)
	}
	if err := appsv1alpha1.AddToScheme(scheme); err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: add kubestasis scheme: %v\n", err)
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Logger:  logr.Discard(),
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: new manager: %v\n", err)
		os.Exit(1)
	}

	if err := (&controller.AppReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Scaler:   scaler.NewEngine(), // 未注入指标源，伸缩保持休眠——用例才确定
		Healer:   healer.NewEngine(),
		Recorder: mgr.GetEventRecorderFor("kubestasis-envtest"),
	}).SetupWithManager(mgr); err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: setup reconciler: %v\n", err)
		os.Exit(1)
	}

	var ctx context.Context
	ctx, cancel = context.WithCancel(context.Background())
	go func() {
		if err := mgr.Start(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "manager exited: %v\n", err)
		}
	}()

	k8sClient = mgr.GetClient()
	code := m.Run()

	cancel()
	if err := env.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "stop envtest: %v\n", err)
	}
	os.Exit(code)
}

// eventually 轮询条件直至满足或超时；超时带最后一次错误，便于定位"卡在哪种状态"。
func eventually(t *testing.T, timeout time.Duration, what string, cond func() (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		ok, err := cond()
		if err == nil && ok {
			return
		}
		lastErr = err
		if time.Now().After(deadline) {
			t.Fatalf("等待超时(%s): %s; lastErr=%v", timeout, what, lastErr)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// appAvailable 检查 App 是否进入 Available=True。
func appAvailable(app *appsv1alpha1.App) bool {
	return meta.IsStatusConditionTrue(app.Status.Conditions, "Available")
}
