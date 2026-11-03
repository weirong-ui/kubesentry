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

package miniinformer

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"
)

// pod 是测试用的最小对象：name 即 key，image 变化代表 Update。
type pod struct {
	name  string
	image string
}

func podKey(p pod) string { return p.name }

// fakeLW 脚本化的 ListWatcher：List 返回固定全量，Watch 推送脚本事件后保持开着。
type fakeLW struct {
	mu       sync.Mutex
	items    []pod
	events   []Event[pod]
	watchCh  chan Event[pod]
	watchN   int // Watch 被调用的次数（验证断线重连）
	closeNow bool
}

func (f *fakeLW) List(ctx context.Context) ([]pod, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]pod, len(f.items))
	copy(out, f.items)
	return out, fmt.Sprintf("rv%d", f.watchN), nil
}

func (f *fakeLW) Watch(ctx context.Context, rv string) (<-chan Event[pod], error) {
	f.mu.Lock()
	f.watchN++
	ch := make(chan Event[pod], len(f.events))
	events := make([]Event[pod], len(f.events))
	copy(events, f.events)
	f.events = nil
	closeNow := f.closeNow
	f.closeNow = false
	f.watchCh = ch
	f.mu.Unlock()
	go func() {
		for _, e := range events {
			ch <- e
		}
		if closeNow {
			close(ch) // 模拟断线
		}
	}()
	return ch, nil
}

// push 向当前活跃的 watch 通道补发事件。
func (f *fakeLW) push(e Event[pod]) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.watchCh != nil {
		f.watchCh <- e
	} else {
		f.events = append(f.events, e)
	}
}

// closeCurrent 关闭当前 watch 通道，模拟断线。
// 测试必须先等第一轮 Sync 消费完再调用，否则第二轮 List 的 Sync 会与队列里
// 还没被消费的第一轮 Sync 去重合并——那测的就是压缩而不是重列了。
func (f *fakeLW) closeCurrent() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.watchCh != nil {
		close(f.watchCh)
		f.watchCh = nil
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for: %s", what)
}

// ---------- DeltaFIFO ----------

func TestDeltaFIFO_AddThenUpdateCompresses(t *testing.T) {
	f := NewDeltaFIFO(podKey)
	f.Add(pod{"a", "v1"})
	f.Update(pod{"a", "v2"}) // 消费者还没见过 a，"新增的什么"应直接是 v2
	if f.Len() != 1 {
		t.Fatalf("Len = %d, want 1 (compressed)", f.Len())
	}
	d, err := f.Pop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d.Type != DeltaAdded || d.Object.image != "v2" {
		t.Fatalf("want Added(v2), got %s(%s)", d.Type, d.Object.image)
	}
}

func TestDeltaFIFO_AddThenDeleteCollapsesToDelete(t *testing.T) {
	f := NewDeltaFIFO(podKey)
	f.Add(pod{"a", "v1"})
	f.Delete(pod{"a", "v1"})
	d, _ := f.Pop(context.Background())
	if d.Type != DeltaDeleted {
		t.Fatalf("Add+Delete must collapse to Deleted, got %s", d.Type)
	}
	if f.Len() != 0 {
		t.Fatalf("Len = %d, want 0", f.Len())
	}
}

func TestDeltaFIFO_DuplicateDeleteDeduped(t *testing.T) {
	f := NewDeltaFIFO(podKey)
	f.Delete(pod{"a", "v1"})
	f.Delete(pod{"a", "v1"})
	if f.Len() != 1 {
		t.Fatalf("duplicate deletes must dedup, Len = %d", f.Len())
	}
}

func TestDeltaFIFO_OrderAndMultiKeys(t *testing.T) {
	f := NewDeltaFIFO(podKey)
	f.Add(pod{"a", "v1"})
	f.Add(pod{"b", "v1"})
	f.Update(pod{"a", "v2"})
	var order []string
	for f.Len() > 0 {
		d, _ := f.Pop(context.Background())
		order = append(order, d.Key)
	}
	if len(order) != 2 || order[0] != "a" || order[1] != "b" {
		t.Fatalf("FIFO order broken: %v", order)
	}
}

func TestDeltaFIFO_PopBlocksUntilClose(t *testing.T) {
	f := NewDeltaFIFO(podKey)
	done := make(chan error, 1)
	go func() {
		_, err := f.Pop(context.Background())
		done <- err
	}()
	select {
	case <-done:
		t.Fatal("Pop should block on empty queue")
	case <-time.After(100 * time.Millisecond):
	}
	f.Close()
	if err := <-done; err != ErrClosed {
		t.Fatalf("after Close, Pop must return ErrClosed, got %v", err)
	}
}

// ---------- Informer ----------

