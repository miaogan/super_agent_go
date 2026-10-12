// Package meta 定义知识库元数据存储抽象。
//
// 数据分治（ADR-006）：
//   - PostgreSQL（此处抽象）：知识库 / 文档 / 切片正文 —— 内容真相源
//   - Milvus（vectordb.Store）：向量 + 检索必需标量
//
// 为什么正文只在 PG、向量只在 Milvus：
//
//	正文需要事务、模糊检索、权限过滤，这些 Milvus 做不了；
//	向量检索需要 HNSW 索引，PG 做不了。各自用擅长的存储。
//
// 最终一致：切片先落 PG（vec_status=0），向量化成功后写 Milvus
// 并回写 vec_status=1；失败置 2 并由补偿任务重试。
package meta

import (
	"context"
	"strings"
	"sync"

	ragv1 "github.com/aegis/idl/kitex_gen/ragv1"
)

// VecStatus 切片向量状态
type VecStatus int32

const (
	VecPending VecStatus = 0 // 待写向量
	VecReady   VecStatus = 1 // 已写入向量
	VecFailed  VecStatus = 2 // 写入失败，待补偿
)

// DocStatus 文档状态
type DocStatus int32

const (
	DocPending DocStatus = 1 // 待解析
	DocReady   DocStatus = 2 // 就绪
	DocFailed  DocStatus = 3 // 失败
	DocPartial DocStatus = 4 // 部分成功
)

// DocumentRecord 文档内部记录（含正文，外部不可见）
type DocumentRecord struct {
	Doc      *ragv1.Document
	Chunks   []*ragv1.Chunk
	RawBytes []byte
	// ObjectKey MinIO 桶内路径（MinIO 直存时由 UploadDocument 写入）
	ObjectKey string
}

// ErrNotFound 未找到
type ErrNotFound struct{ Msg string }

func (e *ErrNotFound) Error() string { return e.Msg }

// ErrConflict 冲突
type ErrConflict struct{ Msg string }

func (e *ErrConflict) Error() string { return e.Msg }

// Store 元数据存储
type Store interface {
	CreateKB(ctx context.Context, kb *ragv1.KnowledgeBase) error
	GetKB(ctx context.Context, kbID int64) (*ragv1.KnowledgeBase, error)
	ListKB(ctx context.Context, keyword, tenant string, ownerUID int64, page, size int) ([]*ragv1.KnowledgeBase, int)
	UpdateKB(ctx context.Context, kb *ragv1.KnowledgeBase, mask []string) error
	DeleteKB(ctx context.Context, kbID int64) error

	CreateDoc(ctx context.Context, rec *DocumentRecord) error
	GetDoc(ctx context.Context, docID int64) (*DocumentRecord, error)
	ListDocs(ctx context.Context, kbID int64, status int32, keyword string, page, size int) ([]*ragv1.Document, int)
	DeleteDoc(ctx context.Context, docID int64) error
	UpdateDocStatus(ctx context.Context, docID int64, status DocStatus, errMsg string) error

	// UpdateChunks 写入/更新切片
	UpdateChunks(ctx context.Context, docID int64, chunks []*ragv1.Chunk) error
	// ListChunks 分页读切片
	ListChunks(ctx context.Context, docID int64, page, size int) ([]*ragv1.Chunk, int)
	// GetChunksByIDs 按 ID 取切片（检索命中后回 PG 取正文）
	GetChunksByIDs(ctx context.Context, chunkIDs []string) ([]*ragv1.Chunk, error)
	// Neighbors 取相邻切片（上下文外扩）
	Neighbors(ctx context.Context, docID int64, seq int32, before, after int) ([]*ragv1.Chunk, error)
	// MarkVecStatus 回写切片向量状态
	MarkVecStatus(ctx context.Context, chunkIDs []string, status VecStatus) error
	// ListPendingChunks 取待补偿的切片
	ListPendingChunks(ctx context.Context, batch int) ([]*ragv1.Chunk, error)
	// DecrKBCount 知识库删除后修正计数
	RecountKB(ctx context.Context, kbID int64) error

	BindAgentKB(ctx context.Context, agentID int64, kbIDs []int64) error
	ListAgentKB(ctx context.Context, agentID int64) ([]*ragv1.KnowledgeBase, error)

	// KeywordSearch PostgreSQL 关键词降级检索
	KeywordSearch(ctx context.Context, query string, kbIDs []int64, tenant string, topK int) ([]*KeywordHit, error)

	// NextKBID 分配知识库 ID。
	// ID 由存储层分配而非 DB 自增回填，原因：CreateKB 前需要
	// 先用 kb_id 预建 Milvus Collection（集合名含 kb_id）。
	// PG 实现为 nextval；内存实现为自增计数器。
	NextKBID(ctx context.Context) (int64, error)
	// NextDocID 分配文档 ID（同上）
	NextDocID(ctx context.Context) (int64, error)

	Stats() *Stats
}

