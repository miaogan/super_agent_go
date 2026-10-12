package metrics

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aegis/pkg/errno"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// counterValue 读取指定标签组合的计数器值；不存在返回 0。
func TestRecordHTTP(t *testing.T) {
	before := counterValue(t, "aegis_http_requests_total", "POST", "/api/v1/agents/:agent_id/chat", "0")
	RecordHTTP("POST", "/api/v1/agents/:agent_id/chat", "0", 12*time.Millisecond)
	after := counterValue(t, "aegis_http_requests_total", "POST", "/api/v1/agents/:agent_id/chat", "0")
	if after != before+1 {
		t.Errorf("HTTP 计数未增长: before=%v after=%v", before, after)
	}
}

// counterValue 读取指定标签组合的计数器值；不存在返回 0。
func counterValue(t *testing.T, name string, labels ...string) float64 {
	t.Helper()
	// 直接查 vec 内部 —— 测试与实现同包，可访问内部结构
	switch name {
	case "aegis_http_requests_total":
		return testutil.ToFloat64(httpRequests.WithLabelValues(labels...))
	case "aegis_rpc_server_requests_total":
		return testutil.ToFloat64(rpcServerRequests.WithLabelValues(labels...))
	case "aegis_rpc_client_requests_total":
		return testutil.ToFloat64(rpcClientRequests.WithLabelValues(labels...))
	case "aegis_rpc_server_active_requests":
		return testutil.ToFloat64(rpcServerActive.WithLabelValues(labels...))
	}
	t.Fatalf("未知指标 %q", name)
	return 0
}

// TestRecordHTTPLabelCardinality 非模板路径不应产生新标签组合（低基数约束）。
func TestRecordHTTPLabelCardinality(t *testing.T) {
	// 高基数路径（含用户数据）理论上不应被传入；此处验证调用不 panic
	RecordHTTP("GET", "/api/v1/agents/1/chat", "200", time.Millisecond)
}

// TestRPCCode rpcCode 映射：成功 0，业务错误码，非业务 50001。
func TestRPCCode(t *testing.T) {
	if got := rpcCode(nil); got != "0" {
		t.Errorf("rpcCode(nil) = %q, 期望 0", got)
	}
	if got := rpcCode(errno.New(errno.ErrForbidden)); got != "20003" {
		t.Errorf("rpcCode(业务错误) = %q, 期望 20003", got)
	}
	if got := rpcCode(errors.New("普通错误")); got != "50001" {
		t.Errorf("rpcCode(普通错误) = %q, 期望 50001", got)
	}
}

// TestKitexServerMiddleware 服务端中间件：执行回调、在途量增减、请求计数。
func TestKitexServerMiddleware(t *testing.T) {
	before := counterValue(t, "aegis_rpc_server_requests_total", "svc", "unknown", "0")

	md := KitexServerMiddleware("svc")
	called := false
	ep := md(func(context.Context, interface{}, interface{}) error {
		called = true
		return nil
	})

	if err := ep(context.Background(), nil, nil); err != nil {
		t.Fatalf("中间件执行失败: %v", err)
	}
	if !called {
		t.Error("中间件应执行下游 Endpoint")
	}

	after := counterValue(t, "aegis_rpc_server_requests_total", "svc", "unknown", "0")
	if after != before+1 {
		t.Errorf("服务端请求计数未增长: before=%v after=%v", before, after)
	}

	// 在途量应归零（Inc 后 Dec）
	active := counterValue(t, "aegis_rpc_server_active_requests", "svc")
	if active != 0 {
		t.Errorf("执行完毕后在途量应归零, 实际 %v", active)
	}

	// 错误路径：下游返回业务错误 → code 标签应带错误码
	beforeErr := counterValue(t, "aegis_rpc_server_requests_total", "svc", "unknown", "40420")
	epErr := md(func(context.Context, interface{}, interface{}) error {
		return errno.New(errno.ErrKBNotFound)
	})
	if err := epErr(context.Background(), nil, nil); err == nil {
		t.Fatal("错误 Endpoint 应返回错误")
	}
	if got := counterValue(t, "aegis_rpc_server_requests_total", "svc", "unknown", "40420"); got != beforeErr+1 {
		t.Errorf("错误路径计数未增长: %v", got)
	}
}

// TestKitexClientMiddleware 调用方中间件：无 RPCInfo 时回落 unknown 标签。
func TestKitexClientMiddleware(t *testing.T) {
	before := counterValue(t, "aegis_rpc_client_requests_total", "caller", "unknown", "unknown", "0")

	md := KitexClientMiddleware("caller")
	called := false
	ep := md(func(context.Context, interface{}, interface{}) error {
		called = true
		return nil
	})
	if err := ep(context.Background(), nil, nil); err != nil {
		t.Fatalf("中间件执行失败: %v", err)
	}
	if !called {
		t.Error("中间件应执行下游 Endpoint")
	}
	after := counterValue(t, "aegis_rpc_client_requests_total", "caller", "unknown", "unknown", "0")
	if after != before+1 {
		t.Errorf("调用方计数未增长: before=%v after=%v", before, after)
	}
}

// TestWriteText 文本渲染不报错且包含 aegis_ 前缀指标。
func TestWriteText(t *testing.T) {
	var b strings.Builder
	if err := WriteText(&b); err != nil {
		t.Fatalf("WriteText 失败: %v", err)
	}
	if !strings.Contains(b.String(), "aegis_") {
		t.Errorf("渲染文本应包含 aegis_ 指标, 实际内容: %.200s", b.String())
	}
}

// TestHandler 处理器可创建（HTTP 层验证留给健康端点冒烟）。
func TestHandler(t *testing.T) {
	h := Handler()
	if h == nil {
		t.Fatal("Handler 不应返回 nil")
	}
}
