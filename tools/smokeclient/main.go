// Command smokeclient 是冒烟测试用的临时 RPC 客户端。
//
// 用途：在 gateway 尚未就绪时，直接以 Kitex client 验证各服务业务链路。
// 正式冒烟脚本以 gateway 的 HTTP 接口为准，本程序仅用于开发期自检。
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	authv1 "github.com/aegis/idl/kitex_gen/authv1"
	authservice "github.com/aegis/idl/kitex_gen/authv1/authservice"
	commonv1 "github.com/aegis/idl/kitex_gen/commonv1"
	llmv1 "github.com/aegis/idl/kitex_gen/llmv1"
	llmservice "github.com/aegis/idl/kitex_gen/llmv1/llmservice"
	ragv1 "github.com/aegis/idl/kitex_gen/ragv1"
	ragservice "github.com/aegis/idl/kitex_gen/ragv1/ragservice"
	userv1 "github.com/aegis/idl/kitex_gen/userv1"
	userservice "github.com/aegis/idl/kitex_gen/userv1/userservice"
	"github.com/cloudwego/kitex/client"
	"github.com/cloudwego/kitex/pkg/endpoint"
	npmeta "github.com/cloudwego/kitex/pkg/remote/trans/nphttp2/metadata"
	"github.com/cloudwego/kitex/pkg/transmeta"
	"github.com/cloudwego/kitex/transport"

	"github.com/bytedance/gopkg/cloud/metainfo"
)

// internalTokenHeader 与 pkg/internalauth.HeaderToken 保持一致
// （smokeclient 是独立 module，不引 pkg，故此处内联常量）
const internalTokenHeader = "x-aegis-internal-token"