func TestInformer_ListThenWatch(t *testing.T) {
	lw := &fakeLW{items: []pod{{"a", "v1"}, {"b", "v1"}}}
	var mu sync.Mutex
	var syncs, adds, updates, deletes []string
	h := Handler[pod]{
		SyncFunc:   func(p pod) { mu.Lock(); syncs = append(syncs, p.name); mu.Unlock() },
		AddFunc:    func(p pod) { mu.Lock(); adds = append(adds, p.name); mu.Unlock() },
		UpdateFunc: func(o, n pod) { mu.Lock(); updates = append(updates, n.name+":"+n.image); mu.Unlock() },
		DeleteFunc: func(p pod) { mu.Lock(); deletes = append(deletes, p.name); mu.Unlock() },
	}
	inf := New[pod](lw, podKey, h)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go inf.Run(ctx)

	eventually(t, "HasSynced", inf.HasSynced)
	eventually(t, "2 syncs replayed", func() bool { mu.Lock(); defer mu.Unlock(); return len(syncs) == 2 })
	if v, ok := inf.Store().Get("a"); !ok || v.image != "v1" {
		t.Fatal("store should contain a=v1 after list")
	}

	// 增量：Add / Update / Delete 各来一条
	lw.push(Event[pod]{Added, pod{"c", "v1"}})
	lw.push(Event[pod]{Modified, pod{"a", "v2"}})
	lw.push(Event[pod]{Deleted, pod{"b", "v1"}})

	eventually(t, "add c", func() bool { mu.Lock(); defer mu.Unlock(); return len(adds) == 1 && adds[0] == "c" })
	eventually(t, "update a", func() bool { mu.Lock(); defer mu.Unlock(); return len(updates) == 1 && updates[0] == "a:v2" })
	eventually(t, "delete b", func() bool { mu.Lock(); defer mu.Unlock(); return len(deletes) == 1 && deletes[0] == "b" })

	if _, ok := inf.Store().Get("b"); ok {
		t.Fatal("b should be gone from store")
	}
	if v, _ := inf.Store().Get("a"); v.image != "v2" {
		t.Fatal("a should be v2 in store")
	}
}

func TestInformer_ResyncAfterWatchClose(t *testing.T) {
	lw := &fakeLW{items: []pod{{"a", "v1"}}}
	var mu sync.Mutex
	var syncs int
	inf := New[pod](lw, podKey, Handler[pod]{
		SyncFunc: func(p pod) { mu.Lock(); syncs++; mu.Unlock() },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go inf.Run(ctx)

	// 等第一轮 Sync 确实被消费（否则断线后第二轮 List 的 Sync 会和
	// 队列里未消费的第一轮 Sync 去重合并，测的就不是重列而是压缩了）
	eventually(t, "first sync consumed", func() bool { mu.Lock(); defer mu.Unlock(); return syncs == 1 })

	lw.closeCurrent() // 模拟断线

	// 断线后应重新 List + Watch，并再回放一次全量
	eventually(t, "watch retried", func() bool { lw.mu.Lock(); defer lw.mu.Unlock(); return lw.watchN >= 2 })
	eventually(t, "relisted and replayed", func() bool { mu.Lock(); defer mu.Unlock(); return syncs >= 2 })
}

// ---------- WorkQueue ----------

func TestWorkQueue_DedupAndDirtyWhileWorking(t *testing.T) {
	q := NewWorkQueue()
	q.Add("a")
	q.Add("a") // 未处理时重复投递：去重
	if q.Len() != 1 {
		t.Fatalf("Len = %d, want 1", q.Len())
	}
	key, _, err := q.Get()
	if err != nil || key != "a" {
		t.Fatalf("Get = %s,%v", key, err)
	}
	q.Add("a") // 处理中再投递：标 dirty，不重复占队
	if q.Len() != 0 {
		t.Fatalf("Len = %d, want 0 while working", q.Len())
	}
	q.Done("a") // Done 发现 dirty → 重新入队（不漏处理）
	if q.Len() != 1 {
		t.Fatalf("dirty key must be re-queued on Done, Len = %d", q.Len())
	}
	q.Get()
	q.Done("a")
	if q.Len() != 0 {
		t.Fatalf("clean Done must not re-queue, Len = %d", q.Len())
	}
}

func TestWorkQueue_FailCountsAttempts(t *testing.T) {
	q := NewWorkQueue()
	q.Add("a")
	q.Get()
	q.Fail("a")
	_, attempts, _ := q.Get()
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	q.Fail("a")
	_, attempts, _ = q.Get()
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	q.Forget("a")
	q.Done("a")
	q.Add("a")
	_, attempts, _ = q.Get()
	if attempts != 0 {
		t.Fatalf("Forget must reset attempts, got %d", attempts)
	}
}

func TestWorkQueue_BlocksUntilClose(t *testing.T) {
	q := NewWorkQueue()
	done := make(chan error, 1)
	go func() {
		_, _, err := q.Get()
		done <- err
	}()
	select {
	case <-done:
		t.Fatal("Get should block on empty queue")
	case <-time.After(100 * time.Millisecond):
	}
	q.Close()
	if err := <-done; err != ErrClosed {
		t.Fatalf("after Close, Get must return ErrClosed, got %v", err)
	}
}

// 防御：Store.List 返回快照，改它不影响内部状态。
func TestStore_ListReturnsSnapshot(t *testing.T) {
	s := NewStore[pod]()
	s.set("a", pod{"a", "v1"})
	list := s.List()
	list[0].image = "hacked"
	if v, _ := s.Get("a"); v.image != "v1" {
		t.Fatal("store must not be affected by mutating the snapshot... (value types make this automatic)")
	}
	keys := []string{}
	for _, p := range s.List() {
		keys = append(keys, p.name)
	}
	sort.Strings(keys)
	if len(keys) != 1 || keys[0] != "a" {
		t.Fatalf("unexpected store contents: %v", keys)
	}
}
