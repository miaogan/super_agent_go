// Package serverx 提供各服务共用的启动骨架。
//
// 目标：六个服务的 main.go 只写"装配"，不写重复的样板。
//
// 统一负责：
//   - 配置加载 + 日志初始化
//   - mock 模式判定（冒烟测试的关键开关）
//   - Nacos 可选初始化与注册
//   - 健康检查 HTTP 端点（K8s probe / 网关探活）
//   - Kitex server 构造与优雅退出
//
// 设计取舍：
//   - 健康检查同时暴露 HTTP（给 K8s / Docker）与 Kitex Health RPC（给网关聚合）
//   - mock 模式不因缺少外部依赖而启动失败，这是冒烟测试能跑通的前提
package serverx

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cloudwego/kitex/pkg/registry"
	"github.com/cloudwego/kitex/server"

	"github.com/aegis/pkg/conf"
	"github.com/aegis/pkg/identity"
	"github.com/aegis/pkg/internalauth"
	"github.com/aegis/pkg/jsonx"
	"github.com/aegis/pkg/logger"
	"github.com/aegis/pkg/metrics"
	"github.com/aegis/pkg/mockx"
	"github.com/aegis/pkg/tracing"
)

// 运行时信息，由构建期注入
var (
	Version   = "dev"
	GitCommit = "unknown"
)

// Options 服务启动参数
type Options struct {
	// ServiceName 服务名，同时作为配置默认值与日志字段
	ServiceName string

	// DefaultRPCPort 未显式配置时的 RPC 端口
	DefaultRPCPort int

	// DefaultHTTPPort 未显式配置时的健康检查端口
	DefaultHTTPPort int

	// Register 注册回调，把 Kitex server 交给调用方挂载 handler
	//
	// 由各服务自行调用 xxxservice.RegisterService(svr, handler)，
	// 便于追加 Kitex server.Option（如限流、拦截器）而不破坏本包抽象。
	//
	// app 提供配置、mock 故障注入器与 logger，
	// 使 handler 装配完全依赖运行期配置而非包级变量。
	//
	// 返回的 cleanup 会在优雅退出阶段被调用（若为 nil 则忽略）。
	// 用于释放 Provider 连接、连接池等需显式关闭的资源。
	Register func(svr server.Server, app *App) (cleanup func(context.Context) error, err error)

	// HTTPServer 纯 HTTP 服务的启动回调（仅 HTTPOnly 模式调用）
	//
	// gateway 不是 RPC 服务：不暴露也不消费 Kitex 接口，
	// 强行起一个空Kitex server 只会占用端口并在日志里制造
	// "我提供 RPC 能力"的错觉。此时由本回调启动 Hertz，
	// 生命周期（启动失败、优雅退出、信号等待）仍由本包统一负责。
	//
	// 返回的 shutdown 在退出阶段被调用，应执行 Hertz 的 Shutdown。
	HTTPServer func(app *App) (shutdown func(context.Context) error, err error)

	// HTTPOnly 以纯 HTTP 模式启动：跳过 Nacos 注册与 Kitex server
	HTTPOnly bool

	// HealthExtra 返回额外的健康检查明细（依赖连通性等）
	HealthExtra func(ctx context.Context) map[string]string

	// OnShutdown 优雅退出前的清理钩子（关连接池等）
	OnShutdown func(ctx context.Context) error
}

// Runtime 运行时句柄
type Runtime struct {
	Cfg  *conf.Config
	Mock *mockx.FaultInjector

	startAt time.Time

	httpSrv  *http.Server
	kitexSvr server.Server

	// httpShutdown 纯 HTTP 模式的退出钩子
	httpShutdown func(context.Context) error

	// Nacos 注册相关（可选，未接入时为 nil）
	registry       NacosRegistry
	registered     bool
	registeredAddr string

	// onShutdown 由 Options 注入
	onShutdown func(context.Context) error
	// registerCleanup 由 Register 回调返回
	registerCleanup func(context.Context) error

	// tracerShutdown otel provider 的优雅关闭（冲刷未导出 span）
	tracerShutdown func(context.Context) error
}

