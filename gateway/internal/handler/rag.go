package handler

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/utils"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	ragv1 "github.com/aegis/idl/kitex_gen/ragv1"
)

// ---------- 知识库 ----------

// createKBReq 创建知识库请求
type createKBReq struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	EmbedModel   string `json:"embed_model"`
	ChunkSize    int32  `json:"chunk_size"`
	ChunkOverlap int32  `json:"chunk_overlap"`
	Splitter     string `json:"splitter"`
}

// CreateKB 创建知识库
func (d *Deps) CreateKB(c *app.RequestContext) {
	var req createKBReq
	if err := c.BindAndValidate(&req); err != nil || req.Name == "" {
		Error(c, consts.StatusBadRequest, "MISSING_PARAM", "name 不能为空")
		return
	}

	ctx, cancel := d.withTimeout(c)
	defer cancel()

	resp, err := d.RAG.CreateKnowledgeBase(ctx, &ragv1.CreateKnowledgeBaseReq{
		Name:         req.Name,
		Description:  req.Description,
		EmbedModel:   req.EmbedModel,
		ChunkSize:    req.ChunkSize,
		ChunkOverlap: req.ChunkOverlap,
		Splitter:     req.Splitter,
		Identity:     idOf(c),
	})
	if err != nil {
		d.handleErr(ctx, c, "rag.CreateKnowledgeBase", err)
		return
	}
	OK(c, utils.H{"kb_id": resp.GetKbId(), "message": resp.GetMessage()})
}

// GetKB 查询知识库
func (d *Deps) GetKB(c *app.RequestContext) {
	kbID := paramInt64(c, "kb_id")
	if kbID == 0 {
		Error(c, consts.StatusBadRequest, "MISSING_PARAM", "kb_id 不能为空")
		return
	}

	ctx, cancel := d.withTimeout(c)
	defer cancel()

	resp, err := d.RAG.GetKnowledgeBase(ctx, &ragv1.GetKnowledgeBaseReq{
		KbId:     kbID,
		Identity: idOf(c),
	})
	if err != nil {
		d.handleErr(ctx, c, "rag.GetKnowledgeBase", err)
		return
	}
	OK(c, resp.GetKb())
}

// ListKB 分页查询知识库
func (d *Deps) ListKB(c *app.RequestContext) {
	ctx, cancel := d.withTimeout(c)
	defer cancel()

	owner, tenant := ownerFilter(c)
	resp, err := d.RAG.ListKnowledgeBases(ctx, &ragv1.ListKnowledgeBasesReq{
		Page:     page(c),
		Keyword:  queryStr(c, "keyword"),
		OwnerUid: owner,
		Tenant:   tenant,
	})
	if err != nil {
		d.handleErr(ctx, c, "rag.ListKnowledgeBases", err)
		return
	}
	OK(c, utils.H{"kbs": resp.GetKbs(), "meta": resp.GetMeta()})
}

// DeleteKB 删除知识库
func (d *Deps) DeleteKB(c *app.RequestContext) {
	kbID := paramInt64(c, "kb_id")
	if kbID == 0 {
		Error(c, consts.StatusBadRequest, "MISSING_PARAM", "kb_id 不能为空")
		return
	}

	ctx, cancel := d.withTimeout(c)
	defer cancel()

	if _, err := d.RAG.DeleteKnowledgeBase(ctx, &ragv1.DeleteKnowledgeBaseReq{
		KbId:     kbID,
		Identity: idOf(c),
	}); err != nil {
		d.handleErr(ctx, c, "rag.DeleteKnowledgeBase", err)
		return
	}
	OK(c, utils.H{"message": "删除成功"})
}

// ---------- 文档 ----------

// uploadDocReq 上传文档请求。
//
// 小文件走 content 直传，大文件走 file_url（对象存储）。
// 二者由服务端按契约自行选择，网关不限制。
type uploadDocReq struct {
	FileName string `json:"file_name"`
	FileType string `json:"file_type"`
	FileURL  string `json:"file_url"`
	FileHash string `json:"file_hash"`
	FileSize int64  `json:"file_size"`
	// Content 文本直传。仅小文件适用：
	// 二进制走 multipart，JSON 里塞base64 会额外膨胀 33%。
	Content string `json:"content"`
}