// KeywordHit 关键词检索命中
type KeywordHit struct {
	ChunkID    string
	KBID       int64
	DocID      int64
	DocTitle   string
	Content    string
	PageNo     int32
	Score      float64
	TokenCount int32
}

// Stats 存储统计
type Stats struct {
	KBs     int
	Docs    int
	Chunks  int
	Pending int
	Failed  int
}

// MemoryStore 内存实现（mock 模式）
type MemoryStore struct {
	mu sync.RWMutex

	kbs  map[int64]*ragv1.KnowledgeBase
	docs map[int64]*DocumentRecord
	// chunkID → chunk
	chunks map[string]*ragv1.Chunk
	// docID → 排序后的 chunkID 列表
	docChunks map[int64][]string
	// agentID → kbIDs
	agentKBs map[int64][]int64

	nextKB  int64
	nextDoc int64
}

// NewMemoryStore 构造内存元数据库
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		kbs:       make(map[int64]*ragv1.KnowledgeBase),
		docs:      make(map[int64]*DocumentRecord),
		chunks:    make(map[string]*ragv1.Chunk),
		docChunks: make(map[int64][]string),
		agentKBs:  make(map[int64][]int64),
		nextKB:    2000,
		nextDoc:   3000,
	}
}

var _ Store = (*MemoryStore)(nil)

// NextKBID 分配知识库 ID（内存实现永不失败）
func (s *MemoryStore) NextKBID(_ context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextKB++
	return s.nextKB, nil
}

// NextDocID 分配文档 ID（内存实现永不失败）
func (s *MemoryStore) NextDocID(_ context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextDoc++
	return s.nextDoc, nil
}

// NextChunkID 分配切片 ID
func (s *MemoryStore) NextChunkID(docID int64, seq int) string {
	return FormatChunkID(docID, seq)
}

func (s *MemoryStore) CreateKB(_ context.Context, kb *ragv1.KnowledgeBase) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.kbs[kb.KbId]; ok {
		return &ErrConflict{Msg: "知识库已存在"}
	}
	// 深拷贝入库：调用方可能继续修改传入对象
	s.kbs[kb.KbId] = cloneKB(kb)
	return nil
}

// cloneKB 返回知识库副本。
//
// KnowledgeBase 当前全为值字段（无 slice/map），
// 浅拷贝即已足够隔离。保留此函数是为了将来新增字段时不至于漏改 ——
// 加字段时必须同步这里，否则会重新引入 data race。
func cloneKB(kb *ragv1.KnowledgeBase) *ragv1.KnowledgeBase {
	cp := *kb
	return &cp
}

func (s *MemoryStore) GetKB(_ context.Context, kbID int64) (*ragv1.KnowledgeBase, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	kb, ok := s.kbs[kbID]
	if !ok {
		return nil, &ErrNotFound{Msg: "知识库不存在"}
	}
	return cloneKB(kb), nil
}

func (s *MemoryStore) ListKB(_ context.Context, keyword, tenant string, ownerUID int64, page, size int) ([]*ragv1.KnowledgeBase, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	kw := strings.ToLower(strings.TrimSpace(keyword))
	var out []*ragv1.KnowledgeBase
	for _, kb := range s.kbs {
		if tenant != "" && kb.GetTenant() != "" && kb.GetTenant() != tenant {
			continue
		}
		if ownerUID != 0 && kb.GetOwnerUid() != ownerUID {
			continue
		}
		if kw != "" && !strings.Contains(strings.ToLower(kb.GetName()), kw) &&
			!strings.Contains(strings.ToLower(kb.GetDescription()), kw) {
			continue
		}
		out = append(out, cloneKB(kb))
	}
	sortKBs(out)
	return paginateKBs(out, page, size)
}

