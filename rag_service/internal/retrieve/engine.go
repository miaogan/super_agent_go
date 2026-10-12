// Package retrieve 实现检索层：多来源混合检索 + 故障隔离 + 可选重排。
//
// ★ 多来源架构（对应 PRD FR-6.9 混合检索）：
//
//	同一查询并行召回多个来源，RRF（倒数排名融合）合并去重：
//	  ① 向量检索（Milvus）—— 语义匹配主力
//	  ② 关键词检索（PG 全文）—— 术语/精确匹配（常驻，不再仅降级）
//	  ③ 网络检索（Web Search）—— 外部知识补充（可配置）
//
// ★ 防单源原则（核心设计约束）：
//
//	任一来源故障**不影响**其他来源产出结果 ——
//	向量库挂了，关键词/网络仍能给出答案；
//	只有**全部来源**都失败，才返回空上下文并置 degraded。
//	绝不让单一来源的故障拖垮整个 RAG。
//
// ★ 可选增强：重排。
//
//	融合后的 topN → Rerank 重排 → 取 topK → 上下文外扩 → 拼 Prompt。
//	重排失败不算检索失败：退回按融合分排序，但 degraded 标记必须置位。
package retrieve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	commonv1 "github.com/aegis/idl/kitex_gen/commonv1"
	llmv1 "github.com/aegis/idl/kitex_gen/llmv1"
	llmservice "github.com/aegis/idl/kitex_gen/llmv1/llmservice"
	ragv1 "github.com/aegis/idl/kitex_gen/ragv1"

	"github.com/aegis/pkg/conf"
	"github.com/aegis/pkg/logger"
	"github.com/aegis/pkg/provider"
	"github.com/aegis/pkg/vectordb"
	"github.com/aegis/pkg/websearch"

	"github.com/aegis/rag_service/internal/meta"
)

// Engine 检索引擎（多来源混合）
type Engine struct {
	meta   meta.Store
	vector vectordb.Store
	llm    llmservice.Client
	cfg    conf.RAGConf
	mcfg   conf.MilvusConf

	// web 网络检索来源（可选；nil 或配置关闭时跳过）
	web websearch.Provider

	// embedModelName 与 dim 来自配置：
	// 两者必须与入库时一致，否则维度校验会失败并触发降级
	embedModelName string
	dimOverride    int

	// mockMode 启用时使用 conf.RAG.MockScoreThreshold 而非生产阈值。
	// 见 RAGConf.MockScoreThreshold 字段注释：mock 的词袋向量相似度
	// 上限约 0.25，套用生产的 0.3 会让检索恒为空。
	mockMode bool
}

// Options 构造参数
type Options struct {
	Meta         meta.Store
	Vector       vectordb.Store
	LLM          llmservice.Client
	RAGConfig    conf.RAGConf
	MilvusConfig conf.MilvusConf
	// EmbedModel Embedding 模型名（通常为 bge-m3）
	EmbedModel string
	// MockMode 启用 mock 阈值
	MockMode bool
	// WebSearch 网络检索来源（可选；未配置时跳过）
	WebSearch websearch.Provider
}

// New 构造检索引擎
func New(opt Options) *Engine {
	return &Engine{
		meta:           opt.Meta,
		vector:         opt.Vector,
		llm:            opt.LLM,
		cfg:            opt.RAGConfig,
		mcfg:           opt.MilvusConfig,
		embedModelName: opt.EmbedModel,
		dimOverride:    opt.MilvusConfig.Dim,
		mockMode:       opt.MockMode,
		web:            opt.WebSearch,
	}
}

// scoreThreshold 返回本次检索生效的阈值
func (e *Engine) scoreThreshold(req float64) float64 {
	// 请求显式指定时以请求为准
	if req > 0 {
		return req
	}
	return effectiveThreshold(e.mockMode,
		e.cfg.DefaultScoreThreshold, e.cfg.MockScoreThreshold)
}

// Request 检索请求
type Request struct {
	Query          string
	KBIDs          []int64
	TopK           int
	ScoreThreshold float64
	ContextWindow  int
	ExcludeDocIDs  []int64
	Identity       *commonv1.Identity
	RequestID      string
	// Debug 开启后返回各阶段明细
	Debug bool
}

// Result 检索结果
type Result struct {
	Query         string
	Hits          []*ragv1.Hit
	Total         int32
	Degraded      bool
	DegradeReason string
	RetrieveMS    int64
	EmbeddingMS   int64
	SearchMS      int64

	// Debug 信息
	Stages       []*ragv1.StageTiming
	MilvusFilter string
	EmbedModel   string
	Dimension    int32
}