// Start 完成全部启动流程并阻塞直到收到退出信号。
//
// 返回 error 表示启动失败（调用方应 os.Exit(1)）。
func Start(opt Options) error {
	cfg, err := loadConfig(opt)
	if err != nil {
		return err
	}

	logger.Init(logger.Options{
		Service: cfg.Service.Name,
		Level:   cfg.Log.Level,
		JSON:    cfg.Log.JSON,
	})

	rt := &Runtime{
		Cfg:        cfg,
		Mock:       mockx.NewFaultInjector(cfg.Mock.Enabled, cfg.Mock.LatencyMS, cfg.Mock.FailTargets),
		startAt:    time.Now(),
		onShutdown: opt.OnShutdown,
	}

	ctx := logger.WithTraceID(context.Background(), fmt.Sprintf("boot-%d", time.Now().UnixNano()%100000))

	// ★ JSON 引擎自检必须在 banner 之前：
	//   引擎不符预期时直接拒绝启动，日志里先给出原因，
	// 避免出现"服务起来了但跑的是错的引擎"这种最难查的状态。
	if err := checkJSONEngine(ctx, cfg); err != nil {
		return err
	}

	// 启动横幅：把关键模式说清楚，避免排查时误判
	rt.banner(ctx, opt)

	// 0) 追踪初始化：必须在 Kitex server 构建之前 ——
	//    server 中间件要取到非 noop 的 Tracer 才能产生 span
	tracerShutdown, err := tracing.Init(ctx, cfg.Service.Name, cfg.Service.Version, tracing.Config{
		Enabled:       cfg.OTel.Enabled,
		Endpoint:      cfg.OTel.Endpoint,
		SamplerRatio:  cfg.OTel.SamplerRatio,
		ExportTimeout: 10 * time.Second,
	})
	if err != nil {
		// 接收端不可用不拖垮业务：追踪降级为 noop（进程内不产生 span）
		logger.Warn(ctx, "otel 初始化失败，追踪降级为 noop", "err", err)
		tracerShutdown = func(context.Context) error { return nil }
	}
	rt.tracerShutdown = tracerShutdown

	// 纯 HTTP 模式：不加载健康检查端口、不碰 Nacos、不起 Kitex
	if opt.HTTPOnly {
		return rt.startHTTPOnly(ctx, opt)
	}

	// 1) 健康检查 HTTP（先起，保证 K8s probe 尽早可用）
	if err := rt.startHealthHTTP(opt); err != nil {
		return err
	}

	// 2) Nacos（可选）
	if cfg.Nacos.Enabled {
		if err := rt.initNacos(ctx); err != nil {
			// Nacos 不可用不应拖垮服务：注册失败只告警
			logger.Warn(ctx, "nacos 初始化失败，降级为本地直连模式", "err", err)
		}
	}

	// 3) Kitex server
	kitexSvr, err := rt.buildKitexServer(opt)
	if err != nil {
		return err
	}
	rt.kitexSvr = kitexSvr

	if err := rt.startRPC(ctx); err != nil {
		return err
	}

	// 4) 阻塞等待退出信号
	rt.waitForSignal(ctx)

	// 5) 优雅退出
	return rt.shutdown(context.Background())
}

// startHTTPOnly 纯 HTTP 服务的启动流程。
//
// gateway 自己的 Hertz 已经把 /healthz、/readyz 都实现了，
// 所以这里不再额外起一个健康检查端口 —— 两个端口争抢同一语义
// 只会让 K8s probe 配置产生歧义。
func (rt *Runtime) startHTTPOnly(ctx context.Context, opt Options) error {
	if opt.HTTPServer == nil {
		return errors.New("HTTPOnly 模式必须提供 HTTPServer 回调")
	}
	app := &App{Cfg: rt.Cfg, Mock: rt.Mock, Log: logger.With("component", "boot")}
	shutdown, err := opt.HTTPServer(app)
	if err != nil {
		return fmt.Errorf("HTTP server 启动失败: %w", err)
	}
	rt.httpShutdown = shutdown

	rt.waitForSignal(ctx)
	return rt.shutdown(context.Background())
}

