// Package fetch —— 带 SSRF 防护 + 大小限制的 HTTP 拉取。
//
// 为什么不直接 http.Get：
//   - http.Get 不做 SSRF 校验，攻击者可以让服务端访问内网
//   - http.Get 无大小限制，攻击者可以让服务端下载数 GB 文件吃爆内存
//   - 校验 URL 和真正发请求是两次动作，中间存在 DNS 重绑定窗口
//
// 关于第三点的实现要点（这里曾经踩过坑，注释也写错过）：
//
//	http.Transport 的 DialContext 收到的 addr 是 **URL 里的原始 host**，
//	不是解析后的 IP —— 解析发生在 net.Dialer 内部。所以「dial 时用
//	net.ParseIP(host) 判断内网」这种写法，对域名目标恒为 no-op：
//	看着有第二层防护，实际一层都没有。
//
//	正确做法是自己解析、校验，再用校验过的那个 IP 去 connect
//	（resolve-and-pin）。URL 仍保留原 host，HTTPS 的 SNI 与 Host 头不受影响。
package fetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/aegis/pkg/ssrf"
)

// ErrTooLarge 下载大小超限
var ErrTooLarge = errors.New("fetch: 文件超过最大限制")

// ErrBlocked SSRF 拦截
var ErrBlocked = ssrf.ErrBlocked

// DefaultUserAgent 通用出站请求 UA
const DefaultUserAgent = "Aegis-Fetcher/1.0"

// RAGUserAgent 知识库 URL 摄取用的 UA（保持与历史行为一致，便于服务端审计区分）
const RAGUserAgent = "Aegis-RAG-Fetcher/1.0"

// Options 拉取参数
type Options struct {
	Timeout      time.Duration
	MaxSizeBytes int64
	SSRF         ssrf.Policy
	// UserAgent 为空时使用 DefaultUserAgent
	UserAgent string
	// Headers 附加请求头；Content-Type 仍由是否有请求体自动决定
	Headers map[string]string
}

// Default 默认选项：15s 超时 + 20MB 上限 + 生产级 SSRF 策略
var Default = Options{
	Timeout:      15 * time.Second,
	MaxSizeBytes: 20 * 1024 * 1024,
	SSRF:         ssrf.Default,
}

func (o Options) normalize() Options {
	if o.Timeout <= 0 {
		o.Timeout = 15 * time.Second
	}
	if o.MaxSizeBytes <= 0 {
		o.MaxSizeBytes = 20 * 1024 * 1024
	}
	if len(o.SSRF.AllowedSchemes) == 0 {
		o.SSRF.AllowedSchemes = []string{"http", "https"}
	}
	if o.UserAgent == "" {
		o.UserAgent = DefaultUserAgent
	}
	return o
}

// NewClient 构造带 SSRF 防护的 http.Client。
//
// 三层防护，缺一层等于没做：
//  1. 发请求前 ssrf.Validate（协议白名单 + IP 字面量判定 + 多次解析比对）
//  2. DialContext 内 resolve-and-pin —— 连的就是刚校验过的 IP，
//     重绑定来不及在两次解析之间塞进内网地址
//  3. CheckRedirect 对每一跳重跑第 1 层（302 指向 169.254.169.254 是最经典的绕过）
//
// 返回的是全新 client，不触碰 http.DefaultClient / http.DefaultTransport。
// 高频调用方应在初始化时构造一次并复用：每次新建 Transport 会让
// 空闲连接与 keep-alive 全部作废。
// dialContextFunc 返回 resolve-and-pin 的 DialContext。
//
// 单独抽出来是为了可单测：这条路径的正确性不依赖能连上外网，
// 直接喂一个「解析到回环地址的域名」就能断言。
// 历史版本把它内联并写成 net.ParseIP(host)，对域名恒为 no-op。
func dialContextFunc(p ssrf.Policy, dialer *net.Dialer, resolver *net.Resolver, timeout time.Duration) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		dial := func(ip string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, net.JoinHostPort(ip, port))
		}

		// 例外主机名单：显式可信目标（如 host.docker.internal）原样连接
		if ssrf.HostExempted(host, p.AllowHosts) {
			return dial(host)
		}

		// IP 字面量：校验后直连
		if ip := net.ParseIP(host); ip != nil {
			if err := ssrf.CheckIP(ip, p); err != nil {
				return nil, err
			}
			return dial(ip.String())
		}

		// 域名：本层做唯一一次权威解析
		rctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		ips, err := resolver.LookupIPAddr(rctx, host)
		if err != nil {
			return nil, fmt.Errorf("%w: 解析 %s 失败: %v", ssrf.ErrBlocked, host, err)
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("%w: %s 未解析到任何 IP", ssrf.ErrBlocked, host)
		}
		// 任一答案命中内网就整体拒绝，而不是「挑个公网的凑」。
		// 同时返回公网+内网正是重绑定的特征，跳过内网那个反而给
		// 攻击者下一次解析「只剩内网」的机会。
		for _, a := range ips {
			if err := ssrf.CheckIP(a.IP, p); err != nil {
				return nil, fmt.Errorf("%w (host=%s)", err, host)
			}
		}
		// 按网络族挑选，避免拿 v6 地址去连 tcp4
		forceV4 := strings.HasSuffix(network, "tcp4")
		forceV6 := strings.HasSuffix(network, "tcp6")
		for _, a := range ips {
			isV4 := a.IP.To4() != nil
			if (forceV4 && !isV4) || (forceV6 && isV4) {
				continue
			}
			return dial(a.IP.String())
		}
		return dial(ips[0].IP.String())
	}
}

