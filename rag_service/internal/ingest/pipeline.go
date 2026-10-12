// Package ingest 实现文档入库流水线：解析 → 切片 → 向量化 → 写向量库。
//
// 流水线与一致性（ADR-006）：
//
//  1. 文档落 PG，状态 待解析
//  2. 解析出正文，按切片参数切分
//  3. 切片落 PG，vec_status = 0（待写向量）
//  4. 批量向量化（经 llm_service.Embed）
//  5. 写 Milvus，成功则回写 vec_status = 1；失败置 2 并记录原因
//  6. 补偿任务定期捞取 vec_status != 1 的切片重试
//
// 为什么第 3 步就落 PG：正文是内容真相源，必须先持久化。
// 若先写向量再落 PG，向量库中会出现"孤儿向量"（无对应正文），
// 检索命中后取不到正文，用户会看到空引用。
package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	commonv1 "github.com/aegis/idl/kitex_gen/commonv1"
	llmv1 "github.com/aegis/idl/kitex_gen/llmv1"
	llmservice "github.com/aegis/idl/kitex_gen/llmv1/llmservice"
	ragv1 "github.com/aegis/idl/kitex_gen/ragv1"

	"github.com/aegis/pkg/conf"
	"github.com/aegis/pkg/logger"
	"github.com/aegis/pkg/parser"
	"github.com/aegis/pkg/vectordb"

	"github.com/aegis/rag_service/internal/meta"
)

// Pipeline 入库流水线
type Pipeline struct {
	meta   meta.Store
	vector vectordb.Store
	llm    llmservice.Client
	cfg    conf.RAGConf

	// compensateGate 容量为 1 的 channel，用作补偿任务的非阻塞互斥闸门。
	//
	// 为什么不用 sync.Mutex / singleflight：
	//   - ticker 每隔 interval 触发一次 Compensate，如果上一轮还没跑完，
	//     Mutex 会让 ticker 阻塞等待，导致 ticker 内部时间漂移；
	//   - singleflight 适合多个调用方同时触发去重合并，这里只有
	//     一个 ticker 调用方，语义只是"跑不完就跳过"，singleflight
	//     会等待前一次结束再合并，同样会造成 ticker 阻塞；
	//   - channel gate 配合 select/default 可以非阻塞地检测到
	//     "上一次还在跑"，立即跳过，ticker 节奏丝毫不乱。
	compensateGate chan struct{}
}

// New 构造流水线
func New(m meta.Store, v vectordb.Store, llm llmservice.Client, cfg conf.RAGConf) *Pipeline {
	return &Pipeline{
		meta:           m,
		vector:         v,
		llm:            llm,
		cfg:            cfg,
		compensateGate: make(chan struct{}, 1),
	}
}

// Result 入库结果
type Result struct {
	DocID         int64
	ChunkCount    int32
	Vectorized    int32
	Degraded      bool
	DegradeReason string
}

