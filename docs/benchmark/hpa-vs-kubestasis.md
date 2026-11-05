# KubeStasis vs 原生 HPA：同一流量时间线下的对照压测

> **版本声明（2026-11-05 补）**：本文数据采集于 commit `565b0fb`（run5），即 `scaling.scaleDownStabilizationSeconds`（缩容稳定窗口，默认 60s，`1301365` 实现）落地**之前**。当前 main 的缩容时序与本文不同：§2.1 第三条（突发#2 起点误缩容）已被稳定窗口修复、不再出现；§2.1 第二条（回到 1 副本慢 65s）现在会**更慢**（约再加一个窗口）；副本-分钟的优势预期会缩小。重测协议已预注册（判定线先于数据入库）：[pre-registration.md](pre-registration.md)。
>
> 一句话结论（2026-11-05 修订口径）：在本实验口径内（见 §4"口径限定"），KubeStasis **全程副本-分钟少 12.5%**——这是经三轮 HPA 侧准重复校核后**效应量最大（≈6σ）、最可复现**的结论（复算见 [variance-precheck.md](variance-precheck.md)）。对突发的首次扩容响应，单次运行中观察到 23s vs 40s，但 HPA 侧跨三轮自身波动就达 16s（σ≈8s），**该差异与噪声同量级，在补齐重复实验前只作定性参考**。其余事实（单轮内）：**空闲窗口真的把副本缩回去了**（HPA 因 300s 缩容稳定窗口，整个空闲#1 期间保持峰值）。代价也是真的：**副本变更次数更多（9 vs 5）**、**空闲后回到 1 副本反而慢 65s**、~~突发#2 起点有一次误缩容~~（已被稳定窗口修复，见上方版本声明），且本次运行中 KubeStasis 侧的 loadgen 失败请求更多（99 vs 33）。
>
> 所有数字都由 `hack/benchmark_report.py` 从 [data.csv](data.csv) 现算，不手填。你可以拿原始 CSV 复算、质疑、推翻本文。

## 1. 实验设置

**公平性约定**（缺一条，对比就不可信）：

| 约定 | 做法 |
|---|---|
| 同一负载镜像 | 两侧都用 `kubestasis-demoapp:v1-bench`，同一二进制、同一 `CPU_SPIN_MS=8`（每请求忙等 8ms，CPU 消耗与 QPS 成正比，CPU 型 HPA 才有意义） |
| 同一资源声明 | requests 250m / limits 500m，单 Pod 吞吐上限 ≈ 62 QPS |
| 同一副本上下限 | 1..5 |
| 同一流量 | 同一个 loadgen 镜像，同一时间线注入（脚本同一时刻切换两侧突发源） |
| 各用原生信号 | HPA 走 metrics-server 的 CPU（50% 利用率目标，全默认行为，含 300s 缩容稳定窗口）；KubeStasis 走 Prometheus 的 QPS（16 QPS/Pod，默认迟滞 10% + 冷却 120s + 渐进缩容 20%） |

**唯一变量是伸缩器本身。** HPA 没有被削弱（没调小稳定窗口、没调容忍度）；KubeStasis 也没有为这场压测调参（全是 CRD 默认值）。QPS 目标 16 的推导：250m × 50% ÷ 8ms ≈ 15.6 QPS/Pod——与 HPA 的 CPU 目标等余量，两侧"安全边际"相同。

**流量时间线**（挂钟驱动，总长 1140s）：

```
0s──120s    基线 12 QPS/侧
120s──360s  突发#1：+150 QPS/侧（共 162 QPS，单 Pod 容量 62 → 必须扩容）
360s──480s  空闲#1：回到 12 QPS
480s──600s  突发#2：+150 QPS/侧
600s──1140s 空闲#2：观察缩容（HPA 300s 稳定窗口 vs KubeStasis 冷却 120s + 渐进 20%）
```

环境：docker-desktop 单节点（v1.36.1），metrics-server 与 Prometheus 各一份。Prometheus 只给 KubeStasis 用——HPA 不依赖它。

## 2. 结果

| 指标 | HPA（CPU 50%） | KubeStasis（QPS 16/Pod） |
|---|---|---|
| 扩容响应（突发#1，首次 >1 副本） | 40s | **23s** |
| 到达峰值用时（突发#1，5 副本） | 67s | **40s** |
| 峰值副本数 | 5 | 5 |
| 空闲#1 结束时副本数 | 5 | **4**（已开始收缩） |
| 突发#2 回到峰值用时 | 0（未缩容，无需回补） | 82s（含一次误缩容） |
| 副本变更次数（全程） | 5 | 9 |
| 副本-分钟（成本代理） | 72.8 | **63.7**（-12.5%） |
| 空闲后回到 1 副本 | **385s** | 450s |

