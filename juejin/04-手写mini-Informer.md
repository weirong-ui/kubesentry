# 图解 client-go Informer：手写一个 mini-Informer 才敢说懂

> 面试里问"Informer 原理"，几乎所有人都能背出这句话：**ListWatch 拉数据 → DeltaFIFO 排队 → Store 缓存 → 回调业务逻辑**。
> 但往下追三层就开始含糊了：DeltaFIFO 为什么要"压缩"？Watch 断了为什么不续传而是重新 List？WorkQueue 明明也存 key，为什么不能替代 DeltaFIFO？
>
> 我花了两个晚上写了 400 行、零 client-go 依赖的 mini-Informer（[代码](https://github.com/weirong-ui/kubestasis/tree/main/pkg/miniinformer)，11 个用例包含断线重列），这篇文章记的是**写完才想明白的部分**。

## 一、先说不写就答不上来的三个问题

**Q1：Informer 到底帮你省了什么？**
省的是 apiserver 的 LIST 压力。控制器若每次 Reconcile 都 `client.List`，几百个控制器 × 每秒数十次 = apiserver 被打爆。Informer 把"全量"做一次，之后只吃增量事件，并把结果放内存（Store）供任意次读取。**Informer 本质是一个"带版本号的本地副本 + 变更通知器"**。

**Q2：DeltaFIFO 和 WorkQueue 都存 key，为什么要有两个队列？**
因为它们的**语义完全不同**：

| | DeltaFIFO | WorkQueue |
|---|---|---|
| 存什么 | key → 对象快照（变更类型 + 对象） | 只有 key |
| 谁写 | Reflector（watch 事件） | 业务代码（EventHandler / Reconcile 结束） |
| 谁读 | Informer 自己的 Pop 循环 | 控制器的 worker |
| 目的 | 把事件**压缩**后同步进 Store | 把"待处理任务"**去重**后交给 worker |

一句话：DeltaFIFO 面向"数据同步"，WorkQueue 面向"任务调度"。Infoformer 的 Store 是只读缓存，控制器拿到的只是 key，要自己回 Store 取对象——这个设计让控制器天然幂等：**同一 key 处理两次，结果一样**。

**Q3：Watch 断线后为什么重新 List，而不是接着 watch？**
因为断线期间的事件已经丢了。client-go 有两条路：靠 `resourceVersion` + bookmark 续传（省流量，但要求 apiserver 还留着那段历史），或者重新 List（重、但绝对正确）。mini 版选了后者——**先正确，再优化**，这个取舍在文末会讲清楚。

## 二、四层数据流，逐层写

### 2.1 ListWatch：数据源，泛型化

```go
type ListWatcher[T any] interface {
    List(ctx context.Context) ([]T, string, error)         // 返回对象 + 当前 resourceVersion
    Watch(ctx context.Context, rv string) (<-chan Event[T], error)
}

type Event[T any] struct {
    Type   EventType // Added / Modified / Deleted
    Object T
}
```

真实实现里 `Watch` 返回的是 `watch.Interface`，事件里带 `resourceVersion`；这里简化成 channel，因为 mini 版不需要断点续传（断了就重列）。

### 2.2 DeltaFIFO：压缩才是灵魂

最直观的实现是"来一个事件就塞一个"。问题是：一次发布可能让同一个 Pod 产生 5 个事件，消费者真的需要看中间 4 个状态吗？不需要。**消费者永远只关心"最新状态"**。

所以 DeltaFIFO 的规则是**按键压缩**：

```
Add + Update   → Add(最新对象)      // 他还没见过它，"新增的是什么"就是最新值
Add/Update + Delete → Deleted        // 最终消失了，中间态没有意义
Delete + Delete → 一个 Deleted        // 删两遍没有意义
```

```go
func (q *DeltaFIFO[T]) push(key string, d Delta[T]) {
    if _, ok := q.items[key]; !ok {
        q.queue = append(q.queue, key) // 新 key 才入队；已存在的只更新快照
    }
    q.items[key] = compress(q.items[key], d)
}
```

注意 `q.queue` 只存 key，**同一个 key 在队列里最多出现一次**——这是"压缩"的另一半：Update 不产生新队列项，只覆盖快照。

> 与 client-go 的差异（面试可能追问）：client-go 的 `DeltaFIFO` 每个 key 保留的是 `Deltas []Delta`（一串变更），只在 `Pop` 时对连续 `Deleted` 去重；mini 版更激进地压成单个 delta。为什么可以？因为我们的 Handler 是"按最终状态回调"，不重放历史。如果你的业务需要"变更审计"，就不能这么压。

### 2.3 Store：读路径的本地副本

```go
type Store[T any] struct {
    mu    sync.RWMutex
    items map[string]T
}
```

Store 只做三件事：`Replace`（List 全量替换）、`Put`、`Delete`。它和 DeltaFIFO 的分工是**写路径 vs 读路径**：事件先进 FIFO，Pop 出来时同时"写 Store + 调 Handler"。

这里有个容易忽略的细节：**Update 回调的 old 对象来自 Store，不是来自事件**。因为事件里只有新对象；想要 old，只能拿 Store 里的旧值。这就是 Handler 签名里 `SyncFunc` 要单独存在的原因——List 回放时没有"变更前"的概念，只能给你"当前快照"。

### 2.4 Handler：四种回调，而不是三种

```go
type Handler[T any] struct {
    AddFunc    func(T)
    UpdateFunc func(oldObj, newObj T) // old 来自 Store
    DeleteFunc func(T)
    SyncFunc   func(T)                // 首次 List 回放：只告知存在，不是变更
}
```

`SyncFunc` 是 mini 版为了"启动时对齐状态"加的。client-go 用 `OnAdd` 统一处理初次同步（通过 `isInInitialList` 标识），语义上等价，但显式分开更好懂。

## 三、WorkQueue：幂等 + 不漏

WorkQueue 只有一个不变量：**同一 key 在队列里最多出现一次，但绝不会因为"正在处理"而丢掉一次变更通知"。**

```
Add(key)     未处理：入队；处理中：只标 dirty
Get()        出队，清 dirty，标 working，返回历史失败次数
Done(key)    清 working；若期间 dirty 过 → 重新入队（不漏）
Fail(key)    attempts+1，重新入队（调用方按 attempts 退避）
Forget(key)  成功后清失败计数
```

三个字段就能实现：`queue []string`、`dirty map[string]bool`、`working map[string]bool`。

**我在这里踩了一个坑**：第一版 `Get()` 出队后忘了清 `dirty`，导致 `Done()` 看到 dirty=true 又把 key 塞回队列——控制器陷入"处理完立刻重做"的死循环，测试里表现为计数器疯涨。修完才理解 client-go 里那几行 `delete(q.dirty, key)` 为什么必须精确到位置。

```go
func (q *WorkQueue) Get() (string, int, bool) {
    for len(q.queue) > 0 {
        key := q.queue[0]
        q.queue = q.queue[1:]
        if !q.dirty[key] {
            continue // 已被处理过且没有新变更，跳过（防御性）
        }
        delete(q.dirty, key)   // ← 关键：出队即消费掉这轮 dirty
        q.working[key] = true
        return key, q.attempts[key], true
    }
    return "", 0, false
}
```

## 四、断线重列与 HasSynced

`Run` 的主循环只有五行：

```go
for {
    items, rv := lw.List(ctx)        // 1. 全量
    store.Replace(items)             // 2. 覆盖缓存
    replayAsSync(items)              // 3. 全量回放成 Sync 事件
    ch := lw.Watch(ctx, rv)          // 4. 从 rv 开始增量
    drain(ch)                        // 5. 通道关闭 = 断线，回到 1
}
```

`HasSynced` 必须在第一轮 List 回放完之后才置 true。**控制器不等它就开始工作，会读到不完整的缓存**——这是"控制器启动时误删资源"这类事故的经典成因。

测试里我用一个"Watch 一开就断"的 fake，断言至少发生两轮 List + 回放，否则重列逻辑相当于没测。

## 五、什么场景才值得自己写

写完之后我的结论可能和你想的相反：**生产环境几乎永远不该自己写 Informer。** 但有三类场景值得：

1. **学习/面试**：只有自己处理过"断线、压缩、去重"这三个麻烦，才敢说懂 client-go 的设计动机。
2. **极简依赖场景**：比如一个只 watch 单一资源的 sidecar，不想拖进整个 client-go 依赖树（虽然 client-go 的 informer 包可以单独引入，但 API 面很大）。
3. **需要非 Kubernetes 数据源的"类 Informer"语义**：比如 watch 一个配置中心、一个数据库变更流，复用同一套"List+Watch→压缩→缓存→回调"的骨架非常合适。这时 client-go 帮不上你，mini 版就是模板。

## 六、为什么生产用 SharedInformer 而不是自己写

| 维度 | mini-Informer | SharedInformer |
|---|---|---|
| 连接复用 | 每个 Informer 一条 watch | 同一类型共享一条 watch（几百个 controller 也只打一份） |
| 索引 | 只有 key→对象 | Indexer 支持任意索引（按 namespace、按 label） |
| 断点续传 | 断了重列 | resourceVersion + bookmark，减少全量 |
| 限流与背压 | 自己实现 | 内置 rate limiter、事件队列 |
| 内存 | 全量对象常驻 | 全量 + 增量，配合 watch cache 分层 |

所以正确的表述是：**"我手写过，所以知道 SharedInformer 每个设计决策在解决什么问题"**，而不是"我不用 client-go"。

## 七、把它接进真实控制器（可选）

mini 版的 `Run` 可以直接驱动一个"取 key → 从 Store 读对象 → 调 Reconcile"的循环。真实项目里我没有这么做——KubeStasis 用的是 controller-runtime 的 Manager（它内部就是 SharedInformer + WorkQueue 的组合）。`pkg/miniinformer` 的定位是**可执行的原理笔记**：它能跑、有测试、有断线重列，但不会被塞进生产路径。

## 八、跑起来

```bash
git clone https://github.com/weirong-ui/kubestasis
cd kubestasis && go test ./pkg/miniinformer/ -v -count=1
```

11 个用例覆盖：压缩规则（Add+Update / Add+Delete / 重复 Delete）、WorkQueue 的 dirty-while-working、断线重列、HasSynced 时机。

---

**系列其他文章**：
1. [HPA 只会伸缩不会自愈？我设计了一个 K8s 应用自愈 Operator](https://juejin.cn/post/7702723478470852646)
2. [从零写一个 K8s Operator：kubebuilder 初始化到第一个 Reconcile](https://juejin.cn/post/7703105289100951561)
3. [为什么 HPA 会震荡？手写一个带冷却窗口的伸缩决策引擎](https://juejin.cn/post/7703869723705061416)
