# KubeStasis 实现状态（真实账本）

> 本文件是**唯一可信的能力清单**。README 的一切对外描述以此为准，每完成一项就把状态改掉。
> 最后更新：2026-11-03 晚（P3 Admission Webhook 落地 + 评审意见修复：冲突写路径、魔法数字可配、CI 覆盖 P2）
> 验收口径：代码能在干净集群上 `deploy → apply App → Available`，且有可复现的证据。
> ⚠️ 项目原名 KubeSentry，2026-11-03 更名 KubeStasis（避免与既有项目重名）；CRD group `apps.sentry.io` 保持不变。

## 一、当前结论

**P0/P1/P2/P3 已闭环**：`apply CR → Deployment + Service → Available` 之外，三条"智能"链路都在真集群上跑通了——
**弹性伸缩**（真实 Prometheus 按 Pod 抓取 + 500 QPS 打流 → 扩容 1→5 → 停流渐进缩回 1，见 [docs/demo/01-scaling.md](demo/01-scaling.md)）、
**分级自愈**（CrashLoop 版本发布 → L2 摘除 → 冷却期内复发自动升级 L3 回滚 → 恢复；金丝雀窗口内错误率超阈值 → 20 秒内自动回滚，见 [docs/demo/02-self-healing.md](demo/02-self-healing.md)）、
**准入控制**（Mutating 默认值注入 + Validating 准入校验，自签证书方案不依赖 cert-manager，`make deploy-webhook` 一键起）。
**P2 自愈链路已进入 CI e2e**（kind 集群里真跑 crash 镜像 → 自动回滚断言），自愈能力不再依赖人工实测。
尚未实现的是 P4（mini-Informer）。

## 二、体检与验证结果（2026-11-02 实测，11-03 晚更新）