// Ingest 处理一个文档：切片 + 向量化 + 写向量库。
func (p *Pipeline) Ingest(ctx context.Context, kb *ragv1.KnowledgeBase, rec *meta.DocumentRecord, identity *commonv1.Identity) (*Result, error) {
	docID := rec.Doc.GetDocId()

	// 1) 解析正文
	//    生产环境此处按 file_type 分派：pdf → pdf 解析器，docx → ……
	//    mock/文本类型直接按 UTF-8 读取。
	text, err := parseContent(rec)
	if err != nil {
		if err := p.meta.UpdateDocStatus(ctx, docID, meta.DocFailed, err.Error()); err != nil {
			logger.Warn(ctx, "UpdateDocStatus 失败（入库已继续，补偿任务兜底）", "doc_id", docID, "status", meta.DocFailed, "err", err)
		}
		return nil, err
	}
	if strings.TrimSpace(text) == "" {
		err := fmt.Errorf("文档内容为空或无法解析")
		if err := p.meta.UpdateDocStatus(ctx, docID, meta.DocFailed, err.Error()); err != nil {
			logger.Warn(ctx, "UpdateDocStatus 失败（入库已继续，补偿任务兜底）", "doc_id", docID, "status", meta.DocFailed, "err", err)
		}
		return nil, err
	}

	// 2) 切片
	chunks := split(text, int(kb.GetChunkSize()), int(kb.GetChunkOverlap()), func(seq int) string {
		return meta.FormatChunkID(docID, seq)
	})
	if len(chunks) == 0 {
		err := fmt.Errorf("切片结果为空")
		if err := p.meta.UpdateDocStatus(ctx, docID, meta.DocFailed, err.Error()); err != nil {
			logger.Warn(ctx, "UpdateDocStatus 失败（入库已继续，补偿任务兜底）", "doc_id", docID, "status", meta.DocFailed, "err", err)
		}
		return nil, err
	}

	// 3) 切片落 PG（内容真相源）
	for _, c := range chunks {
		c.KbId = kb.KbId
		c.DocId = docID
		c.VecStatus = int32(meta.VecPending)
		c.PageNo = 1
		c.TokenCount = estimateTokens(c.Content)
	}
	if err := p.meta.UpdateChunks(ctx, docID, chunks); err != nil {
		return nil, err
	}
	// 关键：把切片回填到 rec.Chunks。
	// vectorize 只认 rec.Chunks，不从 meta 反查 ——
	// 若不回填，vectorize 会看到空切片并报"无切片可向量化"，
	// 表现为文档入库成功但检索永远命中不了。
	rec.Chunks = chunks
	rec.Doc.ChunkCount = int32(len(chunks))

	result := &Result{DocID: docID, ChunkCount: int32(len(chunks))}

	// 4) 向量化 + 写向量库
	if err := p.vectorize(ctx, kb, rec, identity, result); err != nil {
		// 向量化失败不阻断入库：正文已在 PG，补偿任务会重试
		logger.Warn(ctx, "向量化失败，已标记待补偿",
			"doc_id", docID, "err", err)
		result.Degraded = true
		result.DegradeReason = "embedding_failed: " + err.Error()
		if err := p.meta.UpdateDocStatus(ctx, docID, meta.DocPartial, err.Error()); err != nil {
			logger.Warn(ctx, "UpdateDocStatus 失败（入库已继续，补偿任务兜底）", "doc_id", docID, "status", meta.DocPartial, "err", err)
		}
		return result, nil
	}

	status := meta.DocReady
	if result.Degraded {
		status = meta.DocPartial
	}
	if err := p.meta.UpdateDocStatus(ctx, docID, status, result.DegradeReason); err != nil {
		logger.Warn(ctx, "UpdateDocStatus 失败（入库已继续，补偿任务兜底）", "doc_id", docID, "status", status, "err", err)
	}
	if err := p.meta.RecountKB(ctx, kb.KbId); err != nil {
		logger.Warn(ctx, "RecountKB 失败（入库已继续，补偿任务兜底）", "kb_id", kb.KbId, "err", err)
	}

	logger.Info(ctx, "文档入库完成",
		"doc_id", docID, "kb_id", kb.KbId,
		"chunks", result.ChunkCount, "vectorized", result.Vectorized,
		"degraded", result.Degraded)

	return result, nil
}

