# KubeStasis 实现状态（真实账本）

> 本文件是**唯一可信的能力清单**。README 的一切对外描述以此为准，每完成一项就把状态改掉。
> 最后更新：2026-11-04 晚（补就绪/存活探针、缩容稳定窗口、Helm Chart、决策指标导出）
> 验收口径：代码能在干净集群上 `deploy → apply App → Available`，且有可复现的证据。
> ⚠️ 项目原名 KubeSentry，2026-11-03 更名 KubeStasis；CRD group 于 2026-11-04 由 `apps.sentry.io` 改为 `kubestasis.io`（不再沿用第三方商标）。

## 一、当前结论

**P0–P4 全部闭环，并补上了唯一缺的量化证据（vs 原生 HPA 压测）**：`apply CR → Deployment + Service → Available` 之外，三条"智能"链路都在真集群上跑通了——
**弹性伸缩**（真实 Prometheus 按 Pod 抓取 + 500 QPS 打流 → 扩容 1→5 → 停流渐进缩回 1，见 [docs/demo/01-scaling.md](demo/01-scaling.md)）、
**分级自愈**（CrashLoop 版本发布 → L2 摘除 → 冷却期内复发自动升级 L3 回滚 → 恢复；金丝雀窗口内错误率超阈值 → 20 秒内自动回滚，见 [docs/demo/02-self-healing.md](demo/02-self-healing.md)）、
**准入控制**（Mutating 默认值注入 + Validating 准入校验，自签证书方案不依赖 cert-manager，`make deploy-webhook` 一键起）。
**P2 自愈链路已进入 CI e2e**（kind 集群里真跑 crash 镜像 → 自动回滚断言），自愈能力不再依赖人工实测。
**伸缩对比**：同镜像、同资源、同流量时间线下与原生 HPA 的对照实验，见 [docs/benchmark/hpa-vs-kubestasis.md](benchmark/hpa-vs-kubestasis.md)（原始采样 CSV 入库，可复算）。
**11-04 晚补的四件"可交付性"短板**：托管 Deployment 的就绪/存活探针（省略时默认注入 TCP 就绪探针，`Available` 不再是"进程起来了"的乐观信号）、缩容稳定窗口（压测 run5 抓到的误缩容缺口闭合）、Helm Chart（`helm upgrade --install` 全链路真集群验证）、伸缩决策指标导出（`kubestasis_scaling_decisions_total` 等 4 个指标，"为什么没扩"变成一条 PromQL）。

## 二、体检与验证结果（2026-11-02 实测，11-04 深夜更新）

