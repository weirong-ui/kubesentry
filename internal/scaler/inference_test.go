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

// 推理场景信号（QueueDepth / TTFT / KVCacheUsage）的回归测试。
// 这是"通用自愈平移到 AI 推理弹性"的那一步：不重写项目，
// 只是 MetricsSource 接口下的三个新信号类型（默认 vLLM 指标名，WithQuery 可覆盖）。

import (
	"context"
	"fmt"
	"strings"
	"testing"

	appsv1alpha1 "github.com/weirong-ui/kubestasis/api/v1alpha1"
)

func TestPrometheusSource_InferenceSignals(t *testing.T) {
	tests := []struct {
		name    string
		typ     appsv1alpha1.MetricType
		target  string
		val     string
		current int32
		want    int32
		wantOK  bool
	}{
		// 队列 37 个排队请求 / 每 Pod 目标 5 个 → ceil(37/5)=8
		{"队列深度换算副本", appsv1alpha1.MetricQueueDepth, "5", "37", 2, 8, true},
		// 队列清空 = 真实零排队（不是无数据）→ 明确缩到 1，由 Decide 钳到 min
		{"队列清空缩到底", appsv1alpha1.MetricQueueDepth, "5", "0", 6, 1, true},
		// TTFT 2.4s / 目标 800ms → 按 3 倍扩：4*3=12
		{"TTFT 超阈值按比例扩", appsv1alpha1.MetricTTFT, "800ms", "2.4", 4, 12, true},
		// TTFT 达标 → 弃权，绝不压掉别的信号的缩容意见
		{"TTFT 健康弃权", appsv1alpha1.MetricTTFT, "800ms", "0.3", 4, 0, false},
		// KV cache 0.95 / 目标 0.8 → 4*0.95/0.8=4.75 → 5
		{"KV cache 超阈值按比例扩", appsv1alpha1.MetricKVCacheUsage, "0.8", "0.95", 4, 5, true},
		{"KV cache 健康弃权", appsv1alpha1.MetricKVCacheUsage, "0.8", "0.4", 4, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := promFake(t, fmt.Sprintf(
				`{"status":"success","data":{"result":[{"value":[1730000000,%q]}]}}`, tc.val), nil)
			defer srv.Close()
			got, ok := NewPrometheusSource(srv.URL).DesiredReplicas(context.Background(), testApp(),
				appsv1alpha1.MetricTarget{Type: tc.typ, Target: tc.target}, tc.current)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("want (%d, %v); got (%d, %v)", tc.want, tc.wantOK, got, ok)
			}
		})
	}
}

// 默认 PromQL 必须指向 vLLM 指标名——这是"开箱即用对接 vLLM"的承诺。
func TestPrometheusSource_InferenceDefaultsAreVLLM(t *testing.T) {
	var got []string
	srv := promFake(t, `{"status":"success","data":{"result":[]}}`, &got)
	defer srv.Close()

	p := NewPrometheusSource(srv.URL)
	// target 必须各自合法，否则准入期解析就拦截了，根本到不了查询那一步
	targets := map[appsv1alpha1.MetricType]string{
		appsv1alpha1.MetricQueueDepth:   "5",
		appsv1alpha1.MetricTTFT:         "1s",
		appsv1alpha1.MetricKVCacheUsage: "0.8",
	}
	for typ, target := range targets {
		p.DesiredReplicas(context.Background(), testApp(),
			appsv1alpha1.MetricTarget{Type: typ, Target: target}, 1)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 queries, got %d", len(got))
	}
	for _, want := range []string{"vllm:num_requests_waiting", "vllm:time_to_first_token_seconds_bucket", "vllm:gpu_cache_usage_perc"} {
		found := false
		for _, q := range got {
			if strings.Contains(q, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("no query contains %s; queries=%v", want, got)
		}
	}
}

func TestParseTarget_InferenceTypes(t *testing.T) {
	// 占比类：越界必须拒绝（运行期与 webhook 准入同规则）
	for _, bad := range []string{"0", "1", "1.2", "abc"} {
		if _, ok := parseTarget(appsv1alpha1.MetricTarget{Type: appsv1alpha1.MetricKVCacheUsage, Target: bad}); ok {
			t.Errorf("KVCacheUsage target %q must be rejected", bad)
		}
	}
	if v, ok := parseTarget(appsv1alpha1.MetricTarget{Type: appsv1alpha1.MetricKVCacheUsage, Target: "0.85"}); !ok || v != 0.85 {
		t.Errorf("KVCacheUsage 0.85 should parse, got %v,%v", v, ok)
	}
	// TTFT 与 P99 同规则：支持 duration 字符串
	if v, ok := parseTarget(appsv1alpha1.MetricTarget{Type: appsv1alpha1.MetricTTFT, Target: "800ms"}); !ok || v != 0.8 {
		t.Errorf("TTFT 800ms should parse to 0.8s, got %v,%v", v, ok)
	}
	// QueueDepth 与 QPS 同规则：正数
	if _, ok := parseTarget(appsv1alpha1.MetricTarget{Type: appsv1alpha1.MetricQueueDepth, Target: "-1"}); ok {
		t.Error("QueueDepth -1 must be rejected")
	}
}