关键窗口副本数（30s 刻度，完整采样见 CSV）：

| t(s) | 阶段 | HPA | KubeStasis |
|---|---|---|---|
| 0-90 | baseline | 1 | 1 |
| 120 | burst1 注入 | 1 | 1 |
| 150 | burst1 | 1 | 2 |
| 180 | burst1 | 4 | 5 |
| 210-330 | burst1 | 5 | 5 |
| 360-390 | idle1 | 5 | 5 |
| 420-450 | idle1 | 5 | 4 |
| 480-540 | burst2 | 5 | 4→3→5（见 2.2） |
| 570-660 | burst2/idle2 | 5 | 5 |
| 690-780 | idle2 | 5 | 4 |
| 810-900 | idle2 | 5 | 3 |
| 930-960 | idle2 | 5 | 2 |
| 990-1020 | idle2 | **1** | 2 |
| 1050-1140 | idle2 | 1 | 1 |

吞吐与 CPU 对照（突发窗口 t=180-360 均值）：HPA 158 QPS / 1498m；KubeStasis 160 QPS / 1517m。每千 QPS 的 CPU 核数两侧都是 9.5——同一镜像的必然结果，说明对比没有被"哪边跑得更慢"污染。

### 2.1 赢在哪、输在哪

**KubeStasis 赢的三项**

1. **扩容响应 23s vs 40s**。原因不是"我们的控制器更快"，而是**信号不同型**：QPS 信号在请求落地的瞬间就开始爬升（1m 窗口的 rate），CPU 信号要等 metrics-server 15s 采样 + HPA 同步周期 + 利用率跨过容忍度才动作。换成任何 QPS 驱动的伸缩器（KEDA 等）大概率也有这个优势——这是设计选择的红利，不是实现优劣。**（2026-11-05 补：这是本文最不稳的一条结论——HPA 侧三轮准重复的自身波动 σ≈8s、极差 16s，与 17s 的组间差同量级，见 [variance-precheck.md](variance-precheck.md)。要把它坐实需要加 KEDA 组做同型信号对照。）**
2. **空闲#1（120s 窗口）就把 5 缩到 4**。HPA 全程保持 5——它的 300s 缩容稳定窗口比整个空闲窗口还长。不是 HPA 的 bug，是它的默认保守取向，但这 120s 里 HPA 侧确实多花了副本-分钟。
3. **全程副本-分钟 63.7 vs 72.8**。成本代理指标上少 12.5%，主要来自两段空闲期的收缩。

**KubeStasis 输的三项**

1. **回到 1 副本慢 65s（450s vs 385s）**。HPA 在稳定窗口后一步 5→1；KubeStasis 要 4 步（5→4→3→2→1），每步之间一个 120s 冷却。注意口径：HPA 的 385s 里前 ~300s 副本一直挂着 5（白等），KubeStasis 的 450s 里副本是逐级下降的——所以"回到 1 的时间"HPA 赢，"空闲期平均成本"KubeStasis 赢，两个指标互相打架是这套机制的结构性结果。
2. **突发#2 起点有一次误缩容**。t≈535 时 QPS 的 1m 窗口均值还在 30-50（突发流量刚注入、loadgen Pod 在单节点上启动慢，t=528 时实测 QPS 甚至是 0），控制器算出 desired=3 < current=4，冷却刚好到期，于是 4→3；13s 后 QPS 冲到 120+，又直接扩回 5。**根因是没有 HPA 那样的缩容稳定窗口**：任何短促的信号低谷都会被当成缩容依据。这是我们与 HPA 对比中输得最实的一项，不是调参能掩盖的——它需要"信号需持续低于目标 N 秒才缩容"的机制。**（2026-11-05 补：该机制（`scaleDownStabilizationSeconds`，默认 60s）已于 `1301365` 实现并在真集群验证，误缩容不再出现；重测后应以 5 轮数据量化它在 N 轮中出现几次。本条保留为历史记录。）**
3. **副本变更次数 9 vs 5，loadgen 失败 99 vs 33**。变更多本身有成本（Pod 创建/销毁、连接抖动、冷启动），本次运行的失败请求差异（99/13580 = 0.7% vs 33/13406 = 0.2%）也与突发#2 起点少 1 个副本的窗口吻合。副本-分钟这个口径算不到变更成本——对 JVM 预热、模型加载类服务，这个代价会被放大。

## 3. 这份结果是第五轮：前四轮全废

诚实起见把五轮都摆出来——每轮的废数据都归档在同目录，可查证。