func (s *MemoryStore) UpdateKB(_ context.Context, kb *ragv1.KnowledgeBase, mask []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.kbs[kb.KbId]
	if !ok {
		return &ErrNotFound{Msg: "知识库不存在"}
	}
	if len(mask) == 0 {
		mask = []string{"name", "description", "chunk_size", "chunk_overlap", "splitter"}
	}
	for _, f := range mask {
		switch strings.ToLower(f) {
		case "name":
			cur.Name = kb.Name
		case "description":
			cur.Description = kb.Description
		case "chunk_size":
			cur.ChunkSize = kb.ChunkSize
		case "chunk_overlap":
			cur.ChunkOverlap = kb.ChunkOverlap
		case "splitter":
			cur.Splitter = kb.Splitter
		}
	}
	return nil
}

func (s *MemoryStore) DeleteKB(_ context.Context, kbID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.kbs[kbID]; !ok {
		return &ErrNotFound{Msg: "知识库不存在"}
	}
	delete(s.kbs, kbID)
	// 级联删除文档与切片
	for docID, rec := range s.docs {
		if rec.Doc.GetKbId() == kbID {
			for _, cid := range s.docChunks[docID] {
				delete(s.chunks, cid)
			}
			delete(s.docChunks, docID)
			delete(s.docs, docID)
		}
	}
	return nil
}

func (s *MemoryStore) CreateDoc(_ context.Context, rec *DocumentRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.docs[rec.Doc.DocId]; ok {
		return &ErrConflict{Msg: "文档已存在"}
	}
	cp := cloneDocRecord(rec)
	s.docs[rec.Doc.DocId] = cp
	return nil
}

// cloneDoc 返回文档副本。
//
// Document 当前全为值字段，浅拷贝即已完全隔离。
func cloneDoc(d *ragv1.Document) *ragv1.Document {
	if d == nil {
		return nil
	}
	cp := *d
	return &cp
}

// cloneChunk 返回切片副本。Chunk 当前全为值字段。
func cloneChunk(c *ragv1.Chunk) *ragv1.Chunk {
	if c == nil {
		return nil
	}
	cp := *c
	return &cp
}

// cloneDocRecord 深拷贝文档记录。
//
// ★这里的隔离点是 rec.Doc 指针与 rec.Chunks 切片：
//
//	`cp := *rec` 只复制结构体，cp.Doc 仍指向 map 里那一个对象。
//	而 ingest.Ingest 与 rag.go 会在锁外写rec.Doc.ChunkCount、
//	rec.Doc.ParsedChunks（前者 lock 外回填 rec.Chunks，后者回填统计），
//	ListDocs/ListChunks 又在 RLock 下读它们 —— 并发上传同一 KB 即触发 data race。
//
//	pg 生成的结构体本身全是值字段，所以只需复制指针与切片头即可完全隔离。
func cloneDocRecord(rec *DocumentRecord) *DocumentRecord {
	cp := *rec
	cp.Doc = cloneDoc(rec.Doc)
	if rec.Chunks != nil {
		cp.Chunks = make([]*ragv1.Chunk, 0, len(rec.Chunks))
		for _, c := range rec.Chunks {
			cp.Chunks = append(cp.Chunks, cloneChunk(c))
		}
	}
	return &cp
}

func (s *MemoryStore) GetDoc(_ context.Context, docID int64) (*DocumentRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.docs[docID]
	if !ok {
		return nil, &ErrNotFound{Msg: "文档不存在"}
	}
	return cloneDocRecord(rec), nil
}

