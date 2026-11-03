/*
Copyright 2026 weirong-ui

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package miniinformer 是一个不依赖 client-go 的迷你 Informer 实现（P4），
// 用于吃透 Informer 机制本身。数据流与 client-go 完全同构：
//
//	ListWatch（List 全量 + Watch 增量）
//	    ↓  watch 事件流
//	DeltaFIFO（按键去重压缩的有界队列，已知集合支持"删除墓碑"）
//	    ↓  Pop
//	Store（本地只读缓存，Lister 语义）
//	    ↓
//	EventHandler（AddFunc/UpdateFunc/DeleteFunc）
//
// 与 client-go 的取舍差异（有意简化，注释里注明）：
//   - DeltaFIFO 的每个 key 只保留**压缩后**的一个 delta（client-go 保留一串），
//     因为 Handler 关心的永远是最新状态，中间态没有信息量
//   - 没有 sharedIndexInformer 的多 Handler 扇出与 processor 层
//   - Watch 断线直接重新 List（client-go 用 resourceVersion 续传 + 书签优化）；
//     功能等价，代价是一次全量——对 mini 规模可接受
package miniinformer

import (
	"context"
	"errors"
	"sync"
)

// ---------- ListWatch：数据从哪里来 ----------

// EventType Watch 事件的种类（对应 watch.Added/Modified/Deleted）。
type EventType string

const (
	Added    EventType = "Added"
	Modified EventType = "Modified"
	Deleted  EventType = "Deleted"
)

// Event 一条 Watch 事件。
type Event[T any] struct {
	Type   EventType
	Object T
}

// ListWatcher 抽象"全量列举 + 增量订阅"。生产实现是 client-go 的
// ListWatch（REST 调用），测试里用一个脚本化的假实现即可。
type ListWatcher[T any] interface {
	// List 返回当前全量对象与 resourceVersion（之后 Watch 从该版本续传）。
	List(ctx context.Context) (items []T, resourceVersion string, err error)
	// Watch 从 resourceVersion 开始推送事件；通道关闭表示连接断开，需要重新 List。
	Watch(ctx context.Context, resourceVersion string) (<-chan Event[T], error)
}

// KeyFunc 从对象算出唯一键（生产上通常是 namespace/name）。
type KeyFunc[T any] func(T) string

// ---------- DeltaFIFO：去重压缩的变更队列 ----------

// DeltaType 变更类型。Sync 是"List 全量回放"产生的伪变更，
// 让 Handler 能区分"首次见到"与"真实新增"。
type DeltaType string

const (
	DeltaAdded   DeltaType = "Added"
	DeltaUpdated DeltaType = "Updated"
	DeltaDeleted DeltaType = "Deleted"
	DeltaSync    DeltaType = "Sync"
)

// Delta 一次变更。
type Delta[T any] struct {
	Type   DeltaType
	Key    string
	Object T
}

// DeltaFIFO 按键去重压缩的阻塞队列。
//
// 压缩规则（与 client-go dedupDeltas 精神一致，更激进）：
//   - 同 key 已有 Add，又来 Update → 合并成 Add（对象取新）——消费者没见过它，
//     "新增的什么"就是最新值
//   - 同 key 已有 Add/Update，又来 Delete → 压成 Delete（最终没存在过/
//     最终消失了，中间态无需汇报）
//   - 同 key 连续 Delete → 只留第一个（删两遍没有意义）
//
// 零值不可直接用，必须 NewDeltaFIFO。
type DeltaFIFO[T any] struct {
	keyFunc KeyFunc[T]

	mu     sync.Mutex
	cond   *sync.Cond // 替代 chan 做"队列非空"通知：Pop 阻塞等待
	queue  []string   // 键的 FIFO 顺序
	deltas map[string]Delta[T]
	closed bool
}

func NewDeltaFIFO[T any](keyFunc KeyFunc[T]) *DeltaFIFO[T] {
	f := &DeltaFIFO[T]{keyFunc: keyFunc, deltas: map[string]Delta[T]{}}
	f.cond = sync.NewCond(&f.mu)
	return f
}

func (f *DeltaFIFO[T]) enqueue(t DeltaType, obj T) {
	key := f.keyFunc(obj)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	old, exists := f.deltas[key]
	switch {
	case exists && old.Type == DeltaAdded && t == DeltaUpdated:
		// Add+Update → Add（最新对象）
		f.deltas[key] = Delta[T]{Type: DeltaAdded, Key: key, Object: obj}
	case exists && (old.Type == DeltaAdded || old.Type == DeltaUpdated) && t == DeltaDeleted:
		f.deltas[key] = Delta[T]{Type: DeltaDeleted, Key: key, Object: obj}
	case exists && old.Type == DeltaDeleted && t == DeltaDeleted:
		// 连续删除去重
	default:
		if !exists {
			f.queue = append(f.queue, key)
		}
		f.deltas[key] = Delta[T]{Type: t, Key: key, Object: obj}
	}
	f.cond.Broadcast()
}

// Add/Update/Delete/Sync 对应四种入口。
func (f *DeltaFIFO[T]) Add(obj T)    { f.enqueue(DeltaAdded, obj) }
func (f *DeltaFIFO[T]) Update(obj T) { f.enqueue(DeltaUpdated, obj) }
func (f *DeltaFIFO[T]) Delete(obj T) { f.enqueue(DeltaDeleted, obj) }
func (f *DeltaFIFO[T]) Sync(obj T)   { f.enqueue(DeltaSync, obj) }

// Pop 阻塞直到有变更或队列关闭。closed 后返回 ErrClosed。
var ErrClosed = errors.New("miniinformer: DeltaFIFO closed")

func (f *DeltaFIFO[T]) Pop(ctx context.Context) (Delta[T], error) {
	// cond.Wait 无法响应 ctx，用 close 广播做退出路径：
	// Run 的退出依赖 Close()，ctx 参数留给调用方做"先查一次"的快速退出
	select {
	case <-ctx.Done():
		var zero Delta[T]
		return zero, ctx.Err()
	default:
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	for len(f.queue) == 0 && !f.closed {
		f.cond.Wait()
	}
	if len(f.queue) == 0 {
		var zero Delta[T]
		return zero, ErrClosed
	}
	key := f.queue[0]
	f.queue = f.queue[1:]
	d := f.deltas[key]
	delete(f.deltas, key)
	return d, nil
}

// Close 唤醒所有阻塞的 Pop 并让它们返回 ErrClosed。
func (f *DeltaFIFO[T]) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	f.cond.Broadcast()
}

// Len 当前排队数（监控/测试用）。
func (f *DeltaFIFO[T]) Len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queue)
}

// ---------- Store：本地只读缓存 ----------

// Store 是 Informer 的本地缓存，读路径不加锁从 API server——
// 这是 Informer 存在的核心理由：把 LIST 压力从 apiserver 搬到内存。
type Store[T any] struct {
	mu    sync.RWMutex
	items map[string]T
}

func NewStore[T any]() *Store[T] { return &Store[T]{items: map[string]T{}} }

func (s *Store[T]) Get(key string) (T, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.items[key]
	return v, ok
}

func (s *Store[T]) List() []T {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]T, 0, len(s.items))
	for _, v := range s.items {
		out = append(out, v)
	}
	return out
}

func (s *Store[T]) set(key string, v T) { s.mu.Lock(); s.items[key] = v; s.mu.Unlock() }
func (s *Store[T]) del(key string)      { s.mu.Lock(); delete(s.items, key); s.mu.Unlock() }

// ---------- Handler 与 Informer ----------

// Handler 变更回调。Object 始终是变更后的最新对象（Delete 时是最后已知的墓碑）。
type Handler[T any] struct {
	AddFunc    func(T)
	UpdateFunc func(old, new T)
	DeleteFunc func(T)
	// SyncFunc 全量回放（List 之后每个对象一条 Sync）。可用于"启动时预热"。
	SyncFunc func(T)
}

// Informer 把 ListWatch → DeltaFIFO → Store → Handler 串起来。
type Informer[T any] struct {
	lw      ListWatcher[T]
	keyFunc KeyFunc[T]
	handler Handler[T]

	fifo  *DeltaFIFO[T]
	store *Store[T]

	mu        sync.Mutex
	hasSynced bool
}

func New[T any](lw ListWatcher[T], keyFunc KeyFunc[T], handler Handler[T]) *Informer[T] {
	return &Informer[T]{
		lw: lw, keyFunc: keyFunc, handler: handler,
		fifo: NewDeltaFIFO(keyFunc), store: NewStore[T](),
	}
}

// Store 返回本地缓存（读它，不要读 apiserver）。
func (i *Informer[T]) Store() *Store[T] { return i.store }

// HasSynced 首次 List 是否已回放完——在这之前读 Store 可能拿到不完整的数据。
func (i *Informer[T]) HasSynced() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.hasSynced
}

// Run 启动控制循环，直到 ctx 取消。
//
// 一轮 = List（全量回放成 Sync delta + 重建 Store）→ 启动消费协程 → Watch（增量）。
// Watch 通道关闭（断线/超时）→ 重新 List 再来一轮。消费协程在一轮结束时通过
// 局部 done 通道退出，避免断线重列期间旧协程继续消费新队列。
func (i *Informer[T]) Run(ctx context.Context) error {
	for {
		if err := i.listAndWatch(ctx); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return err
		}
	}
}

func (i *Informer[T]) listAndWatch(ctx context.Context) error {
	items, rv, err := i.lw.List(ctx)
	if err != nil {
		return err
	}
	// 全量回放：先发 Sync delta（FIFO 顺序 = List 顺序），再重建 Store
	for _, obj := range items {
		i.fifo.Sync(obj)
	}
	done := make(chan struct{})
	go i.consume(ctx, done)

	stream, err := i.lw.Watch(ctx, rv)
	if err != nil {
		close(done)
		return err
	}
	defer close(done)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-stream:
			if !ok {
				return nil // 断线：返回后外层重新 List
			}
			switch ev.Type {
			case Added:
				i.fifo.Add(ev.Object)
			case Modified:
				i.fifo.Update(ev.Object)
			case Deleted:
				i.fifo.Delete(ev.Object)
			}
		}
	}
}

// consume 把 DeltaFIFO 里的变更落到 Store 并触发 Handler。
func (i *Informer[T]) consume(ctx context.Context, done chan struct{}) {
	for {
		select {
		case <-done:
			return
		default:
		}
		d, err := i.fifo.Pop(ctx)
		if err != nil {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			default:
				continue // Pop 被 Close 之外的假唤醒打断：重来
			}
		}
		old, existed := i.store.Get(d.Key)
		switch d.Type {
		case DeltaSync:
			i.store.set(d.Key, d.Object)
			if i.handler.SyncFunc != nil {
				i.handler.SyncFunc(d.Object)
			}
			// 注意：HasSynced 的标记放在"本轮 List 的最后一条 Sync 消费完"才准确，
			// mini 版简化为：只要消费过 Sync 就认为已同步（consume 是 FIFO 顺序的，
			// Sync 全在最前，第一条 Sync 之后 Store 至少是"曾经完整过"的）
			i.mu.Lock()
			i.hasSynced = true
			i.mu.Unlock()
		case DeltaAdded:
			i.store.set(d.Key, d.Object)
			if i.handler.AddFunc != nil {
				i.handler.AddFunc(d.Object)
			}
		case DeltaUpdated:
			i.store.set(d.Key, d.Object)
			if i.handler.UpdateFunc != nil {
				i.handler.UpdateFunc(old, d.Object)
			}
		case DeltaDeleted:
			i.store.del(d.Key)
			if i.handler.DeleteFunc != nil && existed {
				i.handler.DeleteFunc(old)
			}
		}
	}
}

// ---------- WorkQueue：控制器侧的幂等任务队列 ----------

// WorkQueue 与 DeltaFIFO 职责不同，这是最容易混的地方：
//   - DeltaFIFO 面向"状态变更"（携带对象快照，按键压缩）
//   - WorkQueue 面向"待办任务"（只存 key，同一 key 在处理中再来会标 dirty，
//     Done 后若 dirty 则重新入队——这就是 client-go 的"幂等 + 不漏"语义）
type WorkQueue struct {
	mu       sync.Mutex
	cond     *sync.Cond
	queue    []string
	dirty    map[string]bool // 待处理
	working  map[string]bool // 处理中
	attempts map[string]int
	closed   bool
}

func NewWorkQueue() *WorkQueue {
	q := &WorkQueue{dirty: map[string]bool{}, working: map[string]bool{}, attempts: map[string]int{}}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Add 投递任务。处理中重复投递 → 标 dirty，Done 时重新入队（不漏）。
func (q *WorkQueue) Add(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	if q.working[key] {
		q.dirty[key] = true // 等 Done 时补入队
		return
	}
	if !q.dirty[key] {
		q.dirty[key] = true
		q.queue = append(q.queue, key)
		q.cond.Broadcast()
	}
}

// Get 阻塞取任务，返回 key 与该 key 的历史失败次数（调用方据此做退避）。
func (q *WorkQueue) Get() (key string, attempts int, err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.queue) == 0 && !q.closed {
		q.cond.Wait()
	}
	if len(q.queue) == 0 {
		return "", 0, ErrClosed
	}
	key = q.queue[0]
	q.queue = q.queue[1:]
	delete(q.dirty, key) // 出队即不再是"待处理"：处理期间的重复投递才会被记 dirty
	q.working[key] = true
	return key, q.attempts[key], nil
}

// Done 标记处理完成；期间又来了同 key 任务则重新入队。
func (q *WorkQueue) Done(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.working, key)
	if q.dirty[key] && !contains(q.queue, key) {
		q.queue = append(q.queue, key)
		q.cond.Broadcast()
	} else if !q.dirty[key] {
		// 处理干净了，清失败计数
		delete(q.attempts, key)
	}
}

// Forget 处理成功后调用：清失败计数（与 Done 搭配：先 Forget 再 Done）。
func (q *WorkQueue) Forget(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.attempts, key)
}

// Fail 处理失败：失败计数 +1 并重新入队（调用方负责按 attempts 做延迟）。
func (q *WorkQueue) Fail(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.attempts[key]++
	delete(q.working, key)
	q.dirty[key] = true
	if !contains(q.queue, key) {
		q.queue = append(q.queue, key)
		q.cond.Broadcast()
	}
}

func (q *WorkQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.queue)
}

func (q *WorkQueue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	q.cond.Broadcast()
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
