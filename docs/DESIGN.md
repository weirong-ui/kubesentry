# KubeSentry 设计文档

> 版本：v1.0 ｜ 状态：设计中 → 实现 ｜ 作者：<你的名字>

## 1. 背景与动机

Kubernetes 提供的弹性能力（HPA/VPA）本质是**单信号、无记忆**的：HPA 根据 CPU/内存比值机械地扩缩容，既不理解业务指标（QPS、P99、错误率），也不对故障做任何主动响应。KEDA 补齐了多指标源，但依然只有"伸缩"这一只手。

生产环境真正的痛点是两个：

1. **震荡**：流量尖峰 → 疯狂扩容 → 流量回落 → 缩容 → 再来一次。没有冷却窗口和迟滞机制的伸缩是慢性自杀。
2. **故障不自愈**：新版本发布后 5xx 飙升，值班同学半夜手动 `kubectl rollout undo`。Pod 层面的自愈（kubelet 重启容器）解决不了"版本坏"这种 Pod 重启一百次也没用的问题。

KubeSentry 的目标：**用 Operator 模式把"伸缩决策"和"故障自愈"声明式地交给集群自己完成**。

## 2. 目标与非目标

**目标**
- G1：一个 `App` CRD 描述应用全生命周期：镜像、副本、伸缩策略、健康策略
- G2：多信号伸缩决策引擎，带冷却窗口与迟滞抑制
- G3：分级自愈：扩容 → 降级 → 回滚，动作全部以 Kubernetes Event 留痕
- G4：金丝雀发布观察窗口，错误率超阈值自动回滚
- G5：Admission Webhook 做校验与安全默认值注入
- G6：envtest 单元测试覆盖率 ≥ 70%，GitHub Actions 全自动 CI

**非目标**
- 不替代 HPA/KEDA（可以共存，KubeSentry 管自愈，HPA 管容量）
- 不做通用 PaaS（不管理数据库中间件等有状态服务的运维语义）
- P0~P4 不引入 eBPF（P5 再考虑无侵入采集）

## 3. API 设计

### 3.1 CRD：`App`（Group: `apps.sentry.io`，Version: `v1alpha1`）

```go
type AppSpec struct {
    // 镜像与副本
    Image     string `json:"image"`
    Replicas  *int32 `json:"replicas,omitempty"`

    // 端口与探针
    Port      int32           `json:"port"`
    Readiness *ProbeConfig    `json:"readiness,omitempty"`

    // 伸缩策略
    Scaling ScalingPolicy `json:"scaling"`

    // 健康与自愈策略
    Health HealthPolicy `json:"health"`
}

type ScalingPolicy struct {
    MinReplicas     int32          `json:"minReplicas"`
    MaxReplicas     int32          `json:"maxReplicas"`
    CooldownSeconds int            `json:"cooldownSeconds"`
    Metrics         []MetricTarget `json:"metrics"`
}

type MetricTarget struct {
    Type   MetricType `json:"type"`   // QPS | P99Latency | ErrorRate
    Target string     `json:"target"` // "1000" / "200ms" / "0.01"
}

type HealthPolicy struct {
    Strategy  HealthStrategy `json:"strategy"` // graduated | observability-only
    Canary    CanaryPolicy   `json:"canary"`
}

type CanaryPolicy struct {
    Enabled            bool   `json:"enabled"`
    ObservationSeconds int    `json:"observationSeconds"`
    ErrorRateThreshold string `json:"errorRateThreshold"`
}
```

### 3.2 Status 设计

```go
type AppStatus struct {
    Phase            string       `json:"phase"`   // Provisioning | Stable | Healing | RollingBack
    CurrentReplicas  int32        `json:"currentReplicas"`
    LastScalingAt    *metav1.Time `json:"lastScalingAt,omitempty"`
    LastHealAction   string       `json:"lastHealAction,omitempty"`
    RollbackRevision int64        `json:"rollbackRevision,omitempty"`
    Conditions       []metav1.Condition `json:"conditions,omitempty"`
}
```

要点：
- **Status 只记录决策结果与依据**，不记录过程日志（日志交给 Events 和 Prometheus）
- 所有时间戳字段用于实现冷却窗口的幂等判断，Reconcile 重入安全