// Retrieve 执行检索：多来源并行召回 → RRF 融合 → 重排 → 外扩。
func (e *Engine) Retrieve(ctx context.Context, req *Request) (*Result, error) {
	start := time.Now()

	topK := req.TopK
	if topK <= 0 {
		topK = e.cfg.DefaultTopK
	}
	if topK <= 0 {
		topK = 5
	}
	threshold := e.scoreThreshold(req.ScoreThreshold)
	window := req.ContextWindow
	if window < 0 {
		window = 0
	}
	if window == 0 {
		window = e.cfg.ContextWindow
	}

	res := &Result{
		Query:      req.Query,
		EmbedModel: e.embedModel(),
		Dimension:  int32(e.dim()),
	}

	// ---- 阶段 1：多来源并行召回 ----
	recallStart := time.Now()
	results := e.runSources(ctx, req, topK, threshold)
	recallMS := time.Since(recallStart).Milliseconds()
	if req.Debug {
		res.Stages = append(res.Stages, &ragv1.StageTiming{
			Stage:  "multi_source_recall",
			Ms:     recallMS,
			Detail: fmt.Sprintf("sources=%v", e.enabledSources()),
		})
		for _, r := range results {
			detail := fmt.Sprintf("hits=%d", len(r.chunkIDs)+len(r.webHits))
			if r.err != nil {
				detail = "失败: " + r.err.Error()
			}
			res.Stages = append(res.Stages, &ragv1.StageTiming{
				Stage: r.name, Ms: r.ms, Detail: detail,
			})
		}
	}

	// ---- 阶段 2：融合候选（RRF / vector_only）----
	selected, allFailed, failedReason := e.fuseCandidates(ctx, req, results, topK, threshold)
	if allFailed {
		// ★ 全部来源失败才整体降级（防单源：一路失败绝不拖垮整次检索）
		res.Hits = []*ragv1.Hit{}
		res.Total = 0
		res.Degraded = true
		res.DegradeReason = "all_sources_failed: " + failedReason
		res.RetrieveMS = time.Since(start).Milliseconds()
		if req.Debug {
			res.Stages = append(res.Stages, &ragv1.StageTiming{
				Stage: "fuse", Detail: "全部来源失败，返回空上下文",
			})
		}
		return res, nil
	}
	if req.Debug {
		res.Stages = append(res.Stages, &ragv1.StageTiming{
			Stage:  "fuse",
			Detail: fmt.Sprintf("strategy=%s candidates=%d", e.cfg.EffectiveMergeStrategy(), len(selected)),
		})
	}

	// ---- 阶段 3：重排（可选增强，失败不影响主链路）----
	rrStart := time.Now()
	rrMS := int64(0)
	detail := "disabled"
	if e.rerankOn() && len(selected) > 0 {
		out := e.rerank(ctx, req, selected, topK)
		rrMS = time.Since(rrStart).Milliseconds()
		switch {
		case out.degradeReason != "":
			// 降级：selected 保持候选集原序（按融合分降序），下面统一截断
			res.Degraded = true
			res.DegradeReason = appendReason(res.DegradeReason, out.degradeReason)
			detail = out.degradeReason
			logger.Warn(ctx, "重排未生效，按融合分排序",
				"reason", out.degradeReason, "candidates", len(selected))
		case len(out.picked) == 0:
			// ★ 重排执行成功，只是没有切片达标 —— 不是降级。
			//   结果为空是「模型判定全都不相关」的正常业务结论，
			//   标成 degraded 会把业务判断误报成技术故障。
			selected = nil
			res.DegradeReason = appendReason(res.DegradeReason, fmt.Sprintf(
				"rerank_rejected_all: 重排后无切片达到阈值 %.2f（%d 条候选全部被判定不相关）",
				e.rerankThreshold(), len(selected)))
			detail = "rejected_all"
			logger.Info(ctx, "重排后无切片达标",
				"candidates", len(selected), "threshold", e.rerankThreshold())
		default:
			selected = out.picked
			detail = fmt.Sprintf("kept=%d/%d threshold=%.2f",
				len(selected), len(selected), e.rerankThreshold())
		}
	}
	if topK > 0 && len(selected) > topK {
		selected = selected[:topK]
	}
	if req.Debug {
		res.Stages = append(res.Stages, &ragv1.StageTiming{
			Stage:  "rerank",
			Ms:     rrMS,
			Detail: detail,
		})
	}

	// ---- 阶段 4：上下文外扩（只对入选结果做）----
	expandStart := time.Now()
	e.expandContext(ctx, selected, window)
	if req.Debug {
		res.Stages = append(res.Stages, &ragv1.StageTiming{
			Stage:  "context_expand",
			Ms:     time.Since(expandStart).Milliseconds(),
			Detail: fmt.Sprintf("window=%d hits=%d", window, len(selected)),
		})
	}

	hits := make([]*ragv1.Hit, 0, len(selected))
	for _, c := range selected {
		hits = append(hits, c.hit)
	}
	e.fillTitles(ctx, hits)

	res.Hits = hits
	res.Total = int32(len(hits))
	res.RetrieveMS = time.Since(start).Milliseconds()
	return res, nil
}