func NewClient(opt Options) *http.Client {
	opt = opt.normalize()

	dialer := &net.Dialer{Timeout: opt.Timeout, KeepAlive: 30 * time.Second}
	// PreferGo：走纯 Go 解析器。cgo 路径下 getaddrinfo 会再做一次独立解析，
	// 那正好绕过我们下面校验过的结果。
	resolver := &net.Resolver{PreferGo: true}

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialContextFunc(opt.SSRF, dialer, resolver, opt.Timeout),
		TLSHandshakeTimeout:   opt.Timeout,
		ResponseHeaderTimeout: opt.Timeout,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
	}

	return &http.Client{
		Timeout:   opt.Timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("fetch: 重定向次数过多")
			}
			// 每一跳都重新过完整的第 1 层校验
			if _, err := ssrf.Validate(req.Context(), req.URL.String(), opt.SSRF); err != nil {
				return err
			}
			return nil
		},
	}
}

// Do 发起一次受防护的请求并返回响应体（上限 MaxSizeBytes）。
//
// GET/HEAD 自动省略请求体；有 body 时补 Content-Type: application/json。
// 每次调用新建 client —— 低频场景够用；高频请改用 DoWith 复用 client。
// 非 2xx 视为错误；若需要拿到错误响应体（如工具执行要把上游报错回喂给 LLM），
// 用 DoStatus。
func Do(ctx context.Context, method, rawURL string, body []byte, opt Options) ([]byte, error) {
	return DoWith(ctx, NewClient(opt), method, rawURL, body, opt)
}

// Response 一次请求的结果。非 2xx 不视为 error，由调用方按 StatusCode 判断。
type Response struct {
	StatusCode int
	Body       []byte
}

// DoStatus 同 DoWith，但非 2xx 也正常返回（带上游响应体）。
//
// 单独开这个入口是为了别再复制一遍 dial/重定向/限流逻辑：
// 「把上游错误体交给调用方」是工具执行的真实需求 —— LLM 看到
// 400 的具体原因才有机会改参数重试，只给一个 "上游返回 400" 等于
// 让模型盲猜。
func DoStatus(ctx context.Context, client *http.Client, method, rawURL string, body []byte, opt Options) (*Response, error) {
	if client == nil {
		return nil, errors.New("fetch: client 为 nil")
	}
	opt = opt.normalize()

	method = strings.ToUpper(strings.TrimSpace(method))
	if method == "" {
		method = http.MethodGet
	}
	if (method == http.MethodGet || method == http.MethodHead) && len(body) > 0 {
		body = nil
	}

	// 1) 前置校验
	if _, err := ssrf.Validate(ctx, rawURL, opt.SSRF); err != nil {
		return nil, err
	}

	reqCtx, cancel := context.WithTimeout(ctx, opt.Timeout)
	defer cancel()

	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, rawURL, rdr)
	if err != nil {
		return nil, fmt.Errorf("fetch: 构造请求失败: %v", err)
	}
	req.Header.Set("User-Agent", opt.UserAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range opt.Headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		// 用 %w 保留错误链：调用方靠 errors.Is(err, ErrBlocked) 区分
		// 「被 SSRF 拦了」（应回 403）和「上游连不上」（应回 502）。
		return nil, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	if cl := resp.ContentLength; cl > 0 && cl > opt.MaxSizeBytes {
		return nil, fmt.Errorf("%w: %d > %d", ErrTooLarge, cl, opt.MaxSizeBytes)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, opt.MaxSizeBytes+1))
	if err != nil {
		return nil, fmt.Errorf("fetch: 读取失败: %v", err)
	}
	if int64(len(data)) > opt.MaxSizeBytes {
		return nil, fmt.Errorf("%w: %d > %d", ErrTooLarge, len(data), opt.MaxSizeBytes)
	}
	return &Response{StatusCode: resp.StatusCode, Body: data}, nil
}

// DoWith 同 Do，但复用调用方持有的 client（必须是 NewClient 出来的，
// 否则第 2、3 层防护不存在）。
func DoWith(ctx context.Context, client *http.Client, method, rawURL string, body []byte, opt Options) ([]byte, error) {
	resp, err := DoStatus(ctx, client, method, rawURL, body, opt)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch: 上游返回 %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// Fetch 安全拉取远程内容（GET 便捷封装）。
func Fetch(ctx context.Context, rawURL string, opt Options) ([]byte, error) {
	if opt.UserAgent == "" {
		opt.UserAgent = RAGUserAgent
	}
	return Do(ctx, http.MethodGet, rawURL, nil, opt)
}
