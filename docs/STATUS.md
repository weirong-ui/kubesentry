# KubeSentry 实现状态（真实账本）

> 本文件是**唯一可信的能力清单**。README 的一切对外描述以此为准，每完成一项就把状态改掉。
> 体检时间：2026-11-02 晚 ｜ 体检方式：`curl` 拉取 main 分支全量 → 逐文件对读 → `go build ./...`
> 结论一句话：**当前仓库是「设计完整 + 骨架可读」的代码，不是可运行程序。P0 未完成。**

## 一、体检结论

| 检查项 | 结果 | 实测输出 |
|---|---|---|
| `git clone` | ✅ | 可克隆（Windows 下需 `git -c http.schannelCheckRevoke=false`，本机 schannel 吊销检查失败） |
| `go build ./...` | ❌ | `pattern ./...: directory prefix . does not contain main module or its selected dependencies` |
| `make test` | ❌ | 依赖 `manifests generate` + `go test`，因无 `go.mod` 第一步即失败 |
| 单元测试 | ❌ | `*_test.go` 数量 = 0 |
| CI 状态 | ❌ | **整份 workflow 从未产生过任何 job**。run `41439643551` 显示 `conclusion=failure` 但 `jobs total_count=0`，`run_started_at == updated_at`（耗时 0 秒）——这是 YAML/表达式校验失败的典型特征，不是某条命令失败 |

**根因（一个）**：仓库从未执行过 `kubebuilder init`，缺少 Go 模块与代码生成产物。

```
缺失：go.mod、go.sum、api/v1alpha1/zz_generated.deepcopy.go、
      config/crd/bases/、config/rbac/、config/default/、
      pkg/miniinformer/、internal/webhook/、charts/kubesentry/
```

### 附带发现：CI 徽章从建立那天起就是红的

`.github/workflows/ci.yml` 第 31、45、63 行把 `if: ${{ hashFiles('go.mod') != '' }}` 写在了 **job 级**。
`hashFiles()` 只在 step 上下文可用，job 级无法求值 → 整份 workflow 判定为非法文件 → **连 `docs` job 都不会被创建**。
所以仓库首页的 CI badge 不是"暂时失败"，而是从来就没跑过；把它当成"CI 已接入"写进简历是站不住的。

修复方式（P0 一并做，不要只在本地改）：

```yaml
  lint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - name: Gate
        id: gate
        run: |
          if [ -f go.mod ]; then echo "ready=true" >> "$GITHUB_OUTPUT"
          else echo "ready=false" >> "$GITHUB_OUTPUT"; echo "::notice::go.mod 缺失，跳过 Go 检查"; fi
      - uses: actions/setup-go@v5
        if: steps.gate.outputs.ready == 'true'
        with: { go-version: "1.22", cache: true }
      # 后续 lint / test 步骤同样加 if: steps.gate.outputs.ready == 'true'
```

## 二、三栏表：能力 / 真实状态 / 缺口

状态含义：`✅ 已实现`（有代码且有验证路径）｜`🚧 半成品`（有代码但链路断）｜`🗓 规划中`（只有注释/命名）

