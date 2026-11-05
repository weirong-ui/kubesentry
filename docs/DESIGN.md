# KubeStasis 设计文档

> 版本：v1.2 ｜ 状态：已实现（P0–P4 + 探针/稳定窗口/Helm/决策指标）｜ 作者：weirong
>
> **同步说明**：v1.0 写于编码之前，P0–P4 落地后有多处与代码不一致。v1.1 按 `api/v1alpha1/app_types.go`
> 与 `internal/` 的实际实现逐节校对过（2026-11-04）；v1.2 同步了 11-04 晚新增的四件事
> （就绪/存活探针、缩容稳定窗口、Helm Chart、伸缩决策指标）。本文只描述**已经写进代码的行为**，
> 未实现的部分单独标注「计划中」——以 [STATUS.md](STATUS.md) 为能力账本，两者冲突时以 STATUS 为准。

## 1. 背景与动机

Kubernetes 提供的弹性能力（HPA/VPA）本质是**单信号、无记忆**的：HPA 根据 CPU/内存比值机械地扩缩容，既不理解业务指标（QPS、P99、错误率），也不对故障做任何主动响应。KEDA 补齐了多指标源，但依然只有"伸缩"这一只手。

生产环境真正的痛点是两个：

1. **震荡**：流量尖峰 → 疯狂扩容 → 流量回落 → 缩容 → 再来一次。没有冷却窗口和迟滞机制的伸缩是慢性自杀。
2. **故障不自愈**：新版本发布后 5xx 飙升，值班同学半夜手动 `kubectl rollout undo`。Pod 层面的自愈（kubelet 重启容器）解决不了"版本坏"这种 Pod 重启一百次也没用的问题。

KubeStasis 的目标：**用 Operator 模式把"伸缩决策"和"故障自愈"声明式地交给集群自己完成**。

## 2. 目标与非目标

**目标**
- G1：一个 `App` CRD 描述应用全生命周期：镜像、副本、伸缩策略、健康策略
- G2：多信号伸缩决策引擎，带冷却窗口与迟滞抑制
- G3：分级自愈：L1 扩容 → L2 摘除重建 → L3 回滚，动作全部以 Kubernetes Event 留痕
- G4：金丝雀**观察窗口**，错误率超阈值自动回滚（不做按权重引流——那不是流量治理，灰度粒度粗，见 §2 非目标）
- G5：Admission Webhook 做校验与跨字段默认（`replicas` 跟随 `minReplicas`）；安全默认值注入见 §6，**计划中**
- G6：GitHub Actions 全自动 CI（gofmt / vet / build / 单测 + envtest + kind e2e + 镜像签名）；envtest 见 §5，已落地

**非目标**
- 不替代 HPA/KEDA（可以共存，KubeStasis 管自愈，HPA 管容量）
- 不做通用 PaaS（不管理数据库中间件等有状态服务的运维语义）
- P0~P4 不引入 eBPF（P5 再考虑无侵入采集）

## 3. API 设计

### 3.1 CRD：`App`（Group: `kubestasis.io`，Version: `v1alpha1`）