func (rt *Runtime) banner(ctx context.Context, opt Options) {
	fields := []any{"version", rt.Cfg.Service.Version}
	if opt.HTTPOnly {
		// 纯 HTTP 模式的监听地址由服务的 HTTPServer 回调自行决定
		//（gateway 用 gateway.addr 而非 server.http_addr），
		// 这里不打印 server.http_addr 以免误导。
		fields = append(fields, "mode", "http")
	} else {
		fields = append(fields, "rpc", rt.rpcAddr(),
			"http", rt.Cfg.Server.HTTPAddr, "nacos", rt.Cfg.Nacos.Enabled)
	}
	if rt.Cfg.Mock.Enabled {
		fields = append(fields,
			"mock", true,
			"fail_targets", rt.Cfg.Mock.FailTargets,
		)
	}
	// 把 JSON 引擎并入启动横幅：一眼确认跑在哪个引擎上
	if info := jsonx.Info(); rt.Cfg.JSON.PrintEngine {
		fields = append(fields, info.LogFields()...)
	}
	logger.Info(ctx, "service starting", fields...)
}

// ---------- JSON 引擎自检 ----------

// checkJSONEngine 校验「配置期望的引擎」与「实际引擎」是否一致。
//
// # 为什么需要它
//
// Sonic 是构建期通过 -tags sonic 编译进去的，不是运行期开关。
// 于是存在一类不会报错、只会静默降级的错误：
//
//	构建命令漏了 -tags sonic  →  以为在用 Sonic，实际跑标准库
//	镜像构建在 arm64 上      →  Sonic 自动回落，行为与预期不符
//
// 代码没报错、功能全部正常，只有性能悄悄退化了 2-3 倍 ——
// 这种问题在压测时才暴露，且很难联想到「引擎没生效」。
//
// # 策略
//
//   - PrintEngine=false：完全不打印（省不了一行日志的性能，但会丧失排查能力，故默认开）
//   - Strict=false（默认）：不符时打 WARN 并继续 —— 本地开发与冒烟友好
//   - Strict=true（生产）：不符时拒绝启动 —— 配置错误不该拖到压测才暴露
func checkJSONEngine(ctx context.Context, cfg *conf.Config) error {
	info := jsonx.Info()

	want := strings.ToLower(strings.TrimSpace(cfg.JSON.Engine))
	if want == "" {
		want = conf.JSONEngineAuto
	}

	actual := info.Engine
	isSonic := actual == jsonx.EngineSonic

	// 不符预期的具体原因，用于日志与错误信息
	var mismatch string
	switch want {
	case conf.JSONEngineAuto:
		// 不校验
	case conf.JSONEngineSonic:
		if !isSonic {
			mismatch = fmt.Sprintf(
				"配置要求 sonic，但当前是 %s（%s）", actual, info.FallbackReason)
		}
	case conf.JSONEngineStd:
		if isSonic {
			mismatch = "配置要求 std，但构建启用了 sonic（去掉 -tags sonic 重建）"
		}
	default:
		// 配置写错值不该静默忽略
		return fmt.Errorf("json.engine 取值非法: %q（可选 auto/sonic/std）", cfg.JSON.Engine)
	}

	if mismatch != "" {
		if cfg.JSON.Strict {
			return fmt.Errorf("JSON 引擎自检失败: %s", mismatch)
		}
		logger.Warn(ctx, "JSON 引擎与配置不符（strict=false，继续启动）",
			"want", want, "actual", actual, "reason", mismatch)
		return nil
	}

	// 引擎与配置一致，但都是 sonic 编译进去了却走了回落 —— 同样值得提示
	if isSonic && !info.Native {
		logger.Warn(ctx, "sonic 已编译但当前平台走回落实现，无 JIT 加速",
			"reason", info.FallbackReason, "goarch", info.GOARCH, "goos", info.GOOS)
	}
	return nil
}