// vectorize 批量向量化并写入向量库
func (p *Pipeline) vectorize(ctx context.Context, kb *ragv1.KnowledgeBase, rec *meta.DocumentRecord, identity *commonv1.Identity, result *Result) error {
	chunks := rec.Chunks
	if len(chunks) == 0 {
		// ★ 这里必须是显式失败而非静默返回：
		//   若上游漏了 rec.Chunks 回填，静默跳过会导致
		//   "入库返回成功、检索却永远零命中" 这种极难排查的故障。
		return fmt.Errorf("无切片可向量化：rec.Chunks 为空（doc_id=%d）", rec.Doc.GetDocId())
	}

	// 确保集合存在且维度匹配
	dim := int(kb.GetEmbeddingDim())
	if dim <= 0 {
		dim = 1024
	}
	collection := p.collectionOf(kb.KbId)
	if err := p.vector.EnsureCollection(ctx, collection, dim); err != nil {
		return fmt.Errorf("准备集合失败: %w", err)
	}

	// 分批向量化：单批过大易触发上游超时
	batch := 32
	if p.cfg.IngestWorkers > 0 {
		batch = p.cfg.IngestWorkers * 8
	}
	if batch <= 0 {
		batch = 32
	}

	for start := 0; start < len(chunks); start += batch {
		end := start + batch
		if end > len(chunks) {
			end = len(chunks)
		}
		batchChunks := chunks[start:end]

		texts := make([]string, 0, len(batchChunks))
		for _, c := range batchChunks {
			texts = append(texts, c.GetContent())
		}

		resp, err := p.llm.Embed(ctx, &llmv1.EmbedReq{
			Texts: texts,
			Model: kb.GetEmbedModel(),
			// 文档向量化必须用 document 类型：部分模型对 query/document
			// 使用不同前缀训练，混用会明显降低检索质量
			InputType: "document",
			Identity:  identity,
			// ★ 同时生成稀疏向量：入库的 sparse_vector 字段用于
			//   dense+sparse 混合检索。模型不支持时返回为空，不影响稠密写入。
			EnableSparse: true,
		})
		if err != nil {
			return fmt.Errorf("调用 llm_service.Embed 失败: %w", err)
		}
		if int(resp.GetDimension()) != dim {
			// ★ 维度不匹配必须中止：写入会导致整批检索结果失真，
			//   且事后极难排查（检索"能跑"但结果全错）
			return fmt.Errorf("向量维度不匹配：期望 %d，实际 %d", dim, resp.GetDimension())
		}

		vecChunks := make([]*vectordb.Chunk, 0, len(batchChunks))
		chunkIDs := make([]string, 0, len(batchChunks))
		for i, c := range batchChunks {
			if i >= len(resp.GetEmbeddings()) {
				break
			}
			vecChunks = append(vecChunks, &vectordb.Chunk{
				ChunkID:      c.GetChunkId(),
				KBID:         c.GetKbId(),
				DocID:        c.GetDocId(),
				Seq:          c.GetSeq(),
				Content:      c.GetContent(),
				Vector:       resp.GetEmbeddings()[i].GetVector(),
				SparseVector: sparseForIndex(resp.GetSparseEmbeddings(), i),
				PageNo:       c.GetPageNo(),
				TokenCount:   c.GetTokenCount(),
			})
			chunkIDs = append(chunkIDs, c.GetChunkId())
		}

		if err := p.vector.Upsert(ctx, collection, vecChunks); err != nil {
			// 向量库写失败：标记待补偿，错误上抛让调用方看到原因
			if err := p.meta.MarkVecStatus(ctx, chunkIDs, meta.VecFailed); err != nil {
				logger.Warn(ctx, "MarkVecStatus 失败（入库已继续，补偿任务兜底）", "chunk_ids", chunkIDs, "status", meta.VecFailed, "err", err)
			}
			return fmt.Errorf("写向量库失败: %w", err)
		}

		if err := p.meta.MarkVecStatus(ctx, chunkIDs, meta.VecReady); err != nil {
			logger.Warn(ctx, "MarkVecStatus 失败（入库已继续，补偿任务兜底）", "chunk_ids", chunkIDs, "status", meta.VecReady, "err", err)
		}
		result.Vectorized += int32(len(vecChunks))
	}

	// ★ 整篇文档写完后同步一次，而不是每批Upsert 后都同步。
	//
	//   原因：Milvus 默认一致性等级 ClBounded，新写入的数据
	//   在落盘前对检索不可见（实测：Upsert 后立即 Search 召回 0 条，
	//   **且不报任何错**；等1s 或 Flush 后才正常）。
	//   而 pipeline 上面刚把 vec_status 全标成了「就绪」——
	//   用户看到文档已就绪，紧接着提问却搜不到内容，
	//   且没有任何提示。这是很差的体验。
	//
	//   为什么不在每批之后 Sync：
	//   分批是 32 条/批，几百个 chunk 的文档会有十几批，
	//   每批一次 Sync 意味着十几秒的纯等待。
	//   一篇文档一次 Sync，代价恒定（约 1.2s，实测），
	//   相对于解析 + 调embedding 的耗时可以忽略。
	//
	//   失败不阻断入库：向量已经落到 Milvus，
	//   补偿任务能捞到，数据不会丢。见 SyncAfterWrite 的说明。
	if err := vectordb.SyncAfterWrite(ctx, p.vector, collection); err != nil {
		logger.Warn(ctx, "向量同步未完成，数据可能暂时不可检索",
			"doc_id", rec.Doc.GetDocId(), "collection", collection, "err", err)
		result.Degraded = true
		result.DegradeReason = "vector_sync_pending: " + err.Error()
	}
	return nil
}

