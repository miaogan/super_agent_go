// Package service 实现 rag_service 的业务 handler。
package service

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"time"

	commonv1 "github.com/aegis/idl/kitex_gen/commonv1"
	ragv1 "github.com/aegis/idl/kitex_gen/ragv1"

	"github.com/aegis/pkg/conf"
	"github.com/aegis/pkg/errno"
	"github.com/aegis/pkg/fetch"
	"github.com/aegis/pkg/logger"
	"github.com/aegis/pkg/minio"
	"github.com/aegis/pkg/mockx"
	"github.com/aegis/pkg/pagination"
	"github.com/aegis/pkg/ssrf"
	"github.com/aegis/pkg/vectordb"

	"github.com/aegis/rag_service/internal/ingest"
	"github.com/aegis/rag_service/internal/meta"
	"github.com/aegis/rag_service/internal/retrieve"
)

// RagService 实现 ragv1.RagService
type RagService struct {
	meta      meta.Store
	vector    vectordb.Store
	pipeline  *ingest.Pipeline
	engine    *retrieve.Engine
	mock      *mockx.FaultInjector
	cfg       conf.RAGConf
	minio     minio.Store
	startedAt time.Time
}

// New 构造 handler（meta 传 PG 或内存实现，见 cmd/main.go 装配）
func New(m meta.Store, v vectordb.Store, p *ingest.Pipeline, e *retrieve.Engine,
	mock *mockx.FaultInjector, cfg conf.RAGConf, mstore minio.Store) *RagService {
	return &RagService{
		meta:      m,
		vector:    v,
		pipeline:  p,
		engine:    e,
		mock:      mock,
		cfg:       cfg,
		minio:     mstore,
		startedAt: time.Now(),
	}
}

var _ ragv1.RagService = (*RagService)(nil)

func nowSec() int64 { return time.Now().Unix() }

// ---------- 健康检查 ----------

func (s *RagService) Health(ctx context.Context, _ *commonv1.HealthReq) (*commonv1.HealthResp, error) {
	// 向量库不可用时服务仍算健康：降级链能兜住，
	// 但要在日志中显式告警（这是最容易被忽视的隐患）
	if err := s.vector.Ping(ctx); err != nil {
		logger.Warn(ctx, "向量库不可用，检索将走降级链", "err", err)
	}

	// 统计信息仅在日志中体现（健康响应体字段由 proto 固定，不扩展）
	if st := s.meta.Stats(); st.Pending > 0 || st.Failed > 0 {
		logger.Warn(ctx, "存在待补偿切片",
			"pending", st.Pending, "failed", st.Failed, "chunks", st.Chunks)
	}

	return &commonv1.HealthResp{
		Healthy:   !s.mock.ShouldFail("health"),
		Service:   "rag_service",
		Version:   "dev",
		UptimeSec: uint64(time.Since(s.startedAt).Seconds()),
		MockMode:  s.mock.Enabled(),
	}, nil
}

// ---------- 知识库 ----------

