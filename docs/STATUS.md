# KubeStasis 实现状态（真实账本）

> 本文件是**唯一可信的能力清单**。README 的一切对外描述以此为准，每完成一项就把状态改掉。
> 最后更新：2026-11-04（补 vs 原生 HPA 压测对比、App 支持 resources/env、CRD group 更名 kubestasis.io）
> 验收口径：代码能在干净集群上 `deploy → apply App → Available`，且有可复现的证据。
> ⚠️ 项目原名 KubeSentry，2026-11-03 更名 KubeStasis；CRD group 于 2026-11-04 由 `apps.sentry.io` 改为 `kubestasis.io`（不再沿用第三方商标）。

## 一、当前结论

**P0–P4 全部闭环，并补上了唯一缺的量化证据（vs 原生 HPA 压测）**：`apply CR → Deployment + Service → Available` 之外，三条"智能"链路都在真集群上跑通了——
**弹性伸缩**（真实 Prometheus 按 Pod 抓取 + 500 QPS 打流 → 扩容 1→5 → 停流渐进缩回 1，见 [docs/demo/01-scaling.md](demo/01-scaling.md)）、
**分级自愈**（CrashLoop 版本发布 → L2 摘除 → 冷却期内复发自动升级 L3 回滚 → 恢复；金丝雀窗口内错误率超阈值 → 20 秒内自动回滚，见 [docs/demo/02-self-healing.md](demo/02-self-healing.md)）、
**准入控制**（Mutating 默认值注入 + Validating 准入校验，自签证书方案不依赖 cert-manager，`make deploy-webhook` 一键起）。
**P2 自愈链路已进入 CI e2e**（kind 集群里真跑 crash 镜像 → 自动回滚断言），自愈能力不再依赖人工实测。
**伸缩对比**：同镜像、同资源、同流量时间线下与原生 HPA 的对照实验，见 [docs/benchmark/hpa-vs-kubestasis.md](benchmark/hpa-vs-kubestasis.md)（原始采样 CSV 入库，可复算）。

## 二、体检与验证结果（2026-11-02 实测，11-03 晚更新）