func loadConfig(opt Options) (*conf.Config, error) {
	var cfgPath string
	flag.StringVar(&cfgPath, "config", "", "配置文件路径（YAML）")
	flag.Parse()

	cfg, err := conf.Load(cfgPath)
	if err != nil {
		return nil, err
	}

	if cfg.Service.Name == "" {
		cfg.Service.Name = opt.ServiceName
	}
	if cfg.Service.Version == "" {
		cfg.Service.Version = Version
	}
	if cfg.Service.Port == 0 && opt.DefaultRPCPort > 0 {
		cfg.Service.Port = opt.DefaultRPCPort
	}
	if cfg.Server.RPCAddr == "" {
		cfg.Server.RPCAddr = "0.0.0.0"
	}
	if cfg.Server.HTTPAddr == "" {
		cfg.Server.HTTPAddr = fmt.Sprintf("0.0.0.0:%d", portOr(opt.DefaultHTTPPort, 8081))
	}
	if cfg.Server.ShutdownTimeout <= 0 {
		cfg.Server.ShutdownTimeout = 10 * time.Second
	}
	return cfg, nil
}

func portOr(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

// RPCAddr 返回实际监听的 RPC 地址（host:port）
//
// 端口统一由 Service.Port 提供（rpcAddr() 会拼接），
// 因此 Server.RPCAddr 字段**只填 host 部分**。
//
//	本地开发（dev.sh）：不设 RPCAddr → 空串 → 默认 127.0.0.1 只监听本机（安全）
//	Docker 容器：      显式写 "0.0.0.0" → 保持原样，让 Compose 网络内其他容器能访问
func (rt *Runtime) rpcAddr() string {
	host := rt.Cfg.Server.RPCAddr
	if host == "" {
		// 空串 = 未配置。本地开发 default 到 127.0.0.1（最小暴露面）。
		// 容器部署必须显式写 "0.0.0.0"，compose env 已统一注入。
		host = "127.0.0.1"
	}
	return fmt.Sprintf("%s:%d", host, rt.Cfg.Service.Port)
}

// App 提供给 Register 回调的装配上下文
type App struct {
	Cfg  *conf.Config
	Mock *mockx.FaultInjector
	Log  *slog.Logger
}

// MockMode 是否运行在 mock 模式
func (a *App) MockMode() bool { return a.Mock.Enabled() }

// FaultInjector 返回按名判定失败的注入器，供 handler 注入下游依赖
func (a *App) FaultInjector() *mockx.FaultInjector { return a.Mock }

func (rt *Runtime) buildKitexServer(opt Options) (server.Server, error) {
	addr, err := net.ResolveTCPAddr("tcp", rt.rpcAddr())
	if err != nil {
		return nil, fmt.Errorf("解析 RPC 地址失败: %w", err)
	}

	// 注册信息：服务名 + 权重 + 版本标签。
	// 生产接入 Nacos 后由 SDK 消费此 Info 完成注册。
	regInfo := &registry.Info{
		ServiceName: rt.Cfg.Service.Name,
		Addr:        addr,
		Weight:      rt.Cfg.Nacos.Weight,
		Tags: map[string]string{
			"version":    rt.Cfg.Service.Version,
			"git_commit": GitCommit,
			"mock":       fmt.Sprintf("%t", rt.Cfg.Mock.Enabled),
		},
	}

	opts := []server.Option{
		server.WithServiceAddr(addr),
		server.WithRegistryInfo(regInfo),
		server.WithExitWaitTime(rt.Cfg.Server.ShutdownTimeout),
	}
	// 追踪中间件注册在最外层：先提取上游 traceparent 再进内层逻辑，
	// 让 internalauth / 业务 handler 的报错也落在 span 覆盖范围内。
	opts = append(opts, server.WithMiddleware(tracing.ServerMiddleware(rt.Cfg.Service.Name)))
	opts = append(opts, server.WithMiddleware(metrics.KitexServerMiddleware(rt.Cfg.Service.Name)))
	// Identity 透传：从 req.Identity 注入 ctx，让 handler 里 identity.From(ctx) 始终有效。
	// 必须在 internalauth 之后（认证中间件可能需要身份信息做审计）。
	opts = append(opts, server.WithMiddleware(identity.ServerMiddleware()))
	// 服务间内部认证：token 非空时拒绝一切未携带合法令牌的 RPC。
	// 认证关闭（token 为空）是本地 mock 的合法状态，但必须留痕告警。
	opts = append(opts, internalauth.ServerOptions(rt.Cfg.Internal.Token)...)
	if !internalauth.Enabled(rt.Cfg.Internal.Token) {
		internalauth.WarnIfDisabled(context.Background(), rt.Cfg.Internal.Token, rt.Cfg.Service.Name)
	}

	svr := server.NewServer(opts...)

	if opt.Register != nil {
		app := &App{Cfg: rt.Cfg, Mock: rt.Mock, Log: logger.With("component", "boot")}
		cleanup, err := opt.Register(svr, app)
		if err != nil {
			return nil, fmt.Errorf("注册 handler 失败: %w", err)
		}
		rt.registerCleanup = cleanup
	}
	return svr, nil
}

func (rt *Runtime) startRPC(ctx context.Context) error {
	if err := rt.kitexSvr.Run(); err != nil {
		return fmt.Errorf("RPC server 启动失败: %w", err)
	}
	logger.Info(ctx, "rpc server started", "addr", rt.rpcAddr())
	return nil
}

// ---------- 健康检查 ----------

func (rt *Runtime) startHealthHTTP(opt Options) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", rt.handleHealthz)
	mux.HandleFunc("/livez", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})
	// Prometheus 抓取端点：与探针同端口，内部网络可达即可。
	// 指标含请求基数与服务拓扑信息，禁止绑定公网。
	if rt.Cfg.Metrics.Enabled {
		path := rt.Cfg.Metrics.Path
		if path == "" {
			path = "/metrics"
		}
		mux.Handle(path, metrics.Handler())
	}

	ln, err := net.Listen("tcp", rt.Cfg.Server.HTTPAddr)
	if err != nil {
		return fmt.Errorf("健康检查端口监听失败 %s: %w", rt.Cfg.Server.HTTPAddr, err)
	}
	rt.httpSrv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := rt.httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error(context.Background(), "health http server exited", "err", err)
		}
	}()
	logger.Info(context.Background(), "health http server started", "addr", ln.Addr().String())
	return nil
}