| 轮次 | 废在哪 | 修了什么 | 归档 |
|---|---|---|---|
| run1 | 对称迟滞带把扩容也挡住了：current=4、desired=5 时差 1 落在带内（带宽 1），突发下少 1 个副本 | 迟滞带改为**只抑制缩容、扩容直通**（震荡的根源不是"该扩时扩了"，而是"刚扩完就缩"） | [data-run1-symmetric-hysteresis.csv](data-run1-symmetric-hysteresis.csv) |
| run2 | 250 QPS 突发 = 4 倍超卖，KS 单副本饱和到 `/metrics` 都抓不动，QPS 信号归零——控制器在最需要扩容的时刻失明 | 突发降到 150 QPS（2.4 倍超卖）；QPS 目标 60→16（对齐 HPA 余量口径） | 未归档（时间轴口径也不对，见下） |
| run3 | 上轮修复把 `scrape_timeout: 20s` 写成大于 `scrape_interval: 5s`——**Prometheus 拒绝启动、CrashLoop 整轮**，KS 信号全程 0，run 无效 | interval 15s / timeout 10s（timeout ≤ interval 是硬约束） | [data-run3-prometheus-crashloop.csv](data-run3-prometheus-crashloop.csv) |
| run4 | 时间线全对，但抓到真 bug：渐进缩容在一个采样窗口内 3→2→1 连跳两步，冷却被绕过。机理：`Apply` 写 spec 立刻触发下一轮 Reconcile，而 cache 里的 status（`lastScalingAt`）还没更新 | 修复：写 spec 之前**先写 status 的冷却起点**（watch 事件按 resourceVersion 有序送达，先写的 status 必先到 cache）。注释见 `internal/controller/app_controller.go` | [data-run4-double-step-race.csv](data-run4-double-step-race.csv) |
| run5 | ——（本文数据） | —— | [data.csv](data.csv) |

run2 之前还有一个测量口径 bug：采样循环用循环计数器当时间轴，每轮实际 6-9s，把"20 分钟压测"拉成 35 分钟——HPA 的 300s 稳定窗口是真实时间，被拉伸的时间轴会直接毁掉对比。改成挂钟（`SECONDS`）驱动后才有了后面各轮。

**这五轮是本文最值钱的部分：一个"扩容更快"的结论，是在四轮被自己推翻的数据之后才站住的。**

## 4. 口径限定（哪些结论不能外推）

1. **单节点 docker-desktop**。被测服务、loadgen、Prometheus、metrics-server 挤在同一节点互相抢 CPU——本次突发#2 的 loadgen 启动延迟（~48s 才开始出流量）就是单节点调度压力的直接产物。真实多节点集群会引入调度、镜像拉取、跨节点网络，两边都会变慢，但比例未必保持。
2. **负载是纯忙等**。`CPU_SPIN_MS=8` 让 CPU 与 QPS 严格线性，是为了让 CPU 型 HPA"有得比"，但也意味着没有内存压力、没有 IO、没有长尾请求。真实业务的 HPA 未必这么"听话"。
3. **信号不同型是设计差异，不是 KubeStasis 的功劳**。QPS 信号 vs CPU 信号的响应差，换成任何 QPS 驱动的伸缩器都有。本文能证明"这套设计在响应速度上兑现了"，不能证明"比 HPA 优秀"。
4. **HPA 是全默认行为**。把 `scaleDown stabilization` 调到 60s、行为策略改成快速伸缩，空闲窗口的故事会完全反过来。没这么做，是因为"默认 HPA"才是大多数集群的真实形态——但这个选择本身就是口径。
5. **19 分钟、单次运行**。没有多轮取均值、没有方差。跨轮次的抖动已经肉眼可见（run4 失败数 9/12，run5 是 33/99），严谨结论需要 N 次重复。**（2026-11-05 补实测：HPA 侧三轮准重复的扩容响应为 35/40/51s（σ≈8s），大于本次观察到的部分组间差异；核算与归因见 [variance-precheck.md](variance-precheck.md)——它同时指出三轮是双组同跑，σ 含组间 CPU 争抢污染，只作量级参考。）**
6. **KubeStasis 的参数也是默认值**。冷却 120s、渐进 20%、迟滞 10% 都没调优。参数空间没扫过，"最优"无从谈起。
7. **副本-分钟算不到变更成本**。见 §2.1 第三条。

## 5. 复现

```bash
# 前提：集群已部署 KubeStasis，manager 带
#   --prometheus-url=http://prometheus.bench.svc:9090 --prometheus-window=1m
kubectl apply -f deploy/benchmark/benchmark.yaml    # 双组对照 + loadgen + Prometheus
bash hack/benchmark.sh docs/benchmark/data.csv 1140  # 19 分钟完整时间线
python hack/benchmark_report.py docs/benchmark/data.csv
```
