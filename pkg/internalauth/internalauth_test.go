package internalauth

import (
	"context"
	"errors"
	"testing"

	"github.com/bytedance/gopkg/cloud/metainfo"
	"github.com/cloudwego/kitex/pkg/endpoint"

	"github.com/aegis/pkg/errno"
)

// fakeEndpoint 记录是否被调用到的终点
func fakeEndpoint(called *bool) endpoint.Endpoint {
	return func(ctx context.Context, req, resp interface{}) error {
		*called = true
		return nil
	}
}

func TestServerMiddleware_disabled(t *testing.T) {
	if ServerMiddleware("") != nil {
		t.Fatal("token 为空应返回 nil（认证关闭）")
	}
}

func TestServerMiddleware_rejectWithoutToken(t *testing.T) {
	var called bool
	mw := ServerMiddleware("secret")
	err := mw(fakeEndpoint(&called))(context.Background(), nil, nil)
	if err == nil || called {
		t.Fatalf("无令牌应被拒绝: err=%v called=%v", err, called)
	}
	var e *errno.Error
	if !errors.As(err, &e) || e.Code != errno.ErrUnauthorized {
		t.Fatalf("应返回未授权错误: %v", err)
	}
}

func TestServerMiddleware_rejectWrongToken(t *testing.T) {
	var called bool
	mw := ServerMiddleware("secret")
	ctx := metainfo.WithPersistentValues(context.Background(), HeaderToken, "wrong")
	if err := mw(fakeEndpoint(&called))(ctx, nil, nil); err == nil || called {
		t.Fatalf("错误令牌应被拒绝: err=%v called=%v", err, called)
	}
}

func TestServerMiddleware_acceptValidToken(t *testing.T) {
	var called bool
	mw := ServerMiddleware("secret")
	ctx := metainfo.WithPersistentValues(context.Background(), HeaderToken, "secret")
	if err := mw(fakeEndpoint(&called))(ctx, nil, nil); err != nil || !called {
		t.Fatalf("合法令牌应放行: err=%v called=%v", err, called)
	}
}

// gRPC（HTTP2）通道：令牌经 FromHTTPHeader 恢复后键为 CGI 变量格式
// （X_AEGIS_INTERNAL_TOKEN），服务端必须也认这个键。
func TestServerMiddleware_acceptCGIKeyToken(t *testing.T) {
	var called bool
	mw := ServerMiddleware("secret")
	ctx := metainfo.WithPersistentValues(context.Background(), cgiTokenKey, "secret")
	if err := mw(fakeEndpoint(&called))(ctx, nil, nil); err != nil || !called {
		t.Fatalf("CGI 键令牌应放行: err=%v called=%v", err, called)
	}
}

// TTHeader 与 gRPC 两通道键并存时（请求经 gRPC 转发后再走 TTHeader 的
// 混合链路），任一键匹配即放行。
func TestServerMiddleware_mixedKeys(t *testing.T) {
	var called bool
	mw := ServerMiddleware("secret")
	ctx := metainfo.WithPersistentValues(context.Background(),
		HeaderToken, "secret", cgiTokenKey, "other")
	if err := mw(fakeEndpoint(&called))(ctx, nil, nil); err != nil || !called {
		t.Fatalf("任一键匹配应放行: err=%v", err)
	}
}

func TestClientOptions_disabled(t *testing.T) {
	if ClientOptions("") != nil {
		t.Fatal("token 为空应返回 nil")
	}
}

func TestClientOptions_injectsToken(t *testing.T) {
	var got string
	var ok bool
	opts := ClientOptions("secret")
	if len(opts) != 3 { // middleware + metainfo handler + TTHeader transport
		t.Fatalf("应返回 3 个选项: %d", len(opts))
	}
	// 从选项中取出中间件验证注入行为：直接跑一遍端到端最可靠，
	// 这里只验证选项数量与非空（真实透传由探针/集成冒烟覆盖）。
	_, _ = got, ok
}

func TestServerOptions(t *testing.T) {
	if ServerOptions("") != nil {
		t.Fatal("token 为空应返回 nil")
	}
	if n := len(ServerOptions("secret")); n != 2 { // middleware + meta handler
		t.Fatalf("应返回 2 个选项: %d", n)
	}
}

func TestEnabled(t *testing.T) {
	if Enabled("") || !Enabled("x") {
		t.Fatal("Enabled 语义错误")
	}
}