| 能力 | 真实状态 | 证据与缺口 |
|---|---|---|
| App CRD 类型定义 | 🚧 半成品 | `api/v1alpha1/app_types.go` 类型与 kubebuilder marker 写得完整（QPS/P99/ErrorRate、冷却、金丝雀字段齐全）。缺口：无 `zz_generated.deepcopy.go`、无 `config/crd/bases/*.yaml`，`kubectl apply -f config/crd/bases` 直接报路径不存在 |
| 基础 Reconcile（App → Deployment） | 🚧 半成品 | `internal/controller/app_controller.go:162` `ensureDeployment` 用 `CreateOrUpdate`，字段对齐逻辑正确，`SetControllerReference` 已设 OwnerReference。缺口：编译不过（无 go.mod），未在集群验证 |
| Service 创建 | 🗓 规划中 | README 架构图与快速开始承诺 "Deployment & Service"，代码里**只有 Deployment**，全仓无 `corev1.Service` 构造 |
| Status conditions | 🚧 半成品 | 结构体有 `Conditions []metav1.Condition`，`patchStatus` 只写 `Phase` / `CurrentReplicas` / `LastHealAction`，**从不写 conditions** |
| Status 真实副本数 | ❌ 有 bug | `app_controller.go:195` `CurrentReplicas = *app.Spec.Replicas`——读的是期望值不是 Deployment 实际状态，`kubectl get app` 显示的数字恒等于 spec，会误导 |
| Kubernetes Events 审计 | 🗓 规划中 | `app_controller.go:205` `emitEvent` 是空函数（`// TODO(P0): r.Recorder.Event(...)`），`Recorder` 未注入。README 承诺的 `--field-selector reason=KubeSentryScaleUp` 查不到任何事件 |
| Finalizer 删除时序 | ✅ 已实现 | `app_controller.go:90-108` 添加/移除逻辑正确，但删前导出审计是 TODO |
| Prometheus 指标查询 | 🗓 规划中 | **全仓无 Prometheus client**，无 `prometheus.go`，`go.mod` 也不存在。`healer.Engine` 的 promClient 字段只是注释 |
| 多信号伸缩决策 | ❌ 逻辑空转 | `internal/scaler/engine.go:82-87`：`signals[m.Type] = current`，即每个信号都等于当前副本数 → `desired == current` → **`Decide()` 永远返回 None，永不伸缩** |
| 冷却窗口 | 🚧 半成品 | `inCooldown()` 算法正确，但 `Status.LastScalingAt` 全仓无写入点（`Apply` 是空实现）→ 冷却判断恒为 false，等于没有 |
| 迟滞带 + 缩容渐进 | ✅ 已实现 | `engine.go:97-122` 纯函数逻辑完整，含"缩到 minReplicas 不受迟滞约束""`ceil(0.2×current)` 会缩不动"两个真实坑的处理。缺口：0 个单测（这正是 P1 要补的） |
| Scaler 落地 | 🗓 规划中 | `engine.go:144` `Apply(...) error { return nil }`，签名用 `interface{}` 占位 → 决策不会写进 Deployment |
| 故障识别（CrashLoop / OOM / 5xx） | 🗓 规划中 | `healer/engine.go:83` `Inspect` 直接 `return Report{Unhealthy: false}`，无 Pod List、无 `containerStatuses` 读取 |
| 分级自愈 L1→L2→L3 | 🗓 规划中 | `healer/engine.go:105` `Apply` 返回 `nil, nil` |
| 金丝雀观察 + 自动回滚 | 🗓 规划中 | `InCanaryWindow` 恒 `false`（`engine.go:95`）、`CheckCanary` 恒 `false`、`Rollback` 返回 `rollback: not implemented (P2)` |
| Admission Webhook | 🗓 规划中 | `internal/webhook/` **目录不存在**；`cmd/main.go` 未注册任何 webhook；无证书、无 `config/webhook/` |
| mini-Informer | 🗓 规划中 | `pkg/miniinformer/` **目录不存在**；`Dockerfile` 的 `COPY pkg/ pkg/` 会因此构建失败（提前暴露了） |
| Helm Chart | 🗓 规划中 | `charts/kubesentry/` 不存在，README 项目结构图里写了 |
| CI 流水线 | ❌ 从未运行 | `.github/workflows/ci.yml:31,45,63` job 级 `if: hashFiles(...)` 非法 → 整份 workflow 校验失败，`docs` / `lint` / `test` / `e2e` / `image` **一个 job 都没创建过**，首页 badge 自建立起一直是红的 |
| 单元 / envtest 测试 | 🗓 规划中 | 0 个 `_test.go`，`Makefile` 的 `test` 目标设计完整但跑不起来 |
| 镜像构建 | 🚧 半成品 | `Dockerfile` 是标准 kubebuilder + distroless 多阶段（思路对），但 `COPY go.mod go.sum`、`COPY pkg/` 均指向不存在的文件 |
| 文档资产 | ✅ 已实现 | `README` / `docs/DESIGN.md` / `docs/ROADMAP.md` / `docs/assets/architecture.png` / `juejin/` 4 篇规划与草稿齐备——**这是当前仓库最有价值的部分** |
| `cmd/main.go` 可编译性 | ❌ 有 bug | `Metrics: ctrl.Options{}.Metrics` 是非法写法（controller-runtime v0.19 中该字段为 `metricsserver.Options`，不能自引用）；`Healer` 字段未注入，`r.Healer` 为 nil——当前靠"方法恰好不解引用 e"侥幸不 panic，一旦 `Inspect` 真正实现就会 nil panic |

