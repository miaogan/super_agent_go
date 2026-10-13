// Package parser 文档正文提取：把各种格式转成供切片/向量化使用的纯文本。
//
// 设计：
//   - 统一入口 Parse(ctx, ft, raw) / ParseWith(ctx, ft, raw, pdfEngine)
//   - 纯文本类格式做**真实抽取**而不是透传：HTML 剥标签、CSV 按表头展开、
//     Markdown 规整空白。直接透传会把 `<div class="...">` 这类标记喂进
//     向量库，既污染 embedding 又让命中片段满是噪音。
//   - 二进制类格式委托给对应子解析器
//   - PDF 通过 PDFEngine 接口解耦：本地解析器或远程 MinerU 服务
//   - 解析失败明确返回 error，不静默降级
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
	"encoding/csv"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/aegis/pkg/logger"
	"github.com/ledongthuc/pdf"
	"github.com/xuri/excelize/v2"
	"golang.org/x/net/html"
)

// ErrUnsupportedFormat 文件类型不支持
var ErrUnsupportedFormat = errors.New("parser: 不支持的文件类型")

// ErrParseFailed 解析过程出错（可能是文件损坏、密码保护、格式不规范）
var ErrParseFailed = errors.New("parser: 解析失败")

// PDFEngine PDF 正文提取的可替换实现。
//
// 存在这个接口的原因：扫描件 / 图片型 PDF 用纯 Go 的文本抽取（ledongthuc/pdf）
// 拿不到任何内容，必须交给带版面分析与 OCR 的服务（本项目走 MinerU）。
// 本地实现见 localPDFEngine，远程实现见 mineru.go。
type PDFEngine interface {
	ParsePDF(ctx context.Context, raw []byte, filename string) (string, error)
}

// Parse 用默认引擎解析（PDF 走本地纯文本抽取）。
func Parse(ctx context.Context, ft string, raw []byte) (string, error) {
	return ParseWith(ctx, ft, raw, localPDFEngine{})
}

// ParseWith 用指定 PDF 引擎解析（不带文件名）。
func ParseWith(ctx context.Context, ft string, raw []byte, pdfEngine PDFEngine) (string, error) {
	return ParseFile(ctx, ft, "", raw, pdfEngine)
}

// ParseFile 用指定 PDF 引擎解析，并把原始文件名透传给引擎。
//
// filename 只有远程引擎用得上（MinerU 的响应按文件名组织、日志也需要
// 可定位的标识）；本地各格式解析不读它。
//
// raw 为空一律报错，不返回空文本 ——
// 空文本会被下游当成"解析成功但无内容"，掩盖上传失败。
func ParseFile(ctx context.Context, ft, filename string, raw []byte, pdfEngine PDFEngine) (string, error) {
	ft = normalizeType(ft)
	if pdfEngine == nil {
		pdfEngine = localPDFEngine{}
	}
	if len(raw) == 0 {
		return "", fmt.Errorf("%w: 文件为空", ErrParseFailed)
	}

	switch ft {
	// ---- 需要结构抽取的文本类 ----
	case "html", "htm":
		return parseHTML(raw)
	case "csv":
		return parseCSV(raw)
	// ---- 纯文本：规整空白后直出 ----
	case "txt", "md", "markdown", "log", "json", "xml", "yaml", "yml":
		return normalizeText(string(raw)), nil

	// ---- 二进制：需要专门解析 ----
	case "pdf":
		return pdfEngine.ParsePDF(ctx, raw, filename)
	case "docx":
		return parseDOCX(raw)
	case "xlsx":
		return parseXLSX(raw)
	case "pptx":
		return parsePPTX(raw)

	// ---- 明确不支持 ----
	// doc/xls/ppt 是 OLE 复合文档（非 zip），纯 Go 解析成本高且不可靠；
	// 图片走 OCR 属 MinerU 的能力范围，这里不假装支持。
	// ★ 新增这些格式时请同步 configs 的 rag.allowed_types，
	//   否则文件能上传但解析必失败，用户看到的只是"解析失败"。
	case "doc", "xls", "ppt", "zip", "rar", "7z", "png", "jpg", "jpeg", "gif", "bmp", "webp":
		return "", fmt.Errorf("%w: %s", ErrUnsupportedFormat, ft)

	default:
		// 未知类型：尝试当 UTF-8 文本；
		// 如果含大量 NUL 字节（典型二进制特征）则报错而不是静默降级
		if isBinary(raw) {
			return "", fmt.Errorf("%w: %s", ErrUnsupportedFormat, ft)
		}
		return normalizeText(string(raw)), nil
	}
}

