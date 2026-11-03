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

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	appsv1alpha1 "github.com/weirong-ui/kubestasis/api/v1alpha1"
)

func TestParseTarget(t *testing.T) {
	tests := []struct {
		typ    appsv1alpha1.MetricType
		raw    string
		want   float64
		wantOK bool
	}{
		{appsv1alpha1.MetricQPS, "1000", 1000, true},
		{appsv1alpha1.MetricErrorRate, "0.01", 0.01, true},
		{appsv1alpha1.MetricP99Latency, "200ms", 0.2, true},
		{appsv1alpha1.MetricP99Latency, "1s", 1, true},
		{appsv1alpha1.MetricP99Latency, "0.5", 0.5, true}, // 裸数字按秒
		{appsv1alpha1.MetricQPS, "", 0, false},
		{appsv1alpha1.MetricQPS, "abc", 0, false},
		{appsv1alpha1.MetricQPS, "0", 0, false}, // 非正数没有意义
		{appsv1alpha1.MetricP99Latency, "xxms", 0, false},
	}
	for _, tc := range tests {
		got, ok := parseTarget(appsv1alpha1.MetricTarget{Type: tc.typ, Target: tc.raw})
		if ok != tc.wantOK || (ok && got != tc.want) {
			t.Errorf("parseTarget(%s,%q) = %v,%v want %v,%v", tc.typ, tc.raw, got, ok, tc.want, tc.wantOK)
		}
	}
}

// promFake 返回固定 JSON 的假 Prometheus，同时把收到的 query 记下来。
func promFake(t *testing.T, body string, queries *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if queries != nil {
			*queries = append(*queries, r.URL.Query().Get("query"))
		}
		fmt.Fprint(w, body)
	}))
}

func testApp() *appsv1alpha1.App {
	return &appsv1alpha1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "order", Namespace: "prod"},
	}
}

func TestPrometheusSource_QPS(t *testing.T) {
	srv := promFake(t, `{"status":"success","data":{"resultType":"vector","result":[{"value":[1730000000,"3000"]}]}}`, nil)
	defer srv.Close()

	p := NewPrometheusSource(srv.URL)
	got, ok := p.DesiredReplicas(context.Background(), testApp(),
		appsv1alpha1.MetricTarget{Type: appsv1alpha1.MetricQPS, Target: "1000"}, 3)
	if !ok || got != 3 {
		t.Fatalf("want 3,true; got %d,%v", got, ok)
	}
}

// QPS 查询的 PromQL 必须按 app/namespace 过滤——这是"查的是谁的数据"的唯一保证。
func TestPrometheusSource_QueryContainsAppLabels(t *testing.T) {
	var got []string
	srv := promFake(t, `{"status":"success","data":{"result":[]}}`, &got)
	defer srv.Close()

	p := NewPrometheusSource(srv.URL)
	p.DesiredReplicas(context.Background(), testApp(),
		appsv1alpha1.MetricTarget{Type: appsv1alpha1.MetricQPS, Target: "1000"}, 3)
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 query, got %d", len(got))
	}
	for _, want := range []string{`app="order"`, `namespace="prod"`, "rate(", "[5m]"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("query missing %s: %s", want, got[0])
		}
	}
}

func TestPrometheusSource_HealthSignalsAbstain(t *testing.T) {
	// P99 与错误率在健康时必须弃权（ok=false），
	// 否则多信号取 max 时会把 QPS 的缩容意见压掉，永远缩不下去。
	healthyP99 := promFake(t, `{"status":"success","data":{"result":[{"value":[1730000000,"0.1"]}]}}`, nil)
	defer healthyP99.Close()

	p := NewPrometheusSource(healthyP99.URL)
	if _, ok := p.DesiredReplicas(context.Background(), testApp(),
		appsv1alpha1.MetricTarget{Type: appsv1alpha1.MetricP99Latency, Target: "200ms"}, 5); ok {
		t.Fatal("healthy P99 should abstain (ok=false)")
	}

	lowErr := promFake(t, `{"status":"success","data":{"result":[{"value":[1730000000,"0.005"]}]}}`, nil)
	defer lowErr.Close()
	p2 := NewPrometheusSource(lowErr.URL)
	if _, ok := p2.DesiredReplicas(context.Background(), testApp(),
		appsv1alpha1.MetricTarget{Type: appsv1alpha1.MetricErrorRate, Target: "0.01"}, 5); ok {
		t.Fatal("healthy error rate should abstain (ok=false)")
	}
}

func TestPrometheusSource_BreachingSignals(t *testing.T) {
	tests := []struct {
		name    string
		typ     appsv1alpha1.MetricType
		target  string
		val     string
		current int32
		want    int32
	}{
		// p99=0.4s 目标 0.2s → 按 2 倍比例扩：ceil(10*0.4/0.2)=20
		{"P99 超阈值按比例扩", appsv1alpha1.MetricP99Latency, "200ms", "0.4", 10, 20},
		// 错误率 5% > 1% → 扩 20% 至少 1 个：10+2=12
		{"错误率超阈值扩 20%", appsv1alpha1.MetricErrorRate, "0.01", "0.05", 10, 12},
		// QPS 3000 / 单 Pod 1000 → 3
		{"QPS 换算副本", appsv1alpha1.MetricQPS, "1000", "3000", 1, 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := promFake(t, fmt.Sprintf(
				`{"status":"success","data":{"result":[{"value":[1730000000,%q]}]}}`, tc.val), nil)
			defer srv.Close()

			p := NewPrometheusSource(srv.URL)
			got, ok := p.DesiredReplicas(context.Background(), testApp(),
				appsv1alpha1.MetricTarget{Type: tc.typ, Target: tc.target}, tc.current)
			if !ok || got != tc.want {
				t.Fatalf("want %d,true; got %d,%v", tc.want, got, ok)
			}
		})
	}
}

// 查询失败/空结果/NaN 一律返回 ok=false，绝不能返回 0 —— 那会把副本缩穿。
func TestPrometheusSource_NoDataIsNotZero(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"空向量", `{"status":"success","data":{"resultType":"vector","result":[]}}`},
		{"NaN（histogram 样本不足的正常输出）", `{"status":"success","data":{"result":[{"value":[1730000000,"NaN"]}]}}`},
		{"Prometheus 报错", `{"status":"error","errorType":"bad_data","error":"parse error"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := promFake(t, tc.body, nil)
			defer srv.Close()
			p := NewPrometheusSource(srv.URL)
			if _, ok := p.DesiredReplicas(context.Background(), testApp(),
				appsv1alpha1.MetricTarget{Type: appsv1alpha1.MetricQPS, Target: "1000"}, 5); ok {
				t.Fatalf("%s: should abstain (ok=false)", tc.name)
			}
		})
	}
}

func TestPrometheusSource_EndpointTrailingSlash(t *testing.T) {
	// 尾部斜杠必须被清掉，否则 url.JoinPath 会拼出 //api/v1/query
	srv := promFake(t, `{"status":"success","data":{"result":[]}}`, nil)
	defer srv.Close()

	p := NewPrometheusSource(srv.URL + "/")
	u, err := url.JoinPath(p.Endpoint, "/api/v1/query")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(u, "//api") {
		t.Fatalf("double slash in endpoint: %s", u)
	}
}