// ---------- 多来源召回 ----------

// sourceResult 一个检索来源的产出。
type sourceResult struct {
	name string
	// chunkIDs 该来源召回的 KB 切片（按相关度降序）
	chunkIDs []string
	// scores chunk_id → 来源原始分
	scores map[string]float64
	// webHits 网络检索结果
	webHits []websearch.Result
	ms      int64
	err     error
}

// enabledSources 返回本次启用的来源名（日志/调试）。
func (e *Engine) enabledSources() []string {
	kw, web := e.cfg.SourcesEnabled()
	out := []string{"vector"}
	if kw {
		out = append(out, "keyword")
	}
	if web && e.web != nil {
		out = append(out, "web")
	}
	return out
}

// runSources 并行执行全部启用的来源，故障隔离。
//
// ★ 并发 + 独立错误处理：任一来源 panic/失败都不影响其他来源，
//
//	这是"防单源"的实现保障。
func (e *Engine) runSources(ctx context.Context, req *Request, topK int, threshold float64) []sourceResult {
	kwEnabled, webEnabled := e.cfg.SourcesEnabled()

	runners := []func() sourceResult{
		func() sourceResult { return e.vectorSource(ctx, req, topK, threshold) },
	}
	if kwEnabled {
		runners = append(runners, func() sourceResult { return e.keywordSource(ctx, req, topK) })
	}
	if webEnabled && e.web != nil {
		runners = append(runners, func() sourceResult { return e.webSource(ctx, req, topK) })
	}

	results := make([]sourceResult, len(runners))
	var wg sync.WaitGroup
	for i, run := range runners {
		wg.Add(1)
		go func(idx int, r func() sourceResult) {
			defer wg.Done()
			// 来源实现不应 panic；防御性 recover 保证一路故障不影响整体
			defer func() {
				if v := recover(); v != nil {
					results[idx] = sourceResult{name: "unknown", err: fmt.Errorf("来源 panic: %v", v)}
				}
			}()
			results[idx] = r()
		}(i, run)
	}
	wg.Wait()
	return results
}

// vectorSource 向量检索来源（dense + sparse 双路融合，语义+术语互补）。
//
// ★ 为什么在"向量来源"内部再做一次融合而不是拆成两个来源：
//
//	dense 与 sparse 都查同一个 Milvus 集合、产出 KB 切片，
//	拆开会让关键词/网络来源的融合列表里出现两套同名 id，徒增去重复杂度；
//	合并成一路的语义对上层（多来源 RRF）更干净。
func (e *Engine) vectorSource(ctx context.Context, req *Request, topK int, threshold float64) sourceResult {
	s := sourceResult{name: "vector", scores: map[string]float64{}}
	start := time.Now()
	defer func() { s.ms = time.Since(start).Milliseconds() }()

	// 1) 查询向量化：同时拿稠密 + 稀疏（模型不支持稀疏时 sparse 为空）
	vec, sparseVec, err := e.embed(ctx, req.Query, req.Identity)
	if err != nil {
		s.err = fmt.Errorf("embedding 失败: %w", err)
		return s
	}

	// 2) 稠密检索
	denseIDs, denseScores, errVec := e.searchDense(ctx, req, vec, topK, threshold)
	if errVec != nil {
		s.err = fmt.Errorf("向量库不可用: %w", errVec)
		// 若稀疏路可用，仍可用稀疏结果（防单源在来源内部同样成立）
		denseIDs, denseScores = nil, nil
	}

	// 3) 稀疏检索（可选）
	var sparseIDs []string
	sparseScores := map[string]float64{}
	if len(sparseVec) > 0 {
		sparseIDs, sparseScores, _ = e.searchSparse(ctx, req, sparseVec, topK)
	}

	// 4) 双路 RRF 融合：只保留两路之一的独特命中也不丢
	lists := make([][]string, 0, 2)
	if len(denseIDs) > 0 {
		lists = append(lists, denseIDs)
	}
	if len(sparseIDs) > 0 {
		lists = append(lists, sparseIDs)
	}
	for _, it := range fuseRRF(lists, 0) {
		id := it.id
		score := denseScores[id]
		if ss, ok := sparseScores[id]; ok && ss > score {
			score = ss
		}
		s.chunkIDs = append(s.chunkIDs, id)
		s.scores[id] = score
	}
	return s
}