// iaOpts 内部认证 client 选项。
//
// 从 AEGIS_INTERNAL_TOKEN 读取令牌；为空则不注入 —— 与「关闭认证」
// 的服务端配合（本地 mock）。服务端开启校验后，直连各服务的
// 自检客户端必须携带同一令牌，否则所有调用被拒。
func iaOpts() []client.Option {
	tok := os.Getenv("AEGIS_INTERNAL_TOKEN")
	if tok == "" {
		return nil
	}
	// 与 pkg/internalauth.ClientOptions 保持一致（双通道）：
	//  - metainfo persistent 桶：TTHeader/thrift 服务（auth/user/rag）
	//  - gRPC HTTP2 metadata（rpc-persist- 前缀）：pb streaming 服务
	//    （agentv1 强制 gRPC 传输，metainfo 桶发不出去，见 internalauth 注释）
	// metainfo 需要 MetainfoClientHandler + TTHeader 传输才会真正上线
	//（framed 默认传输不携带 TransInfo）
	return []client.Option{
		client.WithMiddleware(func(next endpoint.Endpoint) endpoint.Endpoint {
			return func(ctx context.Context, req, resp interface{}) error {
				ctx = metainfo.WithPersistentValues(ctx, internalTokenHeader, tok)
				ctx = npmeta.AppendToOutgoingContext(ctx, metainfo.HTTPPrefixPersistent+internalTokenHeader, tok)
				return next(ctx, req, resp)
			}
		}),
		client.WithMetaHandler(transmeta.MetainfoClientHandler),
		client.WithTransportProtocol(transport.TTHeader),
	}
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	fail := func(step string, err error) {
		fmt.Fprintf(os.Stderr, "FAIL %s: %v\n", step, err)
		os.Exit(1)
	}

	// ---- auth_service ----
	auth, err := authservice.NewClient("auth_service",
		append(iaOpts(),
			client.WithHostPorts("127.0.0.1:8881"),
			client.WithRPCTimeout(10*time.Second))...)
	if err != nil {
		fail("connect auth", err)
	}

	h, err := auth.Health(ctx, &commonv1.HealthReq{})
	if err != nil {
		fail("auth.Health", err)
	}
	fmt.Printf("[1] auth.Health        healthy=%v mock=%v\n", h.Healthy, h.MockMode)

	// ---- 本地登录 ----
	login, err := auth.Login(ctx, &authv1.LoginReq{
		Username: "admin",
		Password: "Aegis@2026",
	})
	if err != nil {
		fail("auth.Login", err)
	}
	fmt.Printf("[2] auth.Login         uid=%d username=%s roles=%v token_len=%d\n",
		login.User.GetUid(), login.User.GetUsername(), login.User.GetRoles(),
		len(login.AccessToken))

	// ---- 错误密码应被拒 ----
	if _, err := auth.Login(ctx, &authv1.LoginReq{
		Username: "admin",
		Password: "wrong-password",
	}); err == nil {
		fail("auth.Login(wrong password) 应当失败但成功了", fmt.Errorf("no error"))
	} else {
		fmt.Printf("[3] auth.Login 错误密码  正确拒绝: %v\n", err)
	}

	// ---- 令牌校验 ----
	vt, err := auth.VerifyToken(ctx, &authv1.VerifyTokenReq{
		AccessToken: login.AccessToken,
	})
	if err != nil {
		fail("auth.VerifyToken", err)
	}
	fmt.Printf("[4] auth.VerifyToken   valid=%v uid=%d scopes=%v\n",
		vt.Valid, vt.Uid, vt.Scopes)

	// ---- SSO：授权跳转 ----
	idps, err := auth.ListIdPs(ctx, &authv1.ListIdPsReq{})
	if err != nil {
		fail("auth.ListIdPs", err)
	}
	if len(idps.Idps) == 0 {
		fail("auth.ListIdPs", fmt.Errorf("没有可用 IdP"))
	}
	fmt.Printf("[5] auth.ListIdPs      %d 个，名称=%s\n", len(idps.Idps), idps.Idps[0].Name)

	// ---- 刷新令牌 ----
	refreshed, err := auth.RefreshToken(ctx, &authv1.RefreshTokenReq{
		RefreshToken: login.RefreshToken,
	})
	if err != nil {
		fail("auth.RefreshToken", err)
	}
	fmt.Printf("[6] auth.RefreshToken  新 access 长度=%d\n", len(refreshed.AccessToken))

	// ---- 权限校验 ----
	cp, err := auth.CheckPermission(ctx, &authv1.CheckPermissionReq{
		Uid:      login.User.GetUid(),
		Resource: "kb:1001",
		Action:   "write",
	})
	if err != nil {
		fail("auth.CheckPermission", err)
	}
	fmt.Printf("[7] auth.CheckPermission allowed=%v reason=%s\n", cp.Allowed, cp.Reason)

	// ---- user_service ----
	users, err := userservice.NewClient("user_service",
		append(iaOpts(),
			client.WithHostPorts("127.0.0.1:8882"),
			client.WithRPCTimeout(10*time.Second))...)
	if err != nil {
		fail("connect user", err)
	}
	list, err := users.ListUsers(ctx, &userv1.ListUsersReq{})
	if err != nil {
		fail("user.ListUsers", err)
	}
	fmt.Printf("[8] user.ListUsers     total=%d\n", list.Meta.GetTotal())

	pref, err := users.SetPreference(ctx, &userv1.SetPreferenceReq{
		Uid:   login.User.GetUid(),
		Items: map[string]string{"default_model": "deepseek-chat"},
	})
	if err != nil {
		fail("user.SetPreference", err)
	}
	fmt.Printf("[9] user.SetPreference success=%v\n", pref.Success)

	// ---- llm_service ----
	if err := runLLM(ctx); err != nil {
		fail("llm_service 链路", err)
	}

	// ---- rag_service ----
	if err := runRAG(ctx); err != nil {
		fail("rag_service 链路", err)
	}

	fmt.Println("\nALL PASS")
}

