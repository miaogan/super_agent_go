package parser

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---------- HTML ----------

func TestParseHTML_stripsNonContent(t *testing.T) {
	src := `<html><head><title>MARKER_HEAD_TITLE</title><style>p{color:red}</style></head>
<body><script>var x = "<h1>MARKER_SCRIPT</h1>";</script>
<h1>大标题</h1><p>第一段 有   多余
空格</p><ul><li>项目一</li><li>项目二</li></ul>
<!-- MARKER_COMMENT --><noscript>MARKER_NOSCRIPT</noscript>
</body></html>`

	out, err := testParse("html", []byte(src))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	for _, want := range []string{"大标题", "第一段", "项目一", "项目二"} {
		if !strings.Contains(out, want) {
			t.Errorf("缺少正文片段 %q，输出=%q", want, out)
		}
	}
	// 这些都不该进正文：会污染 embedding 且让命中片段满是噪音。
	// 用 MARKER_ 前缀的唯一串，避免与正常正文发生子串误匹配。
	for _, bad := range []string{"MARKER_HEAD_TITLE", "MARKER_SCRIPT", "MARKER_COMMENT",
		"MARKER_NOSCRIPT", "color:red", "var x", "<p>", "<h1>"} {
		if strings.Contains(out, bad) {
			t.Errorf("不应包含 %q，输出=%q", bad, out)
		}
	}
	// 连续空白应被压成单空格
	if strings.Contains(out, "有   多余") {
		t.Error("文本节点内连续空白未被归一")
	}
}

func TestParseHTML_angleBracketInAttribute(t *testing.T) {
	// 正则剥标签在这里会把 `>链接</a>` 之后的内容吞掉或错切
	src := `<p><a href="/x?a=1&b=2>3">链接</a>后续</p>`
	out, err := testParse("html", []byte(src))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !strings.Contains(out, "链接") || !strings.Contains(out, "后续") {
		t.Errorf("属性含 > 时文本丢失: %q", out)
	}
	if strings.Contains(out, "href") {
		t.Errorf("属性名泄漏进正文: %q", out)
	}
}

func TestParseHTML_nestedSkipTags(t *testing.T) {
	// 同名跳过元素嵌套：布尔标记会提前出栈，把 style 里的文本当正文
	src := `<div><template><template>内层应丢弃</template>外层应丢弃</template>保留</div>`
	out, err := testParse("html", []byte(src))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if strings.Contains(out, "应丢弃") {
		t.Errorf("嵌套 template 未整体丢弃: %q", out)
	}
	if !strings.Contains(out, "保留") {
		t.Errorf("正常内容被误删: %q", out)
	}
}

func TestParseHTML_noVisibleText(t *testing.T) {
	_, err := testParse("html", []byte(`<html><head><style>a{}</style></head><body></body></html>`))
	if !errors.Is(err, ErrParseFailed) {
		t.Fatalf("纯样式文档应报解析失败, got %v", err)
	}
}

// ---------- CSV ----------

func TestParseCSV_expandsHeader(t *testing.T) {
	src := "城市,年份,销售额\n北京,2023,1200\n上海,2024,\"1,300\"\n"
	out, err := testParse("csv", []byte(src))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	// 表头必须与值绑定，否则切片"北京 2023 1200"无法回答"这是什么指标"
	for _, want := range []string{"城市: 北京", "销售额: 1200", "城市: 上海"} {
		if !strings.Contains(out, want) {
			t.Errorf("缺少 %q，输出=%q", want, out)
		}
	}
	// 引号内逗号是一个字段，不能被拆成两列
	if !strings.Contains(out, "销售额: 1,300") {
		t.Errorf("带引号的逗号被错误分列: %q", out)
	}
	if strings.Contains(out, "城市,年份") {
		t.Errorf("原始 CSV 行未被展开: %q", out)
	}
}

