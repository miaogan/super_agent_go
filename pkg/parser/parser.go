// Package parser 文档正文提取：把二进制格式（PDF/DOCX/XLSX）转纯文本。
//
// 设计：
//   - 统一接口 parser.Parse(ft string, raw []byte) (string, error)
//   - 纯文本类型（txt/md/json/html/csv）零依赖直出
//   - 二进制类型委托给对应子解析器
//   - 解析失败明确返回 error，不静默降级（调用方看到 ErrParseFailed
//     后可以选择标记文档状态或提示用户重新上传）
//
// 为什么不把解析逻辑塞到 ingest 包里：
//
//	pkg/parser 是纯粹的"字节→文本"变换，与 RAG 流水线的切片/向量化/
//	入库完全解耦；未来别的服务（如 agent 把附件喂进 prompt）也能复用。
package parser

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aegis/pkg/logger"
	"github.com/ledongthuc/pdf"
	"github.com/xuri/excelize/v2"
)

// ErrUnsupportedFormat 文件类型不支持
var ErrUnsupportedFormat = errors.New("parser: 不支持的文件类型")

// ErrParseFailed 解析过程出错（可能是文件损坏、密码保护、格式不规范）
var ErrParseFailed = errors.New("parser: 解析失败")

// Parse 按文件类型提取正文。
//
// ft 是归一化后的扩展名（小写无点，如 "pdf" / "docx"），
// raw 是文件原始字节。返回的文本已经去掉首尾空白。
func Parse(ft string, raw []byte) (string, error) {
	ft = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(ft, ".")))
	if ft == "" {
		ft = "txt"
	}
	if len(raw) == 0 {
		return "", fmt.Errorf("%w: 文件为空", ErrParseFailed)
	}

	switch ft {
	// ---- 纯文本：零开销 ----
	case "txt", "md", "markdown", "json", "html", "htm", "csv", "xml", "yaml", "yml", "log":
		return strings.TrimSpace(string(raw)), nil

	// ---- 二进制：需要专门解析 ----
	case "pdf":
		return parsePDF(raw)
	case "docx":
		return parseDOCX(raw)
	case "xlsx":
		return parseXLSX(raw)

	// ---- 明确不支持的类型（避免误把二进制当文本）----
	case "pptx", "doc", "xls", "ppt", "zip", "rar", "7z", "png", "jpg", "jpeg", "gif", "bmp", "webp":
		return "", fmt.Errorf("%w: %s", ErrUnsupportedFormat, ft)

	default:
		// 未知类型：尝试当 UTF-8 文本；
		// 如果含大量 NUL 字节（典型二进制特征）则报错而不是静默降级
		if isBinary(raw) {
			return "", fmt.Errorf("%w: %s", ErrUnsupportedFormat, ft)
		}
		return strings.TrimSpace(string(raw)), nil
	}
}

// isBinary 粗略判断字节流是否为二进制。
//
// 方法：扫描前 8KB，NUL 字节密度超过 1% 即视为二进制。
// 纯文本（包括中文 UTF-8）几乎不会出现 NUL，
// 而二进制格式（PDF/DOCX/ZIP/图片）在文件头通常就有。
func isBinary(b []byte) bool {
	const scan = 8 * 1024
	n := len(b)
	if n > scan {
		n = scan
	}
	nul := 0
	for i := 0; i < n; i++ {
		if b[i] == 0 {
			nul++
		}
	}
	return nul*100/n >= 1
}

// logParseErr 解析失败统一日志：带文件类型与大小，便于定位是哪种格式出问题
func logParseErr(ft string, size int, err error) {
	logger.Warn(context.Background(), "文档解析失败",
		"file_type", ft, "size_bytes", size, "err", err)
}

// ---- 各格式解析器 ----
//
// 每个解析器：
//   - 接收 []byte（避免临时落盘，ingest 流水线已经在内存里了）
//   - 出错返回 ErrParseFailed（包装原始错误供调用方定位）
//   - 不做任何额外格式化（调用方要的是纯文本）

func parsePDF(raw []byte) (string, error) {
	r, err := pdf.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		logParseErr("pdf", len(raw), err)
		return "", fmt.Errorf("%w: %v", ErrParseFailed, err)
	}

	var sb strings.Builder
	pages := r.NumPage()
	for i := 1; i <= pages; i++ {
		p := r.Page(i)
		if p.V.IsNull() {
			continue
		}
		text, err := p.GetPlainText(nil)
		if err != nil {
			// 单页失败不放弃整个文件：记日志跳过，其他页继续
			logParseErr("pdf.page", len(raw), err)
			continue
		}
		sb.WriteString(text)
		sb.WriteByte('\n')
	}
	out := strings.TrimSpace(sb.String())
	if out == "" {
		return "", fmt.Errorf("%w: PDF 无可提取文本（可能是扫描件/图片）", ErrParseFailed)
	}
	return out, nil
}

func parseDOCX(raw []byte) (string, error) {
	// ★ 不用 nguyenthenguyen/docx 的 GetContent()：
	//   它把 word/document.xml 的**原始 XML** 当内容返回
	//   （含 <?xml?> 声明与 <w:p>/<w:r> 标签，见该库 wordDocToString）。
	//   这里自己读 zip 并提取 <w:t> 文本，保证入库的是纯文本。
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		logParseErr("docx", len(raw), err)
		return "", fmt.Errorf("%w: %v", ErrParseFailed, err)
	}

	var docFile *zip.File
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			docFile = f
			break
		}
	}
	if docFile == nil {
		return "", fmt.Errorf("%w: 缺少 word/document.xml", ErrParseFailed)
	}

	rc, err := docFile.Open()
	if err != nil {
		logParseErr("docx", len(raw), err)
		return "", fmt.Errorf("%w: %v", ErrParseFailed, err)
	}
	defer rc.Close()

	var sb strings.Builder
	dec := xml.NewDecoder(rc)
	inText := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			logParseErr("docx", len(raw), err)
			return "", fmt.Errorf("%w: %v", ErrParseFailed, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			// document.xml 正文里 Local=="t" 的只有 <w:t>；表格 <w:tc> Local 是 tc
			if t.Name.Local == "t" {
				inText = true
			}
		case xml.CharData:
			if inText {
				sb.Write(t)
				sb.WriteByte('\n')
			}
		case xml.EndElement:
			if t.Name.Local == "t" {
				inText = false
			}
		}
	}

	out := strings.TrimSpace(sb.String())
	if out == "" {
		return "", fmt.Errorf("%w: DOCX 无可提取文本", ErrParseFailed)
	}
	return out, nil
}

func parseXLSX(raw []byte) (string, error) {
	f, err := excelize.OpenReader(bytes.NewReader(raw))
	if err != nil {
		logParseErr("xlsx", len(raw), err)
		return "", fmt.Errorf("%w: %v", ErrParseFailed, err)
	}
	defer f.Close()

	var sb strings.Builder
	for _, name := range f.GetSheetList() {
		rows, err := f.GetRows(name)
		if err != nil {
			logParseErr("xlsx.sheet", len(raw), err)
			continue
		}
		if len(rows) == 0 {
			continue
		}
		sb.WriteString("【Sheet: ")
		sb.WriteString(name)
		sb.WriteString("】\n")
		for _, row := range rows {
			sb.WriteString(strings.Join(row, "\t"))
			sb.WriteByte('\n')
		}
		sb.WriteByte('\n')
	}
	out := strings.TrimSpace(sb.String())
	if out == "" {
		return "", fmt.Errorf("%w: XLSX 无可提取数据", ErrParseFailed)
	}
	return out, nil
}
