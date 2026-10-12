package toolexec

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeRegistry 固定返回一条工具定义，替代 PG tools 表。
type fakeRegistry struct {
	def *ToolDef
	err error
}

func (f *fakeRegistry) GetTool(context.Context, string) (*ToolDef, error) {
	return f.def, f.err
}

// strictOn 返回一个"允许回环"的执行器，用于跑通正常链路：
// httptest 服务器监听在 127.0.0.1，严格策略下必然被拦，
// 所以放行只能靠例外名单，不能靠打开 AllowPrivateIPs。
func executorFor(reg ToolRegistry, allowHosts []string, private bool) ToolExecutor {
	return NewHTTP(reg, Config{
		Timeout:          5 * time.Second,
		MaxResponseBytes: 1 << 20,
		AllowPrivateIPs:  private,
		AllowHosts:       allowHosts,
	})
}

func TestExecuteHTTPPost(t *testing.T) {
	var gotBody, gotCT, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		r.Body.Read(b)
		gotBody, gotCT, gotUA = string(b), r.Header.Get("Content-Type"), r.Header.Get("User-Agent")
		w.Write([]byte(`{"temp":21}`))
	}))
	defer srv.Close()

	// 127.0.0.1 是回环，必须逐个放行而不是整体开内网
	ex := executorFor(&fakeRegistry{def: &ToolDef{Endpoint: srv.URL, Method: "post"}},
		[]string{"127.0.0.1"}, false)

	out, err := ex.Execute(context.Background(), "weather", `{"city":"BJ"}`)
	if err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	if out != `{"temp":21}` {
		t.Errorf("结果不符: %q", out)
	}
	if gotBody != `{"city":"BJ"}` {
		t.Errorf("参数未透传: %q", gotBody)
	}
	if !strings.Contains(gotCT, "application/json") {
		t.Errorf("缺少 JSON Content-Type: %q", gotCT)
	}
	if gotUA != "Aegis-ToolExecutor/1.0" {
		t.Errorf("UA 不符: %q", gotUA)
	}
}

// TestExecuteBlocksLoopbackByDefault 默认策略下，工具 endpoint 指向回环必须被拒。
//
// 这是本次修复的核心断言：旧实现显式设 AllowPrivateIPs=true，
// 任何管理员写一条 http://127.0.0.1:8886/... 就能让服务替它打内网。
func TestExecuteBlocksLoopbackByDefault(t *testing.T) {
	ex := executorFor(&fakeRegistry{def: &ToolDef{Endpoint: "http://127.0.0.1:1/x"}}, nil, false)
	_, err := ex.Execute(context.Background(), "t", `{}`)
	if !errors.Is(err, ErrToolBlocked) {
		t.Fatalf("回环 endpoint 应被 SSRF 拦截, 实际 %v", err)
	}
}

// TestExecuteBlocksCloudMetadata 云元数据地址是最典型的 SSRF 目标。
func TestExecuteBlocksCloudMetadata(t *testing.T) {
	ex := executorFor(&fakeRegistry{def: &ToolDef{Endpoint: "http://169.254.169.254/latest/meta-data/"}}, nil, false)
	_, err := ex.Execute(context.Background(), "t", `{}`)
	if !errors.Is(err, ErrToolBlocked) {
		t.Fatalf("元数据地址应被拦截, 实际 %v", err)
	}
}

// TestExecuteBlocksNonHTTPScheme 协议白名单：file:// 不能借工具执行读本地文件。
func TestExecuteBlocksNonHTTPScheme(t *testing.T) {
	ex := executorFor(&fakeRegistry{def: &ToolDef{Endpoint: "file:///etc/passwd"}}, nil, false)
	_, err := ex.Execute(context.Background(), "t", `{}`)
	if !errors.Is(err, ErrToolBlocked) {
		t.Fatalf("file:// 应被协议白名单拦截, 实际 %v", err)
	}
}

// TestExecuteBlocksRedirectToLoopback 首跳合法、302 跳回环也必须拦。
//
// 旧实现用 http.DefaultClient，它会静默跟随最多 10 次重定向且不校验 Location。
//
// 用 "localhost" 与 "127.0.0.1" 两种写法分离首跳和跳板：例外名单是
// host 字符串匹配，放行前者不会连带放行后者。
func TestExecuteBlocksRedirectToLoopback(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:1/internal", http.StatusFound)
	}))
	defer srv.Close()

	firstHop := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)

	// 先确认基线：首跳放行时确实能拿到 302（说明后面的拦截来自重定向校验，
	// 而不是首跳就被拒）
	exOK := executorFor(&fakeRegistry{def: &ToolDef{Endpoint: firstHop}},
		[]string{"localhost"}, false)
	if _, err := exOK.Execute(context.Background(), "t", `{}`); err == nil {
		t.Fatal("302 之后跳到 127.0.0.1:1 应该失败")
	} else if !errors.Is(err, ErrToolBlocked) {
		t.Fatalf("应被判为 SSRF 拦截而非普通执行失败: %v", err)
	}

	// 首跳也不放行时，直接在第 1 层就被拒
	exStrict := executorFor(&fakeRegistry{def: &ToolDef{Endpoint: firstHop}}, nil, false)
	if _, err := exStrict.Execute(context.Background(), "t", `{}`); !errors.Is(err, ErrToolBlocked) {
		t.Fatalf("未放行的首跳应被拦截, 实际 %v", err)
	}
}

