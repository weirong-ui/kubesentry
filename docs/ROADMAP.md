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
- [ ] 决策过程可观测：Prometheus metrics（`kubestasis_scaling_decisions_total`）+ Grafana 面板
- 📦 产出：演示实录 `docs/demo/01-scaling.md`、文章《为什么 HPA 会震荡：手写伸缩决策引擎》

## P2 — 故障自愈（核心已完成 2026-11-03）
- [x] 故障识别：CrashLoopBackOff / OOMKilled / 高频重启兜底（按 Pod containerStatuses）
- [x] 分级自愈状态机：L1 扩容 → L2 摘除坏 Pod → L3 按 ReplicaSet 修订历史回滚镜像
- [x] 金丝雀观察窗口 + 版本自动回滚（错误率超阈值 ~20s 内自动回滚）
- [ ] kind 集群混沌验证脚本（CI 化，第 3 周）
- 📦 产出：演示实录 `docs/demo/02-self-healing.md`、文章《给 K8s 加一个"自动值班员"：分级自愈引擎设计》

## P3 — Admission Webhook（2 周）
- [ ] Validating Webhook：CR 校验规则
- [ ] Defaulting Webhook：安全上下文/资源限制默认值注入（CKS 落地）
- [ ] cert-manager 集成与本地开发证书方案
- 📦 产出：文章《K8s 准入控制实战：从 Kube-apiserver 到你的 Webhook》

## P4 — 手写 mini-Informer（3 周，面试杀手锏）
- [ ] 不依赖 informer 库，实现 ListWatch → DeltaFIFO → EventHandler → WorkQueue
- [ ] 对比 client-go 原生实现，写原理长文
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
| 2026.11–12 | P0 + P1 | 简历首次写入项目（弹性伸缩 Operator） |
| 2027.01–02 | P2 + P3 | 补充"自愈引擎""准入控制"关键词 |
| 2027.03 | P4 + 系列文章完结 | 补充"手写 Informer""掘金系列 x N 篇" |
| 2027.04–05 | 演示视频 + 面试题库整理 | 简历定稿，秋招冲刺 |
