// Package ssrf —— SSRF（Server-Side Request Forgery）防护。
//
// 核心风险：
//
//	攻击者提交 file_url=http://127.0.0.1:8886/internal/admin/delete
//	让 rag_service 作为"可信内部服务"去请求，绕过鉴权。
//
// 防护策略（分层）：
//  1. 协议白名单：只允许 http/https
//  2. IP 字面量拒绝：host 直接是 IP 时不允许（DNS 绕过的前提）
//  3. DNS 解析后二次校验：解析出的 IP 命中私有网段 → 拒绝
//  4. DNS 重绑定防护：并发多次解析比对（同一 host 若同时返回公网+内网 IP，拒绝）
package ssrf

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ErrBlocked SSRF 拦截错误
var ErrBlocked = errors.New("ssrf: URL 被拦截")

// ErrInvalidURL URL 格式或协议非法
var ErrInvalidURL = errors.New("ssrf: URL 不合法")

// Policy SSRF 防护策略
type Policy struct {
	// AllowPrivateIPs 是否允许内网 IP（生产必须 false）
	AllowPrivateIPs bool
	// AllowedSchemes 允许的协议（默认 http/https）
	AllowedSchemes []string
	// DNSRecheckCount DNS 重新解析次数（检测 DNS 重绑定）
	// <=1 表示只查一次；>=2 时并发多次解析，任一返回内网 IP 就拒绝
	DNSRecheckCount int
}

// Default 默认策略：生产级 SSRF 防护
var Default = Policy{
	AllowPrivateIPs: false,
	AllowedSchemes:  []string{"http", "https"},
	DNSRecheckCount: 3,
}

// Validate 校验 URL 是否符合 SSRF 防护策略。
//
// 返回的 host 是归一化后的 host（不含端口），供调用方做缓存 key 等。
// 校验流程：
//
//	Parse → 协议白名单 → host 校验 → IP 字面量拒绝（若不允许）→ DNS 解析 → 内网网段拒绝
func Validate(ctx context.Context, raw string, p Policy) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("%w: URL 为空", ErrInvalidURL)
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}

	// 0) 协议白名单 —— 必须在 host 检查之前：
	//    file:///etc/passwd 这类 URL 没有 host（Host 为空），
	//    若先查 host 会把「协议非法」误报成「缺少 host」，
	//    错误类别错误 → 上层映射 502 而非 403。
	if !schemeAllowed(u.Scheme, p.AllowedSchemes) {
		return "", fmt.Errorf("%w: 协议 %s 不在白名单", ErrBlocked, u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("%w: 缺少 host", ErrInvalidURL)
	}

	// 2) IP 字面量拒绝
	host := u.Hostname()
	if host == "" {
		return "", fmt.Errorf("%w: 无法提取 host", ErrInvalidURL)
	}
	if ip := net.ParseIP(host); ip != nil {
		if !p.AllowPrivateIPs && IsPrivateIP(ip) {
			return "", fmt.Errorf("%w: 直接使用内网 IP %s", ErrBlocked, host)
		}
		if p.AllowPrivateIPs {
			// 显式信任内网目标：IP 字面量直接放行，跳过 DNS 环节
			return host, nil
		}
		// 公网 IP 字面量也不允许（避免用户绕过 DNS 解析的防护）
		return "", fmt.Errorf("%w: 禁止使用 IP 字面量，请用域名", ErrBlocked)
	}

	// 3) DNS 解析 + 内网网段检测 + 重绑定检测
	ips, err := resolveWithRetry(ctx, host, p.DNSRecheckCount)
	if err != nil {
		return "", fmt.Errorf("%w: DNS 解析失败: %v", ErrBlocked, err)
	}
	if len(ips) == 0 {
		return "", fmt.Errorf("%w: DNS 未返回任何 IP", ErrBlocked)
	}

	for _, ip := range ips {
		if !p.AllowPrivateIPs && IsPrivateIP(ip) {
			return "", fmt.Errorf("%w: 域名 %s 解析到内网 IP %s", ErrBlocked, host, ip)
		}
	}

	return host, nil
}

func schemeAllowed(s string, allow []string) bool {
	if len(allow) == 0 {
		allow = []string{"http", "https"}
	}
	s = strings.ToLower(s)
	for _, a := range allow {
		if strings.EqualFold(a, s) {
			return true
		}
	}
	return false
}

// IsPrivateIP 判断是否为内网/回环/链路本地/保留地址。
// 导出供 Dial 时二次校验使用。
func IsPrivateIP(ip net.IP) bool {
	// IPv4
	if ip4 := ip.To4(); ip4 != nil {
		switch {
		case ip4[0] == 10: // 10.0.0.0/8
			return true
		case ip4[0] == 127: // 127.0.0.0/8 loopback
			return true
		case ip4[0] == 169 && ip4[1] == 254: // 169.254.0.0/16 link-local
			return true
		case ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31: // 172.16.0.0/12
			return true
		case ip4[0] == 192 && ip4[1] == 168: // 192.168.0.0/16
			return true
		case ip4[0] == 0: // 0.0.0.0/8
			return true
		case ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127: // 100.64.0.0/10 CGN
			return true
		}
		return false
	}
	// IPv6
	if ip.To16() != nil {
		if ip.IsLoopback() {
			return true
		}
		if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			return true
		}
		if strings.HasPrefix(ip.String(), "fc") || strings.HasPrefix(ip.String(), "fd") { // ULA
			return true
		}
	}
	return false
}

// resolveWithRetry 多次并发 DNS 解析，检测 DNS 重绑定。
//
// DNS 重绑定攻击：攻击者控制恶意 DNS 服务器，第一次返回公网 IP（通过
// 首轮 SSRF 校验），后续请求返回内网 IP。nslookup 并发几次不同结果时
// 直接拒绝。
func resolveWithRetry(ctx context.Context, host string, n int) ([]net.IP, error) {
	if n < 1 {
		n = 1
	}
	if n > 10 {
		n = 10 // 上限防滥用
	}

	resolver := &net.Resolver{PreferGo: true} // 纯 Go 解析器，不走系统 resolver
	results := make([][]net.IP, n)
	errs := make([]error, n)

	// 并发解析
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			addrs, err := resolver.LookupIPAddr(rctx, host)
			if err != nil {
				errs[idx] = err
				return
			}
			for _, a := range addrs {
				results[idx] = append(results[idx], a.IP)
			}
		}(i)
		// 错开一点时间窗口：DNS 重绑定通常在几秒后变
		time.Sleep(time.Duration(i) * 100 * time.Millisecond)
	}
	wg.Wait()

	// 收集所有错误
	var firstErr error
	for i, err := range errs {
		if err != nil && firstErr == nil {
			firstErr = err
		}
		_ = i
	}

	// 合并所有结果（去重）
	seen := make(map[string]struct{})
	var all []net.IP
	for _, ips := range results {
		for _, ip := range ips {
			k := ip.String()
			if _, ok := seen[k]; !ok {
				seen[k] = struct{}{}
				all = append(all, ip)
			}
		}
	}

	if len(all) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return all, nil
}