## 4. 核心流程

### 4.1 Reconcile 主循环

```
Reconcile(App):
  1. 拉取 App，若被删除则清理（finalizer：导出审计记录后放行删除）
  2. Webhook 已保证 spec 合法，此处只兜底校验
  3. Ensure Deployment/Service 与 spec 一致（CreateOrUpdate）
  4. 若处于金丝雀观察窗口：查询错误率 → 超阈值则触发回滚，结束
  5. HealthCheck：采集 Pod 状态 + 应用指标
     - 无异常 → 交给 Scaler 判断是否伸缩（受冷却窗口约束）
     - 有异常 → 交给 Healer 执行分级自愈
  6. 更新 Status（Phase、CurrentReplicas、Conditions）
```

### 4.2 伸缩决策（scaler 包）

多信号取**最紧急者**：

```
desiredBy(signal) = ceil(currentReplicas × current / target)
desired = max(各信号 desired, minReplicas)，且 ≤ maxReplicas
```

防震荡三件套：
1. **冷却窗口**：`now - lastScalingAt < cooldownSeconds` 时拒绝任何缩容（扩容允许紧急放行）
2. **迟滞带**：`|desired - current| < max(1, current × 10%)` 时不动作
3. **缩容渐进**：缩容每次最多 -20%，扩容可一步到位

### 4.3 自愈引擎（healer 包）

故障识别 → 分级动作：

| 故障信号 | 识别方式 | L1 扩容 | L2 降级 | L3 回滚 |
|---|---|---|---|---|
| CrashLoopBackOff | Pod restartCount 阈值 + State 匹配 | ✅ 分流压力 | ✅ 摘除不健康副本 | ✅ rollbackRevision |
| OOMKilled | `lastState.terminated.reason` | ✅ 不适用 | — | 视版本而定 |
| 5xx 错误率飙升 | Prometheus 查询 | ✅ | ✅ | ✅ |
| P99 突增 | Prometheus 查询 | ✅ | — | — |

分级原则：**每次 Reconcile 只执行一级动作，观测一个窗口（观察期）后再升级**，避免"扩容+回滚"同时发生导致事故现场被破坏。所有动作 Emit Event（reason 统一前缀 `KubeSentry*`）并写回 Status。

### 4.4 Admission Webhook

- **Validating**：minReplicas ≤ maxReplicas、镜像必须带 tag（禁 latest 进生产）、目标值格式合法
- **Defaulting（安全默认值，CKS 落地点）**：自动注入 `resources.limits`、`securityContext`（readOnlyRootFilesystem、runAsNonRoot、drop ALL capabilities）、`seccompProfile: RuntimeDefault`

## 5. 测试策略

| 层级 | 工具 | 覆盖内容 |
|---|---|---|
| 单元 | go test + fake client | Scaler 决策表、Healer 状态机、Webhook 校验规则 |
| 集成 | envtest（真实 apiserver + etcd） | Reconcile 全流程、Finalizer、Status 更新 |
| 混沌 | 本地 kind 集群脚本 | 杀 Pod / 打满内存 / 灌异常流量，验证自愈 |

## 6. 安全设计（呼应 CKS）

- Operator 自身以 distroless 镜像运行，非 root，只读根文件系统
- RBAC 最小权限：只请求 `apps`/`apps.sentry.io` 相关资源权限，拒绝通配符
- Webhook TLS 证书由 cert-manager 自动轮换
- Defaulting Webhook 强制为受管应用注入安全上下文（集群级"兜底防线"思路）

## 7. 技术选型

| 组件 | 选型 | 理由 |
|---|---|---|
| 脚手架 | kubebuilder v4 | 社区标准，CRD/Webhook/配置一键生成 |
| Controller | controller-runtime | 事实标准，对接 Informer/WorkQueue |
| 指标 | Prometheus HTTP API | 生态最广，查询简单 |
| 测试 | envtest | 无需真实集群即可测 Reconcile |
| CI | GitHub Actions | lint + test + image build + kind e2e |
| 部署 | kustomize + Helm 双轨 | 开发用 kustomize，用户用 Helm |
