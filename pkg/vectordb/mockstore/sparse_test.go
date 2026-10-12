package mockstore

import (
	"context"
	"testing"

	"github.com/aegis/pkg/mockx"
	"github.com/aegis/pkg/vectordb"
)

// toVDB 把 mockx.SparseToken 转为 vectordb.SparseToken（测试辅助）。
func toVDB(in []mockx.SparseToken) []vectordb.SparseToken {
	if len(in) == 0 {
		return nil
	}
	out := make([]vectordb.SparseToken, len(in))
	for i, t := range in {
		out[i] = vectordb.SparseToken{Index: t.Index, Value: t.Value}
	}
	return out
}

// TestSparseScore 词项内积：共享词得分，无共享词 0 分。
func TestSparseScore(t *testing.T) {
	q := []vectordb.SparseToken{{Index: 1, Value: 0.5}, {Index: 3, Value: 0.8}}
	// 完全重叠
	if got := sparseScore(q, []vectordb.SparseToken{{Index: 1, Value: 0.5}, {Index: 3, Value: 0.8}}); got < 0.4 {
		t.Errorf("完全重叠应得分: %v", got)
	}
	// 部分重叠
	got := sparseScore(q, []vectordb.SparseToken{{Index: 3, Value: 0.8}, {Index: 7, Value: 0.2}})
	if got < 0.6 || got > 0.7 {
		t.Errorf("部分重叠得分不符: %v", got)
	}
	// 无共享词
	if got := sparseScore(q, []vectordb.SparseToken{{Index: 9, Value: 1}}); got != 0 {
		t.Errorf("无共享词应为 0: %v", got)
	}
	// 空输入
	if got := sparseScore(nil, nil); got != 0 {
		t.Errorf("空输入应为 0: %v", got)
	}
}

// TestSearchSparse 稀疏检索：术语命中的切片按 IP 内积召回。
func TestSearchSparse(t *testing.T) {
	s := New()
	ctx := context.Background()
	if err := s.EnsureCollection(ctx, "c", 4); err != nil {
		t.Fatalf("EnsureCollection 失败: %v", err)
	}

	// 两个切片：一个含"检索"，一个不含
	withTerm := toVDB(mockx.SparseFromText("检索增强生成 知识库"))
	withoutTerm := toVDB(mockx.SparseFromText("天气晴朗 适合散步"))
	if err := s.Upsert(ctx, "c", []*vectordb.Chunk{
		{ChunkID: "a", KBID: 1, DocID: 1, Vector: make([]float32, 4), SparseVector: withTerm},
		{ChunkID: "b", KBID: 1, DocID: 2, Vector: make([]float32, 4), SparseVector: withoutTerm},
	}); err != nil {
		t.Fatalf("Upsert 失败: %v", err)
	}

	query := toVDB(mockx.SparseFromText("检索增强"))
	res, err := s.Search(ctx, &vectordb.SearchRequest{
		Collection: "c", SparseVector: query, TopK: 5,
	})
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}
	if len(res.ChunkIDs) == 0 {
		t.Fatal("术语命中的切片应被稀疏检索召回")
	}
	// 含共享词的切片应排最前（分数更高）；
	// 无共享词切片可能因稀疏哈希碰撞产生极低分，允许出现但在后面
	if res.ChunkIDs[0] != "a" {
		t.Errorf("含共享词的切片应排最前: %v", res.ChunkIDs)
	}
	if res.Scores["a"] <= res.Scores["b"] {
		t.Errorf("共享词切片的分数应高于无共享词切片: a=%v b=%v", res.Scores["a"], res.Scores["b"])
	}
}

// TestSearchSparseSharedWord 稀疏向量生成确定性：相同词 → 相同位置。
func TestSearchSparseSharedWord(t *testing.T) {
	a := mockx.SparseFromText("混合检索 向量")
	b := mockx.SparseFromText("混合检索 稀疏")
	// 共享"混合检索" → 应至少有一个相同位置
	shared := false
	for _, ta := range a {
		for _, tb := range b {
			if ta.Index == tb.Index {
				shared = true
			}
		}
	}
	if !shared {
		t.Error("共享词应映射到相同稀疏位置（确定性哈希）")
	}
}
