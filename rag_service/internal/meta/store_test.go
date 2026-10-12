package meta

import (
	"context"
	"fmt"
	"sync"
	"testing"

	ragv1 "github.com/aegis/idl/kitex_gen/ragv1"
)

// TestConcurrentWriteAndRead 是 data race 的回归防线。
//
// 背景：GetDoc 曾返回浅拷贝（cp.Doc 仍指向 map 内对象），
// 而 ingest.Ingest 会在锁外写 rec.Doc.ChunkCount，
// ListDocs 又在 RLock 下读同一对象 —— 并发上传即可触发竞态。
//
// 用 -race 运行本测试即可捕获回归。
func TestConcurrentWriteAndRead(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()

	kb := &ragv1.KnowledgeBase{
		KbId: 1, Name: "并发测试库", Tenant: "t1", OwnerUid: 1001,
		EmbeddingDim: 1024,
	}
	if err := s.CreateKB(ctx, kb); err != nil {
		t.Fatal(err)
	}

	const docN = 8
	for i := 0; i < docN; i++ {
		if err := s.CreateDoc(ctx, &DocumentRecord{
			Doc: &ragv1.Document{
				DocId: int64(100 + i), KbId: 1,
				FileName: fmt.Sprintf("doc-%d.md", i),
				Status:   int32(DocPending),
			},
			RawBytes: []byte("内容"),
		}); err != nil {
			t.Fatal(err)
		}
	}

	// 每个协程：取文档 → 在锁外改字段（模拟 ingest 回填）
	// 同时其他协程持续 ListDocs / GetDoc / ListChunks
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 读者
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				s.ListDocs(ctx, 1, 0, "", 1, 100)
				for i := 0; i < docN; i++ {
					s.GetDoc(ctx, int64(100+i))
				}
				s.ListKB(ctx, "", "t1", 0, 1, 100)
			}
		}()
	}

	// 写者：模拟 ingest 回填
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for i := 0; i < docN; i++ {
					rec, err := s.GetDoc(ctx, int64(100+i))
					if err != nil {
						continue
					}
					// 这两行正是 ingest.Ingest 的真实行为：锁外写
					rec.Doc.ChunkCount = int32(i)
					rec.Doc.ParsedChunks = int32(i)
					rec.Chunks = append(rec.Chunks, &ragv1.Chunk{
						ChunkId: fmt.Sprintf("c-%d", i), DocId: rec.Doc.DocId, Content: "x",
					})
				}
			}
		}()
	}

	// 跑一小段时间即可，不必等所有 goroutine 自然结束
	for i := 0; i < 200; i++ {
		s.RecountKB(ctx, 1)
	}
	close(stop)
	wg.Wait()
}

