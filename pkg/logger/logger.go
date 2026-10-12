// Package logger 提供结构化日志。
//
// 选型说明：
//   - 使用标准库 log/slog（Go 1.21+），不引入第三方日志库，降低依赖面
//   - 生产输出 JSON，便于采集；本地开发输出文本，便于阅读
//   - 支持从 context 提取 trace_id / uid，实现链路串联
package logger

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
)

// ctxKey 用于在 context 中存取日志字段，避免与业务 key 冲突。
type ctxKey string

const (
	keyTraceID ctxKey = "trace_id"
	keyUID     ctxKey = "uid"
	keyTenant  ctxKey = "tenant"
	keyService ctxKey = "service"
)

// config 包级配置，初始化后只读
type config struct {
	service string
	level   atomic.Int32
	json    atomic.Bool
	base    *slog.Logger
}

var cfg = &config{}

// Options 初始化参数
type Options struct {
	Service string // 服务名，会附加到每条日志
	Level   string // debug / info / warn / error
	JSON    bool   // true=JSON 输出，false=文本输出
}

// Init 初始化全局日志器。应在服务启动最早阶段调用。
func Init(opt Options) {
	cfg.service = opt.Service
	cfg.level.Store(int32(parseLevel(opt.Level)))
	cfg.json.Store(opt.JSON)

	var h slog.Handler
	hopts := &slog.HandlerOptions{
		Level: levelVar{},
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			// 统一时间字段名，便于日志平台解析
			if a.Key == slog.TimeKey && len(groups) == 0 {
				a.Key = "ts"
			}
			return a
		},
	}
	if opt.JSON {
		h = slog.NewJSONHandler(os.Stdout, hopts)
	} else {
		h = slog.NewTextHandler(os.Stdout, hopts)
	}
	cfg.base = slog.New(h).With(slog.String("service", opt.Service))
	slog.SetDefault(cfg.base)
}

// levelVar 让日志级别可在运行时动态变化（配合 Nacos 热更新）。
type levelVar struct{}

func (levelVar) Level() slog.Level {
	return slog.Level(cfg.level.Load())
}

func (levelVar) Enabled(_ context.Context, l slog.Level) bool {
	return l >= slog.Level(cfg.level.Load())
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// SetLevel 运行时调整日志级别（供配置热更新调用）。
func SetLevel(level string) {
	cfg.level.Store(int32(parseLevel(level)))
}

// ---------- 带上下文字段 ----------

// WithTraceID 把 trace_id 写入 context
func WithTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, keyTraceID, traceID)
}

// WithIdentity 把用户身份写入 context（便于审计日志）
func WithIdentity(ctx context.Context, uid int64, tenant string) context.Context {
	ctx = context.WithValue(ctx, keyUID, uid)
	return context.WithValue(ctx, keyTenant, tenant)
}

// TraceIDFrom 从 context 读取 trace_id
func TraceIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(keyTraceID).(string); ok {
		return v
	}
	return ""
}

// fieldsFrom 提取 context 中的公共字段
func fieldsFrom(ctx context.Context) []any {
	var out []any
	if ctx == nil {
		return out
	}
	if v := TraceIDFrom(ctx); v != "" {
		out = append(out, slog.String("trace_id", v))
	}
	if v, ok := ctx.Value(keyUID).(int64); ok && v != 0 {
		out = append(out, slog.Int64("uid", v))
	}
	if v, ok := ctx.Value(keyTenant).(string); ok && v != "" {
		out = append(out, slog.String("tenant", v))
	}
	return out
}

func base() *slog.Logger {
	if cfg.base == nil {
		// 未初始化时给出可用的兜底，避免 panic
		Init(Options{Service: "unknown", Level: "info"})
	}
	return cfg.base
}

// ---------- 包级日志函数 ----------

func Debug(ctx context.Context, msg string, args ...any) {
	if !cfg.base.Enabled(ctx, slog.LevelDebug) {
		return
	}
	base().DebugContext(ctx, msg, append(fieldsFrom(ctx), args...)...)
}

func Info(ctx context.Context, msg string, args ...any) {
	base().InfoContext(ctx, msg, append(fieldsFrom(ctx), args...)...)
}

func Warn(ctx context.Context, msg string, args ...any) {
	base().WarnContext(ctx, msg, append(fieldsFrom(ctx), args...)...)
}

func Error(ctx context.Context, msg string, args ...any) {
	base().ErrorContext(ctx, msg, append(fieldsFrom(ctx), args...)...)
}

// With 返回附加了固定字段的 Logger，适合模块级 logger
func With(args ...any) *slog.Logger {
	return base().With(args...)
}