func normalizeType(ft string) string {
	ft = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(ft, ".")))
	if ft == "" {
		return "txt"
	}
	return strings.TrimPrefix(ft, ".")
}

// normalizeText 规整空白：统一换行符、去掉行尾空格、连续空行压到最多一个。
//
// 只动空白，不动内容 —— 分块与检索需要保留原始行文。
func normalizeText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, ln := range lines {
		ln = strings.TrimRight(ln, " \t")
		if ln == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, ln)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
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
	if n == 0 {
		return false
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

// ---- HTML ----

// htmlSkip 这些元素的子树整体丢弃：内容与正文无关，且会严重污染 embedding。
//   - script/style/noscript：代码与样式
//   - head：meta 与标题不是正文
//   - svg/canvas/video/audio/source/track/img：非文本载体
//   - iframe：跨文档内容，其文本不在本文件里
var htmlSkip = map[string]bool{
	"script": true, "style": true, "noscript": true, "head": true,
	"svg": true, "canvas": true, "video": true, "audio": true,
	"source": true, "track": true, "iframe": true, "template": true,
}

// htmlBlock 这些标签结束时补一个换行，保住段落与列表的可读边界。
var htmlBlock = map[string]bool{
	"p": true, "div": true, "br": true, "li": true, "tr": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"blockquote": true, "pre": true, "section": true, "article": true,
	"table": true, "ul": true, "ol": true, "dl": true, "dt": true, "dd": true,
	"header": true, "footer": true, "aside": true, "nav": true, "main": true,
	"figure": true, "figcaption": true, "hr": true,
}

// parseHTML 抽取 HTML 可见文本。
//
// 用 x/net/html 的 tokenizer 而不是正则剥标签：正则处理不了
// `<a href=">">` 这类属性里含尖括号、以及注释里的伪标签。
func parseHTML(raw []byte) (string, error) {
	z := html.NewTokenizer(bytes.NewReader(raw))
	var sb strings.Builder
	// skipDepth 记录当前处于几个被跳过元素的子树内：
	// 同名元素可嵌套（<template><template>），必须计数而非布尔。
	skip := map[string]int{}
	skipping := 0

	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			if err := z.Err(); err != io.EOF {
				logParseErr("html", len(raw), err)
				return "", fmt.Errorf("%w: %v", ErrParseFailed, err)
			}
			break
		}
		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			name, _ := z.TagName()
			n := string(name)
			if htmlSkip[n] {
				skipping++
				skip[n]++
				// 自闭合标签没有对应的 EndTag，立即出栈
				if tt == html.SelfClosingTagToken {
					skipping--
					skip[n]--
				}
				continue
			}
			if n == "br" || n == "hr" {
				sb.WriteByte('\n')
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			n := string(name)
			if c, ok := skip[n]; ok && c > 0 {
				skipping--
				skip[n] = c - 1
				continue
			}
			if htmlBlock[n] {
				sb.WriteByte('\n')
			}
		case html.TextToken:
			if skipping > 0 {
				continue
			}
			text := string(z.Text())
			if strings.TrimSpace(text) == "" {
				// 保留词间空格的语义，但不为纯空白单独建行
				if sb.Len() > 0 && strings.ContainsAny(text, " \t") &&
					!strings.HasSuffix(sb.String(), " ") &&
					!strings.HasSuffix(sb.String(), "\n") {
					sb.WriteByte(' ')
				}
				continue
			}
			sb.WriteString(collapseSpaces(text))
		}
	}

	out := normalizeText(sb.String())
	if out == "" {
		return "", fmt.Errorf("%w: HTML 无可见文本", ErrParseFailed)
	}
	return out, nil
}