func (s *MemoryStore) ListDocs(_ context.Context, kbID int64, status int32, keyword string, page, size int) ([]*ragv1.Document, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	kw := strings.ToLower(strings.TrimSpace(keyword))
	var out []*ragv1.Document
	for _, rec := range s.docs {
		d := rec.Doc
		if kbID != 0 && d.GetKbId() != kbID {
			continue
		}
		if status != 0 && d.GetStatus() != status {
			continue
		}
		if kw != "" && !strings.Contains(strings.ToLower(d.GetFileName()), kw) {
			continue
		}
		out = append(out, cloneDoc(d))
	}
	// 按 doc_id 升序，保证分页稳定
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].GetDocId() < out[i].GetDocId() {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return paginateDocs(out, page, size)
}

func (s *MemoryStore) DeleteDoc(_ context.Context, docID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.docs[docID]
	if !ok {
		return &ErrNotFound{Msg: "文档不存在"}
	}
	for _, cid := range s.docChunks[docID] {
		delete(s.chunks, cid)
	}
	delete(s.docChunks, docID)
	delete(s.docs, docID)
	_ = rec
	return nil
}

func (s *MemoryStore) UpdateDocStatus(_ context.Context, docID int64, status DocStatus, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.docs[docID]
	if !ok {
		return &ErrNotFound{Msg: "文档不存在"}
	}
	rec.Doc.Status = int32(status)
	rec.Doc.ErrorMsg = errMsg
	return nil
}

func (s *MemoryStore) UpdateChunks(_ context.Context, docID int64, chunks []*ragv1.Chunk) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.docs[docID]; !ok {
		return &ErrNotFound{Msg: "文档不存在"}
	}
	// 覆盖式写入：重试向量化时避免重复
	for _, cid := range s.docChunks[docID] {
		delete(s.chunks, cid)
	}
	ids := make([]string, 0, len(chunks))
	for _, c := range chunks {
		if c == nil {
			continue
		}
		// 深拷贝入库：调用方（ingest）持有并会修改这些对象
		s.chunks[c.ChunkId] = cloneChunk(c)
		ids = append(ids, c.ChunkId)
	}
	s.docChunks[docID] = ids
	return nil
}

func (s *MemoryStore) ListChunks(_ context.Context, docID int64, page, size int) ([]*ragv1.Chunk, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	ids := s.docChunks[docID]
	total := len(ids)
	start := (page - 1) * size
	if start >= total {
		return nil, total
	}
	end := start + size
	if end > total {
		end = total
	}
	out := make([]*ragv1.Chunk, 0, end-start)
	for _, id := range ids[start:end] {
		if c, ok := s.chunks[id]; ok {
			out = append(out, cloneChunk(c))
		}
	}
	return out, total
}

func (s *MemoryStore) GetChunksByIDs(_ context.Context, chunkIDs []string) ([]*ragv1.Chunk, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]*ragv1.Chunk, 0, len(chunkIDs))
	for _, id := range chunkIDs {
		c, ok := s.chunks[id]
		if !ok {
			continue
		}
		// 正文较长，这里去掉向量字段（内存实现本就未存向量）
		out = append(out, cloneChunk(c))
	}
	return out, nil
}

func (s *MemoryStore) Neighbors(_ context.Context, docID int64, seq int32, before, after int) ([]*ragv1.Chunk, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	ids := s.docChunks[docID]
	var out []*ragv1.Chunk
	for i := int(seq) - before; i <= int(seq)+after; i++ {
		if i < 0 || i >= len(ids) {
			continue
		}
		if c, ok := s.chunks[ids[i]]; ok {
			out = append(out, cloneChunk(c))
		}
	}
	return out, nil
}

func (s *MemoryStore) MarkVecStatus(_ context.Context, chunkIDs []string, status VecStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range chunkIDs {
		if c, ok := s.chunks[id]; ok {
			c.VecStatus = int32(status)
		}
	}
	return nil
}

func (s *MemoryStore) ListPendingChunks(_ context.Context, batch int) ([]*ragv1.Chunk, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []*ragv1.Chunk
	for _, c := range s.chunks {
		if c.VecStatus != int32(VecPending) && c.VecStatus != int32(VecFailed) {
			continue
		}
		out = append(out, cloneChunk(c))
		if batch > 0 && len(out) >= batch {
			break
		}
	}
	return out, nil
}

