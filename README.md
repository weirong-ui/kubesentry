# KubeStasis — 云原生应用自愈与智能弹性伸缩 Operator

<p align="center">
  <img src="docs/assets/architecture.png" alt="KubeStasis 架构总览" width="880"/>
</p>

[![Go Version](https://img.shields.io/badge/Go-1.22%2B-00ADD8?logo=go)](https://go.dev)
[![Kubernetes](https://img.shields.io/badge/Kubernetes-1.28%2B-326CE5?logo=kubernetes)](https://kubernetes.io)
[![CI](https://github.com/weirong-ui/kubestasis/actions/workflows/ci.yml/badge.svg)](https://github.com/weirong-ui/kubestasis/actions/workflows/ci.yml)
[![Tests](https://img.shields.io/badge/tests-103%20cases-brightgreen)](#开发)
[![License](https://img.shields.io/badge/License-Apache--2.0-blue.svg)](LICENSE)

> 一个基于 controller-runtime 的 Kubernetes Operator：不只让应用"活着"，还能让它**自己变强、自己疗伤**。
> 项目配套的系列文章发布在掘金（[作者主页](https://juejin.cn/user/1835559210983642)）：
> 1. [HPA 只会伸缩不会自愈？我设计了一个 K8s 应用自愈 Operator：KubeStasis](https://juejin.cn/post/7702723478470852646)
> 2. [从零写一个 K8s Operator：kubebuilder 初始化到第一个 Reconcile](https://juejin.cn/post/7703105289100951561)

## 实现状态

> **P0–P4 全部闭环**：`apply CR → Deployment + Service → Available` 之外，弹性伸缩、分级自愈、金丝雀回滚、
> Admission Webhook、手写 mini-Informer 全部落地，前三条链路真集群验证过——
> 实录见 [docs/demo/01-scaling.md](docs/demo/01-scaling.md)（真 Prometheus + 打流）与 [docs/demo/02-self-healing.md](docs/demo/02-self-healing.md)（CrashLoop 自愈 / 金丝雀自动回滚）。
> CI e2e 覆盖 P0 + P2 + P3 三条链路（kind 里真跑 crash 镜像自动回滚断言）。
> 逐条能力清单见 [docs/STATUS.md](docs/STATUS.md)，最近更新：2026-11-03 晚。
> ⚠️ 项目原名 KubeSentry，2026-11-03 更名 KubeStasis；CRD group `apps.sentry.io` 保持不变。

| 能力 | 状态 | 说明 |
|---|---|---|
| App CRD + 代码生成 | ✅ 已实现 | controller-gen 生成 CRD / DeepCopy / RBAC |
| Reconcile 管理 Deployment | ✅ 已实现 | 幂等 CreateOrUpdate + OwnerReference |
| Service 自动创建 | ✅ 已实现 | ClusterIP，Endpoints 正常挂载后端 |
| Status 真实副本数 | ✅ 已实现 | 取自 `Deployment.status.readyReplicas`，与 spec 分离 |
| Status Conditions | ✅ 已实现 | `Available` / `Progressing` |
| Kubernetes Events 审计 | ✅ 已实现 | 已接入 `EventRecorder`，六类 reason |
| Finalizer 删除时序 | ✅ 已实现 | 删前导出审计是未排期的增强项 |
| 迟滞带 + 缩容渐进算法 | ✅ 已实现 | 纯函数，表驱动单测覆盖；步长/迟滞比例 CRD 可配 |
| Prometheus 多信号伸缩 | ✅ 已实现 | 真 Prometheus + 打流验证：500 QPS 扩 1→5，停流渐进缩 5→1（[实录](docs/demo/01-scaling.md)） |
| AI 推理弹性信号 | ✅ 已实现 | QueueDepth / TTFT / KVCacheUsage 三信号，默认 vLLM 指标名，WithQuery 可覆盖 |
| 冷却窗口 | ✅ 已实现 | 真集群验证：缩容被窗口拦截、过期后放行 |
| 故障识别（CrashLoop / OOM / 高频重启） | ✅ 已实现 | 读 Pod containerStatuses；重启阈值 CRD 可配；真集群验证（[实录](docs/demo/02-self-healing.md)） |
| 分级自愈（扩容→摘除→回滚） | ✅ 已实现 | L1 扩容（比例可配）/ L2 删坏 Pod / L3 按 ReplicaSet 修订回滚；冷却期内复发自动升级；**CI e2e 已覆盖** |
| 金丝雀观察 + 自动回滚 | ✅ 已实现 | 镜像变更开窗，错误率超阈值自动回滚并关窗；真集群 ~20s 完成回滚 |
| 冲突自愈写路径 | ✅ 已实现 | healer/scaler 全部 spec 写路径 RetryOnConflict + Merge Patch；conflict 静默退避不发 Warning |
| Admission Webhook | ✅ 已实现 | Validating（禁 latest、min≤max、target 按类型解析、金丝雀边界）+ Mutating（replicas 跟随 minReplicas）；自签证书方案不依赖 cert-manager；CI 负向用例覆盖 |
| 手写 mini-Informer | ✅ 已实现 | `pkg/miniinformer`：ListWatch → DeltaFIFO → Store → Handler + WorkQueue，11 用例 |
| 镜像签名 + 集群准入策略 | ✅ 已实现 | CI cosign keyless 签 GHCR digest 并自验；`policies/` 下 Kyverno 策略 |
| 单元测试 | ✅ 103 用例 | scaler 37 + controller 8 + healer 33 + webhook 14 + miniinformer 11；envtest 第 3 周 |
| Helm Chart | 🗓 规划中 | 第 3 周 |

## 为什么做这个项目？

Kubernetes 原生的 HPA 只解决"伸缩"，不解决"自愈"。下表是**目标态设计对比**，非当前实现（实现进度见上方状态表）：

| 能力 | HPA | KEDA | KubeStasis（目标） |
|---|---|---|---|
| 基于指标扩缩容 | ✅（CPU/内存） | ✅（多指标源） | 目标：QPS / P99 延迟 / 错误率 |
| 自定义伸缩策略（冷却窗口、震荡抑制） | ⚠️ 有限 | ⚠️ | 目标：内置迟滞算法 |
| 故障自愈（CrashLoop / OOM / 5xx 飙升） | ❌ | ❌ | 目标：扩容 / 降级 / 回滚分级处置 |
| 变更自动回滚（新版本发布后错误率上升） | ❌ | ❌ | 目标：金丝雀观察窗口 |
| Admission 校验与默认值注入 | — | — | ✅ Validating + Mutating Webhook（另附 Kyverno 集群级策略） |
| AI 推理弹性（队列/TTFT/KV cache） | ❌ | ❌ | ✅ 三种推理信号，vLLM 开箱即用 |

## 核心特性（目标能力，实现进度见上表）

- **声明式应用模型**：通过 `App` CRD 描述应用 + 伸缩策略 + 健康策略，一个 YAML 管到底
- **智能弹性伸缩**：基于 Prometheus 指标（QPS / P99 / 错误率）的多信号伸缩决策，带冷却窗口与迟滞抑制，避免副本数震荡；另含 AI 推理场景信号（队列深度 / TTFT / KV cache 占用率，默认 vLLM 指标名）
- **故障自愈引擎**：识别 CrashLoopBackOff、OOMKilled、5xx 错误率飙升等故障模式，执行「扩容 → 降级 → 回滚」分级自愈动作，全程 Kubernetes Events 审计留痕
- **安全发布**：新版本发布先金丝雀小流量观察，错误率超阈值自动回滚
- **Admission Webhook**：CR 合法性校验（禁 latest、跨字段约束、指标 target 解析）+ 跨字段默认值注入；`policies/` 下附 Kyverno 集群级镜像信任策略（呼应 CKS 知识点）
- **手写 mini-Informer**：`pkg/miniinformer` 是不依赖 informer 库、从零实现的 ListWatch + DeltaFIFO + Store + 工作队列，用于吃透 client-go Informer 机制（面试高频考点，[设计笔记](pkg/miniinformer/README.md)）

## 架构（设计目标）

```mermaid
flowchart LR
    User([开发者]) -->|kubectl apply App CR| API[Kubernetes API Server]
    API --> WH[Admission Webhook<br/>校验 + 默认值]
    subgraph KubeStasis
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
docker build -t kubestasis:dev .
#   --build-arg GOPROXY=https://goproxy.cn,direct \
#   --build-arg RUNTIME_IMAGE=m.daocloud.io/gcr.io/distroless/static-debian12:nonroot

# 2. 部署 CRD + RBAC + Manager
make deploy IMG=kubestasis:dev
# 或带 Admission Webhook 的形态（自签证书一条龙，不依赖 cert-manager）：
make deploy-webhook IMG=kubestasis:dev
# 没有 make 的机器（如 Windows）用等价命令：
#   kubectl kustomize config/default | sed 's|IMG_PLACEHOLDER|kubestasis:dev|g' | kubectl apply -f -

# 3. 创建一个受管应用
kubectl apply -f config/samples/app_v1alpha1_app.yaml

# 4.（可选）启用 P1 伸缩：解开 config/manager/manager.yaml 里 --prometheus-url 的注释
#    并指向集群内 Prometheus，重新 deploy；不配则伸缩休眠，其余功能不受影响

# 5. 观察 KubeStasis 的决策过程
kubectl get app sample -w
kubectl get deploy,svc -l app.kubernetes.io/managed-by=kubestasis
kubectl get events --field-selector reason=KubeStasisScaleUp
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
kubestasis/
├── api/v1alpha1/          # App CRD 类型定义 + DeepCopy
├── cmd/                   # 入口：manager（controller + webhook 同进程）/ demoapp（演示负载）
├── internal/
│   ├── controller/        # 核心调和循环（Deployment/Service/Status/Events）
│   ├── scaler/            # 伸缩决策引擎（通用 + AI 推理信号，决策与执行分离）
│   ├── healer/            # 自愈引擎（L1→L2→L3 分级 + 金丝雀回滚）
│   └── webhook/           # Admission Webhook（Validating + Mutating）
├── pkg/miniinformer/      # 手写 Informer（ListWatch/DeltaFIFO/Store/WorkQueue）
├── deploy/                # demoapp 镜像 Dockerfile + demo/ 演示清单（真 Prometheus）
├── policies/              # Kyverno 集群级镜像信任策略（可选安装）
├── config/
│   ├── crd/bases/         # controller-gen 生成的 CRD
│   ├── rbac/              # ClusterRole / Binding / ServiceAccount
│   ├── manager/           # Manager Deployment
│   ├── webhook/           # WebhookConfiguration + Service（controller-gen 生成）
│   ├── webhook-enabled/   # 带 Webhook 的 kustomize overlay（make deploy-webhook 用）
│   ├── default/           # kustomize 入口（make deploy 用）
│   └── samples/           # 示例 App CR
├── charts/kubestasis/ *   # Helm Chart（第 3 周）
├── hack/                  # boilerplate、webhook-certs.sh 自签证书脚本
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
make docker-build IMG=kubestasis:dev
```

> Windows 上没有 `make` 时，直接执行 Makefile 里对应目标的命令即可（`make manifests` 等价于 `go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.16.5 ...`）。

## Roadmap

> 勾选 = 代码已合并**且**可在干净集群复现。只有文档/骨架的阶段一律不勾。

- [x] **P0**：Go 模块 + 代码生成 + App CRD + Reconcile 管理 Deployment/Service + Status/Conditions + Events
- [x] **P1**：Prometheus 多信号伸缩 + 冷却/迟滞端到端验证（真 Prometheus + 打流，[实录](docs/demo/01-scaling.md)）
- [x] **P2**：故障自愈引擎 + 金丝雀自动回滚（真集群验证，[实录](docs/demo/02-self-healing.md)；CI e2e 已纳入回归）
- [x] **P3**：Admission Webhook（校验 + 跨字段默认值注入；自签证书方案 + CI 负向用例）+ cosign 签名 + Kyverno 策略
- [x] **P4**：手写 mini-Informer（ListWatch/DeltaFIFO/Store/WorkQueue，[设计笔记](pkg/miniinformer/README.md)）

> P5（eBPF 无侵入采集）/ P6（多集群调度）已移出承诺范围，降为远景方向。
> 第 3 周剩余项：压测对比表（vs 原生 HPA）、envtest、Helm Chart。

逐条进度与证据见 [docs/STATUS.md](docs/STATUS.md)，里程碑细节见 [docs/ROADMAP.md](docs/ROADMAP.md)。

## License

Apache 2.0
