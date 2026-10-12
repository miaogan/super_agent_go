// Package mockstore 提供内存向量存储实现。
//
// 用途：冒烟测试与本地开发 —— 无需 Milvus 即可完整验证
// "入库 → 向量化 → 检索 → 命中 → 引用" 全链路。
//
// 与真实 Milvus 的差异（必须在测试中留意）：
//   - 无持久化：进程重启数据丢失
//   - 检索为全量暴力比对：结果语义与 HNSW 近似检索一致（都取 top-k），
//     但耗时特征不同（mock 随数据量线性增长）
//   - 不支持标量索引：过滤靠内存遍历
//
// 这些差异不影响链路正确性验证，因此 mock 模式足以作为冒烟基线。
package mockstore

import (
	"context"
	"sort"
	"sync"

	"github.com/aegis/pkg/mockx"
	"github.com/aegis/pkg/vectordb"
)

// Store 内存向量存储
type Store struct {
	mu sync.RWMutex
	// collection → dim
	dims map[string]int
	// collection → chunkID → chunk
	data map[string]map[string]*vectordb.Chunk

	// failMode 模拟 Milvus 故障（验证降级链）
	failMode bool
	closed   bool
}

// New 构造内存向量存储
func New() *Store {
	return &Store{
		dims: make(map[string]int),
		data: make(map[string]map[string]*vectordb.Chunk),
	}
}

var _ vectordb.Store = (*Store)(nil)

// SetFailMode 开关故障模拟。
//
// 冒烟脚本用它验证降级：Milvus 挂掉时检索是否自动退化为关键词检索。
func (s *Store) SetFailMode(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failMode = v
}

// EnsureCollection 确保集合存在
func (s *Store) EnsureCollection(_ context.Context, collection string, dim int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.failMode {
		return vectordb.ErrUnavailable
	}
	if exist, ok := s.dims[collection]; ok {
		if exist != dim {
			// ★ 维度不一致必须报错：真实 Milvus 中 schema 冲突
			//   若静默重建会导致已入库向量全部丢失
			return vectordb.ErrDimMismatch
		}
		return nil
	}
	s.dims[collection] = dim
	s.data[collection] = make(map[string]*vectordb.Chunk)
	return nil
}

// Upsert 写入向量
func (s *Store) Upsert(_ context.Context, collection string, chunks []*vectordb.Chunk) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.failMode {
		return vectordb.ErrUnavailable
	}
	if err := s.validateDim(collection, chunks); err != nil {
		return err
	}
	for _, c := range chunks {
		if c == nil || c.ChunkID == "" {
			continue
		}
		cp := *c
		s.data[collection][c.ChunkID] = &cp
	}
	return nil
}

// validateDim 校验所有 chunk 的向量维度与集合一致。
func (s *Store) validateDim(collection string, chunks []*vectordb.Chunk) error {
	dim, ok := s.dims[collection]
	if !ok {
		return vectordb.ErrUnavailable
	}
	for _, c := range chunks {
		if c != nil && len(c.Vector) != 0 && len(c.Vector) != dim {
			return vectordb.ErrDimMismatch
		}
	}
	return nil
}

// DeleteByDoc 删除文档向量
func (s *Store) DeleteByDoc(_ context.Context, collection string, docID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteLocked(collection, func(c *vectordb.Chunk) bool { return c.DocID == docID })
}

// DeleteByKB 删除知识库向量
func (s *Store) DeleteByKB(_ context.Context, collection string, kbID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteLocked(collection, func(c *vectordb.Chunk) bool { return c.KBID == kbID })
}

func (s *Store) deleteLocked(collection string, match func(*vectordb.Chunk) bool) error {
	if s.failMode {
		return vectordb.ErrUnavailable
	}
	m, ok := s.data[collection]
	if !ok {
		return nil
	}
	for id, c := range m {
		if match(c) {
			delete(m, id)
		}
	}
	return nil
}

// Search 向量检索
//
// 实现为全量暴力余弦比对 —— 结果语义等价于 HNSW 近似检索的 top-k，
// 保证冒烟测试中的排序断言是可靠的。
func (s *Store) Search(_ context.Context, req *vectordb.SearchRequest) (*vectordb.SearchResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.failMode {
		return nil, vectordb.ErrUnavailable
	}
	m, ok := s.data[req.Collection]
	if !ok {
		// 集合不存在 = 无数据，不是错误
		return &vectordb.SearchResult{Scores: map[string]float64{}}, nil
	}

	kbFilter := toSet(req.KBIDs)
	docFilter := toSet(req.ExcludeDocIDs)

	type hit struct {
		id    string
		score float64
	}
	hits := make([]hit, 0, len(m))

	for id, c := range m {
		if len(kbFilter) > 0 && !kbFilter[c.KBID] {
			continue
		}
		if req.Tenant != "" && c.Tenant != "" && c.Tenant != req.Tenant {
			continue
		}
		if len(docFilter) > 0 && docFilter[c.DocID] {
			continue
		}
		var score float64
		if len(req.SparseVector) > 0 {
			// 稀疏检索：词项权重内积（与 Milvus IP 语义一致）
			score = sparseScore(req.SparseVector, c.SparseVector)
		} else {
			score = mockx.Cosine(req.Vector, c.Vector)
		}
		if score < req.ScoreThreshold {
			continue
		}
		hits = append(hits, hit{id: id, score: score})
	}

	// 按分数降序；同分按 ID 保证结果稳定可复现
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].id < hits[j].id
	})

	topK := req.TopK
	if topK <= 0 {
		topK = 5
	}
	if len(hits) > topK {
		hits = hits[:topK]
	}

	out := &vectordb.SearchResult{
		Scores:   make(map[string]float64, len(hits)),
		ChunkIDs: make([]string, 0, len(hits)),
	}
	for _, h := range hits {
		out.Scores[h.id] = h.score
		out.ChunkIDs = append(out.ChunkIDs, h.id)
	}
	return out, nil
}

// QueryByIDs 按 ID 取向量
func (s *Store) QueryByIDs(_ context.Context, collection string, ids []string) ([]*vectordb.Chunk, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.failMode {
		return nil, vectordb.ErrUnavailable
	}
	m, ok := s.data[collection]
	if !ok {
		return nil, nil
	}
	out := make([]*vectordb.Chunk, 0, len(ids))
	for _, id := range ids {
		if c, ok := m[id]; ok {
			cp := *c
			out = append(out, &cp)
		}
	}
	return out, nil
}

// Ping 健康检查
func (s *Store) Ping(_ context.Context) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return vectordb.ErrUnavailable
	}
	if s.failMode {
		return vectordb.ErrUnavailable
	}
	return nil
}

// Close 关闭
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// Stats 统计信息
func (s *Store) Stats() *vectordb.Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var total int64
	for _, m := range s.data {
		total += int64(len(m))
	}
	// 多集合时只报告首个，冒烟场景足够
	dim := 0
	for _, d := range s.dims {
		dim = d
		break
	}
	return &vectordb.Stats{
		Backend:    "mock",
		Collection: firstKey(s.data),
		Dim:        dim,
		Entities:   total,
		Healthy:    !s.failMode && !s.closed,
	}
}

func toSet(vals []int64) map[int64]bool {
	if len(vals) == 0 {
		return nil
	}
	out := make(map[int64]bool, len(vals))
	for _, v := range vals {
		out[v] = true
	}
	return out
}

func firstKey(m map[string]map[string]*vectordb.Chunk) string {
	for k := range m {
		return k
	}
	return ""
}