// searchDense 稠密检索全部 KB（按分数降序，阈值过滤）。
func (e *Engine) searchDense(ctx context.Context, req *Request, vec []float32, topK int, threshold float64) ([]string, map[string]float64, error) {
	var sb *vectordb.SearchResult
	var errVec error
	for _, kbID := range req.KBIDs {
		sbr, err := e.searchOne(ctx, kbID, vec, req, topK, threshold)
		if err != nil {
			if errors.Is(err, vectordb.ErrUnavailable) {
				errVec = err
				break
			}
			logger.Warn(ctx, "单个知识库检索失败", "kb_id", kbID, "err", err)
			continue
		}
		if sb == nil {
			sb = sbr
		} else {
			mergeResults(sb, sbr)
		}
	}
	if errVec != nil {
		return nil, nil, errVec
	}
	if sb == nil {
		return nil, map[string]float64{}, nil
	}
	ids := make([]string, 0, len(sb.ChunkIDs))
	scores := make(map[string]float64, len(sb.ChunkIDs))
	for _, cid := range sb.ChunkIDs {
		score, ok := sb.Scores[cid]
		if !ok || score < threshold {
			continue
		}
		ids = append(ids, cid)
		scores[cid] = score
	}
	return ids, scores, nil
}

// searchSparse 稀疏检索全部 KB（IP 内积，阈值过滤）。
func (e *Engine) searchSparse(ctx context.Context, req *Request, sparse []vectordb.SparseToken, topK int) ([]string, map[string]float64, error) {
	var sb *vectordb.SearchResult
	var errVec error
	for _, kbID := range req.KBIDs {
		sbr, err := e.vector.Search(ctx, &vectordb.SearchRequest{
			Collection:    e.collectionOf(kbID),
			SparseVector:  sparse,
			TopK:          topK,
			Tenant:        req.Identity.GetTenant(),
			ExcludeDocIDs: req.ExcludeDocIDs,
		})
		if err != nil {
			if errors.Is(err, vectordb.ErrUnavailable) {
				errVec = err
				break
			}
			logger.Warn(ctx, "单个知识库稀疏检索失败", "kb_id", kbID, "err", err)
			continue
		}
		if sb == nil {
			sb = sbr
		} else {
			mergeResults(sb, sbr)
		}
	}
	if errVec != nil {
		return nil, nil, errVec
	}
	if sb == nil {
		return nil, map[string]float64{}, nil
	}
	ids := make([]string, 0, len(sb.ChunkIDs))
	scores := make(map[string]float64, len(sb.ChunkIDs))
	for _, cid := range sb.ChunkIDs {
		score, ok := sb.Scores[cid]
		if !ok || score <= 0 {
			// 稀疏 IP 内积：0 分（无共享词）无检索意义
			continue
		}
		ids = append(ids, cid)
		scores[cid] = score
	}
	return ids, scores, nil
}

// keywordSource 关键词检索来源（术语/精确匹配，常驻）。
func (e *Engine) keywordSource(ctx context.Context, req *Request, topK int) sourceResult {
	s := sourceResult{name: "keyword", scores: map[string]float64{}}
	start := time.Now()
	defer func() { s.ms = time.Since(start).Milliseconds() }()

	kwHits, err := e.meta.KeywordSearch(ctx, req.Query, req.KBIDs, req.Identity.GetTenant(), topK)
	if err != nil {
		s.err = fmt.Errorf("关键词检索失败: %w", err)
		return s
	}
	for _, h := range kwHits {
		s.chunkIDs = append(s.chunkIDs, h.ChunkID)
		s.scores[h.ChunkID] = h.Score
	}
	return s
}

// webSource 网络检索来源（外部知识补充）。
func (e *Engine) webSource(ctx context.Context, req *Request, topK int) sourceResult {
	s := sourceResult{name: "web"}
	start := time.Now()
	defer func() { s.ms = time.Since(start).Milliseconds() }()

	webTopK := e.cfg.WebSearch.TopK
	if webTopK <= 0 {
		webTopK = topK
	}
	hits, err := websearch.Search(ctx, e.web, req.Query, webTopK)
	if err != nil {
		s.err = fmt.Errorf("网络检索失败: %w", err)
		return s
	}
	s.webHits = hits
	return s
}