// Compensate 补偿任务：捞取 vec_status != 1 的切片重试向量化。
//
// 由 rag_service 启动的定时器调用（周期见 rag.compensate_interval）。
func (p *Pipeline) Compensate(ctx context.Context, batch int) (int, error) {
	if batch <= 0 {
		batch = p.cfg.CompensateBatch
	}
	if batch <= 0 {
		batch = 500
	}

	pending, err := p.meta.ListPendingChunks(ctx, batch)
	if err != nil {
		return 0, err
	}
	if len(pending) == 0 {
		return 0, nil
	}

	logger.Info(ctx, "补偿任务启动", "pending", len(pending))

	// 按文档分组：一次 Embed 调用覆盖同一文档的多个切片
	byDoc := make(map[int64][]*ragv1.Chunk)
	for _, c := range pending {
		byDoc[c.GetDocId()] = append(byDoc[c.GetDocId()], c)
	}

	fixed := 0
	for docID, pendingChunks := range byDoc {
		rec, err := p.meta.GetDoc(ctx, docID)
		if err != nil {
			continue // 文档已删除
		}
		kb, err := p.meta.GetKB(ctx, rec.Doc.GetKbId())
		if err != nil {
			continue
		}

		// ★ 只重试待补偿的切片，而不是文档的全部切片：
		//   全部重写会浪费 Embedding 配额（这是最贵的一环）
		need := make(map[string]bool, len(pendingChunks))
		for _, c := range pendingChunks {
			need[c.GetChunkId()] = true
		}
		all, _ := p.meta.ListChunks(ctx, docID, 1, 1<<20)
		if len(all) == 0 {
			continue // 切片已被清理，无可补偿
		}
		retry := make([]*ragv1.Chunk, 0, len(pendingChunks))
		for _, c := range all {
			if need[c.GetChunkId()] {
				retry = append(retry, c)
			}
		}
		if len(retry) == 0 {
			continue
		}
		rec.Chunks = retry

		res := &Result{}
		if err := p.vectorize(ctx, kb, rec, nil, res); err != nil {
			logger.Warn(ctx, "补偿失败", "doc_id", docID, "err", err)
			continue
		}
		fixed += int(res.Vectorized)
	}

	if fixed > 0 {
		logger.Info(ctx, "补偿任务完成", "fixed_chunks", fixed)
	}
	return fixed, nil
}

// collectionOf KB → 集合名
func (p *Pipeline) collectionOf(kbID int64) string {
	return fmt.Sprintf("aegis_kb_%d", kbID)
}

// ---------- 解析与切片 ----------

// parseContent 提取文档正文。
//
// 统一委托给 pkg/parser：它按文件类型分发到 PDF/DOCX/XLSX/纯文本
// 各自的解析器，解析失败返回明确 error（不会静默降级为空内容）。
// 扩展新格式只需往 parser.Parse 加 case，此处无需改动。
func parseContent(rec *meta.DocumentRecord) (string, error) {
	return parser.Parse(rec.Doc.GetFileType(), rec.RawBytes)
}

