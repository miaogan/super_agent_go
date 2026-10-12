package vectordb

import (
	"context"
	"errors"
	"testing"
)

// ---------- SyncAfterWrite ----------

// plainStore 只实现 Store，不实现 Syncer。
// 用来验证「后端不支持同步」时的降级行为。
type plainStore struct {
	Store // 嵌入 nil 接口：测试不会调到它的方法
}

// syncingStore 同时实现 Store 与 Syncer。
type syncingStore struct {
	Store
	synced  []string // 记录被调用过 Sync 的集合
	syncErr error
}

func (s *syncingStore) Sync(_ context.Context, collection string) error {
	if s.syncErr != nil {
		return s.syncErr
	}
	s.synced = append(s.synced, collection)
	return nil
}

// ★ 核心用例：不支持 Sync 的后端必须被静默跳过，而不是报错。
//
//	这正是 mockstore 的处境 —— 它写入即可见，
//	没有「同步」这个概念。冒烟测试全部走这条路，
//	若这里返回错误，整个 mock 模式的入库流程都会崩。
func TestSyncAfterWriteSkipsNonSyncingStore(t *testing.T) {
	var s plainStore
	if err := SyncAfterWrite(context.Background(), &s, "c1"); err != nil {
		t.Fatalf("不支持 Sync 的后端应返回 nil，实际 %v", err)
	}
}

// 支持 Sync 的后端必须被真正调用，且集合名原样透传。
func TestSyncAfterWriteCallsSyncer(t *testing.T) {
	s := &syncingStore{}
	if err := SyncAfterWrite(context.Background(), s, "aegis_kb_42"); err != nil {
		t.Fatalf("Sync 调用不应失败，实际 %v", err)
	}
	if len(s.synced) != 1 || s.synced[0] != "aegis_kb_42" {
		t.Fatalf("Sync 未被正确调用，实际 %v", s.synced)
	}
}

// ★ Sync 的错误必须上抛，但由调用方决定严重程度。
//
//	上抛是为了让调用方能记录告警；
//	不阻断入库是调用方的选择（见 ingest/pipeline.go）。
//
//	注意这与上面的「不支持 Sync 返回 nil」不矛盾：
//	一个是「没有这个能力」，一个是「有能力但失败了」，
//	必须区分 —— 否则真实故障会被当成「该后端不需要同步」而静默吞掉。
func TestSyncAfterWritePropagatesError(t *testing.T) {
	want := errors.New("flush 超时")
	s := &syncingStore{syncErr: want}
	if err := SyncAfterWrite(context.Background(), s, "c1"); !errors.Is(err, want) {
		t.Fatalf("Sync 的错误应原样上抛，实际 %v", err)
	}
}

// 补偿任务会对同一集合多次调用，Sync 必须被多次透传
// （幂等性由实现方保证，这里只验证调用次数不被吞）。
func TestSyncAfterWriteRepeatedCalls(t *testing.T) {
	s := &syncingStore{}
	for i := 0; i < 3; i++ {
		if err := SyncAfterWrite(context.Background(), s, "c1"); err != nil {
			t.Fatalf("第 %d 次 Sync 失败: %v", i+1, err)
		}
	}
	if len(s.synced) != 3 {
		t.Fatalf("期望 3 次 Sync 调用，实际 %d 次", len(s.synced))
	}
}
