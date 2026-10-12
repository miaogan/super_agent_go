package kitexx

import (
	"strings"
	"testing"
	"time"
)

// IP 字面量不做 DNS 查询，立即返回
func TestWaitHostPort_ipLiteral(t *testing.T) {
	start := time.Now()
	if err := WaitHostPort("127.0.0.1:8881", time.Second); err != nil {
		t.Fatalf("IP 地址应当立即成功: %v", err)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatalf("IP 地址不应触发等待，耗时 %v", time.Since(start))
	}
}

// 可解析的主机名（localhost）立即成功
func TestWaitHostPort_resolvable(t *testing.T) {
	if err := WaitHostPort("localhost:0", 5*time.Second); err != nil {
		t.Fatalf("localhost 应当可解析: %v", err)
	}
}

// 不可解析的主机名：超时并返回可读错误
func TestWaitHostPort_unresolvable(t *testing.T) {
	start := time.Now()
	err := WaitHostPort("no-such-host.invalid:8882", 1200*time.Millisecond)
	if err == nil {
		t.Fatal("不可解析主机名应当超时报错")
	}
	if !strings.Contains(err.Error(), "超时") {
		t.Fatalf("错误信息应说明超时，实际: %v", err)
	}
	// 至少重试过一次（>1s），且不远超上限
	if el := time.Since(start); el < time.Second || el > 3*time.Second {
		t.Fatalf("耗时异常: %v", el)
	}
}

// 缺端口的地址直接报格式错误
func TestWaitHostPort_badFormat(t *testing.T) {
	err := WaitHostPort("no-port-here", time.Second)
	if err == nil || !strings.Contains(err.Error(), "host:port") {
		t.Fatalf("应报格式错误，实际: %v", err)
	}
}