// TestExecuteBlocksIPv6Loopback 回环的 IPv6 写法同样要拦。
func TestExecuteBlocksIPv6Loopback(t *testing.T) {
	ex := executorFor(&fakeRegistry{def: &ToolDef{Endpoint: "http://[::1]:1/final"}}, nil, false)
	if _, err := ex.Execute(context.Background(), "t", `{}`); !errors.Is(err, ErrToolBlocked) {
		t.Fatalf("IPv6 回环应被拦截, 实际 %v", err)
	}
}

func TestExecuteUpstreamErrorIncludesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"city 必填"}`))
	}))
	defer srv.Close()

	ex := executorFor(&fakeRegistry{def: &ToolDef{Endpoint: srv.URL}}, []string{"127.0.0.1"}, false)
	_, err := ex.Execute(context.Background(), "t", `{}`)
	if err == nil {
		t.Fatal("400 应报错")
	}
	// 上游错误体要带进错误信息，LLM 才有机会改参数重试
	if !strings.Contains(err.Error(), "city 必填") {
		t.Errorf("错误应包含上游响应体, 实际 %v", err)
	}
}

func TestExecuteNonJSONResponseQuoted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("晴 21℃"))
	}))
	defer srv.Close()

	ex := executorFor(&fakeRegistry{def: &ToolDef{Endpoint: srv.URL}}, []string{"127.0.0.1"}, false)
	out, err := ex.Execute(context.Background(), "t", `{}`)
	if err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	// role=tool 的 content 必须是合法 JSON 字符串，否则回喂 LLM 会解析失败
	if !strings.HasPrefix(out, `"`) || !strings.HasSuffix(out, `"`) {
		t.Errorf("非 JSON 响应应被包装成 JSON 字符串: %q", out)
	}
}

// TestExecuteTimeoutCappedByGlobal 工具写一个天文数字 timeout 也不能拖垮 ReAct 循环。
func TestExecuteTimeoutCappedByGlobal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(1200 * time.Millisecond):
		}
	}))
	defer srv.Close()

	ex := NewHTTP(&fakeRegistry{def: &ToolDef{
		Endpoint:  srv.URL,
		TimeoutMS: 99999999, // 故意写大
	}}, Config{Timeout: 200 * time.Millisecond, AllowHosts: []string{"127.0.0.1"}})

	start := time.Now()
	_, err := ex.Execute(context.Background(), "t", `{}`)
	if err == nil {
		t.Fatal("超时应报错")
	}
	if elapsed := time.Since(start); elapsed > 1000*time.Millisecond {
		t.Errorf("全局上限未生效，耗时 %v", elapsed)
	}
}

func TestExecuteUnsupportedType(t *testing.T) {
	ex := executorFor(&fakeRegistry{def: &ToolDef{Type: "grpc", Endpoint: "x"}}, nil, false)
	_, err := ex.Execute(context.Background(), "t", `{}`)
	if err == nil || !strings.Contains(err.Error(), "暂不支持") {
		t.Fatalf("非 http 类型应明确报错, 实际 %v", err)
	}
}

func TestExecuteRegistryErrorPropagates(t *testing.T) {
	sentinel := errors.New("boom")
	ex := executorFor(&fakeRegistry{err: sentinel}, nil, false)
	_, err := ex.Execute(context.Background(), "t", `{}`)
	if !errors.Is(err, sentinel) {
		t.Fatalf("仓储错误应原样上抛, 实际 %v", err)
	}
}

// TestPolicyDefaultsAreStrict 策略零值必须是严格档，避免"忘了配就等于放开"。
func TestPolicyDefaultsAreStrict(t *testing.T) {
	ex := NewHTTP(&fakeRegistry{}, Config{}).(*httpExec)
	if ex.opt.SSRF.AllowPrivateIPs {
		t.Error("零值 Config 不应允许内网")
	}
	if ex.opt.Timeout != 30*time.Second {
		t.Errorf("默认超时应为 30s, 实际 %v", ex.opt.Timeout)
	}
	if ex.opt.MaxSizeBytes != 1<<20 {
		t.Errorf("默认响应上限应为 1MB, 实际 %v", ex.opt.MaxSizeBytes)
	}
	if ex.opt.SSRF.DNSRecheckCount != 3 {
		t.Errorf("默认 DNS 复解析次数应为 3, 实际 %v", ex.opt.SSRF.DNSRecheckCount)
	}
}