// TestGetDocIsolation 确认 GetDoc 返回的是独立副本，
// 修改返回值不会污染存储中的对象。
func TestGetDocIsolation(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()

	if err := s.CreateKB(ctx, &ragv1.KnowledgeBase{KbId: 1, Name: "kb", Tenant: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateDoc(ctx, &DocumentRecord{
		Doc:      &ragv1.Document{DocId: 1, KbId: 1, FileName: "a.md"},
		RawBytes: []byte("c"),
	}); err != nil {
		t.Fatal(err)
	}

	rec1, err := s.GetDoc(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	rec1.Doc.ChunkCount = 999
	rec1.Doc.FileName = "篡改.md"

	rec2, err := s.GetDoc(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if rec2.Doc.ChunkCount == 999 {
		t.Error("★ 修改副本污染了存储中的对象（浅拷贝）")
	}
	if rec2.Doc.FileName == "篡改.md" {
		t.Error("★ FileName 被污染")
	}
}

// TestListDocsIsolation 同理：列表返回的对象也必须是独立副本。
func TestListDocsIsolation(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()

	if err := s.CreateKB(ctx, &ragv1.KnowledgeBase{KbId: 1, Name: "kb", Tenant: "t"}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		if err := s.CreateDoc(ctx, &DocumentRecord{
			Doc:      &ragv1.Document{DocId: int64(i), KbId: 1, FileName: fmt.Sprintf("d%d.md", i)},
			RawBytes: []byte("c"),
		}); err != nil {
			t.Fatal(err)
		}
	}

	docs, total := s.ListDocs(ctx, 1, 0, "", 1, 10)
	if total != 3 {
		t.Fatalf("总数应为 3，实际 %d", total)
	}
	for _, d := range docs {
		d.ChunkCount = 888
	}

	docs2, _ := s.ListDocs(ctx, 1, 0, "", 1, 10)
	for _, d := range docs2 {
		if d.ChunkCount == 888 {
			t.Error("★ ListDocs 返回的对象被外部修改污染")
		}
	}
}

// TestListChunksIsolation 切片同样必须隔离。
func TestListChunksIsolation(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()

	if err := s.CreateKB(ctx, &ragv1.KnowledgeBase{KbId: 1, Name: "kb", Tenant: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateDoc(ctx, &DocumentRecord{
		Doc:      &ragv1.Document{DocId: 1, KbId: 1},
		RawBytes: []byte("c"),
	}); err != nil {
		t.Fatal(err)
	}
	chunks := []*ragv1.Chunk{
		{ChunkId: "c1", KbId: 1, DocId: 1, Seq: 0, Content: "正文一", VecStatus: int32(VecReady)},
		{ChunkId: "c2", KbId: 1, DocId: 1, Seq: 1, Content: "正文二", VecStatus: int32(VecPending)},
	}
	if err := s.UpdateChunks(ctx, 1, chunks); err != nil {
		t.Fatal(err)
	}

	got, total := s.ListChunks(ctx, 1, 1, 10)
	if total != 2 {
		t.Fatalf("切片数应为 2，实际 %d", total)
	}
	for _, c := range got {
		c.Content = "篡改"
		c.VecStatus = 999
	}

	// 入参对象也不应被后续修改影响存储
	chunks[0].Content = "外部改动"

	got2, _ := s.ListChunks(ctx, 1, 1, 10)
	for _, c := range got2 {
		if c.Content == "篡改" || c.Content == "外部改动" {
			t.Errorf("★ 切片内容被污染: %q", c.Content)
		}
		if c.VecStatus == 999 {
			t.Error("★ VecStatus 被污染")
		}
	}
}

// TestPendingChunkFilter 确认补偿任务只捞待补偿切片。
func TestPendingChunkFilter(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()

	if err := s.CreateKB(ctx, &ragv1.KnowledgeBase{KbId: 1, Name: "kb"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateDoc(ctx, &DocumentRecord{
		Doc:      &ragv1.Document{DocId: 1, KbId: 1},
		RawBytes: []byte("c"),
	}); err != nil {
		t.Fatal(err)
	}
	all := []*ragv1.Chunk{
		{ChunkId: "c1", DocId: 1, Content: "a", VecStatus: int32(VecReady)},
		{ChunkId: "c2", DocId: 1, Content: "b", VecStatus: int32(VecPending)},
		{ChunkId: "c3", DocId: 1, Content: "c", VecStatus: int32(VecFailed)},
		{ChunkId: "c4", DocId: 1, Content: "d", VecStatus: int32(VecReady)},
	}
	if err := s.UpdateChunks(ctx, 1, all); err != nil {
		t.Fatal(err)
	}

	pending, err := s.ListPendingChunks(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Errorf("待补偿切片应为 2（pending + failed），实际 %d", len(pending))
	}
	got := map[string]bool{}
	for _, c := range pending {
		got[c.ChunkId] = true
	}
	if !got["c2"] || !got["c3"] {
		t.Errorf("应包含 c2/c3，实际 %v", got)
	}
	if got["c1"] || got["c4"] {
		t.Errorf("不应包含已就绪的切片，实际 %v", got)
	}

	// batch 限制
	limited, _ := s.ListPendingChunks(ctx, 1)
	if len(limited) != 1 {
		t.Errorf("batch=1 应只返回 1 条，实际 %d", len(limited))
	}
}

// TestTenantIsolationInStore 确认租户隔离在存储层生效。
func TestTenantIsolationInStore(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()

	for i, tenant := range []string{"t1", "t2"} {
		if err := s.CreateKB(ctx, &ragv1.KnowledgeBase{
			KbId: int64(i + 1), Name: tenant, Tenant: tenant,
		}); err != nil {
			t.Fatal(err)
		}
	}

	kbs1, _ := s.ListKB(ctx, "", "t1", 0, 1, 10)
	if len(kbs1) != 1 || kbs1[0].Tenant != "t1" {
		t.Errorf("t1 应只看到自己的 1 个知识库，实际 %d", len(kbs1))
	}
	kbs2, _ := s.ListKB(ctx, "", "t2", 0, 1, 10)
	if len(kbs2) != 1 || kbs2[0].Tenant != "t2" {
		t.Errorf("t2 应只看到自己的 1 个知识库，实际 %d", len(kbs2))
	}
}
