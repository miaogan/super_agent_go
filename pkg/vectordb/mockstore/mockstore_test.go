package mockstore

import (
	"context"
	"testing"

	"github.com/aegis/pkg/mockx"
	"github.com/aegis/pkg/vectordb"
)

// TestSearchEndToEnd 验证 "写入 → 检索 → 命中" 的完整链路。
//
// 背景：曾出现入库显示成功、检索恒为 0 命中的问题，
// 根因在向量生成侧（随机向量无语义）。
// 此测试确保 mockstore 自身不会成为问题来源。
func TestSearchEndToEnd(t *testing.T) {
	const dim = 1024
	ctx := context.Background()
	s := New()
	coll := "aegis_kb_2001"

	if err := s.EnsureCollection(ctx, coll, dim); err != nil {
		t.Fatalf("EnsureCollection: %v", err)
	}

	texts := []string{
		"Milvus 的向量维度是多少？知识库使用 1024 维向量，配置项为 embedding_dim。",
		"平台部署在 K8s，网关负责路由，Nacos 提供注册中心与配置中心。",
	}
	chunks := make([]*vectordb.Chunk, 0, len(texts))
	for i, txt := range texts {
		chunks = append(chunks, &vectordb.Chunk{
			ChunkID: string(rune('a' + i)),
			KBID:    2001,
			DocID:   3001,
			Seq:     int32(i),
			Content: txt,
			Vector:  mockx.FakeEmbedding(dim, txt),
		})
	}
	if err := s.Upsert(ctx, coll, chunks); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	qv := mockx.FakeEmbedding(dim, "Milvus 的向量维度是多少")
	res, err := s.Search(ctx, &vectordb.SearchRequest{
		Collection: coll, Vector: qv, TopK: 3, ScoreThreshold: 0.1,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.ChunkIDs) == 0 {
		t.Fatal("检索返回空：向量无语义或阈值不合理")
	}

	// top-1 必须是语义相关的那片
	if res.ChunkIDs[0] != "a" {
		t.Errorf("top-1 应为相关切片 a，实际=%s (score=%.4f)",
			res.ChunkIDs[0], res.Scores[res.ChunkIDs[0]])
	}

	// 分数必须递减
	for i := 1; i < len(res.ChunkIDs); i++ {
		if res.Scores[res.ChunkIDs[i]] > res.Scores[res.ChunkIDs[i-1]] {
			t.Errorf("结果未按分数降序：第 %d 项 %.4f > 第 %d 项 %.4f",
				i, res.Scores[res.ChunkIDs[i]], i-1, res.Scores[res.ChunkIDs[i-1]])
		}
	}
}

// TestFailModeForcesDegradation 验证故障注入生效：
// 向量库不可用时必须返回 ErrUnavailable，让上层能降级到关键词检索。
func TestFailModeForcesDegradation(t *testing.T) {
	ctx := context.Background()
	s := New()
	coll := "aegis_kb_1"

	if err := s.EnsureCollection(ctx, coll, 1024); err != nil {
		t.Fatal(err)
	}
	s.SetFailMode(true)

	// 三个操作都必须报不可用，否则上层无法感知降级
	if _, err := s.Search(ctx, &vectordb.SearchRequest{Collection: coll}); err != vectordb.ErrUnavailable {
		t.Errorf("Search 应返回 ErrUnavailable，实际=%v", err)
	}
	if err := s.Upsert(ctx, coll, nil); err != vectordb.ErrUnavailable {
		t.Errorf("Upsert 应返回 ErrUnavailable，实际=%v", err)
	}
	if err := s.EnsureCollection(ctx, coll, 1024); err != vectordb.ErrUnavailable {
		t.Errorf("EnsureCollection 应返回 ErrUnavailable，实际=%v", err)
	}
}

// TestDimMismatchRejected 确认维度不一致时报错而非静默接受：
// 真实 Milvus 中 schema 冲突若被忽略，会导致已入库向量全部失真。
func TestDimMismatchRejected(t *testing.T) {
	ctx := context.Background()
	s := New()

	if err := s.EnsureCollection(ctx, "c1", 1024); err != nil {
		t.Fatal(err)
	}
	// 用不同维度重建
	if err := s.EnsureCollection(ctx, "c1", 768); err != vectordb.ErrDimMismatch {
		t.Errorf("维度冲突应返回 ErrDimMismatch，实际=%v", err)
	}
	// 写入维度不符的向量
	err := s.Upsert(ctx, "c1", []*vectordb.Chunk{
		{ChunkID: "x", Vector: make([]float32, 512)},
	})
	if err != vectordb.ErrDimMismatch {
		t.Errorf("写入维度不符应返回 ErrDimMismatch，实际=%v", err)
	}
}

// TestSearchEmptyCollection 空集合应返回空结果而非错误 ——
// "无数据"是正常业务状态，不是故障。
func TestSearchEmptyCollection(t *testing.T) {
	ctx := context.Background()
	s := New()
	res, err := s.Search(ctx, &vectordb.SearchRequest{Collection: "not_exist"})
	if err != nil {
		t.Fatalf("集合不存在不应报错：%v", err)
	}
	if len(res.ChunkIDs) != 0 {
		t.Errorf("空集合应返回 0 条，实际 %d", len(res.ChunkIDs))
	}
}

// TestExcludeDocIDs 验证排除文档生效（避免重复召回已引用过的文档）
func TestExcludeDocIDs(t *testing.T) {
	const dim = 1024
	ctx := context.Background()
	s := New()
	coll := "c"
	if err := s.EnsureCollection(ctx, coll, dim); err != nil {
		t.Fatal(err)
	}
	txt := "重复召回测试文档内容"
	if err := s.Upsert(ctx, coll, []*vectordb.Chunk{
		{ChunkID: "a", KBID: 1, DocID: 111, Vector: mockx.FakeEmbedding(dim, txt)},
	}); err != nil {
		t.Fatal(err)
	}

	res, err := s.Search(ctx, &vectordb.SearchRequest{
		Collection: coll, Vector: mockx.FakeEmbedding(dim, txt),
		TopK: 5, ExcludeDocIDs: []int64{111},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ChunkIDs) != 0 {
		t.Errorf("被排除的文档不应召回，实际=%v", res.ChunkIDs)
	}
}

// TestTenantIsolation 验证租户隔离。
func TestTenantIsolation(t *testing.T) {
	const dim = 1024
	ctx := context.Background()
	s := New()
	coll := "c"
	if err := s.EnsureCollection(ctx, coll, dim); err != nil {
		t.Fatal(err)
	}
	txt := "租户隔离测试"
	if err := s.Upsert(ctx, coll, []*vectordb.Chunk{
		{ChunkID: "a", KBID: 1, DocID: 1, Tenant: "t1", Vector: mockx.FakeEmbedding(dim, txt)},
	}); err != nil {
		t.Fatal(err)
	}

	// 租户 t2 不应看到 t1 的数据
	res, err := s.Search(ctx, &vectordb.SearchRequest{
		Collection: coll, Vector: mockx.FakeEmbedding(dim, txt),
		TopK: 5, Tenant: "t2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ChunkIDs) != 0 {
		t.Errorf("跨租户泄漏：t2 检索到 %v", res.ChunkIDs)
	}

	// 本租户应可见
	res, err = s.Search(ctx, &vectordb.SearchRequest{
		Collection: coll, Vector: mockx.FakeEmbedding(dim, txt),
		TopK: 5, Tenant: "t1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ChunkIDs) == 0 {
		t.Error("本租户应能检索到自己的数据")
	}
}
