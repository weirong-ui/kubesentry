# KubeSentry 实现状态（真实账本）

> 本文件是**唯一可信的能力清单**。README 的一切对外描述以此为准，每完成一项就把状态改掉。
> 最后更新：2026-11-02 深夜（P0 收尾完成）
> 验收口径：代码能在干净集群上 `deploy → apply App → Available`，且有可复现的证据。

## 一、当前结论

**P0 已闭环**：一条干净链路跑通了 —— `kubectl apply CR → 自动创建 Deployment + Service → status 反映真实就绪副本 → 写 Events`。
尚未实现的是 P1 之后的**智能**部分（Prometheus 多信号伸缩、自愈、Webhook、mini-Informer），它们仍在下方三栏表里标 🗓。

## 二、体检与验证结果（2026-11-02 实测）

| 检查项 | 结果 | 实测输出 |
|---|---|---|
| `go build ./...` | ✅ | 通过（Go 1.27.3 本机，module `go 1.22.0`；k8s v0.31.1 / controller-runtime v0.19.0） |
| `go vet ./...` | ✅ | 无输出 |
| `gofmt -l` | ✅ | 无未格式化文件 |
| `go test ./...` | ⚠️ | 通过但**用例数为 0**，等于没有测试（第 3 周补） |
| 部署链路 | ✅ | Docker Desktop k8s v1.36.1：`kubectl apply` CRD+RBAC+Manager → 控制器 Pod `1/1 Running` |
| 创建资源 | ✅ | `deployment.apps/sample 2/2`、`service/sample ClusterIP 80/TCP`、`endpoints 10.244.0.9,10.244.0.10` |
| 状态真实性 | ✅ | `NAME sample PHASE Stable READY 2 DESIRED 2`；`status.conditions` 出现 `Available=True / Progressing=False` |
| Events | ✅ | `kubectl get events --field-selector reason=KubeSentryReconcile` → `app available: 2/2 replicas ready` |
| 控制器日志 | ✅ | `ERROR` 计数 = 0 |
| CI | ✅ | 4 个 job 全绿：`docs` / `build`（gofmt+vet+build+test）/ `e2e`（kind 里真部署 + `wait --for=condition=Available`）/ `image`（多架构推送 GHCR）。run [41445360610](https://github.com/weirong-ui/kubesentry/actions/runs/41445360610)，耗时 14m52s |

## 三、三栏表：能力 / 真实状态 / 证据与缺口

状态含义：`✅ 已实现`（有代码且集群验证过）｜`🚧 半成品`（有代码但链路未验证/部分）｜`🗓 规划中`

| 能力 | 真实状态 | 证据与缺口 |
|---|---|---|
| App CRD + 代码生成 | ✅ 已实现 | `config/crd/bases/apps.sentry.io_apps.yaml` 由 controller-gen v0.16.5 生成；`zz_generated.deepcopy.go` 已入库 |
| Reconcile（App → Deployment） | ✅ 已实现 | `ensureDeployment` 幂等 CreateOrUpdate + `SetControllerReference`；集群验证 2/2 就绪 |
| Service 自动创建 | ✅ 已实现 | `ensureService`（ClusterIP，port/targetPort 同源）；Endpoints 有 2 个后端 IP |
| 托管资源标签 | ✅ 已实现 | Deployment/Service 均带 `app.kubernetes.io/managed-by=kubesentry`，README 的 `-l` 查询可直接用 |
| Status 真实副本数 | ✅ 已实现 | `CurrentReplicas` 取自 `deploy.Status.ReadyReplicas`，`DesiredReplicas` 取自 spec，两者分离 |
| Status Conditions | ✅ 已实现 | `Available` / `Progressing`，用 `meta.SetStatusCondition` 维护 `LastTransitionTime` 与去重 |
| Kubernetes Events | ✅ 已实现 | `Recorder` 已注入；`KubeSentryScaleUp/Down/Heal/CanaryFailed/Error/Reconcile` 六类 reason；首次可用时发事件 |
| Finalizer 删除时序 | ✅ 已实现 | 添加/移除正确；删前导出审计仍是 TODO(P2) |
| 迟滞带 + 缩容渐进算法 | ✅ 已实现 | 纯函数逻辑完整（含"缩回 minReplicas 不受迟滞约束""`ceil(0.2×current)` 缩不动"两个坑） |
| 冷却窗口 | ✅ 已实现 | `LastScalingAt` 在 decision≠None 时写入；逻辑已被单测覆盖（冷却拦缩容、放行扩容、过期放行） |
| Prometheus 多信号伸缩 | ✅ 已实现 | `internal/scaler/prometheus.go`：QPS（`rate` 换算副本）、P99（`histogram_quantile` 按比例扩）、错误率（超阈值扩 20%）；`--prometheus-url` 注入，不配则休眠。**集群端到端演示待 11.05（kube-prometheus-stack + hey 打流）** |
| 故障识别（CrashLoop / OOM / 5xx） | 🗓 规划中 | `Inspect` 仍返回 `Unhealthy:false`，P2 |
| 分级自愈 L1→L2→L3 | 🗓 规划中 | `Apply` 仍返回 `nil,nil`，P2 |
| 金丝雀观察 + 自动回滚 | 🗓 规划中 | `InCanaryWindow` / `CheckCanary` 恒 false，`Rollback` 返回 not implemented，P2 |
| Admission Webhook | 🗓 规划中 | `internal/webhook/` 不存在，`cmd/main.go` 未注册，P3 |
| mini-Informer | 🗓 规划中 | `pkg/miniinformer/` 不存在，P4（Dockerfile 已预留注释位） |
| Helm Chart | 🗓 规划中 | `charts/` 不存在，第 3 周在 kustomize 之后再做 |
| 单元 / envtest 测试 | 🚧 半成品 | **28 个用例**（scaler 20 + controller 8）全部通过：迟滞带 / 冷却 / ceil 缩容渐进 / 上下限 / 多信号取 max / 无数据不当 0 / status 读真实副本 / Finalizer 生命周期 / selector-label 一致性。envtest 待第 3 周 |
| 压测与 HPA 对比表 | 🗓 规划中 | 第 3 周 |

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
11. `emitEvent` 接 `Recorder`；首次 `Available=True` 时发 `KubeSentryReconcile` 事件
12. `scaler.Apply` 去掉 `interface{}` 占位，改为**声明式回写 `spec.replicas`**（不直接改 Deployment，避免与 `ensureDeployment` 打架）
13. `scaler.Decide` 在无指标源时明确返回 `no metrics source configured (P1)`，不再用"当前副本数"冒充信号

**发现的原有缺陷（已修）**

14. `+kubebuilder:default={}` 打在 `Scaling` / `Health` / `Canary` 结构体上会生成非法 CRD（`default: {}` 缺少必填子字段），导致 `kubectl apply` CRD 直接被拒 —— 改为字段级 default + `+optional`
15. `status.currentReplicas` 若设为 required，值为 0 时 merge patch 不带该字段会报 `Required value` —— 保持 optional

**CI**

16. 重写 `.github/workflows/ci.yml`：条件判断从 job 级 `if` 移到 step 级（原写法使整份 workflow 校验失败，一个 job 都没创建过）；新增 `build`（gofmt/vet/build/test）与真实 `e2e`（kind + 构建镜像 + `make deploy` + `kubectl wait --for=condition=Available` + 校验 events）
17. 首次跑通全绿：4/4 job success，徽章由红转绿

## 五、环境备忘（下次别再踩）

| 现象 | 处理 |
|---|---|
| `git clone` 报 `CRYPT_E_NO_REVOCATION_CHECK` | `GIT_SSL_NO_VERIFY=1 git clone ...`；curl 加 `--ssl-no-revoke` |
| `proxy.golang.org` 连接被重置/502 | `go env -w GOPROXY=https://goproxy.cn,direct` |
| `docker build` 拉不到 `gcr.io/distroless/*` | `--build-arg RUNTIME_IMAGE=m.daocloud.io/gcr.io/distroless/static-debian12:nonroot` |
| `docker build` 内 `go mod download` 卡住 | `--build-arg GOPROXY=https://goproxy.cn,direct` |
| 本机没有 `make` | 直接跑 Makefile 里的等价命令；CI（Linux）里 `make` 正常 |
| 同 tag 重新构建后集群仍跑旧镜像 | 换 tag（`e2e2`/`e2e3`）或 `imagePullPolicy: Always`；containerd 会缓存同名 tag |
| Docker Desktop 集群拉 `nginx:1.27-alpine` 报 registry-mirror 500 | 先在本机 `docker pull` 预热镜像，再让集群重试 |

## 六、下一步（11.05 起）

1. **P1 端到端演示**：kind 里装 kube-prometheus-stack + hey 打流，观察副本上升，并验证"冷却窗口内不缩容"（代码已就绪，缺的是演示环境）
2. ~~P1 指标源~~：**已完成**（2026-11-03），见 `internal/scaler/prometheus.go`
3. ~~单测~~：**已完成**（2026-11-03），28 个用例
4. **P2 自愈**：`Inspect` 读 `containerStatuses[].state.waiting.reason == CrashLoopBackOff` 与 `lastState.terminated.reason == OOMKilled`
5. **第 3 周**：envtest、Helm Chart、压测对比表

## 七、每日勾选

| 完成日 | 事项 | 状态变化 |
|---|---|---|
| 2026-11-02 | 体检 + STATUS/README 账本 | 建立基线，暴露红线 |
| 2026-11-02 夜 | P0 收尾（脚手架、Service、Status/Conditions、Events、CI） | 链路跑通：CR → Deploy+Service → Available |
| 2026-11-03 | P1 指标源 + 28 个单测 + 仓库描述/topics + GitHub 主页 README | 伸缩引擎从"休眠"到"可用"；0 测试 → 28 用例 |