| 检查项 | 结果 | 实测输出 |
|---|---|---|
| `go build ./...` | ✅ | 通过（Go 1.27.3 本机，module `go 1.22.0`；k8s v0.31.1 / controller-runtime v0.19.0） |
| `go vet ./...` | ✅ | 无输出 |
| `gofmt -l` | ✅ | 无未格式化文件 |
| `go test ./...` | ✅ | **139 个用例**全绿（scaler 61 + controller 16 + healer 24 + webhook 24 + miniinformer 11 + metrics 3，含子测试）；另有 `test/envtest/` 4 条控制循环链路在 CI `envtest` job 用真实 apiserver 跑 |
| 部署链路 | ✅ | Docker Desktop k8s v1.36.1：`kubectl apply` CRD+RBAC+Manager → 控制器 Pod `1/1 Running` |
| 创建资源 | ✅ | `deployment.apps/sample 2/2`、`service/sample ClusterIP 80/TCP`、`endpoints 10.244.0.9,10.244.0.10` |
| 状态真实性 | ✅ | `NAME sample PHASE Stable READY 2 DESIRED 2`；`status.conditions` 出现 `Available=True / Progressing=False` |
| Events | ✅ | `kubectl get events --field-selector reason=KubeStasisReconcile` → `app available: 2/2 replicas ready` |
| 控制器日志 | ✅ | `ERROR` 计数 = 0 |
| CI | ✅ | 6 个 job 全绿：`docs`（链接有效性 + 文档一致性：用例数单一事实源、已完成项不得仍标"计划中"）/ `build`（gofmt+vet+build+test+`-race`，2026-11-05 加）/ `envtest`（真实 apiserver 控制循环回归，34s）/ `chart`（helm lint + 双形态渲染 + Chart CRD 与 controller-gen 产物逐字节 diff）/ `e2e` / `image`（多架构 + cosign 签名自验）。11-04 深夜最新：run [41842502428](https://github.com/weirong-ui/kubestasis/actions/runs/41842502428)，17m36s。e2e 覆盖三条链路：**P0**（部署+Available）+ **P3**（webhook 拒 `:latest`、replicas 跟随 minReplicas 默认值）+ **P2**（crash 镜像 → L2→L3 自动回滚 + 防乒乓断言） |

## 三、三栏表：能力 / 真实状态 / 证据与缺口

状态含义：`✅ 已实现`（有代码且集群验证过）｜`🚧 半成品`（有代码但链路未验证/部分）｜`🗓 规划中`

| 能力 | 真实状态 | 证据与缺口 |
|---|---|---|
| App CRD + 代码生成 | ✅ 已实现 | `config/crd/bases/kubestasis.io_apps.yaml` 由 controller-gen v0.16.5 生成；`zz_generated.deepcopy.go` 已入库 |
| Reconcile（App → Deployment） | ✅ 已实现 | `ensureDeployment` 幂等 CreateOrUpdate + `SetControllerReference`；集群验证 2/2 就绪 |
| Service 自动创建 | ✅ 已实现 | `ensureService`（ClusterIP，port/targetPort 同源）；Endpoints 有 2 个后端 IP |
| 托管资源标签 | ✅ 已实现 | Deployment/Service 均带 `app.kubernetes.io/managed-by=kubestasis`，README 的 `-l` 查询可直接用 |
| Status 真实副本数 | ✅ 已实现 | `CurrentReplicas` 取自 `deploy.Status.ReadyReplicas`，`DesiredReplicas` 取自 spec，两者分离 |
| Status Conditions | ✅ 已实现 | `Available` / `Progressing`，用 `meta.SetStatusCondition` 维护 `LastTransitionTime` 与去重 |
| Kubernetes Events | ✅ 已实现 | `Recorder` 已注入；`KubeStasisScaleUp/Down/Heal/CanaryFailed/Error/Reconcile` 六类 reason；首次可用时发事件 |
| Finalizer 删除时序 | ✅ 已实现 | 添加/移除正确；删前导出审计是未排期的增强项，不在 P0–P4 承诺内 |
| 迟滞带 + 缩容渐进算法 | ✅ 已实现 | 纯函数逻辑完整（含"缩回 minReplicas 不受迟滞约束""`ceil(0.2×current)` 缩不动"两个坑） |
| 冷却窗口 | ✅ 已实现 | `LastScalingAt` 在 decision≠None 时写入；逻辑已被单测覆盖（冷却拦缩容、放行扩容、过期放行） |
| Prometheus 多信号伸缩 | ✅ 已实现 | `internal/scaler/prometheus.go`：QPS（`rate` 换算副本）、P99（`histogram_quantile` 按比例扩）、错误率（超阈值按比例扩，比例可配）；`--prometheus-url`/`--prometheus-window` 注入，不配则休眠。**真实指标源端到端验证**（2026-11-03：集群内真实 Prometheus v2.54.1 按 Pod 抓取 + demoapp 打流 500 QPS → 扩 1→5，停流后渐进缩 5→1，全程 Events 留痕，实录见 [docs/demo/01-scaling.md](demo/01-scaling.md)） |
| AI 推理弹性信号 | ✅ 已实现，⚠️ 未实测 | 同一 `MetricsSource` 接口下的三个新信号：`QueueDepth`（队列深度换算副本，同 QPS 型）、`TTFT`（首 token 延迟超阈值按比例扩）、`KVCacheUsage`（KV cache 占用率超阈值按比例扩）；默认 vLLM 指标名，`WithQuery` 可换 SGLang/TGI。通用自愈平移到推理场景不重写项目。**边界：只有单测覆盖查询构造与换算逻辑，没有在真实 vLLM 集群上跑过**——"用推理负载压一次"在 ROADMAP 里，尚欠实测 |
| 故障识别（CrashLoop / OOM / 高频重启） | ✅ 已实现 | `Inspect` 按 Pod 标签列出实例，读 `containerStatuses[].state.waiting.reason` / `lastState.terminated.reason` / restartCount 兜底；有意忽略"别的 App 的 Pod"与"正在删除的 Pod"。**真集群验证**：crash 镜像发布后 ~2.5min 识别并动作 |
| 分级自愈 L1→L2→L3 | ✅ 已实现 | L1 扩容 20%（受 maxReplicas 钳制）/ L2 删除不健康 Pod / L3 从 ReplicaSet 修订历史回滚镜像（改 App.spec，声明式收敛）。升级路径由 `status.lastHealLevel/lastHealAt` 驱动：冷却期内复发才升级；已到 L3 还在坏则进入观察。**真集群验证**：L2 摘除 → 复发 → 自动升 L3 → 回滚 → 2/2 恢复 |
| 金丝雀观察 + 自动回滚 | ✅ 已实现 | 镜像变更写入 `status.lastImageChangeAt` 开窗；窗口内 `ErrSource.ErrorRate` 超阈值即回滚并关窗（防回滚死循环）。**真集群验证**：50% 错误率版本发布后 ~20s 自动回滚，`status.rollbackRevision=1` |
| 冲突自愈的写路径 | ✅ 已实现 | 11.03 评审指出 L3 回滚撞过 `the object has been modified`：healer/scaler 全部 spec 写路径改 `RetryOnConflict + Merge Patch`（每次重试重新 Get 最新对象重算）；控制器层 conflict 静默退避不再发 Warning 事件。拦截器注入冲突的单测覆盖 |
| 策略参数 CRD 可配 | ✅ 已实现 | 11.03 评审指出魔法数字硬编码：L1 扩容比例（`health.scaleUpPercent`）、高频重启阈值（`health.restartThreshold`）、缩容步长（`scaling.scaleDownStepPercent`）、迟滞带（`scaling.hysteresisPercent`）、错误率扩容比例（`scaling.errorRateScaleUpPercent`）全部入 CRD 带默认值；旧对象零值回退原默认，行为兼容 |
| Admission Webhook | ✅ 已实现 | `internal/webhook/`：Mutating（replicas 跟随 minReplicas 的跨字段默认）+ Validating（禁 latest/无 tag 镜像、min≤max、replicas 落区间、指标 target 按类型可解析、同类型不重复、金丝雀阈值与窗口下限；聚合报错一次列全）。`make deploy-webhook`：overlay + `hack/webhook-certs.sh` 自签证书 + caBundle 注入，不依赖 cert-manager。**CI e2e 负向用例**：`:latest` 被拒、默认值注入生效 |
| mini-Informer | ✅ 已实现 | `pkg/miniinformer/`：手写 ListWatch → DeltaFIFO（按键压缩去重）→ Store → Handler + 控制器侧 WorkQueue（幂等+不漏），泛型实现约 400 行，11 个用例含断线重列；与 client-go 的取舍差异逐条写在包注释与 [README](../pkg/miniinformer/README.md) |
| 镜像签名与准入策略 | ✅ 已实现 | CI image job 用 cosign keyless 签 GHCR digest（GitHub OIDC → Fulcio → Rekor）并签完自验；`policies/kyverno-kubestasis.yaml`：禁 latest/无 tag + kubestasis-system 强制验签。CKS 考点变成可展示证据 |
| 就绪/存活探针 | ✅ 已实现 | `spec.readiness` / `spec.liveness`（`HTTPGet`/`TCPSocket`，path/period/timeout/initialDelay/failureThreshold 可配）。**readiness 省略时默认注入 TCPSocket 探针（端口取 spec.port）**——托管应用默认具备"端口能连上才算就绪"的语义；liveness 只在显式声明时注入（存活探针杀进程的代价比就绪探针高，不敢默认猜）。webhook 校验 HTTP path 必须以 `/` 开头、TCP 配 path 只警告不阻断。真集群验证：显式 HTTPGet 注入正确、Pod Ready、无 Unhealthy 事件 |
| 缩容稳定窗口 | ✅ 已实现 | `scaling.scaleDownStabilizationSeconds`（默认 60s，0 关闭）+ `status.belowTargetSince`：信号需持续低于目标满窗口才允许缩容，任一信号高于目标的采样立即重置计时。补的是压测 run5 抓到的误缩容缺口（突发起点 4→3→13s 又扩回 5）。真集群验证：3 副本零流量下，窗口期内纹丝不动，到点后**单步** 3→2（不连跳——status 先于 spec 写入的竞态修复同时被验证） |
| 伸缩决策指标导出 | ✅ 已实现 | `internal/metrics/`：`kubestasis_scaling_decisions_total{namespace,app,action,reason}`（reason 用固定枚举 ReasonCode，不是自由文本——防 label 基数爆炸）+ `desired/current/signal_replicas` 三个 gauge；App 删除时 `Forget` 清理防残留。真集群验证：4 个指标全部出现在 `/metrics` 端点，含一条 `ScaleDown` 决策记录 |
| Helm Chart | ✅ 已实现 | `charts/kubestasis`（0.1.1）：CRD 放 `crds/`、SA/RBAC/Deployment/metrics Service/webhook 全模板化；webhook 证书二选一——默认 `helm genSelfSignedCert` 渲染期生成（零外部依赖），或 `--set webhook.certManager.enabled=true` 走 cert-manager；`helm lint` + `template` + server-side dry-run + 真集群 `upgrade --install`（p9 镜像）+ 滚动升级全部验证。注意事项与 values 说明见 [charts/kubestasis/README.md](../charts/kubestasis/README.md) |
| 单元测试 | ✅ 139 用例 | scaler 61 + controller 16 + healer 24 + webhook 24 + miniinformer 11 + metrics 3（用 `--- PASS` 行数按包实测，和 §二 同一口径） |
| envtest 集成测试 | ✅ 4 条链路 | `test/envtest/`：进程内起真实 etcd + apiserver，验 watch → enqueue → Reconcile → status 回写 的闭环——创建（Deployment/Service/ownerRef/finalizer/DesiredReplicas）、就绪回写（代写 Deployment.status → App Available=True）、删除放闸、副本漂移拉回。CI 有独立 `envtest` job；本地无二进制时自动 Skip（`KUBESTASIS_REQUIRE_ENVTEST=1` 强制失败） |
| Grafana 面板 | ✅ 已实现 | `deploy/monitoring/grafana/`：4 行 16 格（概览 → 决策 → 信号与窗口 → 控制器健康/运行时），变量按 App 命名空间/App 过滤；`kustomization.yaml` 打成带 `grafana_dashboard: "1"` 的 ConfigMap 供 sidecar 自动发现。**真集群验证**：kind + 真 Prometheus 抓控制器 metrics，跑出一次完整的 2→1（稳定窗口挡了 5 轮 stabilizing）→2；截图 [docs/assets/grafana-dashboard.png](assets/grafana-dashboard.png)，读图指南见 [deploy/monitoring/README.md](../deploy/monitoring/README.md) |
| 压测与 HPA 对比表 | ✅ 已完成 | 2026-11-04：同镜像/同资源/同流量下 vs 原生 HPA（CPU 50%）的对照实验，5 秒粒度采样入库 `docs/benchmark/data.csv`，报告见 [docs/benchmark/hpa-vs-kubestasis.md](benchmark/hpa-vs-kubestasis.md)。结论有限定条件，报告里写清了口径与失效风险 |

## 四、11.02 夜这一轮改了什么（P0 收尾）

**脚手架与构建**

1. 建立 `go.mod` / `go.sum`（k8s.io/api|apimachinery|client-go v0.31.1，controller-runtime v0.19.0）
2. 补 `api/v1alpha1/groupversion_info.go`（GVK/Scheme 注册）与 `zz_generated.deepcopy.go`
3. 生成 `config/crd/bases/`、`config/rbac/role.yaml`；手写 `config/manager/manager.yaml`（含 Namespace、非 root、只读根文件系统、探针）、`config/default/kustomization.yaml`
4. `make deploy` 改为 `kubectl kustomize config/default | sed 's|IMG_PLACEHOLDER|$(IMG)|' | kubectl apply -f -`，不依赖外部 kustomize 二进制
5. `Makefile` 的 controller-gen 调用改为显式包路径（`./...` 在 Go 1.22+ 下对 v0.16.5 会报 `no Go files`）
6. `Dockerfile`：删掉指向不存在目录的 `COPY pkg/`，`# syntax` 指令置顶，新增 `GOPROXY` / `RUNTIME_IMAGE` 两个 build-arg（国内可达性）
7. 全仓统一 LF 行尾 + `.gitattributes`（原先 CRLF 导致 `gofmt -l` 永远报脏）

**控制器代码**

8. `cmd/main.go`：修掉非法的 `Metrics: ctrl.Options{}.Metrics`（改 `metricsserver.Options`）；注入 `Healer`、`Recorder`（原先 `r.Healer` 是 nil，只是碰巧没 panic）
9. 新增 `ensureService`，并把 `Owns(&corev1.Service{})` 加入 `SetupWithManager`
10. `updateStatus` 用 Deployment 真实状态回写，补 `DesiredReplicas` / `ObservedGeneration`；新增 `Available` / `Progressing` Conditions
11. `emitEvent` 接 `Recorder`；首次 `Available=True` 时发 `KubeStasisReconcile` 事件
12. `scaler.Apply` 去掉 `interface{}` 占位，改为**声明式回写 `spec.replicas`**（不直接改 Deployment，避免与 `ensureDeployment` 打架）
13. `scaler.Decide` 在无指标源时明确返回 `no metrics source configured (P1)`，不再用"当前副本数"冒充信号

**发现的原有缺陷（已修）**

14. `+kubebuilder:default={}` 打在 `Scaling` / `Health` / `Canary` 结构体上会生成非法 CRD（`default: {}` 缺少必填子字段），导致 `kubectl apply` CRD 直接被拒 —— 改为字段级 default + `+optional`
15. `status.currentReplicas` 若设为 required，值为 0 时 merge patch 不带该字段会报 `Required value` —— 保持 optional
16. **11.03 新发现**：`Scaling`/`Health` 原为值类型 + `omitempty`——Go 的 `omitempty` 对结构体不生效，控制器回写对象（如加 finalizer）时会把省略的 `health` 序列化成 `{"strategy":""}`，被 CRD 的 enum 校验拒绝 → **省略 spec.health 的任何 App 都会在第一轮 Reconcile 无限报错**（样例 CR 恰好写了全字段所以 P0 没暴露）。修复：改 `*ScalingPolicy` / `*HealthPolicy` 指针，并加了序列化回归测试（`TestAppSpec_OmitsNilPolicies`）

**CI**

16. 重写 `.github/workflows/ci.yml`：条件判断从 job 级 `if` 移到 step 级（原写法使整份 workflow 校验失败，一个 job 都没创建过）；新增 `build`（gofmt/vet/build/test）与真实 `e2e`（kind + 构建镜像 + `make deploy` + `kubectl wait --for=condition=Available` + 校验 events）
17. 首次跑通全绿：4/4 job success，徽章由红转绿

## 五、已知噪音（不阻塞，待优化）

- ~~写路径偶发 `the object has been modified` conflict~~：**已修复**（2026-11-03 晚）——healer/scaler 的 spec 写路径全部改为 `RetryOnConflict + Merge Patch`，控制器层 conflict 静默退避不再发 Warning 事件。
- `ensureDeployment` 的 CreateOrUpdate 仍可能与 Deployment controller 的 status 写入竞争：已不报错（conflict 静默重试），但极端并发下会多一轮 Reconcile，属可接受成本。

## 六、环境备忘（下次别再踩）

| 现象 | 处理 |
|---|---|
| `git clone` 报 `CRYPT_E_NO_REVOCATION_CHECK` | `GIT_SSL_NO_VERIFY=1 git clone ...`；curl 加 `--ssl-no-revoke` |
| `proxy.golang.org` 连接被重置/502 | `go env -w GOPROXY=https://goproxy.cn,direct` |
| `docker build` 拉不到 `gcr.io/distroless/*` | `--build-arg RUNTIME_IMAGE=m.daocloud.io/gcr.io/distroless/static-debian12:nonroot` |
| `docker build` 内 `go mod download` 卡住 | `--build-arg GOPROXY=https://goproxy.cn,direct` |
| 本机没有 `make` | 直接跑 Makefile 里的等价命令；CI（Linux）里 `make` 正常 |
| 同 tag 重新构建后集群仍跑旧镜像 | 换 tag（`e2e2`/`e2e3`）或 `imagePullPolicy: Always`；containerd 会缓存同名 tag |
| Docker Desktop 集群拉 `nginx:1.27-alpine` 报 registry-mirror 500 | 先在本机 `docker pull` 预热镜像，再让集群重试 |

## 七、已知不解决的问题（主动画边界）

这一节存在的原因：一份只列"能做什么"的清单，读者没法判断可信度。下面这些是**明确知道、短期不打算解决**的问题——不是遗漏，是取舍。

| 问题 | 为什么现在不解决 | 真要解决需要什么 |
|---|---|---|
| **有状态应用的存储编排**（PD 分离、主从选举、PVC 生命周期） | KubeStasis 管的是无状态工作负载；有状态编排是另一套知识体系（StatefulSet 语义、卷拓扑、数据迁移），硬塞进来会让主链路不扎实 | 单独设计 StatefulApp CRD；或直接集成 Operator 生态里成熟的方案（如 TiDB Operator、CloudNativePG） |
| **多集群 / 跨集群联邦** | 涉及跨集群服务发现、状态同步、冲突消解，是独立课题；单集群链路还没被真实流量打磨过 | 至少要先有 metrics 聚合层与全局调度器；基于 Karmada/ClusterAPI 做上层编排比自研更现实 |
| **无侵入指标采集（eBPF）** | 需要内核态开发、节点特权与版本兼容矩阵；收益是省掉应用埋点，但本项目假定应用暴露 Prometheus 指标 | 用 Pixie/eBPF exporter 作为 MetricsSource 的另一种实现即可，不必自研 |
| **真正的流量切分式金丝雀**（按权重引流） | 现在做的是"发布后观察错误率，超阈值回滚"——观察真实流量，不做权重控制。名字叫 canary，但不是流量治理 | 引入 Gateway API / Istio 的权重路由；Webhook 侧还要管 VirtualService 生命周期 |
| **控制面高可用** | chart 已开 `leaderElection`，但 `replicaCount` 默认 1——**双副本下的选主接管、伸缩不重复执行从未实测**。个人项目的集群规模用不上，开了反而增加演示复杂度 | 副本数 ≥2 真集群验证切换；配合 PDB 与反亲和 |
| ~~决策可观测性的最后一公里（Grafana 面板）~~ | **已解决**（11-04 深夜，`92c277c`，见 §三）。残余：面板只做单集群单数据源，没有多集群聚合 | —— |
| ~~envtest 级别的控制循环回归~~ | **已解决**（11-04 深夜，`52b3c70`，见 §三）。残余：envtest 没覆盖 webhook 联动与 status 子资源的并发写竞争 | —— |
| **升级/迁移路径**（CRD 版本演进、v1alpha1 → v1beta1） | 还没有真实用户，谈 conversion webhook 为时过早 | 定义 v1beta1 + conversion webhook + 存储版本迁移演练 |
| **安全加固的完整闭环**（NetworkPolicy、seccomp、RBAC 最小化审计） | 已有非 root、只读根文件系统、镜像签名校验；但没有逐条做 CIS Benchmark 对照 | 用 kube-bench / Kubescape 扫一遍，按报告逐项修 |
| **压测结论的普适性** | 见 [压测报告](benchmark/hpa-vs-kubestasis.md) 的"口径限定"一节：单节点、单应用、CPU 忙等模拟，且**方差未补齐**（HPA 侧三轮准重复 σ≈8s，见 [variance-precheck.md](benchmark/variance-precheck.md)），结论只在同口径下成立 | 先做串行 + 多轮重复 + G0/G2 对照（压测扩项）；再谈多节点、多应用混部、真实业务流量回放 |
| **缩容稳定窗口的默认值没经过大样本校准** | 窗口机制已实现（`scaleDownStabilizationSeconds` 默认 60s + `belowTargetSince`），但 60s 这个数只在单节点单应用口径下验证过——它挡住了 run5 那种"突发起点"误缩容。若现在重跑，回到 1 副本会比报告里的 65s 再慢约一个窗口（约 60s，稳定窗口的代价） | 真实流量回放下对比 0/60/300s 三档；如果长期看不出差异，把默认改回 0 并在文档里写明取舍 |



## 八、下一步（11.05 起）

1. ~~P1 指标源~~ / ~~P2 自愈~~ / ~~P3 Webhook~~ / ~~P4 mini-Informer~~ / ~~压测对比~~：**全部完成**（2026-11-03 ~ 11-04）
2. ~~Helm Chart~~ / ~~决策指标导出~~ / ~~就绪探针~~ / ~~缩容稳定窗口~~：**11-04 晚完成**
3. ~~envtest 集成测试~~ / ~~Grafana 面板~~：**11-04 深夜完成**
4. 剩下的硬骨头：
   - **压测扩项**（串行 + 多轮重复 + G0/G2 对照，让核心结论可分辨于噪声——动机与预核算见 [variance-precheck.md](benchmark/variance-precheck.md)）
   - HA 双副本切换验证（§七"控制面高可用"残余）
5. 可选增强（不承诺）：Kyverno 策略进 CI kind 集群实测、CRD v1beta1 + conversion

## 九、每日勾选

| 完成日 | 事项 | 状态变化 |
|---|---|---|
| 2026-11-02 | 体检 + STATUS/README 账本 | 建立基线，暴露红线 |
| 2026-11-02 夜 | P0 收尾（脚手架、Service、Status/Conditions、Events、CI） | 链路跑通：CR → Deploy+Service → Available |
| 2026-11-03 | P1 指标源 + 31 个单测 + 真集群端到端验证 + 仓库描述/topics + GitHub 主页 README | 伸缩引擎从"休眠"到"集群验证过"；0 测试 → 31 用例；修掉 omitempty 结构体序列化 bug |
| 2026-11-03 下午 | P1 真实指标源演示（真 Prometheus + 打流）+ P2 自愈引擎落地 + 26 个 healer 用例 | P1/P2 从"未验证"到"真集群跑通"；31 → 57 用例；踩到并修复"静态 Service 抓取导致 counter 非单调"的指标坑 |
| 2026-11-03 晚 | P3 Webhook + P4 mini-Informer + 评审三连修复（冲突写路径/魔法数字可配/CI 覆盖 P2）+ AI 推理信号 + cosign/Kyverno + 更名 KubeStasis | 57 → 103 用例；P0–P4 全闭环；真集群复现并修掉两个新坑（见下） |
| 2026-11-04 | 对照压测（HPA vs KubeStasis）+ CRD resources/env 透传 + webhook 校验配套 | 五轮压测，前四轮全部被自己推翻（详见压测报告）；修复渐进缩容跨步竞态（status 先于 spec 写入）；报告与原始 CSV 入库；103 → 113 用例 |
| 2026-11-04 晚 | 就绪/存活探针 + 缩容稳定窗口 + Helm Chart + 决策指标导出 | 113 → 137 用例；`Available` 语义修正（有真探针了）；run5 误缩容缺口闭合；`helm upgrade --install` 真集群验证（含滚动升级 p8→p9）；踩到"kustomize 与 Helm 双装导致两套 webhook 打架"的坑（见下） |
| 2026-11-04 深夜 | envtest 控制循环回归落地（原计划第 3 周，提前完成） | 新增 `test/envtest/` 4 条链路 + CI 独立 `envtest` job（5 → 6 job）；P0 最后一个未勾项清零 |
| 2026-11-04 深夜 | Grafana 面板 + 录制规则两处真 bug 修复 | 面板 4 行 16 格真集群截出入库；修 `alerts:` 写在 rules 数组外（PrometheusRule schema 会静默剪掉，告警从未生效）、比例规则无序列时显示 No data（改零填充）；chart ServiceMonitor 补 `honorLabels: true`（否则决策指标的 App 命名空间被目标标签覆盖，面板按 App 过滤全空） |
| 2026-11-05 | 压测报告降级口径 + 方差预检 + 账本回删 + README 定位段 + v0.1.0 tag | 报告加版本声明（数据采集于稳定窗口实现前）；"快 1.7 倍"降级为单次观察（HPA 侧三轮准重复 σ≈8.2s，组间差仅 2.1σ），副本-分钟（≈6σ）提为主结论；`variance-precheck.md` + 复算脚本入库；STATUS §七/§八 回删已解决项（Grafana/envtest）、修正两处因果与日期；README 新增"什么时候该用/不该用"（承认 KEDA+Rollouts+Kyverno 组合更省事）；打 `v0.1.0` |
| 2026-11-05 下午 | 决策函数纯化（外部评审第 4 条） | `Decide()` 不再改 `Engine.nowFunc`（删字段，时钟改为调用参数）也不再改 `app.Status.BelowTargetSince`（改为 `Decision.BelowTarget` 三态建议，由控制器落盘）；新增 `TestDecide_DoesNotMutateInput` 与并发用例，把"纯函数可测"的承诺钉成测试 |
| 2026-11-05 傍晚 | 文档一致性门（外部评审"文档内部不一致"） | 修 README 开发节的 113（漏改为 139）、DESIGN 三处 envtest 仍标"计划中"、G4 补"不做按权重引流"；新增 `hack/doc_consistency.py` + docs job 门——用例数以徽章为单一事实源（历史演进行豁免）、产物已存在就不许再声称未做。人工核对漏过一次，改成机器查 |

### 11.03 晚新踩的坑（已修，写进记忆）

1. **CRD schema 默认值先于 Mutating Webhook 落地**：`spec.replicas` 挂 `default=1` 时，webhook 的"跟随 minReplicas"永远被抢先填 1——跨字段默认的字段不能挂 schema 默认值（真集群实测确认，不是读文档猜的）
2. **自愈冷却期不能短于故障探测周期**：CrashLoop 的"删 Pod → 再进 CrashLoopBackOff"周期约 1-2 分钟；冷却 90s 时"冷却期内复发才升级"永不触发，L1 把 crash 副本从 1 扩到 3 空转。默认 3 分钟刚好覆盖
3. **caBundle 在 `clientConfig` 下**：patch 路径 `/webhooks/N/clientConfig/caBundle`，不是 `/webhooks/N/caBundle`
4. **rollout 完成 ≠ webhook 可服务**：endpoint 就绪有秒级延迟，CI 负向用例要对 InternalError 重试、只对 Forbidden 断言
5. **乒乓回滚**（本轮 kind CI 抓到的真 bug）：回滚到 v1 成功后，旧 RS 的 crash Pod 还没清完，下一轮 Inspect 仍不健康 → "上一个不同镜像"恰好是刚滚走的 v2-crash → 滚回坏版本（v2→v1→v2，同一秒内两次 L3；DD 本地没暴露是因为缓存/滚动时序碰巧错开）。修复：`Inspect` 报告携带 `BadImages`（不健康 Pod 的镜像），L3 选版排除坏镜像；CI 增加"回滚后再盯 3 分钟"的防乒乓断言。教训：**回滚类逻辑的验证不能停在"看到回滚发生"，必须验证"停在回滚结果上"**

### 11.04 压测轮踩的坑

1. **Prometheus 的 `scrape_timeout` 必须 ≤ `scrape_interval`**：为了扛住饱和 Pod 的慢 `/metrics` 把 timeout 调到 20s，却忘了 interval 还是 5s —— Prometheus 直接拒绝启动、整轮压测的 QPS 信号全为 0（run3 报废）。且它崩得不明显：Pod 在 CrashLoopBackOff，压测脚本照常出数据，只是那一列恒为 0
2. **写 spec 会立刻触发下一轮 Reconcile，早于本轮 status 写入可见**：冷却窗口若存在 status 里，spec patch 引发的 Reconcile 会读到旧 status → 冷却形同虚设，渐进缩容 3→2→1 在一个采样窗口连跳（run4 报废）。修复：先写 status 再写 spec（watch 事件按 rv 有序，先写的必先到 cache）。教训：**凡是"本轮写、下轮读"的状态，顺序本身就是正确性的一部分**
3. **压测对照的前提是"两边都真的在跑"**：run3 里 HPA 侧数据完全正常、KS 侧恒为 1 副本——看起来像"KS 不扩容"，实际是监控栈挂了。看对照数据的第一件事是验证**对弱势一侧的输入是否存在**，而不是急着解释结果

### 11.04 晚 Helm 验证踩的坑

1. **kustomize 与 Helm 双装 = 两套 MutatingWebhook 同时生效**：Helm 装到新命名空间后把旧 kustomize manager 缩到 0，但旧 webhook 配置（failurePolicy=Fail）还挂在集群里、service 已无后端 → **所有 App 写入（包括摘 finalizer 的 Update）全部被拒**，App 卡 Terminating、reconcile 只报 connection refused。表象是"新控制器挂了"，实际是死掉的那套在拦人。迁移必须先 `make undeploy` 拆干净旧 webhook 配置，再 `helm install`；已写进 charts/kubestasis/README.md 的"迁移注意"
2. **CRD 在 Helm `crds/` 目录只在首次 install 创建，升级不更新 schema**：本地验证时新版字段（readiness 等）被 apiserver 静默剪掉（pruning），且 kubectl apply 不报错——`last-applied-configuration` 注解里还留着字段，极具迷惑性。升级 chart 后必须手动 `kubectl apply -f charts/kubestasis/crds/`，NOTES.txt 里已提示