// collapseSpaces 把文本节点内部的连续空白压成单个空格。
// HTML 渲染时本就如此，照原样入库会让检索命中片段满是缩进噪音。
func collapseSpaces(s string) string {
	var b strings.Builder
	inSpace := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f' {
			inSpace = true
			continue
		}
		if inSpace {
			b.WriteByte(' ')
			inSpace = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ---- CSV ----

// parseCSV 把表格展开为 "表头: 值" 形式的行。
//
// 为什么不透传：CSV 第一行是列名，后续行只有位置没有语义。
// 直接入库后"北京 2023 1200"这种切片无法回答"这是什么指标"，
// 展开成"城市: 北京\n年份: 2023\n销售额: 1200"才可检索。
func parseCSV(raw []byte) (string, error) {
	r := csv.NewReader(bytes.NewReader(raw))
	r.FieldsPerRecord = -1 // 允许列数不一致：真实导出文件常有缺列
	r.LazyQuotes = true

	header, err := r.Read()
	if err == io.EOF {
		return "", fmt.Errorf("%w: CSV 无内容", ErrParseFailed)
	}
	if err != nil {
		logParseErr("csv", len(raw), err)
		return "", fmt.Errorf("%w: %v", ErrParseFailed, err)
	}
	header = trimAll(header)

	var sb strings.Builder
	rows := 0
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			// 单行解析失败不丢整个文件
			logParseErr("csv.row", len(raw), err)
			continue
		}
		if isBlankRow(rec) {
			continue
		}
		rows++
		sb.WriteString(fmt.Sprintf("第 %d 行\n", rows))
		for i, v := range trimAll(rec) {
			if v == "" {
				continue
			}
			col := "列" + strconv.Itoa(i+1)
			if i < len(header) && header[i] != "" {
				col = header[i]
			}
			sb.WriteString(col)
			sb.WriteString(": ")
			sb.WriteString(v)
			sb.WriteByte('\n')
		}
		sb.WriteByte('\n')
	}
	if rows == 0 {
		return "", fmt.Errorf("%w: CSV 无数据行", ErrParseFailed)
	}
	return normalizeText(sb.String()), nil
}

func trimAll(s []string) []string {
	out := make([]string, len(s))
	for i, v := range s {
		out[i] = strings.TrimSpace(v)
	}
	return out
}

func isBlankRow(rec []string) bool {
	for _, v := range rec {
		if strings.TrimSpace(v) != "" {
			return false
		}
	}
	return true
}

// ---- PDF（本地兜底实现）----

type localPDFEngine struct{}

// ParsePDF 纯 Go 文本抽取。
//
// ★ 局限：只读 PDF 内的文本对象，**没有 OCR**。扫描件/图片型 PDF
//
//	会走到"无可提取文本"分支。需要覆盖这类文件请配置 MinerU
//	（见 mineru.go 与 rag.pdf_parser=mineru）。
func (localPDFEngine) ParsePDF(_ context.Context, raw []byte, _ string) (string, error) {
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
	out := normalizeText(sb.String())
	if out == "" {
		return "", fmt.Errorf("%w: PDF 无可提取文本（可能是扫描件/图片，需配置 rag.pdf_parser=mineru）", ErrParseFailed)
	}
	return out, nil
}

// ---- DOCX ----

func parseDOCX(raw []byte) (string, error) {
	// ★ 不用 nguyenthenguyen/docx 的 GetContent()：
	//   它把 word/document.xml 的**原始 XML** 当内容返回
	//   （含 <?xml?> 声明与 <w:p>/<w:r> 标签，见该库 wordDocToString）。
	//   这里自己读 zip 并提取 <w:t> 文本，保证入库的是纯文本。
	docXML, err := zipEntry(raw, "word/document.xml")
	if errors.Is(err, zip.ErrFormat) || err == errZipEntryNotFound {
		logParseErr("docx", len(raw), err)
		return "", fmt.Errorf("%w: DOCX 缺少 word/document.xml", ErrParseFailed)
	}
	if err != nil {
		logParseErr("docx", len(raw), err)
		return "", fmt.Errorf("%w: %v", ErrParseFailed, err)
	}

	// 段落结束标签补换行：<w:t> 文本本身是无分隔的相邻片段，
	// 不补的话整篇文档会被压成一坨。
	var sb strings.Builder
	if err := extractOOXMLText(docXML, "t", "\n", &sb); err != nil {
		logParseErr("docx", len(raw), err)
		return "", fmt.Errorf("%w: %v", ErrParseFailed, err)
	}

	out := normalizeText(sb.String())
	if out == "" {
		return "", fmt.Errorf("%w: DOCX 无可提取文本", ErrParseFailed)
	}
	return out, nil
}