// ---------- 融合 ----------

// fusedKB 融合后仍需回 PG 取正文的 KB 切片。
// fusedWeb 融合后需要转成候选的网络条目。
func (e *Engine) fuseCandidates(ctx context.Context, req *Request, results []sourceResult, topK int, threshold float64) (selected []candidate, allFailed bool, failedReason string) {
	// 1) 统计各来源成败
	var vectorRes, keywordRes *sourceResult
	var webRes *sourceResult
	okCount := 0
	var failed []string
	for i := range results {
		r := &results[i]
		switch r.name {
		case "vector":
			vectorRes = r
		case "keyword":
			keywordRes = r
		case "web":
			webRes = r
		}
		if r.err != nil {
			failed = append(failed, r.name+": "+r.err.Error())
		} else if len(r.chunkIDs)+len(r.webHits) > 0 {
			okCount++
		}
	}

	// 2) 全部失败 → 整体降级
	if okCount == 0 {
		reason := "无来源产出结果"
		if len(failed) > 0 {
			reason = strings.Join(failed, "; ")
		}
		return nil, true, reason
	}

	// 3) 按策略融合
	strategy := e.cfg.EffectiveMergeStrategy()
	kbScores := make(map[string]float64)
	webByID := make(map[string]websearch.Result)

	var sourceLists [][]string
	switch strategy {
	case conf.MergeStrategyVectorOnly:
		// 仅向量：兼容旧行为
		if vectorRes != nil {
			sourceLists = [][]string{vectorRes.chunkIDs}
			for id, sc := range vectorRes.scores {
				kbScores[id] = sc
			}
		}
	default: // rrf
		if vectorRes != nil {
			sourceLists = append(sourceLists, vectorRes.chunkIDs)
			for id, sc := range vectorRes.scores {
				kbScores[id] = sc
			}
		}
		if keywordRes != nil {
			sourceLists = append(sourceLists, keywordRes.chunkIDs)
			for id, sc := range keywordRes.scores {
				// 关键词与向量同源（同一 chunk_id）：保留较优分
				if _, ok := kbScores[id]; !ok || sc > kbScores[id] {
					kbScores[id] = sc
				}
			}
		}
		if webRes != nil && len(webRes.webHits) > 0 {
			var webIDs []string
			for _, h := range webRes.webHits {
				id := webChunkID(h)
				webIDs = append(webIDs, id)
				webByID[id] = h
			}
			sourceLists = append(sourceLists, webIDs)
		}
	}

	fused := fuseRRF(sourceLists, 0)

	// 4) 还原候选：KB 条目批量取正文，web 条目直接转 Hit
	var kbIDs []string
	for _, it := range fused {
		if _, isWeb := webByID[it.id]; isWeb {
			continue
		}
		if _, isKB := kbScores[it.id]; isKB {
			kbIDs = append(kbIDs, it.id)
		}
	}
	kbChunks := make(map[string]*ragv1.Chunk, len(kbIDs))
	if len(kbIDs) > 0 {
		if chunks, err := e.meta.GetChunksByIDs(ctx, kbIDs); err == nil {
			for _, c := range chunks {
				kbChunks[c.GetChunkId()] = c
			}
		}
	}

	for _, it := range fused {
		if w, ok := webByID[it.id]; ok {
			selected = append(selected, candidate{
				seq: 0,
				hit: &ragv1.Hit{
					ChunkId:  it.id,
					DocTitle: w.Title,
					Score:    it.score,
					Content:  w.Snippet,
				},
			})
			continue
		}
		c, ok := kbChunks[it.id]
		if !ok {
			// PG 中已无此切片（向量库残留）→ 跳过。
			// PG 是内容真相源：向量只是索引，不是数据。
			continue
		}
		selected = append(selected, candidate{
			seq: c.GetSeq(),
			hit: &ragv1.Hit{
				ChunkId: it.id,
				KbId:    c.GetKbId(),
				DocId:   c.GetDocId(),
				PageNo:  c.GetPageNo(),
				Score:   kbScores[it.id],
				Content: c.GetContent(),
			},
		})
	}
	return selected, false, ""
}

// webChunkID 网络条目合成稳定 ID：web: + sha256(title|url) 前 16 hex。
func webChunkID(r websearch.Result) string {
	sum := sha256.Sum256([]byte(r.URL + "|" + r.Title))
	return "web:" + hex.EncodeToString(sum[:8])
}

// ---------- 重排 ----------

// rerankOn 重排是否启用。
func (e *Engine) rerankOn() bool { return e.cfg.RerankEnabled }

