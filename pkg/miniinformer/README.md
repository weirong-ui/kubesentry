# miniinformer —— 手写一个 Informer 才敢说懂

不依赖 client-go 的迷你 Informer（P4）。约 400 行，覆盖 Informer 的完整数据流：

```
ListWatch（List 全量 + Watch 增量）
    ↓  watch 事件流
DeltaFIFO（按键去重压缩的阻塞队列）
    ↓  Pop
Store（本地只读缓存 / Lister 语义）
    ↓
Handler（AddFunc / UpdateFunc / DeleteFunc / SyncFunc）
```

WorkQueue 另立门户：它不在 Informer 数据流里，而是**控制器侧**的任务队列。

## 为什么拆成这四层

| 组件 | 职责 | 最容易答错的面试点 |
|---|---|---|
| ListWatch | 数据从哪来：先全量 List，再从 resourceVersion 续传 Watch | Watch 会断！断了必须重新 List（本实现）或用 bookmark 续传（client-go 的优化） |
| DeltaFIFO | 变更的**去重压缩**缓冲 | 它和 WorkQueue 不是一回事：DeltaFIFO 带对象快照、按键压缩；WorkQueue 只存 key |
| Store | 读路径的本地缓存 | Informer 存在的核心理由：把 LIST 压力从 apiserver 搬到内存 |
| Handler | 业务回调 | UpdateFunc 拿到的 old 来自 Store，不是来自事件本身 |

## 压缩规则（DeltaFIFO 的灵魂）

- `Add + Update → Add(最新对象)`：消费者还没见过它，"新增的什么"就是最新值
- `Add/Update + Delete → Deleted`：最终消失了，中间态无需汇报
- `Delete + Delete → 只留一个`：删两遍没有意义

client-go 的真实实现每个 key 保留一串 deltas（`Deltas []Delta`），只在 Pop 侧做
连续 Delete 去重；mini 版更激进地压成一个 delta——因为 Handler 关心的永远是
最新状态。这是**有意的简化**，面试时被问到要能说出这个差异。

## WorkQueue 的"幂等 + 不漏"

```
Add(key)   未处理：去重；处理中：标 dirty
Get()      出队，清 dirty，标 working，返回历史失败次数
Done(key)  清 working；若期间 dirty 过 → 重新入队（不漏）
Fail(key)  attempts+1，重新入队（调用方按 attempts 退避）
Forget(key) 成功后来一下：清失败计数
```

关键不变量：**同一 key 在队列里最多出现一次**，但绝不会因为"正在处理"而丢掉
一次变更通知（dirty 标记兜底）。这就是控制器可以写得"幂等随便重入"的原因。

## 断线重列

`Run` 的循环：List（全量回放成 Sync delta）→ Watch（增量）→ 断线 → 重新 List。
HasSynced 在第一轮 List 回放完之前是 false——在这之后之前读 Store 可能拿到不完整
数据，控制器必须等它。

## 与 client-go 的对应关系

| miniinformer | client-go |
|---|---|
| `ListWatcher[T]` | `cache.ListWatch` |
| `DeltaFIFO[T]` | `cache.DeltaFIFO`（`NewDeltaFIFOWithOptions`） |
| `Store[T]` | `cache.Store` / `cache.Indexer` |
| `Handler[T]` | `cache.ResourceEventHandlerFuncs` |
| `Informer.Run` | `cache.Reflector` + `controller.Run` |
| `WorkQueue` | `workqueue.TypedRateLimitingInterface`（mini 版把退避留给调用方） |

## 跑测试

```bash
go test ./pkg/miniinformer/ -v -count=1
```