func (s *RagService) CreateKnowledgeBase(ctx context.Context, req *ragv1.CreateKnowledgeBaseReq) (*ragv1.CreateKnowledgeBaseResp, error) {
	if err := s.mock.Sleep(ctx); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(req.GetName())
	if name == "" {
		return nil, errno.New(errno.ErrMissingParam)
	}

	// 校验 Embedding 模型与维度：
	// 维度一旦确定就不能改（改了等于重建整个集合），必须在此拦死
	dim := int32(s.cfgEmbedDim())
	embedModel := req.GetEmbedModel()
	if embedModel == "" {
		// ★ 必须与 llm_service 注册的 embedding 模型一致
		//  （compose AEGIS_LLM__MODELS__1__NAME = text-embedding-v4，
		//   走 DashScope 原生接口返回 dense 1024 维 + sparse SPLADE 向量）。
		embedModel = "text-embedding-v4"
	}
	if got := s.dimOf(embedModel); got != 0 && got != int(dim) {
		return nil, errno.New(errno.ErrEmbeddingDimMismatch)
	}

	chunkSize := req.GetChunkSize()
	if chunkSize <= 0 {
		chunkSize = int32(s.cfg.DefaultChunkSize)
	}
	chunkOverlap := req.GetChunkOverlap()
	if chunkOverlap < 0 {
		chunkOverlap = int32(s.cfg.DefaultChunkOverlap)
	}
	if chunkOverlap >= chunkSize {
		chunkOverlap = chunkSize / 4
	}
	splitter := req.GetSplitter()
	if splitter == "" {
		splitter = "recursive"
	}

	now := nowSec()
	kbID, err := s.meta.NextKBID(ctx)
	if err != nil {
		return nil, err
	}
	kb := &ragv1.KnowledgeBase{
		KbId:         kbID,
		Name:         name,
		Description:  req.GetDescription(),
		EmbedModel:   embedModel,
		EmbeddingDim: dim,
		ChunkSize:    chunkSize,
		ChunkOverlap: chunkOverlap,
		Splitter:     splitter,
		OwnerUid:     req.GetIdentity().GetUid(),
		Tenant:       firstNonEmpty(req.GetIdentity().GetTenant(), "default"),
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	// 预建集合：维度不匹配在此即暴露，而不是等首个文档入库才失败
	if err := s.vector.EnsureCollection(ctx, s.collectionOf(kb.KbId), int(dim)); err != nil {
		logger.Error(ctx, "预建向量集合失败", "kb_id", kb.KbId,
			"collection", s.collectionOf(kb.KbId), "dim", dim, "err", err)
		return nil, errno.Wrap(errno.ErrVectorDBUnavailable, err)
	}

	if err := s.meta.CreateKB(ctx, kb); err != nil {
		var conflict *meta.ErrConflict
		if errors.As(err, &conflict) {
			return nil, errno.Wrap(errno.ErrAlreadyExists, err)
		}
		return nil, err
	}

	logger.Info(ctx, "知识库创建", "kb_id", kb.KbId, "name", kb.Name,
		"dim", dim, "embed_model", embedModel)
	return &ragv1.CreateKnowledgeBaseResp{Success: true, KbId: kb.KbId, Kb: kb}, nil
}

func (s *RagService) GetKnowledgeBase(ctx context.Context, req *ragv1.GetKnowledgeBaseReq) (*ragv1.GetKnowledgeBaseResp, error) {
	if err := s.mock.Sleep(ctx); err != nil {
		return nil, err
	}
	kb, err := s.meta.GetKB(ctx, req.GetKbId())
	if err != nil {
		return nil, errno.Wrap(errno.ErrKBNotFound, err)
	}
	if err := s.checkKBPermission(req.GetIdentity(), kb); err != nil {
		return nil, err
	}
	return &ragv1.GetKnowledgeBaseResp{Success: true, Kb: kb}, nil
}

func (s *RagService) ListKnowledgeBases(ctx context.Context, req *ragv1.ListKnowledgeBasesReq) (*ragv1.ListKnowledgeBasesResp, error) {
	if err := s.mock.Sleep(ctx); err != nil {
		return nil, err
	}
	page, size := pagination.Normalize(req.GetPage())
	kbs, total := s.meta.ListKB(ctx, req.GetKeyword(), req.GetTenant(), req.GetOwnerUid(), page, size)
	return &ragv1.ListKnowledgeBasesResp{
		Kbs:  kbs,
		Meta: pagination.Meta(req.GetPage(), total),
	}, nil
}

func (s *RagService) UpdateKnowledgeBase(ctx context.Context, req *ragv1.UpdateKnowledgeBaseReq) (*ragv1.UpdateKnowledgeBaseResp, error) {
	if err := s.mock.Sleep(ctx); err != nil {
		return nil, err
	}
	kb, err := s.meta.GetKB(ctx, req.GetKbId())
	if err != nil {
		return nil, errno.Wrap(errno.ErrKBNotFound, err)
	}
	if err := s.checkKBPermission(req.GetIdentity(), kb); err != nil {
		return nil, err
	}

	// ★ 切片参数变更只影响后续入库，已入库文档不重切。
	//   若要重切全部文档需显式触发重建，否则会造成"部分文档用新参数"的混乱。
	kb.Name = req.GetName()
	kb.Description = req.GetDescription()
	if req.GetChunkSize() > 0 {
		kb.ChunkSize = req.GetChunkSize()
	}
	if req.GetChunkOverlap() >= 0 {
		kb.ChunkOverlap = req.GetChunkOverlap()
	}
	if req.GetSplitter() != "" {
		kb.Splitter = req.GetSplitter()
	}
	kb.UpdatedAt = nowSec()

	if err := s.meta.UpdateKB(ctx, kb, req.GetUpdateMask()); err != nil {
		return nil, errno.Wrap(errno.ErrKBNotFound, err)
	}
	return &ragv1.UpdateKnowledgeBaseResp{Success: true, Kb: kb}, nil
}

func (s *RagService) DeleteKnowledgeBase(ctx context.Context, req *ragv1.DeleteKnowledgeBaseReq) (*commonv1.OpResult, error) {
	if err := s.mock.Sleep(ctx); err != nil {
		return nil, err
	}
	kb, err := s.meta.GetKB(ctx, req.GetKbId())
	if err != nil {
		return nil, errno.Wrap(errno.ErrKBNotFound, err)
	}
	if err := s.checkKBPermission(req.GetIdentity(), kb); err != nil {
		return nil, err
	}

	// 向量库删除失败不阻断元数据删除：
	// 残留向量因 PG 已无正文，检索时会自然跳过（见 retrieve.hydrate）
	if err := s.vector.DeleteByKB(ctx, s.collectionOf(kb.KbId), kb.KbId); err != nil {
		logger.Warn(ctx, "删除向量失败，可能残留孤儿向量", "kb_id", kb.KbId, "err", err)
	}
	if err := s.meta.DeleteKB(ctx, kb.KbId); err != nil {
		return nil, errno.Wrap(errno.ErrKBNotFound, err)
	}

	logger.Info(ctx, "知识库删除", "kb_id", kb.KbId)
	return &commonv1.OpResult{Success: true, Message: "已删除"}, nil
}

// ---------- 文档 ----------

func (s *RagService) UploadDocument(ctx context.Context, req *ragv1.UploadDocumentReq) (*ragv1.UploadDocumentResp, error) {
	if err := s.mock.Sleep(ctx); err != nil {
		return nil, err
	}
	if !s.cfg.EnableIngest {
		return nil, errno.New(errno.ErrForbidden)
	}

	kb, err := s.meta.GetKB(ctx, req.GetKbId())
	if err != nil {
		return nil, errno.Wrap(errno.ErrKBNotFound, err)
	}
	if err := s.checkKBPermission(req.GetIdentity(), kb); err != nil {
		return nil, err
	}

	if req.GetFileName() == "" {
		return nil, errno.New(errno.ErrMissingParam)
	}
	// content 或 file_url 二选一；file_url 有值时触发安全拉取
	content := req.GetContent()
	if len(content) == 0 && req.GetFileUrl() != "" {
		var err error
		content, err = s.fetchFileURL(ctx, req.GetFileUrl())
		if err != nil {
			return nil, err
		}
	}
	if len(content) == 0 {
		return nil, errno.New(errno.ErrMissingParam)
	}

	// 文件类型白名单
	ft := normalizeFileType(req.GetFileType())
	if !s.isTypeAllowed(ft) {
		return nil, errno.New(errno.ErrUnsupportedFormat)
	}
	// 大小限制（content 是原始字节，拉取场景同样适用）
	if maxMB := s.cfg.MaxFileSizeMB; maxMB > 0 {
		if len(content) > maxMB*1024*1024 {
			return nil, errno.New(errno.ErrDocumentTooLarge)
		}
	}

	// hash 去重（秒传）：同知识库内相同内容不重复入库
	hash := req.GetFileHash()
	if hash == "" {
		hash = ingest.HashContent(content)
	}
	dupDocID, dup := s.findDuplicate(ctx, kb.KbId, hash)
	if dup {
		rec, err := s.meta.GetDoc(ctx, dupDocID)
		if err == nil {
			logger.Info(ctx, "命中文件去重", "kb_id", kb.KbId, "doc_id", dupDocID)
			return &ragv1.UploadDocumentResp{
				Success:    true,
				DocId:      dupDocID,
				Message:    "文件内容重复，已跳过入库",
				Duplicated: true,
				Document:   rec.Doc,
			}, nil
		}
	}

	now := nowSec()
	docID, err := s.meta.NextDocID(ctx)
	if err != nil {
		return nil, err
	}

	// 上传原始副本到 MinIO（可降级：MinIO 挂了不阻断 ingest）
	var objectKey string
	if s.minio != nil && len(content) > 0 {
		ext := strings.TrimPrefix(ft, "x-") // pdf→pdf, application/pdf→留空由 objectKey 默认处理
		if objectKey = minio.ObjectKeyOf(kb.KbId, docID, hash, ext); objectKey != "" {
			if err := s.minio.Put(ctx, objectKey,
				bytes.NewReader(content),
				int64(len(content)), "application/octet-stream"); err != nil {
				logger.Warn(ctx, "MinIO 上传失败（降级为内存直存）",
					"kb_id", kb.KbId, "doc_id", docID, "err", err)
				objectKey = ""
			} else {
				logger.Info(ctx, "MinIO 已存原始文件",
					"kb_id", kb.KbId, "doc_id", docID, "object_key", objectKey, "size", len(content))
			}
		}
	}

	rec := &meta.DocumentRecord{
		Doc: &ragv1.Document{
			DocId:     docID,
			KbId:      kb.KbId,
			FileName:  req.GetFileName(),
			FileType:  ft,
			FileSize:  int64(len(content)),
			FileHash:  hash,
			Status:    int32(meta.DocPending),
			CreatedAt: now,
			UpdatedAt: now,
		},
		RawBytes:  content,
		ObjectKey: objectKey,
	}
	if err := s.meta.CreateDoc(ctx, rec); err != nil {
		var conflict *meta.ErrConflict
		if errors.As(err, &conflict) {
			return nil, errno.Wrap(errno.ErrAlreadyExists, err)
		}
		return nil, err
	}

	// 同步执行入库流水线：mock 与小文件场景下可立即返回完整结果。
	// 生产大文件应改为异步任务 + 轮询（见文档中的异步入库设计）。
	res, err := s.pipeline.Ingest(ctx, kb, rec, req.GetIdentity())
	if err != nil {
		return &ragv1.UploadDocumentResp{
			Success:  false,
			DocId:    docID,
			Message:  err.Error(),
			Document: rec.Doc,
		}, nil
	}

	updated, err := s.meta.GetDoc(ctx, docID)
	if err != nil {
		updated = rec
	}
	return &ragv1.UploadDocumentResp{
		Success: true,
		DocId:   docID,
		Message: messageOf(res),
		Document: func() *ragv1.Document {
			d := updated.Doc
			d.ParsedChunks = res.Vectorized
			return d
		}(),
	}, nil
}

func (s *RagService) GetDocument(ctx context.Context, req *ragv1.GetDocumentReq) (*ragv1.GetDocumentResp, error) {
	if err := s.mock.Sleep(ctx); err != nil {
		return nil, err
	}
	// 文档属主/租户校验：不能只凭 doc_id 取
	if _, _, err := s.docAccess(ctx, req.GetIdentity(), req.GetDocId(), false); err != nil {
		return nil, err
	}
	rec, err := s.meta.GetDoc(ctx, req.GetDocId())
	if err != nil {
		return nil, errno.Wrap(errno.ErrDocNotFound, err)
	}
	return &ragv1.GetDocumentResp{Success: true, Document: rec.Doc}, nil
}

func (s *RagService) ListDocuments(ctx context.Context, req *ragv1.ListDocumentsReq) (*ragv1.ListDocumentsResp, error) {
	if err := s.mock.Sleep(ctx); err != nil {
		return nil, err
	}
	// 按 kbID 校验：否则可通过列举任意 KB 的文档清单探测他人数据
	if _, err := s.kbAccess(ctx, req.GetIdentity(), req.GetKbId()); err != nil {
		return nil, err
	}
	page, size := pagination.Normalize(req.GetPage())
	docs, total := s.meta.ListDocs(ctx, req.GetKbId(), req.GetStatus(), req.GetKeyword(), page, size)
	return &ragv1.ListDocumentsResp{
		Documents: docs,
		Meta:      pagination.Meta(req.GetPage(), total),
	}, nil
}

func (s *RagService) DeleteDocument(ctx context.Context, req *ragv1.DeleteDocumentReq) (*commonv1.OpResult, error) {
	if err := s.mock.Sleep(ctx); err != nil {
		return nil, err
	}
	// 删除是破坏性操作：要求 owner 或 admin（同租户不够）
	rec, _, err := s.docAccess(ctx, req.GetIdentity(), req.GetDocId(), true)
	if err != nil {
		return nil, err
	}
	if err := s.vector.DeleteByDoc(ctx, s.collectionOf(rec.Doc.GetKbId()), rec.Doc.GetDocId()); err != nil {
		logger.Warn(ctx, "删除文档向量失败", "doc_id", req.GetDocId(), "err", err)
	}
	// 级联删除 MinIO 对象（可降级）
	if rec.ObjectKey != "" && s.minio != nil {
		if err := s.minio.Delete(ctx, rec.ObjectKey); err != nil {
			logger.Warn(ctx, "删除 MinIO 对象失败（非阻断）",
				"doc_id", req.GetDocId(), "object_key", rec.ObjectKey, "err", err)
		}
	}
	if err := s.meta.DeleteDoc(ctx, req.GetDocId()); err != nil {
		return nil, errno.Wrap(errno.ErrDocNotFound, err)
	}
	_ = s.meta.RecountKB(ctx, rec.Doc.GetKbId())
	return &commonv1.OpResult{Success: true, Message: "已删除"}, nil
}

func (s *RagService) RetryDocument(ctx context.Context, req *ragv1.RetryDocumentReq) (*commonv1.OpResult, error) {
	if err := s.mock.Sleep(ctx); err != nil {
		return nil, err
	}
	// 重试会重写切片与向量，属破坏性操作：要求 owner 或 admin。
	// 原实现拿到 identity 却只用于传给 Ingest，没做任何授权。
	rec, kb, err := s.docAccess(ctx, req.GetIdentity(), req.GetDocId(), true)
	if err != nil {
		return nil, err
	}

	// re_embed=true 时只重试向量化，保留已有正文与切片
	if !req.GetReEmbed() {
		// 全流程重跑：重新切片
		if len(rec.RawBytes) == 0 {
			return nil, errno.New(errno.ErrParseFailed)
		}
	}

	if _, err := s.pipeline.Ingest(ctx, kb, rec, req.GetIdentity()); err != nil {
		return &commonv1.OpResult{Success: false, Message: err.Error()}, nil
	}
	return &commonv1.OpResult{Success: true, Message: "重试完成"}, nil
}

// ---------- 切片 ----------

func (s *RagService) ListChunks(ctx context.Context, req *ragv1.ListChunksReq) (*ragv1.ListChunksResp, error) {
	if err := s.mock.Sleep(ctx); err != nil {
		return nil, err
	}
	// 切片正文等于文档内容，读权限与 GetDocument 一致
	if _, _, err := s.docAccess(ctx, req.GetIdentity(), req.GetDocId(), false); err != nil {
		return nil, err
	}
	page, size := pagination.Normalize(req.GetPage())
	chunks, total := s.meta.ListChunks(ctx, req.GetDocId(), page, size)
	return &ragv1.ListChunksResp{
		Chunks: chunks,
		Meta:   pagination.Meta(req.GetPage(), total),
	}, nil
}

// ---------- 检索 ----------

func (s *RagService) Retrieve(ctx context.Context, req *ragv1.RetrieveReq) (*ragv1.RetrieveResp, error) {
	if err := s.mock.Sleep(ctx); err != nil {
		return nil, err
	}
	if !s.cfg.EnableRAG {
		return &ragv1.RetrieveResp{
			Query:         req.GetQuery(),
			Hits:          []*ragv1.Hit{},
			Degraded:      true,
			DegradeReason: "RAG 未启用",
		}, nil
	}

	res, err := s.engine.Retrieve(ctx, &retrieve.Request{
		Query:          req.GetQuery(),
		KBIDs:          req.GetKbIds(),
		TopK:           int(req.GetTopK()),
		ScoreThreshold: req.GetScoreThreshold(),
		ContextWindow:  int(req.GetContextWindow()),
		ExcludeDocIDs:  req.GetExcludeDocIds(),
		Identity:       req.GetIdentity(),
		RequestID:      req.GetRequestId(),
	})
	if err != nil {
		return nil, err
	}
	return toRetrieveResp(res), nil
}

func (s *RagService) DebugRetrieve(ctx context.Context, req *ragv1.DebugRetrieveReq) (*ragv1.DebugRetrieveResp, error) {
	if err := s.mock.Sleep(ctx); err != nil {
		return nil, err
	}
	res, err := s.engine.Retrieve(ctx, &retrieve.Request{
		Query:          req.GetQuery(),
		KBIDs:          req.GetKbIds(),
		TopK:           int(req.GetTopK()),
		ScoreThreshold: req.GetScoreThreshold(),
		Identity:       req.GetIdentity(),
		Debug:          true,
	})
	if err != nil {
		return nil, err
	}

	return &ragv1.DebugRetrieveResp{
		Result:       toRetrieveResp(res),
		EmbedModel:   res.EmbedModel,
		Dimension:    res.Dimension,
		MilvusFilter: res.MilvusFilter,
		Stages:       res.Stages,
	}, nil
}

func toRetrieveResp(res *retrieve.Result) *ragv1.RetrieveResp {
	out := &ragv1.RetrieveResp{
		Query:         res.Query,
		Hits:          res.Hits,
		Total:         res.Total,
		Degraded:      res.Degraded,
		DegradeReason: res.DegradeReason,
		RetrieveMs:    res.RetrieveMS,
		EmbeddingMs:   res.EmbeddingMS,
		SearchMs:      res.SearchMS,
	}
	if out.Hits == nil {
		out.Hits = []*ragv1.Hit{}
	}
	return out
}

// ---------- 挂载 ----------

func (s *RagService) BindAgentKB(ctx context.Context, req *ragv1.BindAgentKBReq) (*commonv1.OpResult, error) {
	if err := s.mock.Sleep(ctx); err != nil {
		return nil, err
	}
	if req.GetAgentId() == 0 {
		return nil, errno.New(errno.ErrInvalidParam)
	}
	// 校验所有知识库存在且有权访问，避免挂上不可用的 KB
	for _, id := range req.GetKbIds() {
		kb, err := s.meta.GetKB(ctx, id)
		if err != nil {
			return nil, errno.Wrap(errno.ErrKBNotFound, err)
		}
		if err := s.checkKBPermission(req.GetIdentity(), kb); err != nil {
			return nil, err
		}
	}
	if err := s.meta.BindAgentKB(ctx, req.GetAgentId(), req.GetKbIds()); err != nil {
		return nil, err
	}
	logger.Info(ctx, "Agent 知识库挂载更新",
		"agent_id", req.GetAgentId(), "kb_count", len(req.GetKbIds()))
	return &commonv1.OpResult{Success: true, Message: "已更新"}, nil
}

func (s *RagService) ListAgentKB(ctx context.Context, req *ragv1.ListAgentKBReq) (*ragv1.ListAgentKBResp, error) {
	if err := s.mock.Sleep(ctx); err != nil {
		return nil, err
	}
	kbs, err := s.meta.ListAgentKB(ctx, req.GetAgentId())
	if err != nil {
		return nil, err
	}
	return &ragv1.ListAgentKBResp{Kbs: kbs}, nil
}

// ---------- 辅助 ----------

// checkKBPermission 知识库访问控制
//
// 规则：admin 可访问全部；owner 与同租户可访问；其他拒绝。
func (s *RagService) checkKBPermission(id *commonv1.Identity, kb *ragv1.KnowledgeBase) error {
	if id.GetUid() == 0 {
		return nil // 内部调用未带身份，放行
	}
	if hasAdminScope(id) {
		return nil
	}
	if kb.GetOwnerUid() == id.GetUid() {
		return nil
	}
	// 同租户可读：知识库是租户级共享资源
	if kb.GetTenant() != "" && kb.GetTenant() == id.GetTenant() {
		return nil
	}
	return errno.New(errno.ErrKBAccessDenied)
}

// hasAdminScope 判断是否带 admin 或通配 scope。
func hasAdminScope(id *commonv1.Identity) bool {
	for _, role := range id.GetScopes() {
		if role == "admin" || role == "*" {
			return true
		}
	}
	return false
}

// docAccess 解析文档归属并校验权限，返回文档记录与其所属知识库。
//
// ★ 为什么需要它：文档级接口（Get/List/Delete/Retry/Chunks）过去只按
//
//	doc_id 直查元数据，既不查归属也不校验权限，而网关对 /api/v1 只做
//	「是否登录」不做「是否属于你」——于是任何登录用户枚举自增 doc_id
//	就能读、甚至删除他人文档（连带向量与 MinIO 对象）。
//	KB 级接口一直有 checkKBPermission，这里补齐缺失的那一半。
//
// write=true 用于破坏性操作（删除/重试）：要求 owner 或 admin。
// 同租户可以共享**阅读**知识库，但不该因为同租户就能删别人上传的文档。
//
// 与 checkKBPermission 不同，这里不接受 uid==0 的匿名放行：
// 这些方法只有网关（已过 Auth）会调用，没有内部免身份路径。
// 真出现免身份调用说明上游鉴权被绕过，应当拒绝而不是放行。
func (s *RagService) docAccess(ctx context.Context, id *commonv1.Identity, docID int64, write bool) (*meta.DocumentRecord, *ragv1.KnowledgeBase, error) {
	rec, err := s.meta.GetDoc(ctx, docID)
	if err != nil {
		return nil, nil, errno.Wrap(errno.ErrDocNotFound, err)
	}
	kb, err := s.meta.GetKB(ctx, rec.Doc.GetKbId())
	if err != nil {
		return nil, nil, errno.Wrap(errno.ErrKBNotFound, err)
	}
	if err := s.checkDocPermission(id, kb, write); err != nil {
		return nil, nil, err
	}
	return rec, kb, nil
}

// checkDocPermission 见 docAccess。
func (s *RagService) checkDocPermission(id *commonv1.Identity, kb *ragv1.KnowledgeBase, write bool) error {
	if id.GetUid() == 0 {
		return errno.New(errno.ErrKBAccessDenied)
	}
	if hasAdminScope(id) {
		return nil
	}
	if kb.GetOwnerUid() == id.GetUid() {
		return nil
	}
	if write {
		return errno.New(errno.ErrKBAccessDenied)
	}
	// 读操作沿用知识库的租户级共享语义
	if kb.GetTenant() != "" && kb.GetTenant() == id.GetTenant() {
		return nil
	}
	return errno.New(errno.ErrKBAccessDenied)
}

// kbAccess 按 kbID 校验知识库权限并返回该知识库（读语义）。
//
// ListDocuments 只有 kbID 没有 docID，用它走同一条权限判定，
// 避免"列文档"成为 GetKB 之外的另一条越权读取路径。
func (s *RagService) kbAccess(ctx context.Context, id *commonv1.Identity, kbID int64) (*ragv1.KnowledgeBase, error) {
	kb, err := s.meta.GetKB(ctx, kbID)
	if err != nil {
		return nil, errno.Wrap(errno.ErrKBNotFound, err)
	}
	if err := s.checkKBPermission(id, kb); err != nil {
		return nil, err
	}
	return kb, nil
}

func (s *RagService) collectionOf(kbID int64) string {
	return "aegis_kb_" + itoa(kbID)
}

func (s *RagService) cfgEmbedDim() int { return 1024 }

// dimOf 返回模型对应维度，未知模型返回 0（表示不校验）
func (s *RagService) dimOf(model string) int {
	switch strings.ToLower(model) {
	case "bge-m3", "text-embedding-v3", "gte-large":
		return 1024
	case "text-embedding-v2":
		return 1536
	case "text-embedding-ada-002":
		return 1536
	default:
		return 0
	}
}

func (s *RagService) isTypeAllowed(ft string) bool {
	if len(s.cfg.AllowedTypes) == 0 {
		return true
	}
	for _, t := range s.cfg.AllowedTypes {
		if strings.EqualFold(t, ft) {
			return true
		}
	}
	return false
}

func (s *RagService) findDuplicate(ctx context.Context, kbID int64, hash string) (int64, bool) {
	if hash == "" {
		return 0, false
	}
	docs, total := s.meta.ListDocs(ctx, kbID, 0, "", 1, 1000)
	for _, d := range docs {
		if d.GetFileHash() == hash {
			return d.GetDocId(), true
		}
	}
	_ = total
	return 0, false
}

func normalizeFileType(ft string) string {
	ft = strings.ToLower(strings.TrimSpace(ft))
	if ft == "" {
		return "txt"
	}
	// 去前导点，兼容 ".pdf" 与 "pdf" 两种写法
	return strings.TrimPrefix(ft, ".")
}

// fetchFileURL 安全拉取 file_url 的远程内容（含 SSRF 防护）。
//
// 双层防护：
//  1. ssrf.Validate 前置校验（协议白名单 + IP 字面量拒绝 + DNS 解析内网检测）
//  2. fetch.Fetch 的 HTTP Transport 再做 dial 时 IP 二次校验 + 重定向跟随校验
func (s *RagService) fetchFileURL(ctx context.Context, rawURL string) ([]byte, error) {
	ssrfPolicy := ssrf.Policy{
		AllowPrivateIPs: s.cfg.AllowPrivateIPs,
		AllowedSchemes:  []string{"http", "https"},
		DNSRecheckCount: 3,
	}
	timeout := s.cfg.FetchTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	maxMB := s.cfg.FetchMaxFileSizeMB
	if maxMB <= 0 {
		maxMB = 20
	}

	data, err := fetch.Fetch(ctx, rawURL, fetch.Options{
		Timeout:      timeout,
		MaxSizeBytes: int64(maxMB) * 1024 * 1024,
		SSRF:         ssrfPolicy,
	})
	if err != nil {
		logger.Warn(ctx, "file_url 拉取失败", "url", rawURL, "err", err)
		if errors.Is(err, ssrf.ErrBlocked) {
			return nil, errno.New(errno.ErrForbidden)
		}
		if errors.Is(err, fetch.ErrTooLarge) {
			return nil, errno.New(errno.ErrDocumentTooLarge)
		}
		return nil, errno.Wrap(errno.ErrUpstreamError, err)
	}
	return data, nil
}

func messageOf(res *ingest.Result) string {
	if res.Degraded {
		return "已入库，部分切片待补偿：" + res.DegradeReason
	}
	return "入库成功"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