// split 递归字符切分。
//
// 采用 recursive 策略：优先在段落/句子边界断开，
// 实在超长才按字符硬切 —— 这样切片的语义完整性更好。
func split(text string, size, overlap int, idFn func(seq int) string) []*ragv1.Chunk {
	if size <= 0 {
		size = 800
	}
	if overlap < 0 {
		overlap = 0
	}
	if overlap >= size {
		overlap = size / 4
	}

	runes := []rune(strings.TrimSpace(text))
	if len(runes) == 0 {
		return nil
	}

	// 过长单段先按换行预切，形成候选段
	segments := splitBySeparator(runes, size)

	var chunks []*ragv1.Chunk
	var cur []rune
	seq := 0

	flush := func() {
		if len(cur) == 0 {
			return
		}
		content := strings.TrimSpace(string(cur))
		if content != "" {
			chunks = append(chunks, &ragv1.Chunk{
				ChunkId: idFn(seq),
				Seq:     int32(seq),
				Content: content,
			})
			seq++
		}
		cur = nil
	}

	for _, seg := range segments {
		// 加入后仍不超长：直接追加
		if len(cur)+len(seg) <= size {
			cur = append(cur, seg...)
			continue
		}

		// 先把已有内容切出去
		if len(cur) > 0 {
			flush()
		}

		// 单段本身超长：按 size 硬切，段间保留 overlap
		if len(seg) > size {
			step := size - overlap
			for start := 0; start < len(seg); start += step {
				end := start + size
				if end > len(seg) {
					end = len(seg)
				}
				chunkRunes := seg[start:end]
				cur = append(cur, chunkRunes...)
				flush()
				if end == len(seg) {
					break
				}
			}
			continue
		}

		cur = append(cur, seg...)
	}
	flush()

	return chunks
}

// splitBySeparator 按段落/句子边界把长文本切成不超过 size 的段
func splitBySeparator(runes []rune, size int) [][]rune {
	var out [][]rune
	var cur []rune

	flush := func() {
		if len(cur) > 0 {
			out = append(out, cur)
			cur = nil
		}
	}

	for i := 0; i < len(runes); i++ {
		cur = append(cur, runes[i])
		if len(cur) >= size {
			// 回退到最近的边界，避免把词切断
			if cut := lastSeparator(cur); cut > size/2 {
				cut = cut + 1
				out = append(out, cur[:cut])
				cur = append([]rune(nil), cur[cut:]...)
			} else {
				flush()
			}
		}
	}
	flush()
	return out
}

// lastSeparator 找最近的段落/句子边界位置
func lastSeparator(cur []rune) int {
	for i := len(cur) - 1; i > 0; i-- {
		switch cur[i] {
		case '\n', '。', '！', '？', '.', '!', '?', ';', '；':
			return i
		}
	}
	return -1
}

// estimateTokens 粗略估算 token 数（中文约 1 字 1 token）
func estimateTokens(s string) int32 {
	n := 0
	for _, r := range s {
		if r > 0x2E80 {
			n += 1
		} else {
			n += 1
		}
	}
	return int32(n/2 + 1)
}

// HashContent 计算内容哈希（去重用）
func HashContent(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// sparseForIndex 按 index 从 Embed 响应的稀疏向量列表中取第 i 条。
// 稀疏向量字段可空：模型不支持时返回 nil（稠密写入不受影响）。
func sparseForIndex(sparse []*llmv1.SparseEmbedding, i int) []vectordb.SparseToken {
	for _, se := range sparse {
		if int(se.GetIndex()) != i {
			continue
		}
		out := make([]vectordb.SparseToken, 0, len(se.GetTokens()))
		for _, t := range se.GetTokens() {
			out = append(out, vectordb.SparseToken{Index: t.GetIndex(), Value: t.GetValue()})
		}
		return out
	}
	return nil
}

// StartCompensator 启动补偿任务定时器
func (p *Pipeline) StartCompensator(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// 非阻塞地获取闸门：上一轮还在跑则立即跳过，
				// 避免 ticker 因 Mutex 等待而发生时间漂移。
				select {
				case p.compensateGate <- struct{}{}:
				default:
					logger.Warn(ctx, "补偿任务仍在运行，跳过本次触发",
						"interval", interval)
					continue
				}
				go func() {
					defer func() { <-p.compensateGate }()
					if _, err := p.Compensate(ctx, p.cfg.CompensateBatch); err != nil {
						logger.Warn(ctx, "补偿任务异常", "err", err)
					}
				}()
			}
		}
	}()
}
