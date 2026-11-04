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

package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	appsv1alpha1 "github.com/weirong-ui/kubestasis/api/v1alpha1"
	"github.com/weirong-ui/kubestasis/internal/scaler"
)

func demoApp() *appsv1alpha1.App {
	return &appsv1alpha1.App{ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "default"}}
}

// Action=None 的决策必须被计数：只统计"动过"的决策，就回答不了"为什么一直没扩"。
func TestRecord_CountsNoActionDecisions(t *testing.T) {
	Forget("default", "demo")
	defer Forget("default", "demo")

	Record(demoApp(), scaler.Decision{
		Action:       scaler.ActionNone,
		FromReplicas: 4,
		ToReplicas:   4,
		ReasonCode:   scaler.ReasonStabilizing,
	})

	got := testutil.ToFloat64(decisionsTotal.WithLabelValues("default", "demo", "None", scaler.ReasonStabilizing))
	if got != 1 {
		t.Fatalf("decisions_total{action=None,reason=stabilizing} = %v, want 1", got)
	}
}

// 各信号算出的副本数要能被单独观察，否则"是哪个信号在推伸缩"只能靠猜。
func TestRecord_ExportsSignalsAndDesired(t *testing.T) {
	Forget("default", "demo")
	defer Forget("default", "demo")

	Record(demoApp(), scaler.Decision{
		Action:       scaler.ActionScaleUp,
		FromReplicas: 2,
		ToReplicas:   5,
		ReasonCode:   scaler.ReasonScaleUp,
		Signals: map[appsv1alpha1.MetricType]int32{
			appsv1alpha1.MetricQPS:        5,
			appsv1alpha1.MetricP99Latency: 3,
		},
	})

	if v := testutil.ToFloat64(desiredReplicas.WithLabelValues("default", "demo")); v != 5 {
		t.Errorf("desired_replicas = %v, want 5", v)
	}
	if v := testutil.ToFloat64(signalReplicas.WithLabelValues("default", "demo", "QPS")); v != 5 {
		t.Errorf("signal_replicas{QPS} = %v, want 5", v)
	}
	if v := testutil.ToFloat64(signalReplicas.WithLabelValues("default", "demo", "P99Latency")); v != 3 {
		t.Errorf("signal_replicas{P99Latency} = %v, want 3", v)
	}
}

// App 删除后必须清掉时序：否则反复建删同名 App 会留下永远不归零的残留。
func TestForget_RemovesSeries(t *testing.T) {
	Forget("default", "demo")

	Record(demoApp(), scaler.Decision{
		Action:     scaler.ActionNone,
		ToReplicas: 1,
		ReasonCode: scaler.ReasonNoMetricsSource,
	})
	if n := testutil.CollectAndCount(desiredReplicas); n == 0 {
		t.Fatal("expected at least one series before Forget")
	}

	Forget("default", "demo")
	if n := testutil.CollectAndCount(desiredReplicas); n != 0 {
		t.Fatalf("desired_replicas still has %d series after Forget", n)
	}
	if n := testutil.CollectAndCount(decisionsTotal); n != 0 {
		t.Fatalf("decisions_total still has %d series after Forget", n)
	}
}
