# KubeSentry — 云原生应用自愈与智能弹性伸缩 Operator

<p align="center">
  <img src="docs/assets/architecture.png" alt="KubeSentry 架构总览" width="880"/>
</p>

[![Go Version](https://img.shields.io/badge/Go-1.22%2B-00ADD8?logo=go)](https://go.dev)
[![Kubernetes](https://img.shields.io/badge/Kubernetes-1.28%2B-326CE5?logo=kubernetes)](https://kubernetes.io)
[![CI](https://github.com/weirong-ui/kubesentry/actions/workflows/ci.yml/badge.svg)](https://github.com/weirong-ui/kubesentry/actions/workflows/ci.yml)
[![Tests](https://img.shields.io/badge/tests-57%20cases-brightgreen)](#开发)
[![License](https://img.shields.io/badge/License-Apache--2.0-blue.svg)](LICENSE)

> 一个基于 controller-runtime 的 Kubernetes Operator：不只让应用"活着"，还能让它**自己变强、自己疗伤**。
> 项目配套的系列文章发布在掘金（[作者主页](https://juejin.cn/user/1835559210983642)）：
> 1. [HPA 只会伸缩不会自愈？我设计了一个 K8s 应用自愈 Operator：KubeSentry](https://juejin.cn/post/7702723478470852646)
> 2. [从零写一个 K8s Operator：kubebuilder 初始化到第一个 Reconcile](https://juejin.cn/post/7703105289100951561)

## 实现状态

> **P0/P1/P2 已闭环**：除了 `apply CR → Deployment + Service → Available`，弹性伸缩与分级自愈都在真集群验证过——
> 实录见 [docs/demo/01-scaling.md](docs/demo/01-scaling.md)（真 Prometheus + 打流）与 [docs/demo/02-self-healing.md](docs/demo/02-self-healing.md)（CrashLoop 自愈 / 金丝雀自动回滚）。
> 逐条能力清单见 [docs/STATUS.md](docs/STATUS.md)，最近更新：2026-11-03。
> 下方"核心特性"中带 🗓 的部分仍是目标能力，不代表已可用。

| 能力 | 状态 | 说明 |
|---|---|---|
| App CRD + 代码生成 | ✅ 已实现 | controller-gen 生成 CRD / DeepCopy / RBAC |
| Reconcile 管理 Deployment | ✅ 已实现 | 幂等 CreateOrUpdate + OwnerReference |
| Service 自动创建 | ✅ 已实现 | ClusterIP，Endpoints 正常挂载后端 |
| Status 真实副本数 | ✅ 已实现 | 取自 `Deployment.status.readyReplicas`，与 spec 分离 |
| Status Conditions | ✅ 已实现 | `Available` / `Progressing` |
| Kubernetes Events 审计 | ✅ 已实现 | 已接入 `EventRecorder`，六类 reason |
| Finalizer 删除时序 | ✅ 已实现 | 删前导出审计待 P3 |
| 迟滞带 + 缩容渐进算法 | ✅ 已实现 | 纯函数，表驱动单测覆盖 |
| Prometheus 多信号伸缩 | ✅ 已实现 | 真 Prometheus + 打流验证：500 QPS 扩 1→5，停流渐进缩 5→1（[实录](docs/demo/01-scaling.md)） |
| 冷却窗口 | ✅ 已实现 | 真集群验证：缩容被窗口拦截、过期后放行，每步最多缩 20% |
| 故障识别（CrashLoop / OOM / 高频重启） | ✅ 已实现 | 读 Pod containerStatuses；真集群验证（[实录](docs/demo/02-self-healing.md)） |
| 分级自愈（扩容→摘除→回滚） | ✅ 已实现 | L1 扩 20% / L2 删坏 Pod / L3 按 ReplicaSet 修订回滚；冷却期内复发自动升级 |
| 金丝雀观察 + 自动回滚 | ✅ 已实现 | 镜像变更开窗，错误率超阈值自动回滚并关窗；真集群 ~20s 完成回滚 |
| Admission Webhook | 🗓 规划中 | P3 |
| 手写 mini-Informer | 🗓 规划中 | P4 |
| 单元测试 | ✅ 57 用例 | 伸缩决策/自愈分级/升级路径/回滚选版/金丝雀边界/status 真实性/Finalizer；envtest 第 3 周 |
| Helm Chart | 🗓 规划中 | 第 3 周 |

## 为什么做这个项目？

Kubernetes 原生的 HPA 只解决"伸缩"，不解决"自愈"。下表是**目标态设计对比**，非当前实现（实现进度见上方状态表）：

| 能力 | HPA | KEDA | KubeSentry（目标） |
|---|---|---|---|
| 基于指标扩缩容 | ✅（CPU/内存） | ✅（多指标源） | 目标：QPS / P99 延迟 / 错误率 |
| 自定义伸缩策略（冷却窗口、震荡抑制） | ⚠️ 有限 | ⚠️ | 目标：内置迟滞算法 |
| 故障自愈（CrashLoop / OOM / 5xx 飙升） | ❌ | ❌ | 目标：扩容 / 降级 / 回滚分级处置 |
| 变更自动回滚（新版本发布后错误率上升） | ❌ | ❌ | 目标：金丝雀观察窗口 |
| Admission 校验与默认值注入 | — | — | 目标：Validating + Defaulting Webhook |

## 核心特性（目标能力，实现进度见上表）

- **声明式应用模型**：通过 `App` CRD 描述应用 + 伸缩策略 + 健康策略，一个 YAML 管到底
- **智能弹性伸缩**：基于 Prometheus 指标（QPS / P99 / 错误率）的多信号伸缩决策，带冷却窗口与迟滞抑制，避免副本数震荡
- **故障自愈引擎**：识别 CrashLoopBackOff、OOMKilled、5xx 错误率飙升等故障模式，执行「扩容 → 降级 → 回滚」分级自愈动作，全程 Kubernetes Events 审计留痕
- **安全发布**：新版本发布先金丝雀小流量观察，错误率超阈值自动回滚
- **Admission Webhook**：CR 合法性校验 + 生产安全默认值注入（资源限制、安全上下文，呼应 CKS 知识点）
- **手写 mini-Informer**：`pkg/miniinformer` 是一个不依赖 informer 库、从零实现的 ListWatch + DeltaFIFO + 工作队列，用于吃透 client-go Informer 机制（面试高频考点）

## 架构（设计目标）

```mermaid
flowchart LR
    User([开发者]) -->|kubectl apply App CR| API[Kubernetes API Server]
    API --> WH[Admission Webhook<br/>校验 + 默认值]
    subgraph KubeSentry
        C[App Controller<br/>Reconcile Loop]
        S[Scaler<br/>多信号伸缩决策]
        H[Healer<br/>故障自愈引擎]
        MI[mini-Informer<br/>学习模块]
    end
    API --> C
    C -->|创建/更新| D[Deployment & Service]
    P[Prometheus] -->|QPS/P99/错误率| S
    S --> H
    H -->|扩容/回滚/降级| D
    H -->|Events 审计| API
```

## 快速开始

需要 Kubernetes 1.28+ 与本地可用的镜像仓库（kind / minikube / Docker Desktop 均可）。

```bash
# 1. 构建镜像（国内网络追加注释里的 build-arg）
docker build -t kubesentry:dev .
#   --build-arg GOPROXY=https://goproxy.cn,direct \
#   --build-arg RUNTIME_IMAGE=m.daocloud.io/gcr.io/distroless/static-debian12:nonroot

# 2. 部署 CRD + RBAC + Manager
make deploy IMG=kubesentry:dev
# 没有 make 的机器（如 Windows）用等价命令：
#   kubectl kustomize config/default | sed 's|IMG_PLACEHOLDER|kubesentry:dev|g' | kubectl apply -f -

# 3. 创建一个受管应用
kubectl apply -f config/samples/app_v1alpha1_app.yaml

# 4.（可选）启用 P1 伸缩：解开 config/manager/manager.yaml 里 --prometheus-url 的注释
#    并指向集群内 Prometheus，重新 deploy；不配则伸缩休眠，其余功能不受影响

# 5. 观察 KubeSentry 的决策过程
kubectl get app sample -w
kubectl get deploy,svc -l app.kubernetes.io/managed-by=kubesentry
kubectl get events --field-selector reason=KubeSentryScaleUp
```

预期输出：

```
NAME     PHASE    READY   DESIRED   IMAGE
sample   Stable   2       2         nginx:1.27-alpine

NAME                     READY   UP-TO-DATE   AVAILABLE   AGE
deployment.apps/sample   2/2     2            2           40s

NAME             TYPE        CLUSTER-IP     EXTERNAL-IP   PORT(S)   AGE
service/sample   ClusterIP   10.96.237.92   <none>        80/TCP    40s
```

一个最小的 `App` CR：

```yaml
apiVersion: apps.sentry.io/v1alpha1
kind: App
metadata:
  name: order-service
spec:
  image: registry.example.com/order-service:v1.2.0
  replicas: 3
  port: 8080
  scaling:                        # 可选，省略即不做伸缩决策
    minReplicas: 2
    maxReplicas: 20
    cooldownSeconds: 120          # 冷却窗口，防震荡
    metrics:
      - type: QPS
        target: "1000"            # 每 Pod 目标 QPS（字符串，与 CRD 定义一致）
      - type: P99Latency
        target: "200ms"
  health:                         # 可选
    strategy: graduated           # graduated: 扩容→降级→回滚 分级自愈
    canary:
      enabled: true
      observationSeconds: 300     # 金丝雀观察窗口
      errorRateThreshold: "0.01"  # 错误率超 1% 自动回滚
```

## 项目结构

标 `*` 的目录尚未创建，随对应里程碑落地。

```
kubesentry/
├── api/v1alpha1/          # App CRD 类型定义 + DeepCopy
├── cmd/                   # 入口：manager（webhook 待 P3 并入）/ demoapp（演示负载）
├── internal/
│   ├── controller/        # 核心调和循环（Deployment/Service/Status/Events）
│   ├── scaler/            # 伸缩决策引擎（决策与执行分离，MetricsSource 由 P1 注入）
│   ├── healer/            # 自愈引擎（骨架）
│   └── webhook/ *         # Admission Webhook（P3）
├── pkg/miniinformer/ *    # 手写 Informer（P4）
├── deploy/                # demoapp 镜像 Dockerfile + demo/ 演示清单（真 Prometheus）
├── config/
│   ├── crd/bases/         # controller-gen 生成的 CRD
│   ├── rbac/              # ClusterRole / Binding / ServiceAccount
│   ├── manager/           # Manager Deployment
│   ├── default/           # kustomize 入口（make deploy 用）
│   └── samples/           # 示例 App CR
├── charts/kubesentry/ *   # Helm Chart（第 3 周）
├── hack/                  # boilerplate 等代码生成辅助
└── docs/                  # 设计文档、状态账本、系列文章素材
```

## 开发

```bash
# 本地运行（连当前 kubeconfig 集群）
make run

# 生成 CRD / RBAC / DeepCopy
make manifests generate

# 格式化与静态检查
gofmt -l ./api ./cmd ./internal && go vet ./...

# 单元测试
go test ./... -cover

# 构建镜像（多阶段 + distroless）
make docker-build IMG=kubesentry:dev
```

> Windows 上没有 `make` 时，直接执行 Makefile 里对应目标的命令即可（`make manifests` 等价于 `go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.16.5 ...`）。

## Roadmap

> 勾选 = 代码已合并**且**可在干净集群复现。只有文档/骨架的阶段一律不勾。

- [x] **P0**：Go 模块 + 代码生成 + App CRD + Reconcile 管理 Deployment/Service + Status/Conditions + Events
- [x] **P1**：Prometheus 多信号伸缩 + 冷却/迟滞端到端验证（真 Prometheus + 打流，[实录](docs/demo/01-scaling.md)）
- [x] **P2**：故障自愈引擎 + 金丝雀自动回滚（真集群验证，[实录](docs/demo/02-self-healing.md)）
- [ ] **P3**：Admission Webhook（校验 + 安全默认值注入）
- [ ] **P4**：手写 mini-Informer + 原理系列文章

> P5（eBPF 无侵入采集）/ P6（多集群调度）已移出承诺范围，降为远景方向——
> 对个人项目而言先把 P0–P4 做扎实比铺开摊子更有说服力。

逐条进度与证据见 [docs/STATUS.md](docs/STATUS.md)，里程碑细节见 [docs/ROADMAP.md](docs/ROADMAP.md)。

## License

Apache 2.0
