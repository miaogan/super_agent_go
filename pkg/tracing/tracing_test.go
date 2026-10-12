package tracing

import (
	"context"
	"testing"
	"time"
)

// TestInitDisabled 关闭开关时返回 noop shutdown 且不报错。
func TestInitDisabled(t *testing.T) {
	shutdown, err := Init(context.Background(), "svc", "dev", Config{Enabled: false})
	if err != nil {
		t.Fatalf("Init(disabled) 不应报错: %v", err)
	}
	if shutdown == nil {
		t.Fatal("Init(disabled) 应返回非 nil shutdown")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("noop shutdown 不应报错: %v", err)
	}
}

// TestExportTimeoutOf 导出超时边界：0 → 默认 10s，正数 → 原值。
func TestExportTimeoutOf(t *testing.T) {
	if got := exportTimeoutOf(Config{}); got != 10*time.Second {
		t.Errorf("exportTimeoutOf(0) = %v, 期望 10s", got)
	}
	if got := exportTimeoutOf(Config{ExportTimeout: 3 * time.Second}); got != 3*time.Second {
		t.Errorf("exportTimeoutOf(3s) = %v, 期望 3s", got)
	}
}
