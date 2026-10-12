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
		{"0.0.0.0", true},
		{"1.1.1.1", false},
		{"8.8.8.8", false},
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