```go
type AppSpec struct {
    Image     string            `json:"image"`
    Replicas  *int32            `json:"replicas,omitempty"`  // 故意不给 CRD 默认值，见下方说明
    Port      int32             `json:"port"`                // 默认 8080
    Scaling   *ScalingPolicy    `json:"scaling,omitempty"`   // 指针，见下方说明
    Health    *HealthPolicy     `json:"health,omitempty"`
    Resources *corev1.ResourceRequirements `json:"resources,omitempty"` // 原样透传
    Env       []corev1.EnvVar   `json:"env,omitempty"`       // listType=map，按 name 合并
    Readiness *ProbeConfig      `json:"readiness,omitempty"` // 省略时注入默认 TCP 探针
    Liveness  *ProbeConfig      `json:"liveness,omitempty"`  // 只在显式声明时注入
}

// ProbeConfig: Type HTTPGet|TCPSocket；HTTPGet 需 Path（必须以 "/" 开头，webhook 校验）；
// period/timeout/initialDelay/failureThreshold 可配，0 取各自默认值

type ScalingPolicy struct {
    MinReplicas             int32          `json:"minReplicas"`
    MaxReplicas             int32          `json:"maxReplicas"`
    CooldownSeconds         int            `json:"cooldownSeconds"`
    Metrics                 []MetricTarget `json:"metrics,omitempty"`
    ScaleDownStepPercent    int            `json:"scaleDownStepPercent,omitempty"`    // 默认 20
    HysteresisPercent       int            `json:"hysteresisPercent,omitempty"`       // 默认 10
    ErrorRateScaleUpPercent int            `json:"errorRateScaleUpPercent,omitempty"` // 默认 20
    ScaleDownStabilizationSeconds int      `json:"scaleDownStabilizationSeconds,omitempty"` // 默认 60，0 关闭
}

type MetricType string // QPS | P99Latency | ErrorRate       —— 通用业务信号
                      // QueueDepth | TTFT | KVCacheUsage   —— AI 推理信号（vLLM 风格）

type MetricTarget struct {
    Type   MetricType `json:"type"`
    Target string     `json:"target"` // "1000" / "200ms" / "0.01" / "5" / "1s" / "0.85"
}

type HealthPolicy struct {
    Strategy         HealthStrategy `json:"strategy"`                   // graduated | observability-only
    ScaleUpPercent   int            `json:"scaleUpPercent,omitempty"`   // L1 扩容比例，默认 20
    RestartThreshold int32          `json:"restartThreshold,omitempty"` // 兜底重启判定，默认 5
    Canary           CanaryPolicy   `json:"canary,omitempty"`
}

type CanaryPolicy struct {
    Enabled            bool   `json:"enabled"`
    ObservationSeconds int    `json:"observationSeconds"`  // 默认 300，webhook 拒绝 <30
    ErrorRateThreshold string `json:"errorRateThreshold"`  // 默认 "0.01"
}

type AppStatus struct {
    Phase             AppPhase           `json:"phase"` // Provisioning | Stable | Healing | RollingBack
    ObservedGeneration int64             `json:"observedGeneration,omitempty"`
    CurrentReplicas   int32              `json:"currentReplicas,omitempty"` // 就绪副本，取自 Deployment
    DesiredReplicas   int32              `json:"desiredReplicas,omitempty"`
    LastScalingAt     *metav1.Time       `json:"lastScalingAt,omitempty"`
    LastHealAction    string             `json:"lastHealAction,omitempty"`
    LastHealLevel     int                `json:"lastHealLevel,omitempty"` // 0=L1 1=L2 2=L3
    LastHealAt        *metav1.Time       `json:"lastHealAt,omitempty"`
    CurrentImage      string             `json:"currentImage,omitempty"`
    LastImageChangeAt *metav1.Time       `json:"lastImageChangeAt,omitempty"`
    BelowTargetSince  *metav1.Time       `json:"belowTargetSince,omitempty"` // 缩容稳定窗口的计时起点
    RollbackRevision  int64              `json:"rollbackRevision,omitempty"`
    Conditions        []metav1.Condition `json:"conditions,omitempty"`
}
```

两个类型设计上的坑，都是真集群踩出来的：

- **`Scaling`/`Health` 必须是指针**：`omitempty` 对结构体不生效，值类型会把省略的字段序列化成
  零值对象（`minReplicas: 0`），直接被 CRD 校验拒掉。指针让「未设置」和「设为零值」在序列化层面可区分。
- **`Replicas` 不能给 CRD 默认值**：CRD 默认值在解码阶段（Mutating Webhook 之前）就落进对象，
  一旦 `default=1`，webhook 的「replicas 跟随 minReplicas」跨字段默认将永远失效。
  省略时的兜底：有 webhook → Defaulter 注入；无 webhook → 控制器按 1 处理。

要点：
- **Status 只记录决策结果与依据**，不记录过程日志（日志交给 Events 和 Prometheus）
- 所有时间戳字段用于实现冷却窗口的幂等判断，Reconcile 重入安全

## 4. 核心流程

### 4.1 Reconcile 主循环

```
Reconcile(App):
  1. 拉取 App；删除中则摘 finalizer 放行（审计记录导出是增强方向，未实现）
  2. Webhook 已保证 spec 合法，此处只兜底校验
  3. Ensure Deployment/Service 与 spec 一致（CreateOrUpdate）
  4. 若处于金丝雀观察窗口：查询错误率 → 超阈值则回滚，结束
  5. HealthCheck：采集 Pod 状态 + 应用指标
     - 无异常 → 交给 Scaler 判断是否伸缩（受冷却窗口约束）
     - 有异常 → 交给 Healer 执行分级自愈
  6. 更新 Status（Phase、CurrentReplicas、Conditions）；若本轮有伸缩决策，
     先 patch status.lastScalingAt，再 patch spec.replicas（顺序见 4.2）
```

### 4.2 伸缩决策（scaler 包）