func TestParseCSV_raggedAndBlankRows(t *testing.T) {
	src := "a,b\n1\n\n2,3\n"
	out, err := testParse("csv", []byte(src))
	if err != nil {
		t.Fatalf("列数不一致不应导致整体失败: %v", err)
	}
	// 第二行缺列：只输出有的那一列，不报错也不编造第二列
	if !strings.Contains(out, "a: 1") {
		t.Errorf("缺列行内容丢失: %q", out)
	}
	if strings.Contains(out, "b: 1") || strings.Contains(out, "b: \n") {
		t.Errorf("为缺失的列编造了值: %q", out)
	}
	// 空行被跳过后，只剩两个数据行 → 序号应为 1 和 2
	if strings.Contains(out, "第 3 行") {
		t.Errorf("空行未被跳过，行号被占位: %q", out)
	}
	if !strings.Contains(out, "第 2 行") || !strings.Contains(out, "a: 2") {
		t.Errorf("数据行序号未连续: %q", out)
	}
}

func TestParseCSV_missingHeaderFallsBackToColumnIndex(t *testing.T) {
	out, err := testParse("csv", []byte("姓名\n"))
	if err == nil {
		t.Fatalf("只有表头没有数据行应报错, got %q", out)
	}
	if !errors.Is(err, ErrParseFailed) {
		t.Fatalf("应报解析失败, got %v", err)
	}
}

// ---------- PPTX ----------