// fileTypeOf 从文件名推断文档类型。
//
// 只按扩展名判断，不做魔数嗅探：格式识别是 rag_service 的职责，
// 网关重复实现一套只会产生两处不一致的类型名。
func fileTypeOf(name string) string {
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(name), ".")) {
	case "pdf":
		return "pdf"
	case "doc", "docx":
		return "docx"
	case "xls", "xlsx":
		return "xlsx"
	case "html", "htm":
		return "html"
	case "md", "markdown":
		return "md"
	case "txt", "text":
		return "txt"
	default:
		return ""
	}
}

// UploadDoc 上传文档到知识库。
//
// ★ kb_id 从路径参数取（路由已声明 /knowledge-bases/:kb_id/documents），
//
//	不是从 query。读错来源会让 RESTful 路径参数形同虚设，
//	按接口文档拼 URL 的调用方必然踩坑。
func (d *Deps) UploadDoc(c *app.RequestContext) {
	kbID := paramInt64(c, "kb_id")
	if kbID == 0 {
		Error(c, consts.StatusBadRequest, "MISSING_PARAM", "kb_id 不能为空")
		return
	}

	in, err := readUpload(c, d.MaxBodyBytes)
	if err != nil {
		Error(c, consts.StatusBadRequest, "INVALID_PARAM", err.Error())
		return
	}
	if len(in.Content) == 0 && in.FileURL == "" {
		Error(c, consts.StatusBadRequest, "MISSING_PARAM", "需提供 file 字段或 file_url")
		return
	}

	ctx, cancel := d.withTimeout(c)
	defer cancel()

	resp, err := d.RAG.UploadDocument(ctx, &ragv1.UploadDocumentReq{
		KbId:     kbID,
		FileName: in.FileName,
		FileType: in.FileType,
		Content:  in.Content,
		FileUrl:  in.FileURL,
		// 去重哈希：客户端已算好则尊重其结果，
		// 否则由 rag_service 侧按内容计算（见下方哈希为空时）
		FileHash: in.FileHash,
		FileSize: int64(len(in.Content)),
		Identity: idOf(c),
	})
	if err != nil {
		d.handleErr(ctx, c, "rag.UploadDocument", err)
		return
	}
	doc := resp.GetDocument()
	OK(c, utils.H{
		"doc_id":     resp.GetDocId(),
		"file_name":  doc.GetFileName(),
		"chunks":     doc.GetChunkCount(),
		"vectorized": doc.GetParsedChunks(),
		"duplicated": resp.GetDuplicated(),
		"status":     doc.GetStatus(),
		"message":    resp.GetMessage(),
	})
}

// uploadInput 上传请求的解析结果
type uploadInput struct {
	FileName string
	FileType string
	FileURL  string
	FileHash string
	Content  []byte
}

