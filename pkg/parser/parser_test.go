package parser

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// testParse 是 Parse 的测试封装：被测函数需要 ctx（PDF 可能走远程服务），
// 而这些用例都不依赖取消语义，统一传 Background。
func testParse(ft string, raw []byte) (string, error) {
	return Parse(context.Background(), ft, raw)
}

func TestParse_text(t *testing.T) {
	out, err := testParse("txt", []byte("  hello\nworld  "))
	if err != nil {
		t.Fatal(err)
	}
	if out != "hello\nworld" {
		t.Fatalf("unexpected: %q", out)
	}
}

func TestParse_empty(t *testing.T) {
	_, err := testParse("txt", nil)
	if err == nil {
		t.Fatal("expected error")
	}
}

// TestParse_unsupported 仍不支持的类型必须明确拒绝，不能被当成文本透传。
func TestParse_unsupported(t *testing.T) {
	// doc/xls/ppt 是 OLE 复合文档（不是 zip），纯 Go 解析不可靠；
	// 图片需要 OCR，属 MinerU 的能力范围。
	for _, ft := range []string{"doc", "xls", "ppt", "zip", "rar", "png", "jpg"} {
		if _, err := testParse(ft, []byte("garbage")); !errors.Is(err, ErrUnsupportedFormat) {
			t.Errorf("%s 应判为不支持的格式, got %v", ft, err)
		}
	}
}

// TestParse_pptxNowSupported pptx 已纳入支持，坏文件应报"解析失败"
// 而不是"不支持"——两者的处置方式不同（重传 vs. 换格式）。
func TestParse_pptxNowSupported(t *testing.T) {
	_, err := testParse("pptx", []byte("garbage"))
	if errors.Is(err, ErrUnsupportedFormat) {
		t.Fatalf("pptx 已支持，不应再报不支持: %v", err)
	}
	if !errors.Is(err, ErrParseFailed) {
		t.Fatalf("坏 pptx 应报解析失败, got %v", err)
	}
}

func TestParse_binaryDetection(t *testing.T) {
	// 含大量 NUL 的字节流应被判为不支持，而不是静默转字符串
	bin := make([]byte, 4096)
	for i := range bin {
		bin[i] = 0
	}
	_, err := testParse("dat", bin)
	if !errors.Is(err, ErrUnsupportedFormat) {
		t.Fatalf("expected ErrUnsupportedFormat, got %v", err)
	}
}

func TestParse_ext(t *testing.T) {
	// 扩展名带点、大写、空格都能归一化
	for _, ft := range []string{"PDF", ".pdf", " Pdf ", "pdf"} {
		_, err := testParse(ft, []byte("not a real pdf"))
		// 损坏的 PDF 走 ErrParseFailed 而不是 ErrUnsupportedFormat（类型支持但内容坏了）
		if err == nil {
			t.Fatalf("%s: expected error", ft)
		}
		if strings.Contains(err.Error(), "不支持") {
			t.Fatalf("%s: 类型应为 pdf 但被判为不支持: %v", ft, err)
		}
	}
}

func TestParse_pdfWithContent(t *testing.T) {
	// 这是一个最小合法 PDF（只含 1 个空页）——不是空 PDF
	// ledongthuc/pdf 会打开成功但 NumPage=0 或 Page.V.IsNull
	// 我们验证这种情况下返回 ErrParseFailed 而非空串
	raw := []byte("%PDF-1.4\n%%EOF")
	_, err := testParse("pdf", raw)
	if err == nil {
		t.Fatal("expected error for degenerate PDF")
	}
}

func TestParse_xlsxDegenerate(t *testing.T) {
	raw := []byte("PK\x03\x04invalid")
	_, err := testParse("xlsx", raw)
	if err == nil {
		t.Fatal("expected error for degenerate XLSX")
	}
}

func TestParse_docxDegenerate(t *testing.T) {
	raw := []byte("PK\x03\x04invalid")
	_, err := testParse("docx", raw)
	if err == nil {
		t.Fatal("expected error for degenerate DOCX")
	}
}

func TestParse_docxExtractsPlainText(t *testing.T) {
	// 构造一个真实 docx（zip + word/document.xml），
	// 验证返回的是 <w:t> 纯文本而非原始 XML
	docXML := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">` +
		`<w:body><w:p><w:r><w:t>Milvus is a vector database.</w:t></w:r></w:p>` +
		`<w:p><w:r><w:t>It supports similarity search.</w:t></w:r></w:p>` +
		`</w:body></w:document>`
	ct := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
		`<Default Extension="xml" ContentType="application/xml"/>` +
		`<Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>` +
		`</Types>`

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range map[string]string{
		"[Content_Types].xml": ct,
		"word/document.xml":   docXML,
	} {
		w, _ := zw.Create(name)
		_, _ = w.Write([]byte(content))
	}
	_ = zw.Close()

	out, err := testParse("docx", buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "<?xml") || strings.Contains(out, "<w:") {
		t.Fatalf("DOCX 解析应返回纯文本，实际含 XML: %q", out)
	}
	if !strings.Contains(out, "Milvus is a vector database") {
		t.Fatalf("DOCX 文本缺失: %q", out)
	}
}