// runLLM 验证 llm_service 链路
func runLLM(ctx context.Context) error {
	c, err := llmservice.NewClient("llm_service",
		append(iaOpts(),
			client.WithHostPorts("127.0.0.1:8884"),
			client.WithRPCTimeout(30*time.Second))...)
	if err != nil {
		return err
	}

	caps, err := c.GetCapabilities(ctx, &llmv1.GetCapabilitiesReq{})
	if err != nil {
		return err
	}
	fmt.Printf("[10] llm.Capabilities   provider=%s is_mock=%v tool_call=%v\n",
		caps.Provider, caps.IsMock, caps.ToolCall)

	models, err := c.ListModels(ctx, &llmv1.ListModelsReq{OnlyEnabled: true})
	if err != nil {
		return err
	}
	fmt.Printf("[11] llm.ListModels     %d 个，默认=%s\n", len(models.Models), models.DefaultModel)

	// Embed：同一文本连续两次，第二次应命中缓存
	texts := []string{"Aegis 平台的向量维度是 1024", "Milvus 3.0.2 启用了授权"}
	e1, err := c.Embed(ctx, &llmv1.EmbedReq{Texts: texts, InputType: "query"})
	if err != nil {
		return err
	}
	e2, err := c.Embed(ctx, &llmv1.EmbedReq{Texts: texts, InputType: "query"})
	if err != nil {
		return err
	}
	fmt.Printf("[12] llm.Embed          dim=%d vectors=%d tokens=%d\n",
		e1.Dimension, len(e1.Embeddings), e1.Usage.TotalTokens)
	fmt.Printf("[13] llm.Embed 缓存命中 cached=%v（第二次全部命中）\n", e2.Cached)
	if e1.Dimension != 1024 {
		return fmt.Errorf("维度错误：期望 1024，实际 %d", e1.Dimension)
	}

	// Chat 同步
	ch, err := c.Chat(ctx, &llmv1.ChatReq{
		Model:    "deepseek-chat",
		Messages: []*llmv1.Message{{Role: "user", Content: "介绍一下 Aegis 平台"}},
	})
	if err != nil {
		return err
	}
	fmt.Printf("[14] llm.Chat           model=%s finish=%s tokens=%d\n",
		ch.Model, ch.FinishReason, ch.Usage.TotalTokens)
	fmt.Printf("     内容: %s\n", truncate(ch.Content, 70))

	// ChatStream：验证真的分片且末帧带 usage
	st, err := c.ChatStream(ctx, &llmv1.ChatReq{
		Messages: []*llmv1.Message{{Role: "user", Content: "流式测试"}},
	})
	if err != nil {
		return err
	}
	var sb strings.Builder
	chunks, doneSeen, usageSeen := 0, false, false
	for {
		ck, err := st.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		chunks++
		sb.WriteString(ck.Delta)
		if ck.Done {
			doneSeen = true
			if ck.Usage != nil && ck.Usage.TotalTokens > 0 {
				usageSeen = true
			}
		}
	}
	fmt.Printf("[15] llm.ChatStream     帧数=%d done=%v usage=%v 内容长度=%d\n",
		chunks, doneSeen, usageSeen, sb.Len())
	if chunks < 3 {
		return fmt.Errorf("流式帧数过少（%d），可能未真分片", chunks)
	}
	if !doneSeen {
		return fmt.Errorf("未收到 done 末帧")
	}

	// 计量
	usage, err := c.GetUsage(ctx, &llmv1.GetUsageReq{})
	if err != nil {
		return err
	}
	fmt.Printf("[16] llm.GetUsage       记录=%d 总token=%d\n",
		len(usage.Records), usage.TotalTokens)

	return nil
}