// readUpload 解析上传请求，同时兼容 multipart 与 JSON 两种提交方式。
//
// multipart：file=@xxx（浏览器 / curl -F）
// JSON：      {"file_name":..., "content": "..."} 或 {"file_url": "https://..."}
func readUpload(c *app.RequestContext, maxBody int64) (uploadInput, error) {
	var in uploadInput

	// multipart 分支：Content-Type 前缀匹配，不能用 ==，
	// 后面还跟着 boundary
	if strings.HasPrefix(string(c.Request.Header.ContentType()), "multipart/form-data") {
		fh, err := c.FormFile("file")
		if err != nil {
			return in, errors.New("缺少 file 字段")
		}
		// fh.Size 不可信（可伪造），所以既用它做快速拒绝，
		// 也用 LimitReader 做实际兜底 —— 两道都不能省。
		if maxBody > 0 && fh.Size > maxBody {
			return in, fmt.Errorf("文件超过上限 %d 字节", maxBody)
		}
		f, err := fh.Open()
		if err != nil {
			return in, errors.New("读取上传文件失败")
		}
		defer f.Close()

		limit := maxBody
		if limit <= 0 {
			limit = defaultMaxBodyBytes
		}
		buf, err := io.ReadAll(io.LimitReader(f, limit+1))
		if err != nil {
			return in, errors.New("读取上传文件失败")
		}
		if int64(len(buf)) > limit {
			return in, fmt.Errorf("文件超过上限 %d 字节", limit)
		}

		in.FileName = fh.Filename
		in.FileType = fileTypeOf(fh.Filename)
		in.Content = buf
		return in, nil
	}

	// JSON 分支
	var req uploadDocReq
	if err := c.BindAndValidate(&req); err != nil {
		return in, errors.New("请求格式不正确")
	}
	in.FileName = firstNonEmptyStr(req.FileName, queryStr(c, "file_name"))
	in.FileType = firstNonEmptyStr(req.FileType, queryStr(c, "file_type"))
	if in.FileType == "" {
		in.FileType = fileTypeOf(in.FileName)
	}
	in.FileURL = req.FileURL
	in.FileHash = req.FileHash
	switch {
	case req.FileURL != "":
		// ★ file_url 模式：请求体只是元数据（file_name/file_type/file_url），
		//   绝不能把整个 JSON body 当成文件内容 —— 否则 SSRF 拉取分支
		//   永远走不到，反而把 {"file_url":...} 当纯文本入库。
		in.Content = nil
	case req.Content != "":
		in.Content = []byte(req.Content)
	default:
		in.Content = c.Request.Body()
	}
	if maxBody > 0 && int64(len(in.Content)) > maxBody {
		return in, fmt.Errorf("请求体超过上限 %d 字节", maxBody)
	}
	return in, nil
}

// ListDocs 分页查询文档
func (d *Deps) ListDocs(c *app.RequestContext) {
	kbID := paramInt64(c, "kb_id")
	if kbID == 0 {
		Error(c, consts.StatusBadRequest, "MISSING_PARAM", "kb_id 不能为空")
		return
	}

	ctx, cancel := d.withTimeout(c)
	defer cancel()

	resp, err := d.RAG.ListDocuments(ctx, &ragv1.ListDocumentsReq{
		KbId:     kbID,
		Page:     page(c),
		Status:   int32(queryInt(c, "status", 0)),
		Keyword:  queryStr(c, "keyword"),
		Identity: idOf(c),
	})
	if err != nil {
		d.handleErr(ctx, c, "rag.ListDocuments", err)
		return
	}
	OK(c, utils.H{"docs": resp.GetDocuments(), "meta": resp.GetMeta()})
}

// DeleteDoc 删除文档
func (d *Deps) DeleteDoc(c *app.RequestContext) {
	docID := paramInt64(c, "doc_id")
	if docID == 0 {
		Error(c, consts.StatusBadRequest, "MISSING_PARAM", "doc_id 不能为空")
		return
	}

	ctx, cancel := d.withTimeout(c)
	defer cancel()

	if _, err := d.RAG.DeleteDocument(ctx, &ragv1.DeleteDocumentReq{
		DocId:    docID,
		Identity: idOf(c),
	}); err != nil {
		d.handleErr(ctx, c, "rag.DeleteDocument", err)
		return
	}
	OK(c, utils.H{"message": "删除成功"})
}

// ListChunks 分页查询切片
func (d *Deps) ListChunks(c *app.RequestContext) {
	docID := paramInt64(c, "doc_id")
	if docID == 0 {
		Error(c, consts.StatusBadRequest, "MISSING_PARAM", "doc_id 不能为空")
		return
	}

	ctx, cancel := d.withTimeout(c)
	defer cancel()

	resp, err := d.RAG.ListChunks(ctx, &ragv1.ListChunksReq{
		DocId:    docID,
		Page:     page(c),
		Identity: idOf(c),
	})
	if err != nil {
		d.handleErr(ctx, c, "rag.ListChunks", err)
		return
	}
	OK(c, utils.H{"chunks": resp.GetChunks(), "meta": resp.GetMeta()})
}

// ---------- 检索 ----------

// retrieveReq 检索请求
type retrieveReq struct {
	Query          string  `json:"query"`
	KbIds          []int64 `json:"kb_ids"`
	TopK           int32   `json:"top_k"`
	ScoreThreshold float64 `json:"score_threshold"`
}