// buildPPTX 造一个最小合法的 pptx：正文在 ppt/slides/slideN.xml 的 <a:t> 里。
// text 为空串时生成"真的没有文本节点"的页面，用于验证空白页占位行为。
func buildPPTX(t *testing.T, slides map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, text := range slides {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		var body string
		if text != "" {
			body = fmt.Sprintf(
				`<a:p><a:r><a:t>%s</a:t></a:r></a:p>`+
					`<a:p><a:r><a:t>%s 第二行</a:t></a:r></a:p>`, text, text)
		}
		fmt.Fprintf(w, `<p:sld xmlns:a="http://p"><p:cSld><p:sp><p:txBody>%s</p:txBody></p:sp></p:cSld></p:sld>`, body)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestParsePPTX_ordersBySlideNumber(t *testing.T) {
	raw := buildPPTX(t, map[string]string{
		"ppt/slides/slide1.xml":           "第一页",
		"ppt/slides/slide2.xml":           "第二页",
		"ppt/slides/slide10.xml":          "第十页",
		"ppt/notesSlides/notesSlide1.xml": "备注不该出现",
	})
	out, err := testParse("pptx", raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	i1, i2, i10 := strings.Index(out, "第一页"), strings.Index(out, "第二页"), strings.Index(out, "第十页")
	if i1 < 0 || i2 < 0 || i10 < 0 {
		t.Fatalf("页内容缺失: %q", out)
	}
	// 字典序会把 slide10 排在 slide2 之前，正文顺序直接错乱
	if !(i1 < i2 && i2 < i10) {
		t.Errorf("未按页码排序: 第一页=%d 第二页=%d 第十页=%d", i1, i2, i10)
	}
	if strings.Contains(out, "备注不该出现") {
		t.Error("备注页内容不应抽取")
	}
	if !strings.Contains(out, "【第 10 页】") {
		t.Errorf("缺少页码标记: %q", out)
	}
}

func TestParsePPTX_emptySlideMarked(t *testing.T) {
	raw := buildPPTX(t, map[string]string{
		"ppt/slides/slide1.xml": "有内容",
		"ppt/slides/slide2.xml": "",
	})
	out, err := testParse("pptx", raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !strings.Contains(out, "本页无文本内容") {
		t.Errorf("空白页应留占位以保住页码连续性: %q", out)
	}
}

func TestParsePPTX_noSlides(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("[Content_Types].xml")
	w.Write([]byte("x"))
	zw.Close()
	if _, err := testParse("pptx", buf.Bytes()); !errors.Is(err, ErrParseFailed) {
		t.Fatalf("无幻灯片的 pptx 应报解析失败, got %v", err)
	}
}

// ---------- 空白规整 ----------

func TestNormalizeTextThroughParse(t *testing.T) {
	src := "行一\r\n\r\n\r\n\r\n行二   \n\t行三"
	out, err := testParse("md", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "\r") {
		t.Error("CRLF 未统一")
	}
	if strings.Contains(out, "\n\n\n") {
		t.Errorf("连续空行未压缩: %q", out)
	}
	if strings.Contains(out, "行二   \n") {
		t.Errorf("行尾空格未去除: %q", out)
	}
	if !strings.Contains(out, "行三") {
		t.Errorf("内容丢失: %q", out)
	}
}

func TestParse_emptyFileRejected(t *testing.T) {
	// 返回空文本会被下游当成"解析成功但无内容"，掩盖上传失败
	for _, ft := range []string{"txt", "pdf", "docx"} {
		if _, err := testParse(ft, nil); !errors.Is(err, ErrParseFailed) {
			t.Errorf("%s 空文件应报错, got %v", ft, err)
		}
	}
}

// ---------- MinerU ----------

// mineruHandler 记录收到的请求并返回契约规定的响应。
func mineruHandler(t *testing.T, resp string) (*httptest.Server, *string) {
	t.Helper()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/file_parse" {
			t.Errorf("路径应为 /file_parse, 实际 %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("方法应为 POST, 实际 %s", r.Method)
		}
		ct := r.Header.Get("Content-Type")
		if !strings.HasPrefix(ct, "multipart/form-data") {
			t.Errorf("Content-Type 应为 multipart，实际 %q（fetch 默认会填 application/json）", ct)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("解析 multipart 失败: %v", err)
			return
		}
		// 契约要求：字段名 files（复数）、return_md=true
		if v := r.FormValue("return_md"); v != "true" {
			t.Errorf("return_md 应为 true, 实际 %q", v)
		}
		f, hdr, err := r.FormFile("files")
		if err != nil {
			t.Errorf("应存在名为 files 的文件字段: %v", err)
			return
		}
		defer f.Close()
		b, _ := io.ReadAll(f)
		got = fmt.Sprintf("%s|%s|%s", hdr.Filename, string(b), r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, resp)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func TestMinerU_happyPath(t *testing.T) {
	srv, captured := mineruHandler(t,
		`{"results":{"doc.pdf":{"md_content":"# 标题\n\n正文内容","info":{"page_count":3}}}}`)

	// BaseURL 的主机自动进入 SSRF 例外名单：httptest 在 127.0.0.1，
	// 严格策略下不加例外必然被拦，这里同时验证了自动放行生效。
	eng, err := NewMinerU(MinerUConfig{BaseURL: srv.URL, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	out, err := eng.ParsePDF(context.Background(), []byte("%PDF-1.7 fake"), "doc.pdf")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !strings.Contains(out, "正文内容") || !strings.Contains(out, "# 标题") {
		t.Errorf("Markdown 正文未取到: %q", out)
	}
	if !strings.HasPrefix(*captured, "doc.pdf|%PDF-1.7 fake|") {
		t.Errorf("上传的文件名或内容不符: %q", *captured)
	}
}

func TestMinerU_sendsBearerToken(t *testing.T) {
	srv, captured := mineruHandler(t, `{"results":{"a.pdf":{"md_content":"ok"}}}`)
	eng, err := NewMinerU(MinerUConfig{BaseURL: srv.URL, Timeout: 5 * time.Second, AuthToken: "secret-tok"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.ParsePDF(context.Background(), []byte("x"), "a.pdf"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(*captured, "|Bearer secret-tok") {
		t.Errorf("Bearer 头未发送: %q", *captured)
	}
}

// TestMinerU_neverFallsBackToLocal 配置了 MinerU 就不能回退本地解析。
//
// 静默回退的表现是"以为在用 OCR，实际入库空白文本"，
// 根因在检索质量上完全看不出，所以这里必须拿到明确错误。
func TestMinerU_neverFallsBackToLocal(t *testing.T) {
	cases := []struct {
		name string
		resp string
	}{
		{"无 results 字段", `{"detail":"ok"}`},
		{"md_content 为空", `{"results":{"a.pdf":{"md_content":"   "}}}`},
		{"返回多条结果", `{"results":{"a":{"md_content":"一"},"b":{"md_content":"二"}}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, _ := mineruHandler(t, c.resp)
			eng, err := NewMinerU(MinerUConfig{BaseURL: srv.URL, Timeout: 5 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			_, err = eng.ParsePDF(context.Background(), []byte("%PDF fake"), "a.pdf")
			if err == nil {
				t.Fatal("应报错，不能静默返回空正文")
			}
			if errors.Is(err, ErrUnsupportedFormat) {
				t.Fatalf("不应误判为格式不支持: %v", err)
			}
			if strings.Contains(err.Error(), "无可提取文本") {
				t.Errorf("错误来自本地解析器，说明发生了回退: %v", err)
			}
		})
	}
}

func TestMinerU_upstreamErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"error":"gpu busy"}`)
	}))
	defer srv.Close()

	eng, err := NewMinerU(MinerUConfig{BaseURL: srv.URL, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_, err = eng.ParsePDF(context.Background(), []byte("x"), "a.pdf")
	if err == nil {
		t.Fatal("5xx 应报错")
	}
	if !errors.Is(err, ErrParseFailed) {
		t.Errorf("应包装为 ErrParseFailed, got %v", err)
	}
}

func TestMinerU_timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(1500 * time.Millisecond):
		}
		io.WriteString(w, `{"results":{"a":{"md_content":"太晚了"}}}`)
	}))
	defer srv.Close()

	eng, err := NewMinerU(MinerUConfig{BaseURL: srv.URL, Timeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	_, err = eng.ParsePDF(context.Background(), []byte("x"), "a.pdf")
	if err == nil {
		t.Fatal("超时应报错")
	}
	if !errors.Is(err, ErrParseFailed) {
		t.Errorf("应包装为 ErrParseFailed, got %v", err)
	}
}

// TestMinerU_rejectsPrivateHostNotInAllowlist 例外名单只放 BaseURL 那一个主机，
// 不能因为"这是内部服务"就把整个内网打开。
func TestMinerU_rejectsPrivateHostNotInAllowlist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"results":{"a":{"md_content":"x"}}}`)
	}))
	defer srv.Close()

	// 把目标伪造成另一个未被放行的内网地址：应在 SSRF 层被拦，
	// 且错误链路上不应出现"已回退本地"
	eng, err := NewMinerU(MinerUConfig{
		BaseURL: "http://10.255.255.1:9",
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = eng.ParsePDF(context.Background(), []byte("x"), "a.pdf")
	if err == nil {
		t.Fatal("未列入例外名单的内网地址应被拒")
	}
}

func TestNewMinerU_validatesConfig(t *testing.T) {
	for name, cfg := range map[string]MinerUConfig{
		"空 BaseURL": {},
		"非法协议":      {BaseURL: "ftp://mineru.internal"},
		"缺 host":    {BaseURL: "http://"},
		"无法解析的URL":  {BaseURL: "://bad"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewMinerU(cfg); err == nil {
				t.Fatal("应拒绝该配置而不是带着它启动")
			} else if !errors.Is(err, ErrParseFailed) {
				t.Errorf("应报 ErrParseFailed, got %v", err)
			}
		})
	}
}

// TestLocalPDFErrorMentionsMinerU 报错文案要给出下一步动作，
// 否则运维只会看到"解析失败"却不知道是缺 OCR 能力。
func TestLocalPDFErrorMentionsMinerU(t *testing.T) {
	// 一个结构合法但没有任何可提取文本的 PDF 触发该分支很困难，
	// 这里直接断言本地引擎对非法 PDF 的报错同样带出提示方向。
	_, err := testParse("pdf", []byte("%PDF-1.4 这不是真的 PDF 内容"))
	if err == nil {
		t.Fatal("应报错")
	}
	if !errors.Is(err, ErrParseFailed) {
		t.Errorf("应报 ErrParseFailed, got %v", err)
	}
}
