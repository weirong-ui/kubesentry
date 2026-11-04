# KubeStasis Roadmap

> 与大三下学期 + 秋招时间线对齐，每个里程碑都是一次简历更新点 + 一篇掘金文章素材。

## P0 — MVP：让 Operator 转起来（已完成 2026-11-02）
- [x] Go 模块 + controller-gen 代码生成，App CRD / DeepCopy / RBAC 落地
- [x] Reconcile 循环：App → Deployment/Service 的 CreateOrUpdate
- [x] Status 反映真实就绪副本 + Available/Progressing Conditions + Events 留痕
- [x] Finalizer 处理删除逻辑
- [ ] envtest 集成测试跑通第一个用例（顺延到第 3 周）
- [x] GitHub Actions：gofmt + go vet + go build + go test + kind e2e
- 📦 产出：GitHub 仓库可运行、掘金文章《从零写一个 K8s Operator》

## P1 — 智能伸缩（核心已完成 2026-11-03）
- [x] 对接 Prometheus HTTP API 查询 QPS / P99 / 错误率（`internal/scaler/prometheus.go`）
- [x] 多信号伸缩决策 + 冷却窗口 + 迟滞带 + 缩容渐进（真集群：500 QPS 扩 1→5，停流缩 5→1）
- [x] AI 推理信号扩展：QueueDepth / TTFT / KVCacheUsage（默认 vLLM 指标名，WithQuery 可覆盖，2026-11-03 晚）
- [x] 决策过程可观测：Prometheus metrics（`kubestasis_scaling_decisions_total` + 3 个 gauge，2026-11-04 晚）；Grafana 面板仍未做
- 📦 产出：演示实录 `docs/demo/01-scaling.md`、文章《为什么 HPA 会震荡：手写伸缩决策引擎》

## P2 — 故障自愈（核心已完成 2026-11-03）
- [x] 故障识别：CrashLoopBackOff / OOMKilled / 高频重启兜底（按 Pod containerStatuses）
- [x] 分级自愈状态机：L1 扩容 → L2 摘除坏 Pod → L3 按 ReplicaSet 修订历史回滚镜像
- [x] 金丝雀观察窗口 + 版本自动回滚（错误率超阈值 ~20s 内自动回滚）
- [x] P2 自愈链路 CI 化（kind e2e：crash 镜像 → L2→L3 自动回滚断言，2026-11-03）
- [ ] kind 集群混沌验证脚本（随机杀 Pod，第 3 周）
- 📦 产出：演示实录 `docs/demo/02-self-healing.md`、文章《给 K8s 加一个"自动值班员"：分级自愈引擎设计》

## P3 — Admission Webhook（已完成 2026-11-03）
- [x] Validating Webhook：禁 latest/无 tag、min≤max、replicas 落区间、指标 target 按类型解析、金丝雀阈值与窗口下限，聚合报错
- [x] Mutating Webhook：跨字段默认值（replicas 跟随 minReplicas）——踩到并修复"CRD schema 默认值先于 Mutating Webhook 落地"的顺序坑
- [x] 证书方案：hack/webhook-certs.sh 自签 + caBundle 注入，不依赖 cert-manager
- [x] 超额完成：CI image job cosign keyless 签名 GHCR digest + 签完自验；policies/ Kyverno 策略（禁 latest + 验签）
- 📦 产出：文章《K8s 准入控制实战：从 Kube-apiserver 到你的 Webhook》（素材含上面两个真实坑）

## P4 — 手写 mini-Informer（已完成 2026-11-03）
- [x] 不依赖 informer 库，实现 ListWatch → DeltaFIFO → Store → EventHandler → WorkQueue（泛型，约 400 行）
- [x] 与 client-go 的取舍差异逐条记录（pkg/miniinformer/README.md），11 个用例含断线重列
- 📦 产出：`pkg/miniinformer` + 掘金深度文《图解 client-go Informer：手写一个才敢说懂》（这篇有爆款潜质）

## 远景（不承诺，不在 11 月–秋招计划内）

以下两项对个人项目来说投入产出比不划算，从里程碑里移除，仅作方向记录：
- eBPF 无侵入指标采集（cilium/ebpf）
- 多集群容灾调度

秋招前有余力才做的事：
- [ ] KubeCon 形式的英文 README + release v1.0 打 tag

## 时间线总览

| 时间 | 里程碑 | 简历动作 |
|---|---|---|
| 2026.11.02–03 | P0–P4 全部闭环（进度大幅提前） | 简历一次到位：弹性伸缩 + 自愈 + 准入控制 + 手写 Informer |
| 2026.11.04 | ✅ 压测对比表（[报告](benchmark/hpa-vs-kubestasis.md)，五轮迭代 + 原始 CSV 入库） | 把"比 HPA 怎样"从定性论证变成可复算的证据——包括输的项 |
| 2026.11.04 晚 | ✅ 探针 / 缩容稳定窗口 / Helm Chart / 决策指标 | v0.1 前的最后四块拼图；`Available` 语义修正、误缩容缺口闭合 |
| 2026.11–12 | envtest + Grafana 面板 + 系列文章补齐 | 补控制循环级回归与现成面板；系列文章完结 |
| 2027.01–03 | 掘金系列完结 + 演示视频 | 补充"掘金系列 x N 篇" |
| 2027.04–05 | 面试题库整理 | 简历定稿，秋招冲刺 |
