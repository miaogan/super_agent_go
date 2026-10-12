package handler

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/cloudwego/hertz/pkg/route/param"

	"github.com/aegis/gateway/internal/middleware"
)

// newCtx 构造带 query/path/header 的 RequestContext
func newCtx(method, path string) *app.RequestContext {
	c := app.NewContext(0)
	c.Request.SetMethod(method)
	c.Request.SetRequestURI(path)
	return c
}

// newJSONCtx 构造带 JSON body 的 RequestContext。
//
// ★ 必须显式设置 ContentLength：Hertz 的 binder 在
//
//	ContentLength() <= 0 时直接跳过 body 解析（preBindBody）。
//	Request.SetBody 不会设置它，只有真实网络请求才会。
//	测试里漏掉会导致字段全为零值，且没有任何报错。
func newJSONCtx(path, body string) *app.RequestContext {
	c := newCtx("POST", path)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.SetBody([]byte(body))
	c.Request.Header.SetContentLength(len(body))
	return c
}

func TestQueryInt64Negative(t *testing.T) {
	c := newCtx("GET", "/x?uid=-5")
	if got := queryInt64(c, "uid"); got != -5 {
		t.Fatalf("queryInt64(uid=-5) = %d, 期望 -5", got)
	}
}

// ★ 这条测试守护一个真实踩过的坑：
//
//	queryInt64 早期复用按 int 累加的 queryInt，
//	"-5" 被解析成 5。负数读成正数意味着
//	"排除最后一个用户"会变成"只要最后一个用户"。
func TestQueryInt64NegativeNotFlipped(t *testing.T) {
	c := newCtx("GET", "/x?uid=-1")
	got := queryInt64(c, "uid")
	if got >= 0 {
		t.Fatalf("uid=-1 解析为 %d，符号被翻转", got)
	}
}

func TestQueryInt64Overflow(t *testing.T) {
	// 超出 int64 范围必须返回 0，而不是回绕成负数
	c := newCtx("GET", "/x?n=99999999999999999999")
	if got := queryInt64(c, "n"); got != 0 {
		t.Fatalf("溢出值解析为 %d, 期望 0", got)
	}
}

func TestQueryInt64Invalid(t *testing.T) {
	for _, v := range []string{"abc", "1.5", "", "0x10", " 1"} {
		c := newCtx("GET", "/x?n="+strings.ReplaceAll(v, " ", "%20"))
		if got := queryInt64(c, "n"); got != 0 {
			t.Fatalf("queryInt64(%q) = %d, 期望 0", v, got)
		}
	}
}

func TestParamInt64(t *testing.T) {
	c := app.NewContext(0)
	c.Params = append(c.Params, param.Param{Key: "kb_id", Value: "2001"})
	if got := paramInt64(c, "kb_id"); got != 2001 {
		t.Fatalf("paramInt64 = %d, 期望 2001", got)
	}
	// 未声明的参数返回 0
	if got := paramInt64(c, "doc_id"); got != 0 {
		t.Fatalf("未声明参数应返回 0, 实际 %d", got)
	}
}

func TestPageClampsPageSize(t *testing.T) {
	// 不限上限时 ?page_size=1000000 能让下游一次拉全表
	c := newCtx("GET", "/x?page=1&page_size=1000000")
	if got := page(c).GetPageSize(); got != 200 {
		t.Fatalf("page_size 未被截断: %d", got)
	}
}

func TestPageClampsNonPositive(t *testing.T) {
	for _, q := range []string{"?page=0&page_size=0", "?page=-3&page_size=-9", ""} {
		p := page(newCtx("GET", "/x"+q))
		if p.GetPage() < 1 || p.GetPageSize() < 1 {
			t.Fatalf("%q → page=%d size=%d, 期望均 >= 1", q, p.GetPage(), p.GetPageSize())
		}
	}
}

func TestPageDefaults(t *testing.T) {
	p := page(newCtx("GET", "/x"))
	if p.GetPage() != 1 || p.GetPageSize() != 20 {
		t.Fatalf("默认分页 = %d/%d, 期望 1/20", p.GetPage(), p.GetPageSize())
	}
}

func TestOwnerFilterIgnoresTenantQuery(t *testing.T) {
	//★ 越权防线：tenant 必须来自令牌，绝不能读 query。
	//	允许 ?tenant=other 就能跨租户读数据。
	c := newCtx("GET", "/x?tenant=other&owner_uid=9999")
	c.Set(middleware.CtxKey, contextWithIdentity(1001, "mine"))

	owner, tenant := ownerFilter(c)
	if tenant != "mine" {
		t.Fatalf("tenant = %q, 期望取令牌身份 \"mine\" 而非 query 的 \"other\"", tenant)
	}
	if owner != 9999 {
		t.Fatalf("owner = %d, 期望保留显式的 owner_uid 9999", owner)
	}
}