| 检查项 | 结果 | 实测输出 |
|---|---|---|
| `go build ./...` | ✅ | 通过（Go 1.27.3 本机，module `go 1.22.0`；k8s v0.31.1 / controller-runtime v0.19.0） |
| `go vet ./...` | ✅ | 无输出 |
| `gofmt -l` | ✅ | 无未格式化文件 |
| `go test ./...` | ✅ | **113 个用例**全绿（scaler 46 + controller 11 + healer 24 + webhook 21 + miniinformer 11，含子测试） |
| 部署链路 | ✅ | Docker Desktop k8s v1.36.1：`kubectl apply` CRD+RBAC+Manager → 控制器 Pod `1/1 Running` |
| 创建资源 | ✅ | `deployment.apps/sample 2/2`、`service/sample ClusterIP 80/TCP`、`endpoints 10.244.0.9,10.244.0.10` |
| 状态真实性 | ✅ | `NAME sample PHASE Stable READY 2 DESIRED 2`；`status.conditions` 出现 `Available=True / Progressing=False` |
| Events | ✅ | `kubectl get events --field-selector reason=KubeStasisReconcile` → `app available: 2/2 replicas ready` |
| 控制器日志 | ✅ | `ERROR` 计数 = 0 |
| CI | ✅ | 4 个 job 全绿：`docs` / `build`（gofmt+vet+build+test）/ `e2e` / `image`（多架构 + cosign 签名自验）。11-04 最新：run [41801203117](https://github.com/weirong-ui/kubestasis/actions/runs/41801203117)，26m06s。e2e 覆盖三条链路：**P0**（部署+Available）+ **P3**（webhook 拒 `:latest`、replicas 跟随 minReplicas 默认值）+ **P2**（crash 镜像 → L2→L3 自动回滚 + 防乒乓断言） |

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
| AI 推理弹性信号 | ✅ 已实现 | 同一 `MetricsSource` 接口下的三个新信号：`QueueDepth`（队列深度换算副本，同 QPS 型）、`TTFT`（首 token 延迟超阈值按比例扩）、`KVCacheUsage`（KV cache 占用率超阈值按比例扩）；默认 vLLM 指标名，`WithQuery` 可换 SGLang/TGI。通用自愈平移到推理场景不重写项目 |
| 故障识别（CrashLoop / OOM / 高频重启） | ✅ 已实现 | `Inspect` 按 Pod 标签列出实例，读 `containerStatuses[].state.waiting.reason` / `lastState.terminated.reason` / restartCount 兜底；有意忽略"别的 App 的 Pod"与"正在删除的 Pod"。**真集群验证**：crash 镜像发布后 ~2.5min 识别并动作 |
| 分级自愈 L1→L2→L3 | ✅ 已实现 | L1 扩容 20%（受 maxReplicas 钳制）/ L2 删除不健康 Pod / L3 从 ReplicaSet 修订历史回滚镜像（改 App.spec，声明式收敛）。升级路径由 `status.lastHealLevel/lastHealAt` 驱动：冷却期内复发才升级；已到 L3 还在坏则进入观察。**真集群验证**：L2 摘除 → 复发 → 自动升 L3 → 回滚 → 2/2 恢复 |
| 金丝雀观察 + 自动回滚 | ✅ 已实现 | 镜像变更写入 `status.lastImageChangeAt` 开窗；窗口内 `ErrSource.ErrorRate` 超阈值即回滚并关窗（防回滚死循环）。**真集群验证**：50% 错误率版本发布后 ~20s 自动回滚，`status.rollbackRevision=1` |
| 冲突自愈的写路径 | ✅ 已实现 | 11.03 评审指出 L3 回滚撞过 `the object has been modified`：healer/scaler 全部 spec 写路径改 `RetryOnConflict + Merge Patch`（每次重试重新 Get 最新对象重算）；控制器层 conflict 静默退避不再发 Warning 事件。拦截器注入冲突的单测覆盖 |
| 策略参数 CRD 可配 | ✅ 已实现 | 11.03 评审指出魔法数字硬编码：L1 扩容比例（`health.scaleUpPercent`）、高频重启阈值（`health.restartThreshold`）、缩容步长（`scaling.scaleDownStepPercent`）、迟滞带（`scaling.hysteresisPercent`）、错误率扩容比例（`scaling.errorRateScaleUpPercent`）全部入 CRD 带默认值；旧对象零值回退原默认，行为兼容 |
| Admission Webhook | ✅ 已实现 | `internal/webhook/`：Mutating（replicas 跟随 minReplicas 的跨字段默认）+ Validating（禁 latest/无 tag 镜像、min≤max、replicas 落区间、指标 target 按类型可解析、同类型不重复、金丝雀阈值与窗口下限；聚合报错一次列全）。`make deploy-webhook`：overlay + `hack/webhook-certs.sh` 自签证书 + caBundle 注入，不依赖 cert-manager。**CI e2e 负向用例**：`:latest` 被拒、默认值注入生效 |
| mini-Informer | ✅ 已实现 | `pkg/miniinformer/`：手写 ListWatch → DeltaFIFO（按键压缩去重）→ Store → Handler + 控制器侧 WorkQueue（幂等+不漏），泛型实现约 400 行，11 个用例含断线重列；与 client-go 的取舍差异逐条写在包注释与 [README](../pkg/miniinformer/README.md) |
| 镜像签名与准入策略 | ✅ 已实现 | CI image job 用 cosign keyless 签 GHCR digest（GitHub OIDC → Fulcio → Rekor）并签完自验；`policies/kyverno-kubestasis.yaml`：禁 latest/无 tag + kubestasis-system 强制验签。CKS 考点变成可展示证据 |
| Helm Chart | 🗓 规划中 | `charts/` 不存在，当前安装形态只有 kustomize。**未完成就是未完成**，README 不把它写成能力 |
| 单元测试 | ✅ 113 用例 | scaler 46 + controller 11 + healer 24 + webhook 21 + miniinformer 11 = 113（用 `--- PASS` 行数按包实测，和 §二 同一口径）。envtest 仍未做——现在的控制循环回归靠 kind e2e |
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
| **控制面高可用** | 单副本 manager + leader election 未开启；个人项目的集群规模用不上，开了反而增加演示复杂度 | 打开 `--leader-elect`、副本数 ≥2、配合 PDB 与反亲和 |
| **伸缩决策的可观测性**（`kubestasis_scaling_decisions_total`、Grafana 面板） | 决策过程目前只在 Events 和日志里；导出指标是"锦上添花"，不影响正确性 | 接 controller-runtime 的 metrics registry + 一组 recording rules；工作量不大，排期在后 |
| **envtest 级别的控制循环回归** | 现在是 fake client 单测（快、但不跑真实 apiserver 默认值/校验）+ kind e2e（真、但慢且只覆盖三条链路）；中间层确实缺 | 引入 envtest（需要 etcd/kube-apiserver 二进制），把 CRD 默认值、webhook 联动、status 子资源这些"只有真 apiserver 才有的行为"纳入回归 |
| **升级/迁移路径**（CRD 版本演进、v1alpha1 → v1beta1） | 还没有真实用户，谈 conversion webhook 为时过早 | 定义 v1beta1 + conversion webhook + 存储版本迁移演练 |
| **安全加固的完整闭环**（NetworkPolicy、seccomp、RBAC 最小化审计） | 已有非 root、只读根文件系统、镜像签名校验；但没有逐条做 CIS Benchmark 对照 | 用 kube-bench / Kubescape 扫一遍，按报告逐项修 |
| **压测结论的普适性** | 见 [压测报告](benchmark/hpa-vs-kubestasis.md) 的"口径限定"一节：单节点、单应用、CPU 忙等模拟，结论只在同口径下成立 | 多节点、多应用混部、真实业务流量回放 |
| **缩容没有稳定窗口**（短促低谷会触发误缩容） | 压测 run5 抓到实证：突发#2 起点 loadgen 还没出流量（1m 窗口均值 30-50），控制器按信号把 4 缩到 3，13s 后又扩回 5。HPA 的 300s 稳定窗口就是防这个的，我们没做——加"信号需持续低于目标 N 秒才缩容"是语义级改动（新 CRD 字段 + 状态机），不想在参数没校准的情况下拍一个数 | 在 AppStatus 里记"连续低于目标的评估次数"，连续 ≥2 次才允许缩容；配合 envtest 回归 |



## 八、下一步（11.05 起）

1. ~~P1 指标源~~ / ~~P2 自愈~~ / ~~P3 Webhook~~ / ~~P4 mini-Informer~~ / ~~压测对比~~：**全部完成**（2026-11-03 ~ 11-04）
2. **第 3 周剩下的硬骨头**：
   - envtest 集成测试（控制循环级回归，补 fake client 与 kind e2e 之间的断层）
   - Helm Chart（kustomize 之外的第二种安装形态）
   - 决策指标导出 + Grafana 面板（让"为什么这次没扩"可被查询，而不是翻日志）
3. 可选增强（不承诺）：Kyverno 策略进 CI kind 集群实测、CRD v1beta1 + conversion

## 九、每日勾选

| 完成日 | 事项 | 状态变化 |
|---|---|---|
| 2026-11-02 | 体检 + STATUS/README 账本 | 建立基线，暴露红线 |
| 2026-11-02 夜 | P0 收尾（脚手架、Service、Status/Conditions、Events、CI） | 链路跑通：CR → Deploy+Service → Available |
| 2026-11-03 | P1 指标源 + 31 个单测 + 真集群端到端验证 + 仓库描述/topics + GitHub 主页 README | 伸缩引擎从"休眠"到"集群验证过"；0 测试 → 31 用例；修掉 omitempty 结构体序列化 bug |
| 2026-11-03 下午 | P1 真实指标源演示（真 Prometheus + 打流）+ P2 自愈引擎落地 + 26 个 healer 用例 | P1/P2 从"未验证"到"真集群跑通"；31 → 57 用例；踩到并修复"静态 Service 抓取导致 counter 非单调"的指标坑 |
| 2026-11-03 晚 | P3 Webhook + P4 mini-Informer + 评审三连修复（冲突写路径/魔法数字可配/CI 覆盖 P2）+ AI 推理信号 + cosign/Kyverno + 更名 KubeStasis | 57 → 103 用例；P0–P4 全闭环；真集群复现并修掉两个新坑（见下） |
| 2026-11-04 | 对照压测（HPA vs KubeStasis）+ CRD resources/env 透传 + webhook 校验配套 | 五轮压测，前四轮全部被自己推翻（详见压测报告）；修复渐进缩容跨步竞态（status 先于 spec 写入）；报告与原始 CSV 入库；103 → 113 用例 |

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
