# KubeSentry Roadmap

> 与大三下学期 + 秋招时间线对齐，每个里程碑都是一次简历更新点 + 一篇掘金文章素材。

## P0 — MVP：让 Operator 转起来（3 周）
- [ ] kubebuilder 初始化项目，App CRD 定义 + DeepCopy 生成
- [ ] Reconcile 循环：App → Deployment/Service 的 CreateOrUpdate
- [ ] Finalizer 处理删除逻辑
- [ ] envtest 集成测试跑通第一个用例
- [ ] GitHub Actions：golangci-lint + go test + 构建镜像
- 📦 产出：GitHub 仓库可运行、掘金文章《从零写一个 K8s Operator》

## P1 — 智能伸缩（3 周）
- [ ] 对接 Prometheus HTTP API 查询 QPS / P99 / 错误率
- [ ] 多信号伸缩决策 + 冷却窗口 + 迟滞带 + 缩容渐进
- [ ] 决策过程可观测：Prometheus metrics（`kubesentry_scaling_decisions_total`）+ Grafana 面板
- 📦 产出：压测演示（wrk 灌流量 → 观察扩容曲线）、文章《为什么 HPA 会震荡：手写伸缩决策引擎》

## P2 — 故障自愈（3 周）
- [ ] 故障识别：CrashLoopBackOff / OOMKilled / 5xx 飙升
- [ ] 分级自愈状态机：扩容 → 降级（摘除不健康副本）→ 回滚
- [ ] 金丝雀观察窗口 + 版本自动回滚
- [ ] kind 集群混沌验证脚本
- 📦 产出：自愈演示 GIF、文章《给 K8s 加一个"自动值班员"：分级自愈引擎设计》

## P3 — Admission Webhook（2 周）
- [ ] Validating Webhook：CR 校验规则
- [ ] Defaulting Webhook：安全上下文/资源限制默认值注入（CKS 落地）
- [ ] cert-manager 集成与本地开发证书方案
- 📦 产出：文章《K8s 准入控制实战：从 Kube-apiserver 到你的 Webhook》

## P4 — 手写 mini-Informer（3 周，面试杀手锏）
- [ ] 不依赖 informer 库，实现 ListWatch → DeltaFIFO → EventHandler → WorkQueue
- [ ] 对比 client-go 原生实现，写原理长文
- 📦 产出：`pkg/miniinformer` + 掘金深度文《图解 client-go Informer：手写一个才敢说懂》（这篇有爆款潜质）

## P5+ — 冲刺亮点（秋招前按余力）
- [ ] eBPF 无侵入指标采集（cilium/ebpf）
- [ ] 多集群容灾调度
- [ ] KubeCon 形式的英文 README + release v1.0 打 tag

## 时间线总览

| 时间 | 里程碑 | 简历动作 |
|---|---|---|
| 2026.11–12 | P0 + P1 | 简历首次写入项目（弹性伸缩 Operator） |
| 2027.01–02 | P2 + P3 | 补充"自愈引擎""准入控制"关键词 |
| 2027.03 | P4 + 系列文章完结 | 补充"手写 Informer""掘金系列 x N 篇" |
| 2027.04–05 | 演示视频 + 面试题库整理 | 简历定稿，秋招冲刺 |