// candidateK 送入「选哪几条」环节的候选数量。
//
// 重排的意义在于「从更多候选里挑出更好的几条」，若召回数与 topK 相同，
// 重排只能重新排序、无法替换，等于白花一次调用。
func (e *Engine) candidateK(topK int) int {
	if e.rerankOn() && e.cfg.RerankTopK > topK {
		return e.cfg.RerankTopK
	}
	return topK
}

// rerankThreshold 本次生效的重排阈值。
//
// ★ mock 模式必须下调，原因与 RAGConf.MockScoreThreshold 完全相同：
//
//	mockprovider.Rerank 用词袋余弦模拟重排，其相似度上限受数学限制约 0.25；
//	而生产阈值 0.3 是为真实 rerank 模型标定的。
//	直接套用会让 mock 模式下重排恒为 0 命中，链路在冒烟测试里形同虚设。
func (e *Engine) rerankThreshold() float64 {
	t := e.cfg.RerankScoreThreshold
	// min 预先处理「生产阈值已低于 mock 阈值时，不应被上调」这一分支
	return effectiveThreshold(e.mockMode, t, min(t, e.cfg.MockScoreThreshold))
}

// rerankOutcome 重排结果。
//
// ★ 为什么要用结构体而不是 (results, error)：
//
//	重排有**三种**结果，都不是「失败」：
//	  1. 成功 → 若干条入选
//	  2. 能力缺失/调用失败 → 降级，调用方用候选原序
//	  3. 成功但全被阈值拦掉 → 空结果，这是业务结论不是故障
//
//	用 (results, degradeReason string) 的二元返回表达不了第 3 种：
//	「空结果」和「降级」会被混为一谈 —— 前者不该置 degraded，
//	后者必须置。把两者显式分开，避免又出现
//	「模型判定全部不相关」被误报成技术故障这类错误。
type rerankOutcome struct {
	// picked 入选结果，已按重排分降序、已过滤阈值、已截断 topN
	picked []candidate
	// degradeReason 非空表示**降级**（能力缺失或调用失败），
	// 调用方应退回候选集原序并置 degraded 标记
	degradeReason string
}

// rerank 对候选切片重排。
//
// 不返回 error：重排是检索链路的增强环节，不是必需环节，
// 它失败不该让整次检索失败。
func (e *Engine) rerank(ctx context.Context, req *Request, cands []candidate, topN int) rerankOutcome {
	docs := make([]string, len(cands))
	for i, c := range cands {
		docs[i] = c.hit.Content
	}

	// 复用 MilvusConf.Timeout 作为 RPC 超时：重排是同步增强环节，
	// 不值得为它单配一个更长的超时（否则会拖垮整个检索的响应预算）。
	rctx, cancel := context.WithTimeout(ctx, e.mcfg.Timeout)
	defer cancel()

	resp, err := e.llm.Rerank(rctx, &llmv1.RerankReq{
		Query:     req.Query,
		Documents: docs,
		Model:     e.cfg.RerankModel,
		TopN:      int32(topN),
		Identity:  req.Identity,
		// 不回带原文：候选文本就在上面 docs 里，
		// 让下游再传一遍是重复搬运 + 双份内存。
		ReturnDocuments: false,
	})
	if err != nil {
		return rerankOutcome{degradeReason: e.degradeReasonFor(err)}
	}

	thr := e.rerankThreshold()
	picked := make([]candidate, 0, topN)
	for _, r := range resp.GetResults() {
		// 越界防护：异常返回体不能让上层切片越界崩溃
		if r.GetIndex() < 0 || int(r.GetIndex()) >= len(cands) {
			continue
		}
		if r.GetRelevanceScore() < thr {
			continue
		}
		c := cands[r.GetIndex()]
		// ★ Score 覆盖为重排分：调用方按「最终排序依据」判断置信度，
		//   而重排才是真正的决策依据。向量分在 Debug 的 milvus_search
		//   阶段明细里仍可追溯。
		//
		//   两者量纲不同（向量余弦 vs rerank 0–1 相关性分），
		//   混用会让调用方的阈值判断失真 —— 见 provider.RerankResult 注释。
		c.hit.Score = r.GetRelevanceScore()
		picked = append(picked, c)
		if len(picked) >= topN {
			break
		}
	}

	// picked 为空不是降级 —— 由调用方按业务结论处理
	return rerankOutcome{picked: picked}
}

