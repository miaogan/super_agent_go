package ssrf

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

func TestValidate_scheme(t *testing.T) {
	// http/https 是允许的协议：即使后续 DNS/IP 校验失败（如本机无外网 DNS），
	// 错误也不应来自「协议白名单」。
	for _, u := range []string{"http://example.com/a", "https://example.com/a"} {
		_, err := Validate(context.Background(), u, Default)
		if err != nil && strings.Contains(err.Error(), "协议") {
			t.Fatalf("%s 的 http/https scheme 应被允许: %v", u, err)
		}
	}
	for _, u := range []string{"file:///etc/passwd", "gopher://evil.com", "ftp://files.com/x", "dict://127.0.0.1"} {
		_, err := Validate(context.Background(), u, Default)
		if err == nil {
			t.Fatalf("%s should be blocked", u)
		}
		if !errors.Is(err, ErrBlocked) {
			t.Fatalf("%s 协议拦截应映射 ErrBlocked，实际 %v", u, err)
		}
	}
}

func TestValidate_ipLiteralRejected(t *testing.T) {
	// 即使是公网 IP 字面量也拒绝（防 DNS 绕过）
	_, err := Validate(context.Background(), "http://1.1.1.1/pub", Default)
	if err == nil {
		t.Fatal("IP 字面量应该被拒绝")
	}
}

func TestValidate_loopbackBlocked(t *testing.T) {
	_, err := Validate(context.Background(), "http://127.0.0.1/internal", Default)
	if err == nil {
		t.Fatal("loopback 应被拦截")
	}
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("应为 ErrBlocked，实际 %v", err)
	}
}

func TestValidate_fileSchemeBlocked(t *testing.T) {
	// file:///etc/passwd 无 host，应归为协议拦截（ErrBlocked → 403），
	// 而非「缺少 host」（ErrInvalidURL → 502）
	_, err := Validate(context.Background(), "file:///etc/passwd", Default)
	if err == nil {
		t.Fatal("file:// 应被拦截")
	}
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("应为 ErrBlocked，实际 %v", err)
	}
}

func TestValidate_localhostBlocked(t *testing.T) {
	// localhost 解析到 127.0.0.1，DNS 检查应该拦掉
	_, err := Validate(context.Background(), "http://localhost:8886/internal", Default)
	if err == nil {
		t.Fatal("localhost 解析到 loopback，应被拦截")
	}
}

func TestValidate_allowPrivateWhenConfigured(t *testing.T) {
	p := Default
	p.AllowPrivateIPs = true
	_, err := Validate(context.Background(), "http://127.0.0.1/x", p)
	if err != nil {
		t.Fatalf("AllowPrivateIPs=true 时不应拦截: %v", err)
	}
}

func TestIsPrivateIP(t *testing.T) {
	cases := []struct {
		ip   string
		priv bool
	}{
		{"10.0.0.1", true},
		{"127.0.0.1", true},
		{"172.16.0.1", true},
		{"172.31.255.255", true},
		{"172.32.0.0", false}, // 超出 16-31
		{"192.168.1.1", true},
		{"169.254.1.1", true},
		{"169.254.169.254", true}, // 云厂商元数据地址
		{"0.0.0.0", true},
		{"1.1.1.1", false},
		{"8.8.8.8", false},
		// RFC 6890 保留/特殊用途段：旧实现逐字节判断时全部漏网
		{"198.18.0.50", true},     // 基准测试
		{"198.19.255.255", true},  // 基准测试上界
		{"192.0.2.1", true},       // TEST-NET-1
		{"198.51.100.1", true},    // TEST-NET-2
		{"203.0.113.1", true},     // TEST-NET-3
		{"192.0.0.1", true},       // IETF 协议分配
		{"100.64.0.1", true},      // CGNAT
		{"100.127.255.255", true}, // CGNAT 上界
		{"100.128.0.0", false},    // 超出 CGNAT /10
		{"224.0.0.1", true},       // 组播
		{"239.255.255.255", true}, // 组播上界
		{"240.0.0.1", true},       // 保留
		{"255.255.255.255", true}, // 受限广播
		// IPv6
		{"::1", true},
		{"fe80::1", true},
		{"fd12::1", true},       // ULA
		{"fc00::1", true},       // ULA
		{"ff02::1", true},       // 组播
		{"2001:db8::1", true},   // 文档
		{"2606:4700::1", false}, // 真实 v6 公网（Cloudflare）
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("invalid ip: %s", c.ip)
		}
		got := IsPrivateIP(ip)
		if got != c.priv {
			t.Errorf("%s: got=%v want=%v", c.ip, got, c.priv)
		}
	}
}

func TestHostExempted(t *testing.T) {
	allow := []string{"host.docker.internal", "*.svc.internal", " 127.0.0.1 "}
	cases := []struct {
		host string
		want bool
	}{
		{"host.docker.internal", true},   // 精确
		{"HOST.Docker.INTERNAL", true},   // 大小写不敏感
		{"a.svc.internal", true},         // 后缀
		{"svc.internal", true},           // 后缀含 apex
		{"127.0.0.1", true},              // 允许表带空格应被 trim
		{"xhost.docker.internal", false}, // 不能前缀粘连命中
		{"evilsvc.internal", false},      // 不能无点前缀粘连命中
		{"a.b.svc.internal", true},       // 多级子域
		{"svc.internal.evil.com", false}, // 后缀伪装
		{"localhost", false},
		{"", false},
	}
	for _, c := range cases {
		if got := HostExempted(c.host, allow); got != c.want {
			t.Errorf("HostExempted(%q)=%v want=%v", c.host, got, c.want)
		}
	}
	// 空名单不应放行任何 host
	if HostExempted("a.svc.internal", nil) {
		t.Error("空例外名单不应放行")
	}
}
