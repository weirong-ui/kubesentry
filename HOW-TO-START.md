# KubeStasis — 如何用这套材料起步

> 这份文档告诉你：拿到 `kubestasis/` 目录后，怎么一步步变成一个真实可运行、可写进简历的 GitHub 项目。

## 0. 你拿到了什么

```
kubestasis/
├── README.md                     # GitHub 仓库首页（改 weirong-ui 后直接用）
├── docs/
│   ├── DESIGN.md                 # 详细设计文档（也是掘金文章 1 的素材库）
│   └── ROADMAP.md                # 里程碑规划（与秋招时间线对齐）
├── juejin/
│   ├── 00-系列文章规划.md         # 7 篇文章的标题/考点/发布节奏 + 简历写法
│   └── 01-发布文章-01-项目设计.md  # 第 1 篇文章全文，可直接发掘金
├── api/v1alpha1/app_types.go     # CRD 类型定义（带 kubebuilder markers）
├── cmd/main.go                   # Manager 入口
├── internal/controller/          # 核心 Reconcile 循环（含详细注释）
├── internal/scaler/              # 伸缩决策引擎骨架（决策与执行分离，可单测）
├── internal/healer/              # 分级自愈引擎骨架
├── config/samples/               # 示例 App CR
├── Makefile / Dockerfile / .github/workflows/ci.yml
└── HOW-TO-START.md               # 本文档
```

## 1. 初始化真实项目（第 1 天，约 1 小时）

代码骨架是"逻辑骨架"——要能编译运行，还需要 kubebuilder 生成脚手架文件（go.mod、zz_generated、config/kustomize 等）。步骤：

```bash
# 1. 用 kubebuilder 生成标准脚手架
kubebuilder init --domain sentry.io --repo github.com/weirong-ui/kubestasis
kubebuilder create api --group apps --version v1alpha1 --kind App --resource --controller

# 2. 用我给你的文件覆盖对应位置：
#    api/v1alpha1/app_types.go、cmd/main.go、internal/controller/app_controller.go 等
#    （把文件里 weirong-ui 全局替换成你的 GitHub 用户名）

# 3. 生成 CRD 与 DeepCopy
make manifests generate

# 4. 本地跑起来（需要 kind 或现成集群）
kind create cluster
make install
make run
# 另开终端：
kubectl apply -f config/samples/app_v1alpha1_app.yaml
kubectl get deploy -w   # 应该看到 sample Deployment 被创建
```

## 2. 建仓库 + 开 CI（第 2 天）

```bash
git init && git add . && git commit -m "feat: KubeStasis P0 skeleton"
gh repo create kubestasis --public --push
```

- CI 文件已写好：lint → test（envtest）→ e2e（kind 集群冒烟）→ 镜像发布（GHCR，多架构）
- 第一颗绿勾出现后，去 README 把 CI badge 的 `weirong-ui` 改掉

## 3. 按 ROADMAP 推进，按文章规划发文

- 每完成一个 P，先打 tag（如 `v0.1.0-p0-mvp`），再发对应掘金文章
- P0 阶段把代码里的 `TODO(P0)` 全部消化掉；P1 消化 `TODO(P1)`，以此类推
- scaler/healer 的决策逻辑已做成纯函数，`Decide()` 可以直接写表驱动单元测试——这是面试聊"可测试性设计"的好素材

## 4. 掘金发文清单

1. 先发 `juejin/01-发布文章-01-项目设计.md`（建议工作日晚上 8–10 点发，流量最好）
2. 文末仓库地址记得换成真实链接
3. 后续文章标题和考点见 `juejin/00-系列文章规划.md`
4. 每篇文章开头放一张架构图/演示 GIF，掘金的推荐量会明显更好

## 5. 简历怎么写

见 `juejin/00-系列文章规划.md` 末尾的"简历写法建议"——每个 bullet 都对应一个能聊 20 分钟的面试话题。**原则：写上去的每一行，都要自己能讲透。**
