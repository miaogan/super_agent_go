// MinerU PDF 解析引擎。
//
// 为什么需要它：pkg/parser 的本地 PDF 实现只做文本对象抽取，**没有 OCR 与
// 版面分析**。扫描件、图片型 PDF、多栏排版的论文，本地抽出来要么是空、
// 要么顺序错乱。MinerU 这类服务提供 OCR + 版面还原并直接输出 Markdown，
// 对 RAG 来说 Markdown 还额外保留了标题层级与表格结构。
//
// 契约（与 MinerU 2.x 自带的 HTTP server 一致）：
//
//	POST {base_url}/file_parse
//	multipart/form-data：files=<pdf 字节>，return_md=true
//	响应：{"results": {"<原始文件名>": {"md_content": "..."}}}
//
// ★ 不回退本地解析器：配置了 MinerU 就意味着"正文以它的输出为准"。
// 连接失败、超时、返回空正文一律显式报错。静默回退到本地会让运维以为
// 在用 MinerU，实际入库的是扫描件抽出来的空白文本 —— 这种故障在检索
// 质量上表现为"什么都搜不到"，排查成本远高于一次明确的上传失败。
package parser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aegis/pkg/fetch"
	"github.com/aegis/pkg/ssrf"
)

// MinerUConfig MinerU 服务接入参数。
type MinerUConfig struct {
	// BaseURL 服务根地址，如 http://mineru.internal:8000（不含 /file_parse）
	BaseURL string
	// Timeout 单次解析超时。版面分析很慢，默认 120s。
	Timeout time.Duration
	// MaxResponseBytes 响应体上限，默认 32MB。
	MaxResponseBytes int64
	// AuthToken 可选：非空时以 Authorization: Bearer 发送。
	AuthToken string
	// ExtraAllowHosts 追加到 SSRF 例外名单的主机（一般不需要，
	// BaseURL 的主机会自动放行）。
	ExtraAllowHosts []string
}

// MinerU 基于 HTTP 的 PDFEngine 实现。
type MinerU struct {
	endpoint string
	opt      fetch.Options
	token    string
	client   *http.Client
}

var _ PDFEngine = (*MinerU)(nil)

// ErrMinerUEmpty MinerU 正常返回但没有正文
var ErrMinerUEmpty = errors.New("parser: MinerU 未返回正文")

// mineruResponse 只声明需要的字段：MinerU 的 results 值里还有
// images/content_list 等大字段，整体反序列化既慢又易因结构变动失败。
type mineruResponse struct {
	Results map[string]struct {
		MdContent string `json:"md_content"`
	} `json:"results"`
}

// NewMinerU 构造引擎。配置非法直接返回 error，交由调用方在启动阶段失败。
func NewMinerU(cfg MinerUConfig) (*MinerU, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, fmt.Errorf("%w: MinerU BaseURL 未配置", ErrParseFailed)
	}
	u, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	if err != nil {
		return nil, fmt.Errorf("%w: MinerU BaseURL 非法: %v", ErrParseFailed, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%w: MinerU BaseURL 协议必须为 http/https", ErrParseFailed)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("%w: MinerU BaseURL 缺少 host", ErrParseFailed)
	}

	if cfg.Timeout <= 0 {
		cfg.Timeout = 120 * time.Second
	}
	if cfg.MaxResponseBytes <= 0 {
		cfg.MaxResponseBytes = 32 << 20 // 32MB
	}

	// MinerU 部署在内网是常态，SSRF 策略默认会拦。
	// 这里把 BaseURL 的主机显式加入例外名单：它是运维自己写进配置的
	// 目标，等同于"我明确信任它"，比把 AllowPrivateIPs 整体打开安全得多。
	hosts := append([]string{u.Hostname()}, cfg.ExtraAllowHosts...)

	opt := fetch.Options{
		Timeout:      cfg.Timeout,
		MaxSizeBytes: cfg.MaxResponseBytes,
		SSRF: ssrf.Policy{
			AllowPrivateIPs: false,
			AllowedSchemes:  []string{"http", "https"},
			DNSRecheckCount: 1, // dial 层已 resolve-and-pin，多次解析只会拖慢
			AllowHosts:      hosts,
		},
		UserAgent: "Aegis-MinerU-Client/1.0",
	}

	return &MinerU{
		endpoint: strings.TrimRight(cfg.BaseURL, "/") + "/file_parse",
		opt:      opt,
		token:    cfg.AuthToken,
		client:   fetch.NewClient(opt),
	}, nil
}

// ParsePDF 上传 PDF 并取回 Markdown 正文。
//
// filename 仅用于日志与错误信息；结果按"results 里唯一条目"取，
// 不依赖服务端把键名回显成我们上传时的文件名。
func (m *MinerU) ParsePDF(ctx context.Context, raw []byte, filename string) (string, error) {
	if len(raw) == 0 {
		return "", fmt.Errorf("%w: PDF 为空", ErrParseFailed)
	}

	body, boundary, err := buildMultipart(raw, filename)
	if err != nil {
		return "", fmt.Errorf("%w: 构造 MinerU 请求失败: %v", ErrParseFailed, err)
	}

	opt := m.opt
	// Content-Type 必须在 Options 里给：fetch.DoStatus 先按"有请求体"
	// 设成 application/json，再用 Headers 覆盖，这里正是靠顺序覆盖生效。
	opt.Headers = map[string]string{
		"Content-Type": "multipart/form-data; boundary=" + boundary,
	}
	if m.token != "" {
		opt.Headers["Authorization"] = "Bearer " + m.token
	}

	data, err := fetch.DoWith(ctx, m.client, http.MethodPost, m.endpoint, body, opt)
	if err != nil {
		logParseErr("pdf.mineru", len(raw), err)
		return "", fmt.Errorf("%w: 调用 MinerU 失败: %v", ErrParseFailed, err)
	}

	var resp mineruResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		logParseErr("pdf.mineru.decode", len(raw), err)
		return "", fmt.Errorf("%w: MinerU 响应无法解析: %v", ErrParseFailed, err)
	}
	if len(resp.Results) == 0 {
		return "", fmt.Errorf("%w: 响应里没有 results 字段（文件名=%s）", ErrMinerUEmpty, filename)
	}
	if len(resp.Results) > 1 {
		// 只传了一个文件却返回多条：服务端行为与契约不符，
		// 随便挑一条会把别的文档内容混进来，宁可报错。
		return "", fmt.Errorf("%w: 期望 1 条结果，实际 %d 条", ErrMinerUEmpty, len(resp.Results))
	}
	var md string
	for _, v := range resp.Results {
		md = v.MdContent
	}

	out := normalizeText(md)
	if out == "" {
		return "", fmt.Errorf("%w: MinerU 返回的 md_content 为空（扫描件未识别出文字？文件名=%s）",
			ErrMinerUEmpty, filename)
	}
	return out, nil
}

// buildMultipart 组装 file_parse 的请求体。
//
// 字段名用 files（MinerU 的 HTTP server 收的是复数字段）；
// return_md=true 必须带 —— 不带时响应里只有 content_list 而没有 md_content。
func buildMultipart(raw []byte, filename string) ([]byte, string, error) {
	if strings.TrimSpace(filename) == "" {
		filename = "document.pdf"
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("return_md", "true"); err != nil {
		return nil, "", err
	}
	part, err := w.CreateFormFile("files", filename)
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(raw); err != nil {
		return nil, "", err
	}
	// Close 会写入结束分界，必须在取 buf.Bytes() 之前完成。
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), w.Boundary(), nil
}
