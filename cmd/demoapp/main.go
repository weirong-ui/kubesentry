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

// demoapp 是 KubeStasis 的演示负载：一个暴露 Prometheus 指标的最小 HTTP 服务。
//
// 设计取舍：
//   - 不引 client_golang：/metrics 就是几行文本，手写文本格式零依赖，
//     镜像里少一层 supply chain
//   - 指标标签 app=/namespace= 来自 KubeStasis 注入的 APP_NAME/POD_NAMESPACE
//     环境变量，与 internal/scaler/prometheus.go 的 PromQL 模板约定一致
//   - -crash 模式用于构建 crashloop 镜像，验证自愈引擎
//   - -loadgen 模式把同一个二进制变成负载发生器，demo 少维护一个镜像
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"sync/atomic"
	"time"
)

var (
	requestsOK  atomic.Int64
	requests5xx atomic.Int64
)

func main() {
	listen := flag.String("listen", ":8080", "HTTP listen address")
	crash := flag.Bool("crash", false, "exit immediately (builds a crashloop image)")
	loadgen := flag.String("loadgen", "", "load generator mode: target base URL")
	qps := flag.Int("qps", 100, "loadgen requests per second")
	flag.Parse()

	if *crash {
		fmt.Fprintln(os.Stderr, "demoapp: -crash specified, exiting (simulating a broken release)")
		os.Exit(1)
	}
	if *loadgen != "" {
		runLoadgen(*loadgen, *qps)
		return
	}

	appName := os.Getenv("APP_NAME")
	if appName == "" {
		appName = "unknown"
	}
	namespace := os.Getenv("POD_NAMESPACE")
	if namespace == "" {
		namespace = "default"
	}

	// ERROR_RATE=0.3 表示 30% 的请求返回 500，用于金丝雀回滚演示
	errorRate := 0.0
	if v := os.Getenv("ERROR_RATE"); v != "" {
		fmt.Sscanf(v, "%f", &errorRate)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if errorRate > 0 && rand.Float64() < errorRate {
			requests5xx.Add(1)
			http.Error(w, "injected failure", http.StatusInternalServerError)
			return
		}
		requestsOK.Add(1)
		fmt.Fprintf(w, "ok from %s/%s\n", namespace, appName)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		// Prometheus 文本格式。标签维度与 scaler 默认 PromQL 对齐：
		//   sum(rate(http_requests_total{app="...",namespace="..."}[5m]))
		//   sum(rate(http_requests_total{...,status=~"5.."}[5m]))
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintf(w, "# HELP http_requests_total Total HTTP requests.\n# TYPE http_requests_total counter\n")
		fmt.Fprintf(w, "http_requests_total{app=%q,namespace=%q,status=\"200\"} %d\n",
			appName, namespace, requestsOK.Load())
		fmt.Fprintf(w, "http_requests_total{app=%q,namespace=%q,status=\"500\"} %d\n",
			appName, namespace, requests5xx.Load())
	})

	fmt.Printf("demoapp listening on %s (app=%s namespace=%s errorRate=%.2f)\n", *listen, appName, namespace, errorRate)
	if err := http.ListenAndServe(*listen, mux); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// runLoadgen 以固定 QPS 打目标地址。token bucket 简化版：按 tick 发放配额。
func runLoadgen(target string, qps int) {
	if qps <= 0 {
		qps = 1
	}
	fmt.Printf("loadgen: %d qps -> %s\n", qps, target)
	client := &http.Client{Timeout: 2 * time.Second}
	interval := time.Second / time.Duration(qps)
	var ok, fail atomic.Int64
	go func() { // 每 10s 打一次统计，demo 录屏时肉眼可见
		for range time.Tick(10 * time.Second) {
			fmt.Printf("loadgen stats: ok=%d fail=%d\n", ok.Load(), fail.Load())
		}
	}()
	for range time.Tick(interval) {
		go func() {
			resp, err := client.Get(target + "/")
			if err != nil {
				fail.Add(1)
				return
			}
			resp.Body.Close()
			if resp.StatusCode < 500 {
				ok.Add(1)
			} else {
				fail.Add(1)
			}
		}()
	}
}
