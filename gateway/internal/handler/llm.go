package handler

import (
	"strconv"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/utils"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	llmv1 "github.com/aegis/idl/kitex_gen/llmv1"
)

// ---------- 模型元信息 ----------

// ListModels 列出可用模型目录（供前端模型选择器使用）
func (d *Deps) ListModels(c *app.RequestContext) {
	ctx, cancel := d.withTimeout(c)
	defer cancel()

	resp, err := d.LLM.ListModels(ctx, &llmv1.ListModelsReq{
		OnlyEnabled: queryStr(c, "only_enabled") == "true",
	})
	if err != nil {
		d.handleErr(ctx, c, "llm.ListModels", err)
		return
	}
	OK(c, utils.H{
		"models":        resp.GetModels(),
		"default_model": resp.GetDefaultModel(),
	})
}

// GetCapabilities 当前 Provider 能力声明
func (d *Deps) GetCapabilities(c *app.RequestContext) {
	ctx, cancel := d.withTimeout(c)
	defer cancel()

	resp, err := d.LLM.GetCapabilities(ctx, &llmv1.GetCapabilitiesReq{})
	if err != nil {
		d.handleErr(ctx, c, "llm.GetCapabilities", err)
		return
	}
	OK(c, utils.H{
		"tool_call":  resp.GetToolCall(),
		"reasoning":  resp.GetReasoning(),
		"multimodal": resp.GetMultimodal(),
		"embeddings": resp.GetEmbeddings(),
		"provider":   resp.GetProvider(),
		"is_mock":    resp.GetIsMock(),
	})
}

// GetUsage 查询模型用量（self-service：默认查当前用户；
// admin 可通过 query uid= 查任意用户）
func (d *Deps) GetUsage(c *app.RequestContext) {
	// uid 默认当前身份；admin 可显式指定
	uid := idOf(c).GetUid()
	if s := queryStr(c, "uid"); s != "" {
		if v, err := strconv.ParseInt(s, 10, 64); err == nil && v > 0 {
			uid = v
		}
	}

	ctx, cancel := d.withTimeout(c)
	defer cancel()

	resp, err := d.LLM.GetUsage(ctx, &llmv1.GetUsageReq{
		Uid:    uid,
		Tenant: idOf(c).GetTenant(),
		Model:  queryStr(c, "model"),
		Page:   page(c),
	})
	if err != nil {
		d.handleErr(ctx, c, "llm.GetUsage", err)
		return
	}
	OK(c, utils.H{
		"records":      resp.GetRecords(),
		"meta":         resp.GetMeta(),
		"total_tokens": resp.GetTotalTokens(),
		"total_cost":   resp.GetTotalCost(),
	})
}

// ---------- 重排 ----------

// rerankReq 重排请求
type rerankReq struct {
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	Model     string   `json:"model"`
	TopN      int32    `json:"top_n"`
}

// Rerank 对候选文档重排（供前端调试检索质量、高级检索场景使用）
func (d *Deps) Rerank(c *app.RequestContext) {
	var req rerankReq
	if err := c.BindAndValidate(&req); err != nil || req.Query == "" || len(req.Documents) == 0 {
		Error(c, consts.StatusBadRequest, "MISSING_PARAM", "query 与 documents 不能为空")
		return
	}

	ctx, cancel := d.withTimeout(c)
	defer cancel()

	resp, err := d.LLM.Rerank(ctx, &llmv1.RerankReq{
		Query:           req.Query,
		Documents:       req.Documents,
		Model:           req.Model,
		TopN:            req.TopN,
		Identity:        idOf(c),
		ReturnDocuments: true,
	})
	if err != nil {
		d.handleErr(ctx, c, "llm.Rerank", err)
		return
	}
	OK(c, utils.H{
		"results": resp.GetResults(),
		"model":   resp.GetModel(),
		"usage":   resp.GetUsage(),
	})
}
