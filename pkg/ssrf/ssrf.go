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
	// AllowHosts 主机名例外名单：命中即豁免内网 IP 拦截。
	//
	// 为什么需要它：本地/容器内联调的工具端点（如 host.docker.internal:3009、
	// 127.0.0.1:18888 的 mock server）本身就解析到内网。若没有这个口子，
	// 开发期只能把 AllowPrivateIPs 设成 true —— 那等于对整个内网（含
	// 169.254.169.254 元数据地址）开门。逐个主机放行才是生产可用的姿势。
	//
	// 匹配的是 URL 里的原始 host（大小写不敏感），不是解析后的 IP。
	// 支持 "*.suffix" 形式的域名后缀匹配。
	AllowHosts []string
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

	// 例外名单优先于内网判定：命中即视为显式可信目标。
	// 放在这里而不是最前面，是为了让协议白名单继续生效——
	// 白名单里写个 file://host 不应该因此就能走通。
	if HostExempted(host, p.AllowHosts) {
		return host, nil
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

// HostExempted 判断 host 是否命中例外名单。
//
// 匹配规则（大小写不敏感）：
//   - 精确匹配："host.docker.internal"
//   - 后缀匹配："*.example.internal" 命中 "a.example.internal" 与 "example.internal"
//
// 只匹配 URL 中的原始 host，不匹配解析后的 IP——调用方若已在 dial 层
// 拿到 IP，应改用 CheckIP。
func HostExempted(host string, allow []string) bool {
	if host == "" || len(allow) == 0 {
		return false
	}
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	for _, a := range allow {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" {
			continue
		}
		if strings.HasPrefix(a, "*.") {
			suffix := a[1:] // ".example.internal"
			if h == strings.TrimPrefix(suffix, ".") || strings.HasSuffix(h, suffix) {
				return true
			}
			continue
		}
		if h == a {
			return true
		}
	}
	return false
}

// CheckIP 按策略判断某个已解析 IP 是否可连接（供 dial 层复用同一套判定）。
func CheckIP(ip net.IP, p Policy) error {
	if p.AllowPrivateIPs {
		return nil
	}
	if IsPrivateIP(ip) {
		return fmt.Errorf("%w: 目标 IP %s 属于内网/保留网段", ErrBlocked, ip)
	}
	return nil
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

// reservedNets 不可作为出站目标的地址段（RFC 6890 特殊用途地址 + 私有网段）。
//
// 为什么不能只盯 10/8、192.168/16 这几个"内网段"：SSRF 的价值在于目标是
// 任何**不该被服务端直连**的地址，而不只是"内网"。以下都属此类：
//
//   - 0.0.0.0/8       本网络，Linux 上等价于本机
//   - 169.254.0.0/16  链路本地 —— 云厂商元数据服务 169.254.169.254 在此段内
//   - 198.18.0.0/15   基准测试保留段。某些本机 TUN/代理实现会把未知域名
//     通配解析到这一段（本机实测 evilsvc.internal → 198.18.0.50），
//     旧实现把它当公网放过，等于给 SSRF 留了个后门
//   - 192.0.2/198.51.100/203.0.113  TEST-NET 文档段
//   - 240.0.0.0/4     保留（含 255.255.255.255 受限广播）
//   - 224.0.0.0/4     组播
var reservedNets = func() []*net.IPNet {
	cidrs := []string{
		"0.0.0.0/8",
		"10.0.0.0/8",
		"100.64.0.0/10", // CGNAT
		"127.0.0.0/8",
		"169.254.0.0/16", // link-local，含云元数据
		"172.16.0.0/12",
		"192.0.0.0/24", // IETF 协议分配
		"192.0.2.0/24", // TEST-NET-1
		"192.168.0.0/16",
		"198.18.0.0/15",   // 基准测试
		"198.51.100.0/24", // TEST-NET-2
		"203.0.113.0/24",  // TEST-NET-3
		"224.0.0.0/4",     // 组播
		"240.0.0.0/4",     // 保留
		// IPv6
		"::/128",
		"::1/128",
		"64:ff9b::/98",  // NAT64
		"100::/64",      // discard-only
		"2001::/32",     // Teredo（可封装到内网）
		"2001:20::/28",  // ORCHIDv2
		"2001:db8::/32", // 文档
		"fc00::/7",      // ULA
		"fe80::/10",     // link-local
		"ff00::/8",      // 组播
	}
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		if _, n, err := net.ParseCIDR(c); err == nil {
			nets = append(nets, n)
		}
	}
	return nets
}()

// IsPrivateIP 判断是否为内网/回环/链路本地/组播/保留地址。
//
// 名字沿用历史，语义已扩为"不可直连目标"。IPv4 映射地址（::ffff:a.b.c.d）
// 先取 To4() 按 v4 判定，避免把 v4 的公网地址误当成 v6 段拦掉。
func IsPrivateIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	// 逐个保留段比对。Contains 内部会处理 v4/v6 表示差异，
	// 但仍先归一到 4 字节形式，减少歧义。
	v4 := ip.To4()
	for _, n := range reservedNets {
		if v4 != nil {
			if n.IP.To4() == nil {
				continue // v4 地址不参与纯 v6 段匹配
			}
		}
		if n.Contains(ip) {
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
