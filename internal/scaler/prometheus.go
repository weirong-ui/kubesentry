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
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	appsv1alpha1 "github.com/weirong-ui/kubesentry/api/v1alpha1"
)

// 默认 PromQL 模板。占位符：{app} {namespace} {window}。
// 指标名沿用 Prometheus 社区约定（http_requests_total / http_request_duration_seconds_bucket），
// 不匹配自家指标时用 WithQuery 覆盖，不要改源码默认值去迁就一套私有指标名。
const (
	defaultQPSQuery = `sum(rate(http_requests_total{app="{app}",namespace="{namespace}"}[{window}]))`
	defaultP99Query = `histogram_quantile(0.99, sum by (le) (rate(http_request_duration_seconds_bucket{app="{app}",namespace="{namespace}"}[{window}])))`
	defaultErrQuery = `sum(rate(http_requests_total{app="{app}",namespace="{namespace}",status=~"5.."}[{window}]))` +
		` / clamp_min(sum(rate(http_requests_total{app="{app}",namespace="{namespace}"}[{window}])), 0.001)`

	defaultWindow = "5m"
)

// PrometheusSource 通过 Prometheus HTTP Query API 取指标，实现 MetricsSource。
//
// 只依赖标准库：Prometheus 的 /api/v1/query 就是一个 GET，引入 client_golang
// 会拖进来一整套依赖树，而这里只需要"查一个数"。
type PrometheusSource struct {
	Endpoint string
	Window   string
	Client   *http.Client
	Queries  map[appsv1alpha1.MetricType]string
}

// NewPrometheusSource 创建一个查询源。endpoint 形如
// http://prometheus.monitoring.svc.cluster.local:9090（尾部斜杠会被清掉）。
func NewPrometheusSource(endpoint string) *PrometheusSource {
	return &PrometheusSource{
		Endpoint: strings.TrimRight(endpoint, "/"),
		Window:   defaultWindow,
		Client:   &http.Client{Timeout: 5 * time.Second},
		Queries: map[appsv1alpha1.MetricType]string{
			appsv1alpha1.MetricQPS:        defaultQPSQuery,
			appsv1alpha1.MetricP99Latency: defaultP99Query,
			appsv1alpha1.MetricErrorRate:  defaultErrQuery,
		},
	}
}

// WithQuery 覆盖某个指标的 PromQL。
func (p *PrometheusSource) WithQuery(t appsv1alpha1.MetricType, promQL string) *PrometheusSource {
	p.Queries[t] = promQL
	return p
}

// ErrorRate 实现 healer.ErrorRateSource：返回当前 5xx 错误率（0~1）。
// 查询失败/无数据返回 ok=false —— 金丝雀检查"宁可放过，绝不误回滚"。
func (p *PrometheusSource) ErrorRate(ctx context.Context, app *appsv1alpha1.App) (float64, bool) {
	tmpl, ok := p.Queries[appsv1alpha1.MetricErrorRate]
	if !ok {
		return 0, false
	}
	val, ok, err := p.queryScalar(ctx, p.render(tmpl, app))
	if err != nil || !ok {
		return 0, false
	}
	return val, true
}