func (s *MemoryStore) RecountKB(_ context.Context, kbID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	kb, ok := s.kbs[kbID]
	if !ok {
		return nil
	}
	var docCount, chunkCount int32
	for _, rec := range s.docs {
		if rec.Doc.GetKbId() != kbID {
			continue
		}
		docCount++
		chunkCount += int32(len(s.docChunks[rec.Doc.GetDocId()]))
	}
	kb.DocCount = docCount
	kb.ChunkCount = int64(chunkCount)
	return nil
}

func (s *MemoryStore) BindAgentKB(_ context.Context, agentID int64, kbIDs []int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 全量覆盖
	s.agentKBs[agentID] = append([]int64(nil), kbIDs...)
	return nil
}

func (s *MemoryStore) ListAgentKB(_ context.Context, agentID int64) ([]*ragv1.KnowledgeBase, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := s.agentKBs[agentID]
	out := make([]*ragv1.KnowledgeBase, 0, len(ids))
	for _, id := range ids {
		if kb, ok := s.kbs[id]; ok {
			out = append(out, cloneKB(kb))
		}
	}
	return out, nil
}

// KeywordSearch 关键词降级检索。
//
// 实现为内存倒排式匹配：按分词后的词频与命中覆盖率打分。
// PostgreSQL 版本会用 tsvector / GIN 索引，语义保持一致。
func (s *MemoryStore) KeywordSearch(_ context.Context, query string, kbIDs []int64, tenant string, topK int) ([]*KeywordHit, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	terms := tokenize(query)
	if len(terms) == 0 {
		return nil, nil
	}
	allowed := make(map[int64]bool, len(kbIDs))
	for _, id := range kbIDs {
		allowed[id] = true
	}

	// chunkID → 分数
	scores := make(map[string]float64)
	for _, c := range s.chunks {
		if len(allowed) > 0 && !allowed[c.KbId] {
			continue
		}
		// 租户隔离通过知识库维度判定：
		// Chunk 本身不冗余 tenant 字段，避免两处数据不一致
		if tenant != "" && !s.kbBelongsToTenantLocked(c.KbId, tenant) {
			continue
		}
		lowered := strings.ToLower(c.GetContent())
		hit := 0
		for _, t := range terms {
			if strings.Contains(lowered, t) {
				hit++
			}
		}
		if hit == 0 {
			continue
		}
		// 覆盖率打分：命中词越多分越高
		scores[c.ChunkId] = float64(hit) / float64(len(terms))
	}

	// 取 topK
	list := make([]ScorePair, 0, len(scores))
	for id, sc := range scores {
		list = append(list, ScorePair{ID: id, Score: sc})
	}
	SortScorePairs(list)

	if topK <= 0 {
		topK = 5
	}
	if len(list) > topK {
		list = list[:topK]
	}

	out := make([]*KeywordHit, 0, len(list))
	for _, item := range list {
		c, ok := s.chunks[item.ID]
		if !ok {
			continue
		}
		title := ""
		if rec, ok := s.docs[c.DocId]; ok {
			title = rec.Doc.GetFileName()
		}
		out = append(out, &KeywordHit{
			ChunkID:    c.ChunkId,
			KBID:       c.KbId,
			DocID:      c.DocId,
			DocTitle:   title,
			Content:    c.GetContent(),
			PageNo:     c.GetPageNo(),
			Score:      item.Score,
			TokenCount: c.GetTokenCount(),
		})
	}
	return out, nil
}

func (s *MemoryStore) Stats() *Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	st := &Stats{
		KBs:    len(s.kbs),
		Docs:   len(s.docs),
		Chunks: len(s.chunks),
	}
	for _, c := range s.chunks {
		switch VecStatus(c.VecStatus) {
		case VecPending:
			st.Pending++
		case VecFailed:
			st.Failed++
		}
	}
	return st
}

// kbBelongsToTenantLocked 判断知识库是否属于指定租户。
// 调用方必须已持有锁。
func (s *MemoryStore) kbBelongsToTenantLocked(kbID int64, tenant string) bool {
	kb, ok := s.kbs[kbID]
	if !ok {
		return false
	}
	return kb.GetTenant() == "" || kb.GetTenant() == tenant
}
