// Package router 组装 HTTP 路由。
//
// 路由分三组，对应三类调用方：
//   - /api/v1/auth/*   登录/登出/SSO，无需令牌
//   - /api/v1/*        业务接口，必须携带有效令牌
//   - /api/v1/agents/*/chat/stream  流式（SSE），令牌可走 query
package router

import (
	"context"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/aegis/gateway/internal/auth"
	"github.com/aegis/gateway/internal/handler"
	"github.com/aegis/gateway/internal/middleware"
	"github.com/aegis/pkg/metrics"
)

// Options 路由装配参数
type Options struct {
	Deps *handler.Deps
	// StaticDir 前端构建产物目录；为空则不挂载静态资源
	StaticDir string
	// AllowOrigins 跨域白名单
	AllowOrigins []string
	// MaxAge 预检缓存时长
	MaxAge time.Duration
	// MetricsEnabled 是否暴露 /metrics（AEGIS_METRICS__ENABLED）
	MetricsEnabled bool
	// MetricsPath 指标路径，默认 /metrics
	MetricsPath string
}

// Register 装配全部路由
func Register(h *server.Hertz, opt Options) {
	d := opt.Deps

	// ---- 全局中间件 ----
	// 顺序很重要：TraceID 最先（后续日志都带它），
	// Recovery 次之（尽早兜住 panic），
	// CORS 在鉴权之前（预检请求不应被 401 拦掉）。
	// Metrics/Trace 在 Recovery 之后：Recovery 外层兜住 panic，
	// 指标与 span 才能在正常与异常路径上都正确收尾。
	h.Use(
		middleware.TraceID(),
		middleware.Recovery(),
		middleware.CORS(opt.AllowOrigins, opt.MaxAge),
		middleware.RequestLog(),
		middleware.Metrics(),
		middleware.Trace(),
	)

	// 令牌校验器：把 auth_service 的 VerifyToken 包成中间件需要的窄接口
	verifier := auth.NewVerifier(d.Auth)

	// ---- 健康检查（无需鉴权）----
	h.GET("/healthz", func(ctx context.Context, c *app.RequestContext) {
		c.JSON(consts.StatusOK, map[string]any{
			"healthy": true,
			"service": "gateway",
		})
	})
	h.GET("/livez", func(ctx context.Context, c *app.RequestContext) {
		c.String(consts.StatusOK, "ok")
	})
	h.GET("/readyz", func(ctx context.Context, c *app.RequestContext) {
		c.String(consts.StatusOK, "ready")
	})

	// ---- 聚合健康检查：并发探活所有下游 ----
	h.GET("/healthz/full", func(ctx context.Context, c *app.RequestContext) {
		auth.CheckAll(ctx, c, d)
	})

	// ---- Prometheus 指标（HTTPOnly 模式的健康端口即 gateway 主端口）----
	if opt.MetricsEnabled {
		path := opt.MetricsPath
		if path == "" {
			path = "/metrics"
		}
		h.GET(path, func(ctx context.Context, c *app.RequestContext) {
			c.Header("Content-Type", metrics.ContentTypeText)
			if err := metrics.WriteText(c.Response.BodyWriter()); err != nil {
				c.JSON(consts.StatusInternalServerError, map[string]any{
					"error": map[string]any{"code": "INTERNAL", "message": "指标渲染失败"},
				})
			}
		})
	}

	// ---- 认证（免鉴权）----
	authGroup := h.Group("/api/v1/auth")
	{
		authGroup.POST("/login", handler.H(d.Login))
		authGroup.POST("/refresh", handler.H(d.Refresh))
		authGroup.POST("/logout", middleware.Auth(verifier, ""), handler.H(d.Logout))
		authGroup.GET("/idps", handler.H(d.ListIdPs))
		// SSO：Authorize 返回 JSON，AuthorizeRedirect 直接 302
		authGroup.POST("/oauth/authorize", handler.H(d.OAuthAuthorize))
		authGroup.GET("/oauth/authorize", handler.H(d.OAuthAuthorizeRedirect))
		authGroup.GET("/oauth/callback", handler.H(d.OAuthCallback))
		authGroup.GET("/verify", handler.H(d.Verify))
		authGroup.GET("/me", middleware.Auth(verifier, ""), handler.H(d.Me))
	}

	// ---- 业务接口（需鉴权）----
	api := h.Group("/api/v1", middleware.Auth(verifier, ""))
	{
		// ---- Agent ----
		agents := api.Group("/agents")
		{
			agents.POST("", handler.H(d.CreateAgent))
			agents.GET("", handler.H(d.ListAgents))
			agents.GET("/:agent_id", handler.H(d.GetAgent))
			agents.PUT("/:agent_id", handler.H(d.UpdateAgent))
			agents.DELETE("/:agent_id", handler.H(d.DeleteAgent))
			agents.POST("/:agent_id/chat", handler.H(d.Chat))
			agents.POST("/:agent_id/chat/stream", handler.H(d.ChatStream))
			// Agent ↔ 知识库绑定（RAG 检索配置）
			agents.POST("/:agent_id/knowledge-bases/bind", handler.H(d.BindAgentKB))
			agents.GET("/:agent_id/knowledge-bases", handler.H(d.ListAgentKB))
			// Agent↔Tool 绑定
			agents.GET("/:agent_id/tools", handler.H(d.ListAgentTools))
			agents.PUT("/:agent_id/tools", handler.H(d.BindAgentTools))
			agents.DELETE("/:agent_id/tools", handler.H(d.UnbindAgentTools))
		}

		// ---- 会话 ----
		sessions := api.Group("/sessions")
		{
			sessions.POST("", handler.H(d.CreateSession))
			sessions.GET("", handler.H(d.ListSessions))
			sessions.GET("/:session_id", handler.H(d.GetSession))
			sessions.DELETE("/:session_id", handler.H(d.DeleteSession))
			sessions.GET("/:session_id/messages", handler.H(d.ListMessages))
		}

		// ---- 用户管理（仅 admin scope）----
		// ★ 用户 CRUD 是管理员域：普通用户只能操作自己的偏好（/me/preferences）。
		users := api.Group("/users")
		{
			users.Use(middleware.RequireScope("admin"))
			users.POST("", handler.H(d.CreateUser))
			users.GET("", handler.H(d.ListUsers))
			users.GET("/:uid", handler.H(d.GetUser))
			users.PUT("/:uid", handler.H(d.UpdateUser))
			users.DELETE("/:uid", handler.H(d.DeleteUser))
		}
		// self-service 偏好：登录用户读写自己的偏好，
		// uid 由 handler 从身份上下文取，不接受请求体覆盖。
		api.PUT("/users/me/preferences", handler.H(d.SetPreference))
		api.GET("/users/me/preferences", handler.H(d.GetPreference))

		// ---- 知识库 ----
		kbs := api.Group("/knowledge-bases")
		{
			kbs.POST("", handler.H(d.CreateKB))
			kbs.GET("", handler.H(d.ListKB))
			kbs.GET("/:kb_id", handler.H(d.GetKB))
			kbs.PUT("/:kb_id", handler.H(d.UpdateKB))
			kbs.DELETE("/:kb_id", handler.H(d.DeleteKB))
			kbs.POST("/:kb_id/documents", handler.H(d.UploadDoc))
			kbs.GET("/:kb_id/documents", handler.H(d.ListDocs))
			kbs.GET("/documents/:doc_id", handler.H(d.GetDoc))
			kbs.POST("/documents/:doc_id/retry", handler.H(d.RetryDoc))
			kbs.DELETE("/documents/:doc_id", handler.H(d.DeleteDoc))
			kbs.GET("/documents/:doc_id/chunks", handler.H(d.ListChunks))
		}

		// ---- 检索 ----
		rag := api.Group("/rag")
		{
			rag.POST("/retrieve", handler.H(d.Retrieve))
			// debug 检索会暴露候选明细/分数等内部信息，仅限 admin scope
			//（scope 来自登录时角色展开，admin 角色 → "admin" scope）
			rag.POST("/debug/retrieve", middleware.RequireScope("admin"), handler.H(d.DebugRetrieve))
		}

		// ---- LLM 元信息与能力 ----
		llm := api.Group("/llm")
		{
			llm.GET("/models", handler.H(d.ListModels))
			llm.GET("/capabilities", handler.H(d.GetCapabilities))
			llm.GET("/usage", handler.H(d.GetUsage))
			llm.POST("/rerank", handler.H(d.Rerank))
		}

		// ---- 工具管理 ----
		// 写入（增/改/删）需 admin scope，只读（列表/详情）所有登录用户可用
		tools := api.Group("/tools")
		{
			tools.GET("", handler.H(d.ListTools))
			tools.GET("/:tool_id", handler.H(d.GetTool))
			tools.POST("", middleware.RequireScope("admin"), handler.H(d.CreateTool))
			tools.PUT("/:tool_id", middleware.RequireScope("admin"), handler.H(d.UpdateTool))
			tools.DELETE("/:tool_id", middleware.RequireScope("admin"), handler.H(d.DeleteTool))
		}
	}

	// ---- SSE 专用组（仅 GET）----
	//
	// ★ 为什么单独分组：
	//   EventSource 无法设置请求头，令牌只能走 query 参数。
	//   鉴权中间件已支持 query 读取，但要在这里显式标注，
	//   避免被"抄"到普通接口上。
	//
	// ★ 为什么只注册 GET：
	//   POST 版本已在上面的业务组注册过，
	//   Hertz 不允许同一 method+path 注册两次（启动时直接 panic）。
	//   GET 专门服务浏览器 EventSource，POST 服务 fetch 客户端 ——
	//   后者能设置请求头，走标准 Authorization 而不必把令牌暴露在 URL 里。
	sseGroup := h.Group("/api/v1/agents")
	sseGroup.Use(middleware.Auth(verifier, ""))
	{
		sseGroup.GET("/:agent_id/chat/stream", handler.H(d.ChatStream)) // EventSource 只能 GET
	}

	// ---- 前端静态资源 ----
	if opt.StaticDir != "" {
		// IndexNames 而非生成目录页：没有 index.html 的目录直接 404，
		// 避免把源码目录结构暴露成可浏览列表。
		h.StaticFS("/", &app.FS{
			Root:            opt.StaticDir,
			IndexNames:      []string{"index.html"},
			AcceptByteRange: true, // 前端视频/大文件拖拽需要 Range
		})
		// SPA 回退：非 /api 路径一律返回 index.html，
		// 让前端路由（/agents/123）刷新时不会 404。
		h.NoRoute(func(ctx context.Context, c *app.RequestContext) {
			path := string(c.Request.URI().Path())
			if len(path) >= 4 && path[:4] == "/api" {
				c.JSON(consts.StatusNotFound, map[string]any{
					"error": map[string]any{"code": "NOT_FOUND", "message": "接口不存在"},
				})
				return
			}
			c.File(opt.StaticDir + "/index.html")
		})
	}
}
