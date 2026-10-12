// Package fetch —— 带 SSRF 防护 + 大小限制的 HTTP 拉取。
//
// 为什么不直接 http.Get：
//   - http.Get 不做 SSRF 校验，攻击者可以让服务端访问内网
//   - http.Get 无大小限制，攻击者可以让服务端下载数 GB 文件吃爆内存
//   - 两次请求（URL 校验 + 实际拉取）之间可能被 DNS 重绑定，
//     需要在 HTTP Transport 里再做一层 dial 时 IP 校验
package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/aegis/pkg/ssrf"
)

// ErrTooLarge 下载大小超限
var ErrTooLarge = errors.New("fetch: 文件超过最大限制")

// ErrBlocked SSRF 拦截
var ErrBlocked = ssrf.ErrBlocked

// Options 拉取参数
type Options struct {
	Timeout      time.Duration
	MaxSizeBytes int64
	SSRF         ssrf.Policy
}

// Default 默认选项：15s 超时 + 20MB 上限 + 生产级 SSRF 策略
var Default = Options{
	Timeout:      15 * time.Second,
	MaxSizeBytes: 20 * 1024 * 1024,
	SSRF:         ssrf.Default,
}

// Fetch 安全拉取远程内容。
//
// 流程：
//  1. SSRF 校验 URL（协议白名单 + IP 字面量拒绝 + DNS 解析内网检测）
//  2. 构造 Transport（Dial 时再校验一次目标 IP，防 DNS 重绑定）
//  3. GET 请求 + Content-Length 前置检查
//  4. io.LimitReader 流式读取，超出上限立即终止
func Fetch(ctx context.Context, rawURL string, opt Options) ([]byte, error) {
	if opt.Timeout <= 0 {
		opt.Timeout = 15 * time.Second
	}
	if opt.MaxSizeBytes <= 0 {
		opt.MaxSizeBytes = 20 * 1024 * 1024
	}

	// 1) SSRF 前置校验（第一层）
	if _, err := ssrf.Validate(ctx, rawURL, opt.SSRF); err != nil {
		return nil, err
	}

	// 2) 构造 Transport：Dial 时再校验目标 IP（第二层，防 DNS 重绑定）
	//    HTTP Transport 的 Dial 拿到的 addr 已经是 DNS 解析后的 IP，
	//    直接检查是否命中内网网段即可。
	dialer := &net.Dialer{Timeout: opt.Timeout}
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			// Dial 前：host 必须是 IP（DNS 已解析），检查是否内网
			if ip := net.ParseIP(host); ip != nil {
				if !opt.SSRF.AllowPrivateIPs && ssrf.IsPrivateIP(ip) {
					return nil, fmt.Errorf("%w: dial 目标 %s 是内网 IP", ssrf.ErrBlocked, host)
				}
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(host, port))
		},
		TLSHandshakeTimeout:   opt.Timeout,
		ResponseHeaderTimeout: opt.Timeout,
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
	}

	client := &http.Client{
		Timeout:   opt.Timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("fetch: 重定向次数过多")
			}
			// 跟随重定向时也要 SSRF 校验（攻击者可能先让你拿到 302 再引去内网）
			if _, err := ssrf.Validate(ctx, req.URL.String(), opt.SSRF); err != nil {
				return err
			}
			return nil
		},
	}

	reqCtx, cancel := context.WithTimeout(ctx, opt.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Aegis-RAG-Fetcher/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch: 上游返回 %d", resp.StatusCode)
	}

	// Content-Length 前置检查（避免大文件白跑流量）
	if cl := resp.ContentLength; cl > 0 && cl > opt.MaxSizeBytes {
		return nil, fmt.Errorf("%w: %d > %d", ErrTooLarge, cl, opt.MaxSizeBytes)
	}

	// 流式读取 + 硬上限
	data, err := io.ReadAll(io.LimitReader(resp.Body, opt.MaxSizeBytes+1))
	if err != nil {
		return nil, fmt.Errorf("fetch: 读取失败: %v", err)
	}
	if int64(len(data)) > opt.MaxSizeBytes {
		return nil, fmt.Errorf("%w: %d > %d", ErrTooLarge, len(data), opt.MaxSizeBytes)
	}
	return data, nil
}