| 检查项 | 结果 | 实测输出 |
|---|---|---|
| `go build ./...` | ✅ | 通过（Go 1.27.3 本机，module `go 1.22.0`；k8s v0.31.1 / controller-runtime v0.19.0） |
| `go vet ./...` | ✅ | 无输出 |
| `gofmt -l` | ✅ | 无未格式化文件 |
| `go test ./...` | ✅ | **79 个用例**全绿（scaler 27 + controller 8 + healer 30 + webhook 14） |
| 部署链路 | ✅ | Docker Desktop k8s v1.36.1：`kubectl apply` CRD+RBAC+Manager → 控制器 Pod `1/1 Running` |
| 创建资源 | ✅ | `deployment.apps/sample 2/2`、`service/sample ClusterIP 80/TCP`、`endpoints 10.244.0.9,10.244.0.10` |
| 状态真实性 | ✅ | `NAME sample PHASE Stable READY 2 DESIRED 2`；`status.conditions` 出现 `Available=True / Progressing=False` |
| Events | ✅ | `kubectl get events --field-selector reason=KubeStasisReconcile` → `app available: 2/2 replicas ready` |
| 控制器日志 | ✅ | `ERROR` 计数 = 0 |
| CI | ✅ | 4 个 job 全绿：`docs` / `build`（gofmt+vet+build+test）/ `e2e` / `image`（多架构推送 GHCR）。run [41445360610](https://github.com/weirong-ui/kubestasis/actions/runs/41445360610)，耗时 14m52s。11-03 晚起 e2e 覆盖扩到三条链路：**P0**（部署+Available）+ **P3**（webhook 拒 `:latest`、replicas 跟随 minReplicas 默认值）+ **P2**（crash 镜像 → L2→L3 自动回滚断言） |

## 三、三栏表：能力 / 真实状态 / 证据与缺口

状态含义：`✅ 已实现`（有代码且集群验证过）｜`🚧 半成品`（有代码但链路未验证/部分）｜`🗓 规划中`

| 能力 | 真实状态 | 证据与缺口 |
|---|---|---|
| App CRD + 代码生成 | ✅ 已实现 | `config/crd/bases/apps.sentry.io_apps.yaml` 由 controller-gen v0.16.5 生成；`zz_generated.deepcopy.go` 已入库 |
| Reconcile（App → Deployment） | ✅ 已实现 | `ensureDeployment` 幂等 CreateOrUpdate + `SetControllerReference`；集群验证 2/2 就绪 |
| Service 自动创建 | ✅ 已实现 | `ensureService`（ClusterIP，port/targetPort 同源）；Endpoints 有 2 个后端 IP |
| 托管资源标签 | ✅ 已实现 | Deployment/Service 均带 `app.kubernetes.io/managed-by=kubestasis`，README 的 `-l` 查询可直接用 |
| Status 真实副本数 | ✅ 已实现 | `CurrentReplicas` 取自 `deploy.Status.ReadyReplicas`，`DesiredReplicas` 取自 spec，两者分离 |
| Status Conditions | ✅ 已实现 | `Available` / `Progressing`，用 `meta.SetStatusCondition` 维护 `LastTransitionTime` 与去重 |
| Kubernetes Events | ✅ 已实现 | `Recorder` 已注入；`KubeStasisScaleUp/Down/Heal/CanaryFailed/Error/Reconcile` 六类 reason；首次可用时发事件 |
| Finalizer 删除时序 | ✅ 已实现 | 添加/移除正确；删前导出审计是未排期的增强项，不在 P0–P4 承诺内 |
| 迟滞带 + 缩容渐进算法 | ✅ 已实现 | 纯函数逻辑完整（含"缩回 minReplicas 不受迟滞约束""`ceil(0.2×current)` 缩不动"两个坑） |
| 冷却窗口 | ✅ 已实现 | `LastScalingAt` 在 decision≠None 时写入；逻辑已被单测覆盖（冷却拦缩容、放行扩容、过期放行） |
| Prometheus 多信号伸缩 | ✅ 已实现 | `internal/scaler/prometheus.go`：QPS（`rate` 换算副本）、P99（`histogram_quantile` 按比例扩）、错误率（超阈值扩 20%）；`--prometheus-url`/`--prometheus-window` 注入，不配则休眠。**真实指标源端到端验证**（2026-11-03：集群内真实 Prometheus v2.54.1 按 Pod 抓取 + demoapp 打流 500 QPS → 扩 1→5，停流后渐进缩 5→1，全程 Events 留痕，实录见 [docs/demo/01-scaling.md](demo/01-scaling.md)） |
| 故障识别（CrashLoop / OOM / 高频重启） | ✅ 已实现 | `Inspect` 按 Pod 标签列出实例，读 `containerStatuses[].state.waiting.reason` / `lastState.terminated.reason` / restartCount 兜底；有意忽略"别的 App 的 Pod"与"正在删除的 Pod"。**真集群验证**：crash 镜像发布后 ~2.5min 识别并动作 |
| 分级自愈 L1→L2→L3 | ✅ 已实现 | L1 扩容 20%（受 maxReplicas 钳制）/ L2 删除不健康 Pod / L3 从 ReplicaSet 修订历史回滚镜像（改 App.spec，声明式收敛）。升级路径由 `status.lastHealLevel/lastHealAt` 驱动：冷却期内复发才升级；已到 L3 还在坏则进入观察。**真集群验证**：L2 摘除 → 复发 → 自动升 L3 → 回滚 → 2/2 恢复 |
| 金丝雀观察 + 自动回滚 | ✅ 已实现 | 镜像变更写入 `status.lastImageChangeAt` 开窗；窗口内 `ErrSource.ErrorRate` 超阈值即回滚并关窗（防回滚死循环）。**真集群验证**：50% 错误率版本发布后 ~20s 自动回滚，`status.rollbackRevision=1` |
| 冲突自愈的写路径 | ✅ 已实现 | 11.03 评审指出 L3 回滚撞过 `the object has been modified`：healer/scaler 全部 spec 写路径改 `RetryOnConflict + Merge Patch`（每次重试重新 Get 最新对象重算）；控制器层 conflict 静默退避不再发 Warning 事件。拦截器注入冲突的单测覆盖 |
| 策略参数 CRD 可配 | ✅ 已实现 | 11.03 评审指出魔法数字硬编码：L1 扩容比例（`health.scaleUpPercent`）、高频重启阈值（`health.restartThreshold`）、缩容步长（`scaling.scaleDownStepPercent`）、迟滞带（`scaling.hysteresisPercent`）、错误率扩容比例（`scaling.errorRateScaleUpPercent`）全部入 CRD 带默认值；旧对象零值回退原默认，行为兼容 |
| Admission Webhook | ✅ 已实现 | `internal/webhook/`：Mutating（replicas 跟随 minReplicas 的跨字段默认）+ Validating（禁 latest/无 tag 镜像、min≤max、replicas 落区间、指标 target 按类型可解析、同类型不重复、金丝雀阈值与窗口下限；聚合报错一次列全）。`make deploy-webhook`：overlay + `hack/webhook-certs.sh` 自签证书 + caBundle 注入，不依赖 cert-manager。**CI e2e 负向用例**：`:latest` 被拒、默认值注入生效 |
| mini-Informer | 🗓 规划中 | `pkg/miniinformer/` 不存在，P4（Dockerfile 已预留注释位） |
| Helm Chart | 🗓 规划中 | `charts/` 不存在，第 3 周在 kustomize 之后再做 |
| 单元测试 | ✅ 79 用例 | scaler 27 + controller 8 + healer 30 + webhook 14（新增：冲突重试吸收、可配参数生效、webhook 全规则正反例）。envtest 待第 3 周 |
| 压测与 HPA 对比表 | 🗓 规划中 | 第 3 周——这是证明"迟滞算法优于原生 HPA"的唯一硬证据，优先级最高 |

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

## 七、下一步（11.05 起）

1. ~~P1 指标源~~：**已完成**（2026-11-03），真实 Prometheus + 打流验证，实录 `docs/demo/01-scaling.md`
2. ~~P2 自愈~~：**已完成**（2026-11-03），分级自愈 + 金丝雀回滚，实录 `docs/demo/02-self-healing.md`
3. ~~单测~~：**已完成**（2026-11-03），57 个用例
4. **P3 Admission Webhook**：校验（禁 latest、min≤max）+ 默认值注入（资源限制、安全上下文）——呼应 CKS 考点
5. **P4 mini-Informer**：ListWatch + DeltaFIFO + WorkQueue 手写实现（面试高频）
6. **第 3 周**：envtest、Helm Chart、压测对比表、把 P2 链路搬进 CI e2e

## 八、每日勾选

| 完成日 | 事项 | 状态变化 |
|---|---|---|
| 2026-11-02 | 体检 + STATUS/README 账本 | 建立基线，暴露红线 |
| 2026-11-02 夜 | P0 收尾（脚手架、Service、Status/Conditions、Events、CI） | 链路跑通：CR → Deploy+Service → Available |
| 2026-11-03 | P1 指标源 + 31 个单测 + 真集群端到端验证 + 仓库描述/topics + GitHub 主页 README | 伸缩引擎从"休眠"到"集群验证过"；0 测试 → 31 用例；修掉 omitempty 结构体序列化 bug |
| 2026-11-03 下午 | P1 真实指标源演示（真 Prometheus + 打流）+ P2 自愈引擎落地 + 26 个 healer 用例 | P1/P2 从"未验证"到"真集群跑通"；31 → 57 用例；踩到并修复"静态 Service 抓取导致 counter 非单调"的指标坑 |
