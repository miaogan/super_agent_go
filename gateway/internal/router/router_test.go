package router

import (
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/route"
)

// newEngine 构造一个不真正监听的 Hertz 实例
func newEngine() *server.Hertz {
	return server.New(server.WithHostPorts("127.0.0.1:0"))
}

func TestRegisterNoDuplicateRoute(t *testing.T) {
	// ★ 这条测试守护一个启动即崩的问题：
	//
	//   Hertz 在注册重复 method+path 时直接 panic
	//   （handlers are already registered for path ...）。
	//   SSE 组曾同时注册 POST 与业务组的同路径 POST，
	//   导致网关进程根本起不来。
	h := newEngine()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("路由注册 panic: %v", r)
		}
	}()
	Register(h, Options{Deps: newTestDeps()})
}

func TestRegisterContainsAllRoutes(t *testing.T) {
	h := newEngine()
	Register(h, Options{Deps: newTestDeps()})

	want := []string{
		"GET /healthz",
		"GET /livez",
		"GET /readyz",
		"GET /healthz/full",
		"POST /api/v1/auth/login",
		"POST /api/v1/auth/refresh",
		"POST /api/v1/auth/logout",
		"GET /api/v1/auth/idps",
		"GET /api/v1/auth/oauth/callback",
		"GET /api/v1/auth/me",
		"POST /api/v1/agents",
		"GET /api/v1/agents",
		"GET /api/v1/agents/:agent_id",
		"PUT /api/v1/agents/:agent_id",
		"DELETE /api/v1/agents/:agent_id",
		"POST /api/v1/agents/:agent_id/chat",
		"POST /api/v1/agents/:agent_id/chat/stream",
		// EventSource 只能 GET，必须单独存在
		"GET /api/v1/agents/:agent_id/chat/stream",
		"POST /api/v1/sessions",
		"GET /api/v1/sessions",
		"DELETE /api/v1/sessions/:session_id",
		"GET /api/v1/sessions/:session_id/messages",
		"POST /api/v1/knowledge-bases",
		"GET /api/v1/knowledge-bases",
		"GET /api/v1/knowledge-bases/:kb_id",
		"DELETE /api/v1/knowledge-bases/:kb_id",
		"POST /api/v1/knowledge-bases/:kb_id/documents",
		"GET /api/v1/knowledge-bases/:kb_id/documents",
		"DELETE /api/v1/knowledge-bases/documents/:doc_id",
		"GET /api/v1/knowledge-bases/documents/:doc_id/chunks",
		"POST /api/v1/rag/retrieve",
		"POST /api/v1/rag/debug/retrieve",
	}

	got := collectRoutes(h.Routes())
	for _, w := range want {
		if !got[w] {
			t.Errorf("缺少路由 %s", w)
		}
	}
}

// collectRoutes 把 Hertz 路由树摊平成 "METHOD /path"
func collectRoutes(all route.RoutesInfo) map[string]bool {
	out := make(map[string]bool, len(all))
	for _, r := range all {
		out[r.Method+" "+r.Path] = true
	}
	return out
}
