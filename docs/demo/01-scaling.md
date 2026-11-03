# Demo #1：真实 Prometheus 驱动的弹性伸缩

> 验证时间：2026-11-03 ｜ 环境：Docker Desktop Kubernetes v1.36.1（单节点）
> 指标源：**真实的 Prometheus v2.54.1**（Pod 级服务发现，5s 抓取间隔），不是 mock。
> 工作负载：`cmd/demoapp`（暴露 `http_requests_total{app,namespace,status}` 的最小 HTTP 服务）

## 拓扑

```
loadgen (-loadgen -qps 500)
   │  HTTP
   ▼
Service scaledemo ──► demoapp Pod × N   （KubeSentry App CR 管理）
                          ▲ /metrics
                          │ 每 5s 抓取（按 Pod 逐个抓）
                     Prometheus ──► KubeSentry（rate[1m] 查 QPS）
```

**为什么必须按 Pod 逐个抓**：最初用静态配置抓 Service 的 ClusterIP，每次抓取被
kube-proxy 随机路由到不同后端 Pod，counter 序列非单调，`rate()` 算出 150 QPS
（真实值 500）。改成 `kubernetes_sd_configs: role: pod` 按 Pod 抓取后才准确。
这是本次演示真实踩到的坑，已记入排查手册。

## 实录

```bash
kubectl apply -f deploy/demo/demo.yaml          # Prometheus + App(scaledemo) + loadgen(停流)
# operator 启动参数：--prometheus-url=http://prometheus.demo.svc:9090 --prometheus-window=1m
```

### 扩容：打流 500 QPS，目标每 Pod 100

```
T+0    loadgen --replicas=1（500 QPS）
T+90s  实测 QPS 406（rate[1m] 爬坡中）→ 事件: KubeSentryScaleUp "ScaleUp: 1 -> 3 (load above targets)"
T+106s 事件: KubeSentryScaleUp "ScaleUp: 3 -> 5 (load above targets)"
T+107s scaledemo 5/5 Ready，Stable
```

### 缩容：停流后渐进回收（冷却 30s + 迟滞 + 每次最多缩 20%）

```
T+0    loadgen --replicas=0
T+50s  5 -> 4   KubeSentryScaleDown "ScaleDown: 5 -> 4 (load below targets)"
T+100s 4 -> 3 → 3 -> 2（每 30s 一级，冷却窗口约束）
T+150s 2 -> 1，收敛到 minReplicas=1
```

事件流（`kubectl get events --field-selector involvedObject.name=scaledemo`）：

```
09:15:04  KubeSentryScaleUp    ScaleUp: 1 -> 3 (load above targets)
09:15:20  KubeSentryScaleUp    ScaleUp: 3 -> 5 (load above targets)
09:16:50  KubeSentryScaleDown  ScaleDown: 5 -> 4 (load below targets)
09:17:20  KubeSentryScaleDown  ScaleDown: 4 -> 3 (load below targets)
09:17:50  KubeSentryScaleDown  ScaleDown: 3 -> 2 (load below targets)
09:18:20  KubeSentryScaleDown  ScaleDown: 2 -> 1 (load below targets)
```

## 这个演示证明了什么

- 指标链路是真的：Prometheus 真实抓取 → `rate()` → 决策 → `kubectl get app` 可见
- 扩容激进（可连升两级）、缩容渐进（冷却 + 每步 ≤20%）—— 不对称设计符合预期
- 零流量不是"无数据"：QPS=0 时明确缩向 minReplicas，而不是弃权不动