// ---- PPTX ----

// parsePPTX 抽取每页幻灯片的文本框内容。
//
// 与 DOCX 同属 OOXML：zip 包内是 XML。正文在 ppt/slides/slideN.xml 的
// <a:t> 元素里。必须按**页码数值**排序 —— 目录是字典序的话
// slide10 会排在 slide2 之前，正文顺序直接错乱。
//
// 备注页（notesSlides）不抽取：那是演讲者提示，通常与正文重复或无关。
func parsePPTX(raw []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		logParseErr("pptx", len(raw), err)
		return "", fmt.Errorf("%w: %v", ErrParseFailed, err)
	}

	type slide struct {
		num  int
		data []byte
	}
	var slides []slide
	for _, f := range zr.File {
		name := f.Name
		if !strings.HasPrefix(name, "ppt/slides/slide") || !strings.HasSuffix(name, ".xml") {
			continue
		}
		numStr := strings.TrimSuffix(strings.TrimPrefix(name, "ppt/slides/slide"), ".xml")
		num, err := strconv.Atoi(numStr)
		if err != nil {
			continue // 形如 slide10.xml.rels 之类，非幻灯片本体
		}
		rc, err := f.Open()
		if err != nil {
			logParseErr("pptx.slide.open", len(raw), err)
			continue
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			logParseErr("pptx.slide.read", len(raw), err)
			continue
		}
		slides = append(slides, slide{num: num, data: data})
	}
	if len(slides) == 0 {
		return "", fmt.Errorf("%w: PPTX 没有可解析的幻灯片", ErrParseFailed)
	}
	sort.Slice(slides, func(i, j int) bool { return slides[i].num < slides[j].num })

	var sb strings.Builder
	for _, s := range slides {
		sb.WriteString(fmt.Sprintf("【第 %d 页】\n", s.num))
		var body strings.Builder
		if err := extractOOXMLText(s.data, "t", "\n", &body); err != nil {
			logParseErr("pptx.slide", len(raw), err)
			continue // 单页坏不影响其余页
		}
		text := normalizeText(body.String())
		if text == "" {
			text = "（本页无文本内容）"
		}
		sb.WriteString(text)
		sb.WriteString("\n\n")
	}

	out := normalizeText(sb.String())
	if out == "" {
		return "", fmt.Errorf("%w: PPTX 无可提取文本", ErrParseFailed)
	}
	return out, nil
}

// ---- XLSX ----

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
	out := normalizeText(sb.String())
	if out == "" {
		return "", fmt.Errorf("%w: XLSX 无可提取数据", ErrParseFailed)
	}
	return out, nil
}

// ---- OOXML 文本抽取辅助 ----

var errZipEntryNotFound = errors.New("zip entry not found")

// zipEntry 从 zip 包中取指定条目的字节。
func zipEntry(raw []byte, want string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		if f.Name == want {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
	}
	return nil, errZipEntryNotFound
}

// extractOOXMLText 从 OOXML 片段里取出目标标签的文本，每段后补 suffix。
//
// DOCX 取 <w:t>（local="t"）、PPTX 取 <a:t>（local="t"）：两者 Local 名相同，
// 命名空间前缀不同，而 xml.Decoder 的 Name.Local 已是去前缀的本地名，
// 所以一个函数即可覆盖。
func extractOOXMLText(data []byte, local, suffix string, sb *strings.Builder) error {
	dec := xml.NewDecoder(bytes.NewReader(data))
	inText := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == local {
				inText = true
			}
		case xml.CharData:
			if inText {
				sb.Write(t)
				sb.WriteString(suffix)
			}
		case xml.EndElement:
			if t.Name.Local == local {
				inText = false
			}
		}
	}
}
