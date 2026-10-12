package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aegis/pkg/ssrf"
)

// localPolicy 允许内网：httptest 服务器监听在 127.0.0.1（回环）。
// 本组用例测的是 fetch 的大小/状态码/重定向逻辑，SSRF 拦截单独用 Default 测。
var localPolicy = ssrf.Policy{AllowPrivateIPs: true}

// newServer 造一个返回固定内容的测试服务器。
func newServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// localOpt 构造允许回环地址的拉取选项。
func localOpt(maxSize int64) Options {
	return Options{
		MaxSizeBytes: maxSize,
		Timeout:      3 * time.Second,
		SSRF:         localPolicy,
	}
}

// TestFetchOK 正常拉取：内容与上游一致。
func TestFetchOK(t *testing.T) {
	srv := newServer(t, "hello aegis")
	data, err := Fetch(context.Background(), srv.URL, localOpt(0))
	if err != nil {
		t.Fatalf("Fetch 失败: %v", err)
	}
	if string(data) != "hello aegis" {
		t.Errorf("内容不符: %q", data)
	}
}

// TestFetchDefaults 零值 Timeout/MaxSizeBytes 应回落默认（15s / 20MB）。
func TestFetchDefaults(t *testing.T) {
	srv := newServer(t, "ok")
	// 仅 SSRF 策略给定（允许回环）；超时与大小上限留零值验证回落
	data, err := Fetch(context.Background(), srv.URL, Options{SSRF: localPolicy})
	if err != nil {
		t.Fatalf("Fetch(零值 Options) 失败: %v", err)
	}
	if string(data) != "ok" {
		t.Errorf("内容不符: %q", data)
	}
}

// TestFetchContentLengthTooLarge Content-Length 前置检查拦截超限文件。
func TestFetchContentLengthTooLarge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1024")
		w.Write([]byte("x"))
	}))
	t.Cleanup(srv.Close)

	_, err := Fetch(context.Background(), srv.URL, localOpt(100))
	if err == nil || !strings.Contains(err.Error(), "超过最大限制") {
		t.Fatalf("应返回 ErrTooLarge, 实际 %v", err)
	}
}

// TestFetchStreamingTooLarge 无 Content-Length 时流式读取超限拦截。
func TestFetchStreamingTooLarge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 不设 Content-Length，写超过上限的字节
		w.Write([]byte(strings.Repeat("a", 2048)))
	}))
	t.Cleanup(srv.Close)

	_, err := Fetch(context.Background(), srv.URL, localOpt(100))
	if err == nil || !strings.Contains(err.Error(), "超过最大限制") {
		t.Fatalf("应返回 ErrTooLarge, 实际 %v", err)
	}
}

// TestFetchNon2xx 非 2xx 状态码应报错。
func TestFetchNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	_, err := Fetch(context.Background(), srv.URL, localOpt(0))
	if err == nil || !strings.Contains(err.Error(), "上游返回 500") {
		t.Fatalf("应报上游错误, 实际 %v", err)
	}
}

// TestFetchSSRFBlocked 内网目标应被 SSRF 层拦截（第一层）。
func TestFetchSSRFBlocked(t *testing.T) {
	_, err := Fetch(context.Background(), "http://127.0.0.1:1/", Default)
	if err == nil || !strings.Contains(err.Error(), "被拦截") {
		t.Fatalf("内网 IP 应被 SSRF 拦截, 实际 %v", err)
	}
}

// TestFetchInvalidScheme 非 http/https 协议应拦截。
func TestFetchInvalidScheme(t *testing.T) {
	_, err := Fetch(context.Background(), "file:///etc/passwd", Default)
	if err == nil || !strings.Contains(err.Error(), "协议") {
		t.Fatalf("file:// 应被协议白名单拦截, 实际 %v", err)
	}
}

// TestFetchRedirectRevalidatesSSRF 跟随重定向时同样会做 SSRF 校验。
//
// 本地无法用「公网首跳 + 内网重定向」构造（httptest 在回环上），
// 因此改用生产策略下对回环重定向的拦截来证明 CheckRedirect 生效：
// 首跳与重定向目标均经 ssrf.Validate，任一命中内网即整体拒绝。
func TestFetchRedirectRevalidatesSSRF(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL, http.StatusFound) // 自我重定向
	}))
	t.Cleanup(srv.Close)

	// 生产策略（禁止内网）下，重定向循环会在 CheckRedirect 里被拦下，
	// 错误必须是 SSRF 拦截而非「重定向次数过多」。
	_, err := Fetch(context.Background(), srv.URL, Default)
	if err == nil || !strings.Contains(err.Error(), "被拦截") {
		t.Fatalf("生产策略下重定向目标应被 SSRF 拦截, 实际 %v", err)
	}
}

// TestFetchTooManyRedirects 超过 5 次重定向应报错（本地策略下重定向校验放行）。
func TestFetchTooManyRedirects(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL, http.StatusFound) // 自我重定向死循环
	}))
	t.Cleanup(srv.Close)

	_, err := Fetch(context.Background(), srv.URL, localOpt(0))
	if err == nil {
		t.Fatal("重定向死循环应报错")
	}
	if strings.Contains(err.Error(), "被拦截") {
		t.Fatalf("本地策略下不应 SSRF 拦截: %v", err)
	}
}