多信号取**最紧急者**：

```
desiredBy(signal) = ceil(currentReplicas × current / target)
desired = max(各信号 desired, minReplicas)，且 ≤ maxReplicas
```

防震荡四件套：
1. **冷却窗口**：`now - lastScalingAt < cooldownSeconds` 时拒绝任何缩容（扩容允许紧急放行）
2. **迟滞带（只挡缩容）**：缩容方向上 `current - desired ≤ max(1, current × hysteresisPercent%)` 时不动作；
   扩容方向直通，不看迟滞带。**对称迟滞是个坑**：突发时 `desired` 只比当前多 1 个副本，被迟滞带吃掉，
   集群在饱和状态下原地不动（压测 run1 实测：期望 5 实际 4，差 1 ≤ 带 1）。扩容是应急动作，不该被平滑。
3. **缩容渐进**：缩容每次最多 -scaleDownStepPercent%（默认 20%），扩容可一步到位
4. **缩容稳定窗口**（11-04 补，压测 run5 的教训）：信号需**持续**低于目标满
   `scaleDownStabilizationSeconds`（默认 60s）才允许缩容，`status.belowTargetSince` 记计时起点，
   任一高于目标的采样立即重置。冷却窗口防的是"刚扩完就缩"（时间上贴近上次动作），
   稳定窗口防的是"信号还没站稳就缩"（时间上贴近信号本身）——两个窗口挡的是不同的病，
   HPA 的 300s stabilization 只有后者的语义

写路径的时序约束（压测 run4 抓到的竞态）：**先写 status 的 `lastScalingAt`，再写 spec 的 `replicas`**。
Apply 写 spec 会立刻触发下一轮 Reconcile，而 informer cache 里可能还没有刚写的 status，冷却窗口被绕过，
表现为 3→2→1 在一个采样窗口内连跳两步。watch 事件按 resourceVersion 有序送达，先写的 status 必然先到 cache。

**决策可观测**（`internal/metrics`）：每轮决策（含 `action=None`）都打点
`kubestasis_scaling_decisions_total{namespace,app,action,reason}`，另有 desired/current/signal_replicas
三个 gauge。两个设计约束：reason 必须是**固定枚举** ReasonCode（自由文本会带秒数/副本数，
每轮一个新 label，时序基数几小时打爆 Prometheus）；App 删除时 `Forget` 清空该 App 的全部时序
（CounterVec/GaugeVec 不会自回收 label 组合）。"这次为什么没扩"从翻日志变成一条 PromQL。

### 4.3 自愈引擎（healer 包）

故障识别 → 分级动作（`Inspect` 只读，不做任何写操作）：

| 故障信号 | 识别方式 | 分级动作 |
|---|---|---|
| CrashLoopBackOff | `containerStatuses[].state.waiting.reason` | **L2 摘除重建**——实例本身起不来，扩容只会多几个起不来的实例；冷却期内复发升级 L3 回滚 |
| OOMKilled | `lastState.terminated.reason`（当前 terminated 也算） | **L1 扩容**——先分摊内存压力 |
| 重启次数 ≥ `health.restartThreshold`（默认 5） | `restartCount` | **L1**，兜底信号（覆盖其他 waiting reason） |
| 镜像拉不下来 / 配置错 | 不在 `Inspect` 里判 | 交给 CrashLoop 路径：起不来最终都会落到 CrashLoopBackOff |
| 5xx 错误率飙升 | Prometheus，**金丝雀窗口内**由控制器判断 | 不走分级，直接 **L3 回滚**（版本坏，扩容和摘除都救不了） |
| P99 突增 | — | 只作为**伸缩信号**，不参与自愈判定 |

三条边界要写清楚，它们都是"看起来该有、实际没有"的：
- **L1 不作用于 CrashLoop**：见上表第一行，这是刻意的方向选择
- **错误率自愈只在金丝雀窗口内生效**：窗口内 `InCanaryWindow` + `CheckCanary` 命中即回滚；**窗口外根本不查错误率**，
  它只作为伸缩信号（scaler 的 `ErrorRate`）——避免把业务侧故障误判成版本故障
- **没有 L2→L1 的回退路径**：升级是单向的，降级靠冷却窗口到期后重新从 L1 开始

分级原则：**每次 Reconcile 只执行一级动作，观测一个观察期（默认 3 分钟）后再升级**，避免"扩容+回滚"同时发生导致事故现场被破坏。L3 选版时用 `Report.BadImages` 排除坏镜像，防止回滚后又滚回坏版本（乒乓回滚）。所有动作 Emit Event（reason 统一前缀 `KubeStasis*`）并写回 Status。