**统计**：23 项能力中 `✅ 3 项`（Finalizer 时序、迟滞/缩容算法、文档资产）、`🚧 5 项`、`🗓 11 项`、`❌ 4 项`（3 个确凿缺陷 + CI 从未运行）。

## 三、这件事的严重性（写给未来的自己）

- 面试官 `git clone` 后第一条命令是 `go build ./...` → 立即失败 → 判定"包装"，比没有项目更差。
- README 的对比表把"自愈""金丝雀回滚""Webhook"都打了 ✅ → 诱使面试官追问 → 一问"你的 CrashLoop 是怎么识别 waiting.reason 的"就穿帮。
- 好消息：**骨架质量不差**。`scaler` 的两个坑（迟滞带要放行回落 min、`ceil` 缩容陷阱）是真踩过才写得出的，`healer` 的分级设计思路清楚。缺的是"接线"，不是"想法"。
- 因此 11 月的策略从"写新功能"改为"把已有的接上、跑通、录下来"。

## 四、P0 收尾清单（11.03–11.04，严格按顺序）

前 4 条是取消红线的**前置条件**，不做完不要碰任何业务代码。

1. [ ] `kubebuilder init --domain sentry.io --repo github.com/weirong-ui/kubesentry` → 产出 `go.mod` / `Makefile` / `config/{default,manager,rbac}`；再用现有文件覆盖，标记以现有版本为准
2. [ ] `kubebuilder create api --group apps --version v1alpha1 --kind App --resource --controller` → 产出 `zz_generated.deepcopy.go`（此文件必须进版本库）
3. [ ] `make manifests generate` → 产出 `config/crd/bases/apps.sentry.io_apps.yaml`；`kubectl apply` 成功
4. [ ] 修 `cmd/main.go`：`Metrics` 改为 `metricsserver.Options{BindAddress: metricsAddr}`；注入 `Healer: healer.NewEngine()`；`Recorder: mgr.GetEventRecorderFor("kubesentry")`
5. [ ] 补 Service 创建（`ensureService`，ClusterIP + 端口映射 `spec.port`），或把 README 架构图里的 "& Service" 删掉——**二选一，不要留空承诺**
6. [ ] 修 `Status.CurrentReplicas`：改读 `deploy.Status.ReadyReplicas`（或 `AvailableReplicas`），与 spec 分离
7. [ ] 写 `Conditions`：`Ready`（Deployment 可用）/ `Progressing`，`meta.SetStatusCondition`
8. [ ] `emitEvent` 接 `Recorder`，先只做 ScaleUp / ScaleDown 两个 reason，验证 `kubectl get events` 能查到
9. [ ] 修 CI：把 job 级 `hashFiles` 改成 step 级 gate（见上文），push 后确认 workflow **真的创建出 job**——`docs` 能绿、其余显示 skipped 才算修好

**验收**：`kubectl apply -f config/samples/app_v1alpha1_app.yaml` → `kubectl get deploy,svc -l app.kubernetes.io/managed-by=kubesentry` 有输出 → `kubectl get app sample -o jsonpath='{.status.phase}'` 输出 `Stable`。

## 五、之后的每日勾选

| 完成日 | 事项 | 状态变化 |
|---|---|---|
| 2026-11-02 | 体检 + 本文件 + README「实现状态」段 | 建立基线（本次） |