// HealthSnapshot 健康快照，同时用于 HTTP 输出与 Kitex Health RPC
func (rt *Runtime) HealthSnapshot(ctx context.Context) HealthInfo {
	info := HealthInfo{
		Healthy:   true,
		Service:   rt.Cfg.Service.Name,
		Version:   rt.Cfg.Service.Version,
		GitCommit: GitCommit,
		UptimeSec: uint64(time.Since(rt.startAt).Seconds()),
		MockMode:  rt.Cfg.Mock.Enabled,
		Extra:     map[string]string{},
	}
	if rt.Cfg.Nacos.Enabled {
		info.Extra["nacos"] = "enabled"
	} else {
		info.Extra["nacos"] = "disabled"
	}
	return info
}

// HealthInfo 健康信息快照
type HealthInfo struct {
	Healthy   bool
	Service   string
	Version   string
	GitCommit string
	UptimeSec uint64
	MockMode  bool
	Extra     map[string]string
}

func (rt *Runtime) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	info := rt.HealthSnapshot(ctx)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if !info.Healthy {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	// 手工序列化：健康端点不应因编码库问题而 500
	fmt.Fprintf(w, `{"healthy":%t,"service":%q,"version":%q,"git_commit":%q,"uptime_sec":%d,"mock_mode":%t}`,
		info.Healthy, info.Service, info.Version, info.GitCommit, info.UptimeSec, info.MockMode)
}

// ---------- 退出 ----------

func (rt *Runtime) waitForSignal(ctx context.Context) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	logger.Info(ctx, "shutdown signal received", "signal", sig.String())
}