### 4.4 Admission Webhook

- **Mutating**（`mapp.kubestasis.io`）：目前只有一条跨字段默认——`replicas` 未显式给出时跟随
  `scaling.minReplicas`，而不是恒为 1。否则用户写 `minReplicas: 3` 期望"起步就 3 个"，
  第一轮却被当 1 个创建、再被伸缩器扩上去，多一次冷启动抖动。
- **Validating**（`vapp.kubestasis.io`）：禁 `latest`/无 tag 镜像、`minReplicas ≤ maxReplicas`、
  `replicas` 落在区间内、指标类型不重复且 target 按类型可解析、金丝雀阈值 ∈ (0,1) 且观察窗口 ≥ 30s、
  `resources.requests ≤ limits`、`env` 不得声明保留变量（`APP_NAME`/`POD_NAMESPACE`，由控制器注入）
  且不得重名、探针 `HTTPGet.path` 必须以 `/` 开头（TCPSocket 配 path 只警告不阻断——静默忽略的
  笔误不配拦）。**聚合报错**一次列全，不让用户改一条撞下一条。
- **不注入安全上下文**（计划中）：`securityContext`（`readOnlyRootFilesystem`/`runAsNonRoot`/drop ALL）
  与 `resources` 的默认值**没有**实现，`resources` 是原样透传、不偷加默认值。写在这里是为了不让人
  误以为已经有一层兜底——目前 Pod 的安全配置由用户自己负责。

## 5. 测试策略

| 层级 | 工具 | 覆盖内容 | 现状 |
|---|---|---|---|
| 单元 | go test + fake client | Scaler 决策表、Healer 状态机、Webhook 校验规则、mini-Informer、指标埋点 | ✅ 139 用例 |
| 集成 | envtest（真实 apiserver + etcd） | Reconcile 全流程、Finalizer、Status 更新、副本漂移拉回 | ✅ 4 条链路（CI `envtest` job） |
| 混沌 | kind 集群脚本（CI） | 杀 Pod / CrashLoop 版本发布 / 金丝雀错误率，验证自愈与回滚 | ✅ 覆盖 P0/P2/P3 |

## 6. 安全设计（呼应 CKS）

- Operator 自身以 distroless 镜像运行，非 root，只读根文件系统
- RBAC 最小权限：只列点到点的资源——App CRD + `pods`/`pods/status`/`services`/`events`/
  `deployments`/`replicasets`，无通配符（`*`）、无 `secrets`、无 `nodes`
- Webhook TLS 证书两条路：kustomize 形态用 `hack/webhook-certs.sh` 自签（openssl 生成 CA + 带 SAN 的
  服务端证书，写进 Secret 并把 CA 注入 `caBundle`）；Helm 形态默认用渲染期 `genSelfSignedCert`（升级
  自动重签 + checksum 注解触发滚动），也可切 cert-manager。**不强制依赖 cert-manager**：目标集群
  不一定装了它，多一个依赖就多一个装不上的理由。代价是自签证书轮换要重跑脚本或重新 upgrade，
  没有 cert-manager 的自动续期
- 部署双轨：`config/`（kustomize，开发与 CI）与 `charts/kubestasis`（Helm，交付形态）装的是同一个控制器
- Defaulting Webhook 为受管应用注入安全上下文（集群级"兜底防线"）——**计划中，未实现**；当前不注入任何
  `securityContext`，Operator 自身的安全配置（distroless、非 root、只读根文件系统）在 Deployment 里是硬编码的
- 镜像供应链：CI 用 cosign keyless 签名并自验，示例策略见 `policies/kyverno-kubestasis.yaml`（验签后才准入）

## 7. 技术选型

| 组件 | 选型 | 理由 |
|---|---|---|
| 脚手架 | kubebuilder v4 | 社区标准，CRD/Webhook/配置一键生成 |
| Controller | controller-runtime | 事实标准，对接 Informer/WorkQueue |
| 指标 | Prometheus HTTP API | 生态最广，查询简单 |
| 测试 | go test + envtest + kind e2e | 单测跑决策表，控制循环回归有 envtest 与真集群两层 |
| CI | GitHub Actions | gofmt + vet + build + test / kind e2e / 多架构镜像 + cosign 签名 |
| 部署 | kustomize + Helm Chart 双轨 | 开发/CI 用 kustomize overlay，交付用 `charts/kubestasis`（crds/ 仅首装创建） |