func TestOwnerFilterAnonymous(t *testing.T) {
	// 未鉴权时tenant 应为空而非 fallback 到任何默认值
	c := newCtx("GET", "/x")
	c.Set(middleware.CtxKey, context.Background())
	_, tenant := ownerFilter(c)
	if tenant != "" {
		t.Fatalf("匿名请求 tenant = %q, 期望空", tenant)
	}
}

func TestExtractBearer(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Bearer abc", "abc"},
		{"bearer abc", "abc"}, //大小写不敏感
		{"BEARER  abc ", "abc"},
		{"Basic abc", ""}, // 非 Bearer 前缀不宽容
		{"abc", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := extractBearer(tc.in); got != tc.want {
			t.Fatalf("extractBearer(%q) = %q, 期望 %q", tc.in, got, tc.want)
		}
	}
}

func TestFileTypeOf(t *testing.T) {
	cases := map[string]string{
		"a.pdf":      "pdf",
		"a.PDF":      "pdf",
		"a.docx":     "docx",
		"a.doc":      "docx",
		"a.xlsx":     "xlsx",
		"a.html":     "html",
		"a.htm":      "html",
		"a.md":       "md",
		"a.markdown": "md",
		"a.txt":      "txt",
		"a.exe":      "", // 未知类型交给 rag_service 判定
		"":           "",
	}
	for in, want := range cases {
		if got := fileTypeOf(in); got != want {
			t.Fatalf("fileTypeOf(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

func TestFirstNonEmptyStr(t *testing.T) {
	if got := firstNonEmptyStr("", "", "c"); got != "c" {
		t.Fatalf("got %q", got)
	}
	if got := firstNonEmptyStr(); got != "" {
		t.Fatalf("空输入应返回空串, 实际 %q", got)
	}
}

func TestReadUploadRejectsOversize(t *testing.T) {
	c := newJSONCtx("/x", strings.Repeat("a", 100))
	if _, err := readUpload(c, 50); err == nil {
		t.Fatal("超过上限的请求体应被拒绝")
	}
}

func TestReadUploadJSON(t *testing.T) {
	c := newJSONCtx("/x", `{"file_name":"notes.md","content":"hello"}`)

	in, err := readUpload(c, 1024)
	if err != nil {
		t.Fatalf("readUpload: %v", err)
	}
	if in.FileName != "notes.md" || string(in.Content) != "hello" {
		t.Fatalf("in = %+v", in)
	}
	// ★ 未显式给 file_type 时必须从文件名推断，
	//   否则 rag_service 收到空类型只能按 txt 处理，
	//   markdown 表格会被切碎。
	if in.FileType != "md" {
		t.Fatalf("FileType = %q, 期望从文件名推断为 md", in.FileType)
	}
}

// TestBindWithoutContentLengthIsSilentZero 记录 Hertz binder 的一个陷阱：
// ContentLength 未设置时 Bind 不报错但字段全为零值。
// 网关的 JSON 接口若走到这条路径，表现为"参数 mysteriously 消失"。
func TestBindWithoutContentLengthIsSilentZero(t *testing.T) {
	c := newCtx("POST", "/x")
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.SetBody([]byte(`{"name":"x"}`))

	var req struct {
		Name string `json:"name"`
	}
	if err := c.BindAndValidate(&req); err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if req.Name != "" {
		t.Skip("Hertz binder 行为已变更：现在会解析无 Content-Length 的 body")
	}
	t.Log("确认：未设Content-Length 时 binder 静默不解析")
}

func TestOKEnvelope(t *testing.T) {
	c := app.NewContext(0)
	OK(c, map[string]string{"k": "v"})
	if c.Response.StatusCode() != consts.StatusOK {
		t.Fatalf("status = %d", c.Response.StatusCode())
	}
	if !strings.Contains(string(c.Response.Body()), `"data"`) {
		t.Fatalf("响应体未包裹 data: %s", c.Response.Body())
	}
}

func TestErrorEnvelope(t *testing.T) {
	c := app.NewContext(0)
	Error(c, consts.StatusBadRequest, "MISSING_PARAM", "kb_id 不能为空")
	if c.Response.StatusCode() != consts.StatusBadRequest {
		t.Fatalf("status = %d", c.Response.StatusCode())
	}
	body := string(c.Response.Body())
	for _, want := range []string{"MISSING_PARAM", "kb_id 不能为空"} {
		if !strings.Contains(body, want) {
			t.Fatalf("响应体缺少 %q: %s", want, body)
		}
	}
}

// TestHAdapterSetsCtx 保证没有中间件写 ctx 时
// ctxOf 仍能拿到可用 context（否则所有下游调用都退化为 context.Background()）
func TestHAdapterSetsCtx(t *testing.T) {
	c := app.NewContext(0)
	var gotCtxOK bool
	h := H(func(c *app.RequestContext) {
		_, gotCtxOK = c.Get(middleware.CtxKey)
	})
	h(context.Background(), c)
	if !gotCtxOK {
		t.Fatal("H 适配器未写入 ctx")
	}
}
