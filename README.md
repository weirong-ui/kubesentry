# KubeSentry — 云原生应用自愈与智能弹性伸缩 Operator

<p align="center">
  <img src="docs/assets/architecture.png" alt="KubeSentry 架构总览" width="880"/>
</p>

[![Go Version](https://img.shields.io/badge/Go-1.22%2B-00ADD8?logo=go)](https://go.dev)
[![Kubernetes](https://img.shields.io/badge/Kubernetes-1.28%2B-326CE5?logo=kubernetes)](https://kubernetes.io)
[![CI](https://github.com/weirong-ui/kubesentry/actions/workflows/ci.yml/badge.svg)](.github/workflows/ci.yml)
[![License](https://img.shields.io/badge/License-Apache--2.0-blue.svg)](LICENSE)

> 一个基于 controller-runtime 的 Kubernetes Operator：不只让应用"活着"，还能让它**自己变强、自己疗伤**。
> 项目配套的系列文章发布在掘金：[《手写一个 K8s Operator：KubeSentry 设计与实现》](https://juejin.cn)

## 实现状态

> **当前仓库处于 P0 开发期：设计文档与代码骨架完整，尚未产出可运行版本。**
> 逐条能力清单与缺口见 [docs/STATUS.md](docs/STATUS.md)，最近更新：2026-11-02。
> 下方所有"核心特性"均为**目标能力**，不代表已可用；请以本表为准。

| 能力 | 状态 | 说明 |
|---|---|---|
| App CRD 类型定义 | ✅ 已实现 | 多信号 / 冷却 / 金丝雀字段完整 |
| Reconcile 管理 Deployment | 🚧 开发中 | 逻辑已写，缺 Go 模块与代码生成，暂不能编译 |
| Finalizer 删除时序 | ✅ 已实现 | 添加/移除正确，删前审计待补 |
| 迟滞带 + 缩容渐进算法 | ✅ 已实现 | 纯函数，单测待补（P1） |
| Status Conditions | 🚧 开发中 | 字段已定义，尚未写入 |
| Kubernetes Events 审计 | 🚧 开发中 | `emitEvent` 待接 `EventRecorder` |
| Service 自动创建 | 🗓 规划中 | P0 |
| Prometheus 多信号伸缩 | 🗓 规划中 | P1；当前 `Decide()` 信号恒等于当前副本数，不会触发伸缩 |
| 故障识别（CrashLoop / OOM / 5xx） | 🗓 规划中 | P2 |
| 分级自愈（扩容→降级→回滚） | 🗓 规划中 | P2 |
| 金丝雀观察 + 自动回滚 | 🗓 规划中 | P2 |
| Admission Webhook | 🗓 规划中 | P3 |
| 手写 mini-Informer | 🗓 规划中 | P4 |
| 单元 / envtest 测试 | 🗓 规划中 | 当前 0 个用例 |
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

> ⚠️ **暂不可用**：以下步骤依赖 `config/crd/bases` 与 `config/default`，二者随 P0 脚手架一并落地。
> 在那之前请勿按此操作，克隆后 `go build ./...` 会失败。进度见 [docs/STATUS.md](docs/STATUS.md)。

```bash
# 1. 安装 CRD
kubectl apply -f config/crd/bases

# 2. 部署 Operator（含 RBAC 与 Webhook）
make deploy IMG=<your-registry>/kubesentry:latest

# 3. 创建一个受管应用
kubectl apply -f config/samples/app_v1alpha1_app.yaml

# 4. 观察 KubeSentry 的决策过程
kubectl get app myapp -w
kubectl get events --field-selector reason=KubeSentryScaleUp
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
  scaling:
    minReplicas: 2
    maxReplicas: 20
    cooldownSeconds: 120          # 冷却窗口，防震荡
    metrics:
      - type: QPS
        target: "1000"            # 每 Pod 目标 QPS（字符串，与 CRD 定义一致）
      - type: P99Latency
        target: "200ms"
  health:
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
├── api/v1alpha1/          # App CRD 类型定义
├── cmd/                   # 入口：manager（webhook 待 P3 并入）
├── internal/
│   ├── controller/        # 核心调和循环
│   ├── scaler/            # 伸缩决策引擎（决策与执行分离）
│   ├── healer/            # 自愈引擎（骨架）
│   └── webhook/ *         # Admission Webhook（P3）
├── pkg/miniinformer/ *    # 手写 Informer（P4）
├── config/
│   ├── samples/           # 示例 App CR
│   ├── crd/ *             # controller-gen 生成（P0）
│   ├── rbac/ *            # （P0）
│   └── default/ *         # kustomize 入口（P0）
├── charts/kubesentry/ *   # Helm Chart（第 3 周）
└── docs/                  # 设计文档、状态账本、系列文章素材
```

## 开发

> 当前 `make test` / `make run` 均不可用（缺 `go.mod`），P0 完成后恢复。

```bash
# 本地运行（连当前 kubeconfig 集群）
make run

# 单元测试（envtest 起 etcd + apiserver）
make test

# 生成 CRD / DeepCopy 代码
make manifests generate

# 构建镜像（多阶段 + distroless）
make docker-build IMG=kubesentry:dev
```

## Roadmap

> 勾选 = 代码已合并**且**可在 kind 集群复现。只有文档/骨架的阶段一律不勾。

- [ ] **P0**：Go 模块 + 代码生成 + App CRD + Reconcile 管理 Deployment/Service + Events
- [ ] **P1**：Prometheus 多信号伸缩 + 冷却/迟滞算法落地（算法本身已实现，待接指标源）
- [ ] **P2**：故障自愈引擎 + 金丝雀自动回滚
- [ ] **P3**：Admission Webhook（校验 + 安全默认值注入）
- [ ] **P4**：手写 mini-Informer + 原理系列文章
- [ ] **P5**：基于 eBPF 的无侵入指标采集
- [ ] **P6**：多集群容灾调度

已完成的基础件：App CRD 类型定义、Finalizer 时序、迟滞/缩容渐进算法、文档与文章素材 —— 详见 [docs/STATUS.md](docs/STATUS.md)。

里程碑细节见 [docs/ROADMAP.md](docs/ROADMAP.md)。

## License

Apache 2.0