// Retrieve 向量检索
func (d *Deps) Retrieve(c *app.RequestContext) {
	var req retrieveReq
	if err := c.BindAndValidate(&req); err != nil || req.Query == "" {
		Error(c, consts.StatusBadRequest, "MISSING_PARAM", "query 不能为空")
		return
	}

	// 检索比普通 RPC 更重（涉及 embedding），给更宽的超时
	ctx, cancel := d.withTimeout(c)
	defer cancel()

	resp, err := d.RAG.Retrieve(ctx, &ragv1.RetrieveReq{
		Query:          req.Query,
		KbIds:          req.KbIds,
		TopK:           req.TopK,
		ScoreThreshold: req.ScoreThreshold,
		Identity:       idOf(c),
		RequestId:      queryStr(c, "request_id"),
	})
	if err != nil {
		d.handleErr(ctx, c, "rag.Retrieve", err)
		return
	}
	// degraded 如实透出：前端应告知用户"本次未使用向量检索"
	OK(c, utils.H{
		"hits":           resp.GetHits(),
		"total":          resp.GetTotal(),
		"degraded":       resp.GetDegraded(),
		"degrade_reason": resp.GetDegradeReason(),
		"retrieve_ms":    resp.GetRetrieveMs(),
	})
}

// DebugRetrieve 带阶段耗时的检索（排查 RAG 性能用）
func (d *Deps) DebugRetrieve(c *app.RequestContext) {
	var req retrieveReq
	if err := c.BindAndValidate(&req); err != nil || req.Query == "" {
		Error(c, consts.StatusBadRequest, "MISSING_PARAM", "query 不能为空")
		return
	}

	ctx, cancel := d.withTimeout(c)
	defer cancel()

	resp, err := d.RAG.DebugRetrieve(ctx, &ragv1.DebugRetrieveReq{
		Query:    req.Query,
		KbIds:    req.KbIds,
		TopK:     req.TopK,
		Identity: idOf(c),
	})
	if err != nil {
		d.handleErr(ctx, c, "rag.DebugRetrieve", err)
		return
	}
	res := resp.GetResult()
	OK(c, utils.H{
		"hits":           res.GetHits(),
		"total":          res.GetTotal(),
		"degraded":       res.GetDegraded(),
		"degrade_reason": res.GetDegradeReason(),
		"embed_model":    resp.GetEmbedModel(),
		"dimension":      resp.GetDimension(),
		"milvus_filter":  resp.GetMilvusFilter(),
		"stages":         resp.GetStages(),
		"timing_ms": utils.H{
			"retrieve":  res.GetRetrieveMs(),
			"embedding": res.GetEmbeddingMs(),
			"search":    res.GetSearchMs(),
		},
	})
}

// ---------- 知识库更新 ----------

// updateKBReq 更新知识库请求（update_mask 指定要更新的字段）
type updateKBReq struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	ChunkSize    int32    `json:"chunk_size"`
	ChunkOverlap int32    `json:"chunk_overlap"`
	Splitter     string   `json:"splitter"`
	UpdateMask   []string `json:"update_mask"`
}

// UpdateKB 更新知识库。
//
// 只更新 update_mask 里点名的字段；mask 为空时全部字段生效。
// ★ 与 UpdateAgent 同构：kb_id 以路径参数为准，不接受 body 里的覆盖。
func (d *Deps) UpdateKB(c *app.RequestContext) {
	kbID := paramInt64(c, "kb_id")
	if kbID == 0 {
		Error(c, consts.StatusBadRequest, "MISSING_PARAM", "kb_id 不能为空")
		return
	}

	var req updateKBReq
	if err := c.BindAndValidate(&req); err != nil {
		Error(c, consts.StatusBadRequest, "INVALID_PARAM", "请求体非法")
		return
	}

	ctx, cancel := d.withTimeout(c)
	defer cancel()

	resp, err := d.RAG.UpdateKnowledgeBase(ctx, &ragv1.UpdateKnowledgeBaseReq{
		KbId:         kbID,
		Name:         req.Name,
		Description:  req.Description,
		ChunkSize:    req.ChunkSize,
		ChunkOverlap: req.ChunkOverlap,
		Splitter:     req.Splitter,
		UpdateMask:   req.UpdateMask,
		Identity:     idOf(c),
	})
	if err != nil {
		d.handleErr(ctx, c, "rag.UpdateKnowledgeBase", err)
		return
	}
	OK(c, resp.GetKb())
}