func (rt *Runtime) shutdown(ctx context.Context) error {
	timeout := rt.Cfg.Server.ShutdownTimeout

	if rt.httpSrv != nil {
		sctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		if err := rt.httpSrv.Shutdown(sctx); err != nil {
			logger.Error(ctx, "health http shutdown error", "err", err)
		}
	}

	// 纯 HTTP 服务：先停 Hertz（停止接收新连接），
	// 再执行下方 cleanup 关闭下游客户端 —— 顺序反了会让在途请求
	// 拿到已关闭的连接池并报错。
	if rt.httpShutdown != nil {
		sctx, cancel := context.WithTimeout(ctx, timeout)
		if err := rt.httpShutdown(sctx); err != nil {
			logger.Error(ctx, "http server shutdown error", "err", err)
		}
		cancel()
	}

	if rt.kitexSvr != nil {
		if err := rt.kitexSvr.Stop(); err != nil {
			logger.Error(ctx, "kitex server stop error", "err", err)
		}
	}

	// 优雅退出：先冲刷追踪数据（此时 RPC 已停、不再有新 span），
	// 再做业务清理，最后注销服务。
	tracerFlush := []func(context.Context) error{rt.tracerShutdown}
	for _, fn := range tracerFlush {
		if fn == nil {
			continue
		}
		sctx, cancel := context.WithTimeout(ctx, timeout)
		if err := fn(sctx); err != nil {
			logger.Error(ctx, "tracer shutdown error", "err", err)
		}
		cancel()
	}

	// 释放业务资源：先停 RPC（不再有新请求），再关连接
	shutdownFns := []func(context.Context) error{rt.registerCleanup, rt.onShutdown}
	for _, fn := range shutdownFns {
		if fn == nil {
			continue
		}
		sctx, cancel := context.WithTimeout(ctx, timeout)
		if err := fn(sctx); err != nil {
			logger.Error(ctx, "cleanup error", "err", err)
		}
		cancel()
	}

	// 退出前主动注销，避免等 Nacos 心跳超时导致流量仍被路由过来
	rt.Deregister(ctx)

	logger.Info(ctx, "service stopped")
	return nil
}

// ---------- Nacos ----------

// NacosRegistry 抽象 Nacos 客户端，避免 pkg 直接依赖具体 SDK 造成耦合。
//
// ★ 当前状态：接口与本文件内的注册/注销流程已就绪，但**没有任何实现**，
//
//	也没有调用方——六个服务的 cmd/main.go 从未调用 SetRegistry，
//	因此 initNacos 恒走 registry==nil 分支，打印
//	"nacos enabled but registry not wired yet, running in direct mode"，
//	实际全部经 WithHostPorts 直连。
//	这里刻意不引入 SDK 依赖：接入时只需提供一个实现 + 在 main 里注入。
type NacosRegistry interface {
	Register(ctx context.Context, serviceName, ip string, port int, meta map[string]string) error
	Deregister(ctx context.Context, serviceName, ip string, port int) error
}

// SetRegistry 注入 Nacos 注册实现（在 Run 之前调用）
func (rt *Runtime) SetRegistry(r NacosRegistry) {
	rt.registry = r
}

func (rt *Runtime) initNacos(ctx context.Context) error {
	if rt.registry == nil {
		// 未接入 SDK：仅记录意图，不阻塞启动
		logger.Info(ctx, "nacos enabled but registry not wired yet, running in direct mode")
		return nil
	}
	host, portStr, err := net.SplitHostPort(rt.rpcAddr())
	if err != nil {
		return err
	}
	var port int
	_, _ = fmt.Sscanf(portStr, "%d", &port)

	meta := map[string]string{
		"version": rt.Cfg.Service.Version,
		"mock":    fmt.Sprintf("%t", rt.Cfg.Mock.Enabled),
	}
	if err := rt.registry.Register(ctx, rt.Cfg.Service.Name, host, port, meta); err != nil {
		return err
	}
	rt.registered = true
	rt.registeredAddr = fmt.Sprintf("%s:%d", host, port)
	logger.Info(ctx, "registered to nacos", "service", rt.Cfg.Service.Name, "addr", rt.registeredAddr)
	return nil
}

// Deregister 主动注销（优雅退出时调用，避免等待 Nacos 心跳超时）
func (rt *Runtime) Deregister(ctx context.Context) {
	if rt.registry == nil || !rt.registered {
		return
	}
	host, portStr, err := net.SplitHostPort(rt.registeredAddr)
	if err != nil {
		return
	}
	var port int
	_, _ = fmt.Sscanf(portStr, "%d", &port)
	if err := rt.registry.Deregister(ctx, rt.Cfg.Service.Name, host, port); err != nil {
		logger.Warn(ctx, "deregister from nacos failed", "err", err)
		return
	}
	logger.Info(ctx, "deregistered from nacos", "service", rt.Cfg.Service.Name)
}