// degradeReasonFor 把重排错误翻译成可读的降级原因。
//
// ★ 必须区分「能力缺失」与「调用失败」：
//
//	前者是配置问题（重排模型没配 / 缺 rerank_path），重试无用；
//	后者是故障，重试可能成功。llm_service 已把它们映射为
//	ErrNotImplemented(501) 与 ErrModelUnavailable(503)/ErrTimeout(504)，
//	这里按错误码判别即可。
func (e *Engine) degradeReasonFor(err error) string {
	msg := err.Error()
	switch {
	case provider.IsUnsupported(err), strings.Contains(msg, "not implemented"),
		strings.Contains(msg, "501"):
		return "rerank_unsupported: 未配置重排模型或该模型缺 rerank_path，已按融合分排序（" + msg + "）"
	case errors.Is(err, context.DeadlineExceeded), strings.Contains(msg, "timeout"),
		strings.Contains(msg, "504"):
		return "rerank_timeout: 重排超时，已按融合分排序（" + msg + "）"
	default:
		return "rerank_failed: 重排服务报错，已按融合分排序（" + msg + "）"
	}
}

// appendReason 追加降级原因，避免覆盖更早阶段的根因。
//
// 根因在前：若 embedding 失败触发关键词降级、后续又发生别的降级，
// 先写的才是导致本次检索降级的起点。
func appendReason(existing, next string) string {
	if next == "" {
		return existing
	}
	if existing == "" {
		return next
	}
	return existing + "; " + next
}

// ---------- 单来源检索辅助 ----------

// searchOne 单知识库向量检索
// window 参数已随「外扩后置」移除：外扩不再消耗召回名额，
// 故不再需要额外多召回 window*2 条。
func (e *Engine) searchOne(ctx context.Context, kbID int64, vec []float32, req *Request, topK int, threshold float64) (*vectordb.SearchResult, error) {
	// 召回数按 candidateK 算，而不是 topK：
	// 重排要从更多候选里挑出更好的几条，召回数若等于 topK，
	// 重排就只能重新排序、无法替换，等于白花一次调用。
	//
	// ★ 不再 "+ window*2"：上下文外扩已推迟到重排之后，
	//   外扩不再消耗召回名额。多召回的部分纯粹是为了抵消
	//   PG 中切片缺失被跳过造成的损耗。
	recall := e.candidateK(topK)
	if recall <= 0 {
		recall = topK
	}
	sbr, err := e.vector.Search(ctx, &vectordb.SearchRequest{
		Collection:     e.collectionOf(kbID),
		Vector:         vec,
		TopK:           recall,
		ScoreThreshold: threshold,
		Tenant:         req.Identity.GetTenant(),
		ExcludeDocIDs:  req.ExcludeDocIDs,
		MetricType:     e.mcfg.MetricType,
		HNSWEf:         e.mcfg.HNSWEf,
	})
	if err != nil {
		return nil, err
	}
	return sbr, nil
}

// candidate 候选切片。
//
// ★ 为什么要自己包一层而不是直接用 *ragv1.Hit：
//
//	外扩需要切片的 seq（Neighbors 按 seq 取相邻切片），
//	但 ragv1.Hit 里只有 PageNo、没有 seq —— 拆分 hydrate 时
//	若只透传 Hit，外扩就只能拿页码当序号用（错的）。
//
//	这一层纯内部，序列化回 pb 时只取 hit。
type candidate struct {
	hit *ragv1.Hit
	seq int32
}

// expandContext 对已入选的切片做上下文外扩：把相邻切片并入内容。
//
// 只对最终 topN 做 —— 外扩会放大文本长度，
// 对还在候选池里的切片做纯属浪费（它们大概率不会被选中）。
//
// covered 收集**已入选 + 已外扩**的全部切片 id：
// 相邻切片可能同时是另一条命中结果的邻居，不去重会被重复拼进多处。
func (e *Engine) expandContext(ctx context.Context, cands []candidate, window int) {
	if window <= 0 || len(cands) == 0 {
		return
	}
	covered := make(map[string]bool, len(cands)*3)
	for _, c := range cands {
		covered[c.hit.ChunkId] = true
	}
	for i := range cands {
		h := cands[i].hit
		// web 条目（DocId=0）没有邻居，跳过外扩
		if h.GetDocId() == 0 {
			continue
		}
		neighbors, err := e.meta.Neighbors(ctx, h.DocId, cands[i].seq, window, window)
		if err != nil || len(neighbors) <= 1 {
			// 外扩失败不是致命问题：保留原文即可。
			// 强行中断会让「可用的部分结果」变成「整次检索失败」
			continue
		}
		var parts []string
		for _, n := range neighbors {
			if !covered[n.GetChunkId()] {
				parts = append(parts, n.GetContent())
				covered[n.GetChunkId()] = true
			}
		}
		if len(parts) == 0 {
			continue
		}
		// 自身内容始终排第一，外扩内容追加在后
		parts = append([]string{h.Content}, parts...)
		h.Content = joinContent(parts)
	}
}