// ---------- 文档详情与重试 ----------

// GetDoc 文档详情
func (d *Deps) GetDoc(c *app.RequestContext) {
	docID := paramInt64(c, "doc_id")
	if docID == 0 {
		Error(c, consts.StatusBadRequest, "MISSING_PARAM", "doc_id 不能为空")
		return
	}

	ctx, cancel := d.withTimeout(c)
	defer cancel()

	resp, err := d.RAG.GetDocument(ctx, &ragv1.GetDocumentReq{
		DocId:    docID,
		Identity: idOf(c),
	})
	if err != nil {
		d.handleErr(ctx, c, "rag.GetDocument", err)
		return
	}
	OK(c, resp.GetDocument())
}

// retryDocReq 重试文档请求
type retryDocReq struct {
	ReEmbed bool `json:"re_embed"`
}

// RetryDoc 重试失败文档（解析失败或向量化失败）
func (d *Deps) RetryDoc(c *app.RequestContext) {
	docID := paramInt64(c, "doc_id")
	if docID == 0 {
		Error(c, consts.StatusBadRequest, "MISSING_PARAM", "doc_id 不能为空")
		return
	}

	var req retryDocReq
	_ = c.BindAndValidate(&req) // re_embed 可选，解析失败不阻塞

	ctx, cancel := d.withTimeout(c)
	defer cancel()

	if _, err := d.RAG.RetryDocument(ctx, &ragv1.RetryDocumentReq{
		DocId:    docID,
		ReEmbed:  req.ReEmbed,
		Identity: idOf(c),
	}); err != nil {
		d.handleErr(ctx, c, "rag.RetryDocument", err)
		return
	}
	OK(c, utils.H{"message": "已触发重试"})
}

// ---------- Agent ↔ 知识库绑定 ----------

// bindAgentKBReq 绑定请求：kb_ids 全量覆盖
type bindAgentKBReq struct {
	KbIds []int64 `json:"kb_ids"`
}

// BindAgentKB 全量覆盖设置 Agent 关联的知识库。
//
// 绑定后 agent 对话会自动携带这些 KB 做检索（见 agent_service 的 PlanFrom）。
func (d *Deps) BindAgentKB(c *app.RequestContext) {
	agentID := paramInt64(c, "agent_id")
	if agentID == 0 {
		Error(c, consts.StatusBadRequest, "MISSING_PARAM", "agent_id 不能为空")
		return
	}

	var req bindAgentKBReq
	if err := c.BindAndValidate(&req); err != nil {
		Error(c, consts.StatusBadRequest, "INVALID_PARAM", "请求体非法")
		return
	}

	ctx, cancel := d.withTimeout(c)
	defer cancel()

	if _, err := d.RAG.BindAgentKB(ctx, &ragv1.BindAgentKBReq{
		AgentId:  agentID,
		KbIds:    req.KbIds,
		Identity: idOf(c),
	}); err != nil {
		d.handleErr(ctx, c, "rag.BindAgentKB", err)
		return
	}
	OK(c, utils.H{"agent_id": agentID, "kb_ids": req.KbIds, "message": "绑定成功"})
}

// ListAgentKB 列出 Agent 已绑定的知识库
func (d *Deps) ListAgentKB(c *app.RequestContext) {
	agentID := paramInt64(c, "agent_id")
	if agentID == 0 {
		Error(c, consts.StatusBadRequest, "MISSING_PARAM", "agent_id 不能为空")
		return
	}

	ctx, cancel := d.withTimeout(c)
	defer cancel()

	resp, err := d.RAG.ListAgentKB(ctx, &ragv1.ListAgentKBReq{
		AgentId:  agentID,
		Identity: idOf(c),
	})
	if err != nil {
		d.handleErr(ctx, c, "rag.ListAgentKB", err)
		return
	}
	OK(c, utils.H{"kbs": resp.GetKbs()})
}