// DesiredReplicas 计算该指标下"应该"有多少副本。
//
// 设计约定（很重要）：**每个信号只在"不满意"时才有意见**。
//   - QPS 是绝对信号，总能算出副本数（没流量时视为无数据）
//   - P99 / 错误率 只在超阈值时返回扩容意见，健康时返回 ok=false（弃权）
//
// 如果让"健康的 P99"也返回意见（比如返回 current），多信号取 max 时会把
// QPS 提出的缩容意见压掉，导致永远缩不下去。弃权才是正确的表达。
func (p *PrometheusSource) DesiredReplicas(ctx context.Context, app *appsv1alpha1.App,
	target appsv1alpha1.MetricTarget, current int32) (int32, bool) {

	tmpl, ok := p.Queries[target.Type]
	if !ok {
		return 0, false
	}
	targetVal, ok := parseTarget(target)
	if !ok {
		return 0, false
	}

	promQL := p.render(tmpl, app)
	val, ok, err := p.queryScalar(ctx, promQL)
	if err != nil || !ok {
		return 0, false
	}

	switch target.Type {
	case appsv1alpha1.MetricQPS:
		// desired = ceil(总 QPS / 单 Pod 目标 QPS)。
		// 注意区分两种"0"：查询成功且值为 0（真实零流量 → 明确缩到 1，由
		// Decide 钳制到 minReplicas）与无数据/查询失败（ok=false 弃权）。
		// 若把真实零流量也当弃权，应用在流量退去后将永远缩不回去。
		if val <= 0 {
			return 1, true
		}
		return int32(math.Ceil(val / targetVal)), true

	case appsv1alpha1.MetricP99Latency:
		// 只在超阈值时按比例扩容；健康时弃权
		if val <= targetVal {
			return 0, false
		}
		return int32(math.Ceil(float64(current) * val / targetVal)), true

	case appsv1alpha1.MetricErrorRate:
		// 错误率无法换算成副本数：按 20%（至少 1 个）扩容吸收，
		// 连续超阈值会在多个 Reconcile 里持续扩容，直到错误率回落或撞上 maxReplicas
		if val <= targetVal {
			return 0, false
		}
		step := int32(math.Ceil(float64(current) * 0.2))
		if step < 1 {
			step = 1
		}
		return current + step, true
	}
	return 0, false
}

// parseTarget 把 CR 里的字符串目标值解析成 float（延迟统一为秒）。
func parseTarget(t appsv1alpha1.MetricTarget) (float64, bool) {
	if strings.TrimSpace(t.Target) == "" {
		return 0, false
	}
	switch t.Type {
	case appsv1alpha1.MetricQPS, appsv1alpha1.MetricErrorRate:
		v, err := strconv.ParseFloat(t.Target, 64)
		if err != nil || v <= 0 {
			return 0, false
		}
		return v, true

	case appsv1alpha1.MetricP99Latency:
		// 支持 "200ms" / "0.2" / "1s"。裸数字按秒处理。
		if v, err := strconv.ParseFloat(t.Target, 64); err == nil && v > 0 {
			return v, true
		}
		d, err := time.ParseDuration(t.Target)
		if err != nil || d <= 0 {
			return 0, false
		}
		return d.Seconds(), true
	}
	return 0, false
}

// render 填充 PromQL 模板占位符。
func (p *PrometheusSource) render(tmpl string, app *appsv1alpha1.App) string {
	window := p.Window
	if window == "" {
		window = defaultWindow
	}
	r := strings.NewReplacer("{app}", app.Name, "{namespace}", app.Namespace, "{window}", window)
	return r.Replace(tmpl)
}

// promResponse 只解码用得上的字段。
type promResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Value []any `json:"value"` // [unixTS, "字符串形式的数值"]
		} `json:"result"`
	} `json:"data"`
}

// queryScalar 执行 instant query 并返回标量值。
// 返回 ok=false 表示"没有可用数据"（空结果 / NaN / 非 success），不视为错误。
func (p *PrometheusSource) queryScalar(ctx context.Context, promQL string) (float64, bool, error) {
	u, err := url.JoinPath(p.Endpoint, "/api/v1/query")
	if err != nil {
		return 0, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, false, err
	}
	q := req.URL.Query()
	q.Set("query", promQL)
	req.URL.RawQuery = q.Encode()

	resp, err := p.Client.Do(req)
	if err != nil {
		return 0, false, fmt.Errorf("prometheus query: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, false, fmt.Errorf("prometheus returned HTTP %d", resp.StatusCode)
	}

	var out promResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, false, fmt.Errorf("decode prometheus response: %w", err)
	}
	if out.Status != "success" {
		return 0, false, fmt.Errorf("prometheus error (%s): %s", out.ErrorType, out.Error)
	}
	if len(out.Data.Result) == 0 || len(out.Data.Result[0].Value) < 2 {
		return 0, false, nil // 空向量：指标还没采集到
	}

	s, ok := out.Data.Result[0].Value[1].(string)
	if !ok {
		return 0, false, nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		// NaN 是 histogram_quantile 在样本不足时的正常输出，不是故障
		return 0, false, nil
	}
	return v, true, nil
}