// runRAG 验证 rag_service 链路（入库 → 检索 → 降级）
func runRAG(ctx context.Context) error {
	c, err := ragservice.NewClient("rag_service",
		append(iaOpts(),
			client.WithHostPorts("127.0.0.1:8885"),
			client.WithRPCTimeout(60*time.Second))...)
	if err != nil {
		return err
	}

	identity := &commonv1.Identity{Uid: 1001, Tenant: "default", Username: "admin", Scopes: []string{"admin", "user"}}

	kb, err := c.CreateKnowledgeBase(ctx, &ragv1.CreateKnowledgeBaseReq{
		Name:         "冒烟知识库",
		Description:  "用于验证入库与检索",
		EmbedModel:   "text-embedding-v3",
		ChunkSize:    200,
		ChunkOverlap: 40,
		Identity:     identity,
	})
	if err != nil {
		return err
	}
	fmt.Printf("[17] rag.CreateKB       kb_id=%d dim=%d chunk_size=%d\n",
		kb.KbId, kb.Kb.EmbeddingDim, kb.Kb.ChunkSize)

	// 上传文档（内容直接覆盖 rag 的四个主题）
	doc, err := c.UploadDocument(ctx, &ragv1.UploadDocumentReq{
		KbId:     kb.KbId,
		FileName: "平台架构.md",
		FileType: "md",
		Content: []byte(
			"Aegis 平台使用 CloudWeGo 生态构建，包含 gateway、auth_service、user_service、" +
				"agent_service、llm_service、rag_service 六个微服务。" +
				"认证采用 OAuth2.0 授权码模式加 PKCE，并自签 RS256 JWT。" +
				"向量数据库选择 Milvus 3.0.2，通过自研 client/v3 适配层接入。" +
				"PostgreSQL 18 存储业务元数据与文档正文，是内容真相源。" +
				"Embedding 模型使用 bge-m3，向量维度为 1024，度量方式为 COSINE。" +
				"检索支持三级降级：Milvus 向量检索、PostgreSQL 关键词检索、纯 LLM 对话。"),
		Identity: identity,
	})
	if err != nil {
		return err
	}
	fmt.Printf("[18] rag.UploadDoc      doc_id=%d 切片=%d 已向量化=%d\n",
		doc.DocId, doc.Document.ChunkCount, doc.Document.ParsedChunks)

	// 不同内容 → 正常入库
	doc2, err := c.UploadDocument(ctx, &ragv1.UploadDocumentReq{
		KbId:     kb.KbId,
		FileName: "部署说明.md",
		FileType: "md",
		Content: []byte(
			"测试环境使用 Docker Compose，正式环境使用 Kubernetes。" +
				"Nacos 提供注册中心与配置中心能力。" +
				"PostgreSQL 18 与 Milvus 3.0.2 均以容器方式部署在测试环境。"),
		Identity: identity,
	})
	if err != nil {
		return err
	}
	fmt.Printf("[19] rag.UploadDoc #2   doc_id=%d 切片=%d\n", doc2.DocId, doc2.Document.ChunkCount)

	// 检索
	ret, err := c.Retrieve(ctx, &ragv1.RetrieveReq{
		Query:    "Milvus 的向量维度是多少",
		KbIds:    []int64{kb.KbId},
		TopK:     3,
		Identity: identity,
	})
	if err != nil {
		return err
	}
	fmt.Printf("[20] rag.Retrieve       命中=%d 降级=%v 耗时=%dms(embed=%d search=%d)\n",
		ret.Total, ret.Degraded, ret.RetrieveMs, ret.EmbeddingMs, ret.SearchMs)
	for i, h := range ret.Hits {
		fmt.Printf("     [%d] score=%.4f doc=%s 页=%d: %s\n",
			i+1, h.Score, h.DocTitle, h.PageNo, truncate(h.Content, 46))
	}
	if ret.Total == 0 {
		return fmt.Errorf("检索无命中：入库或向量化链路可能断裂")
	}

	// 调试检索：返回各阶段耗时
	dbg, err := c.DebugRetrieve(ctx, &ragv1.DebugRetrieveReq{
		Query:    "OAuth2.0 认证方式",
		KbIds:    []int64{kb.KbId},
		TopK:     2,
		Identity: identity,
	})
	if err != nil {
		return err
	}
	fmt.Printf("[21] rag.DebugRetrieve  model=%s dim=%d 阶段数=%d\n",
		dbg.EmbedModel, dbg.Dimension, len(dbg.Stages))
	for _, st := range dbg.Stages {
		fmt.Printf("     stage=%s ms=%d detail=%s\n", st.Stage, st.Ms, truncate(st.Detail, 46))
	}

	// 切片列表：验证 vec_status 已全部就绪
	chunks, err := c.ListChunks(ctx, &ragv1.ListChunksReq{DocId: doc.DocId, Page: &commonv1.PageReq{Page: 1, PageSize: 50}, Identity: identity})
	if err != nil {
		return err
	}
	ready := 0
	for _, ch := range chunks.Chunks {
		if ch.VecStatus == 1 {
			ready++
		}
	}
	fmt.Printf("[22] rag.ListChunks     总数=%d 已向量化=%d\n", chunks.Meta.Total, ready)
	if int32(ready) != chunks.Meta.Total {
		return fmt.Errorf("存在未完成向量化的切片：%d/%d", ready, chunks.Meta.Total)
	}

	// Agent 挂载
	if _, err := c.BindAgentKB(ctx, &ragv1.BindAgentKBReq{
		AgentId: 1, KbIds: []int64{kb.KbId}, Identity: identity,
	}); err != nil {
		return err
	}
	list, err := c.ListAgentKB(ctx, &ragv1.ListAgentKBReq{AgentId: 1, Identity: identity})
	if err != nil {
		return err
	}
	fmt.Printf("[23] rag.AgentKB 挂载   %d 个知识库\n", len(list.Kbs))

	// 去重验证
	dupResp, err := c.UploadDocument(ctx, &ragv1.UploadDocumentReq{
		KbId:     kb.KbId,
		FileName: "重复内容.md",
		FileType: "md",
		Content:  []byte("测试环境使用 Docker Compose，正式环境使用 Kubernetes。Nacos 提供注册中心与配置中心能力。PostgreSQL 18 与 Milvus 3.0.2 均以容器方式部署在测试环境。"),
		Identity: identity,
	})
	if err != nil {
		return err
	}
	fmt.Printf("[24] rag 文件去重       duplicated=%v doc_id=%d（原 %d）\n",
		dupResp.Duplicated, dupResp.DocId, doc2.DocId)

	return nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