// embed 查询向量化：同时返回稠密与稀疏向量。
func (e *Engine) embed(ctx context.Context, query string, identity *commonv1.Identity) ([]float32, []vectordb.SparseToken, error) {
	if query == "" {
		return nil, nil, fmt.Errorf("检索词为空")
	}
	resp, err := e.llm.Embed(ctx, &llmv1.EmbedReq{
		Texts: []string{query},
		Model: e.embedModel(),
		// 查询与文档必须用不同 input_type，否则召回质量下降
		InputType: "query",
		Identity:  identity,
		// ★ 同时请求稀疏向量：模型支持时返回 sparse_embeddings，否则为空
		EnableSparse: true,
	})
	if err != nil {
		return nil, nil, err
	}
	if len(resp.GetEmbeddings()) == 0 || len(resp.GetEmbeddings()[0].GetVector()) == 0 {
		return nil, nil, fmt.Errorf("embedding 结果为空")
	}
	// ★ 维度一致性校验：与知识库声明不一致时宁可降级，
	//   也不要用错误维度的向量去检索（结果会完全失真）
	got := int(resp.GetDimension())
	want := e.dim()
	if want > 0 && got != want {
		return nil, nil, fmt.Errorf("向量维度不匹配：期望 %d，实际 %d", want, got)
	}
	// 取第一条的稀疏向量（查询为单条输入）
	var sparse []vectordb.SparseToken
	if se := resp.GetSparseEmbeddings(); len(se) > 0 {
		for _, t := range se[0].GetTokens() {
			sparse = append(sparse, vectordb.SparseToken{Index: t.GetIndex(), Value: t.GetValue()})
		}
	}
	return resp.GetEmbeddings()[0].GetVector(), sparse, nil
}

func (e *Engine) embedModel() string { return e.embedModelName }

func (e *Engine) dim() int {
	if e.dimOverride > 0 {
		return e.dimOverride
	}
	return 1024
}

func (e *Engine) collectionOf(kbID int64) string {
	return fmt.Sprintf("aegis_kb_%d", kbID)
}

func (e *Engine) fillTitles(ctx context.Context, hits []*ragv1.Hit) {
	need := make(map[int64]bool)
	for _, h := range hits {
		// web 条目（DocId=0）自带标题，跳过
		if h.DocTitle == "" && h.DocId != 0 {
			need[h.DocId] = true
		}
	}
	for docID := range need {
		rec, err := e.meta.GetDoc(ctx, docID)
		if err != nil {
			continue
		}
		title := rec.Doc.GetFileName()
		for _, h := range hits {
			if h.DocId == docID {
				h.DocTitle = title
			}
		}
	}
}

// mergeResults 合并多知识库检索结果（按分数降序）
func mergeResults(dst, src *vectordb.SearchResult) {
	if src == nil {
		return
	}
	if dst.Scores == nil {
		dst.Scores = make(map[string]float64)
	}
	for id, score := range src.Scores {
		dst.Scores[id] = score
	}
	dst.ChunkIDs = append(dst.ChunkIDs, src.ChunkIDs...)
	sortByScore(dst)
}

func sortByScore(r *vectordb.SearchResult) {
	// 简单插入排序：结果集很小（topK 量级），不需要更复杂算法
	for i := 1; i < len(r.ChunkIDs); i++ {
		id := r.ChunkIDs[i]
		score := r.Scores[id]
		j := i - 1
		for j >= 0 && r.Scores[r.ChunkIDs[j]] < score {
			r.ChunkIDs[j+1] = r.ChunkIDs[j]
			j--
		}
		r.ChunkIDs[j+1] = id
	}
}

func joinContent(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "\n"
		}
		out += p
	}
	return out
}

// effectiveThreshold 生产/模拟阈值选择器。
//
// mock 模式下把阈值下调到接近 0，让 topN 数量由候选池大小而非分数决定 ——
// 这样 mock provider 返回的随机分数也能稳定产出可预期的命中条数，
// 测试断言不用跟具体分数耦合。
//
// mockDefault <= 0 视为未配置，回退到 prod 阈值（见 RAGConf.MockScoreThreshold）。
func effectiveThreshold(mockMode bool, prod, mockDefault float64) float64 {
	if mockMode && mockDefault > 0 {
		return mockDefault
	}
	return prod
}
