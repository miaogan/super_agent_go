// Command auth_service 认证授权服务。
//
// 双通道：
//  1. 本地账号密码登录（凭据来自 user_service）
//  2. OAuth2.0 授权码 + PKCE 对接外部 IdP（SSO）
//
// 两条通道最终都签发本系统自签 JWT（RS256），下游只认 JWT，
// 不关心用户从哪条通道来 —— 这是认证层解耦的关键。
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	authservice "github.com/aegis/idl/kitex_gen/authv1/authservice"
	userservice "github.com/aegis/idl/kitex_gen/userv1/userservice"
	"github.com/cloudwego/kitex/client"
	"github.com/cloudwego/kitex/server"

	"github.com/aegis/auth_service/internal/mockidp"
	"github.com/aegis/auth_service/internal/oauth2"
	"github.com/aegis/auth_service/internal/service"
	"github.com/aegis/auth_service/internal/token"
	"github.com/aegis/pkg/conf"
	"github.com/aegis/pkg/internalauth"
	"github.com/aegis/pkg/kitexx"
	"github.com/aegis/pkg/logger"
	"github.com/aegis/pkg/metrics"
	"github.com/aegis/pkg/serverx"
	"github.com/aegis/pkg/tracing"
)

// mockIdpPort 内嵌 IdP 监听端口（与六个业务服务的健康端口不冲突）
const mockIdpPort = 8091

// mockIdpRedirectURI 内嵌 IdP 允许回调的地址
// 冒烟时由 smoke.sh 通过 curl 直接跟随后端点，因此约定为 gateway 的回调路由。
const mockIdpRedirectURI = "http://127.0.0.1:8080/api/v1/auth/callback"

func main() {
	ctx := context.Background()

	// cleanup 收集需要在退出时释放的资源（如内嵌 IdP 的 HTTP server）
	var cleanup []func(context.Context) error

	err := serverx.Start(serverx.Options{
		ServiceName:     "auth_service",
		DefaultRPCPort:  8881,
		DefaultHTTPPort: 8090,

		OnShutdown: func(ctx context.Context) error {
			// 逆序释放，保证依赖者先于被依赖者关闭
			for i := len(cleanup) - 1; i >= 0; i-- {
				if err := cleanup[i](ctx); err != nil {
					logger.Warn(ctx, "cleanup failed", "err", err)
				}
			}
			return nil
		},

		Register: func(svr server.Server, app *serverx.App) (func(context.Context) error, error) {
			c := app.Cfg

			// 1) JWT 签名器：生产走配置密钥，本地/冒烟自动生成 RS256 密钥对
			signer, err := token.LoadSigner(ctx, c.Auth, keyDir(app.MockMode()))
			if err != nil {
				return nil, fmt.Errorf("装载 JWT 签名器: %w", err)
			}

			// 2) 令牌管理器与登录失败守卫
			tokens, err := token.New(token.Options{
				Signer:     signer,
				Issuer:     c.Auth.Issuer,
				AccessTTL:  c.Auth.AccessTokenTTL,
				RefreshTTL: c.Auth.RefreshTokenTTL,
				JWTSecret:  c.Auth.JWTSecret,
			})
			if err != nil {
				return nil, fmt.Errorf("构造令牌管理器: %w", err)
			}
			guard := token.NewLoginGuard(
				c.Auth.MaxLoginFailures,
				c.Auth.LockDuration,
				c.Auth.CaptchaAfterFailures,
			)

			// 3) user_service 客户端
			users, err := newUserClient(c.Upstream.UserService, c.Internal.Token)
			if err != nil {
				return nil, fmt.Errorf("连接 user_service 失败: %w", err)
			}

			// 4) OAuth2.0：未配置外部 IdP 且处于 mock 模式时启动内嵌 IdP，
			//    使授权跳转 → code → token → userinfo → JWT 全链路可被冒烟验证
			idps := c.Auth.IdPs
			if len(idps) == 0 && app.MockMode() {
				stop, err := startMockIdP(ctx)
				if err != nil {
					logger.Warn(ctx, "内嵌 IdP 启动失败，SSO 通道将不可用", "err", err)
				} else {
					cleanup = append(cleanup, stop)
					idps = []conf.IdPConf{mockIdPConf()}
				}
			}

			authservice.RegisterService(svr, service.New(
				tokens,
				guard,
				oauth2.NewClient(10*time.Second),
				users,
				app.FaultInjector(),
				idps,
			))

			logger.Info(ctx, "auth_service ready",
				"jwt_alg", string(signer.Alg()),
				"idps", len(idps),
				"user_service", c.Upstream.UserService)
			// 内嵌 IdP 的清理函数已在 Register 内注册到 cleanup 列表
			return nil, nil
		},
	})
	if err != nil {
		logger.Error(ctx, "auth_service 启动失败", "err", err)
		os.Exit(1)
	}
}

// keyDir JWT 密钥目录。
// mock 模式用临时目录，保证冒烟环境无需预置任何密钥文件。
func keyDir(mockMode bool) string {
	if mockMode {
		return "/tmp/aegis-keys"
	}
	if v := os.Getenv("AEGIS_KEY_DIR"); v != "" {
		return v
	}
	return "/etc/aegis/keys"
}

// newUserClient 构造 user_service 客户端。
//
// 未接 Nacos 时按固定地址直连；接入 Nacos 服务发现后改为 resolver，
// 此处调用点无需改动 —— 这正是 UpstreamConf 存在的意义。
func newUserClient(addr, internalToken string) (userservice.Client, error) {
	if addr == "" {
		return nil, fmt.Errorf("upstream.user_service 未配置")
	}
	// 冷启动时下游 DNS 可能尚未注册：WithHostPorts 解析失败会把地址
	// 永久标记为 unix 网络（见 pkg/kitexx.WaitHostPort 注释）
	if err := kitexx.WaitHostPort(addr, 30*time.Second); err != nil {
		return nil, err
	}
	opts := []client.Option{
		client.WithHostPorts(addr),
		client.WithRPCTimeout(5 * time.Second),
	}
	opts = append(opts, internalauth.ClientOptions(internalToken)...)
	// 追踪 + 指标：traceparent 双通道注入与调用方视角耗时统计
	opts = append(opts, client.WithMiddleware(tracing.ClientMiddleware()))
	opts = append(opts, client.WithMiddleware(metrics.KitexClientMiddleware("auth_service")))
	return userservice.NewClient("user_service", opts...)
}

// mockIdPConf 内嵌 IdP 的客户端配置
func mockIdPConf() conf.IdPConf {
	return conf.IdPConf{
		Name:          "aegis-mock-sso",
		Provider:      "oidc",
		ClientID:      "aegis-platform",
		ClientSecret:  "aegis-mock-secret",
		AuthorizeURL:  fmt.Sprintf("http://127.0.0.1:%d/authorize", mockIdpPort),
		TokenURL:      fmt.Sprintf("http://127.0.0.1:%d/token", mockIdpPort),
		UserInfoURL:   fmt.Sprintf("http://127.0.0.1:%d/userinfo", mockIdpPort),
		JWKSURL:       fmt.Sprintf("http://127.0.0.1:%d/jwks", mockIdpPort),
		RedirectURI:   mockIdpRedirectURI,
		Scopes:        []string{"openid", "profile", "email"},
		Enabled:       true,
		UsePKCE:       true,
		UsernameField: "preferred_username",
	}
}

// startMockIdP 启动内嵌 IdP，返回清理函数
func startMockIdP(ctx context.Context) (func(context.Context) error, error) {
	srv := mockidp.New("aegis-platform", "aegis-mock-secret", mockIdpRedirectURI)
	addr := fmt.Sprintf("127.0.0.1:%d", mockIdpPort)
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error(ctx, "内嵌 IdP 异常退出", "err", err)
		}
	}()

	logger.Info(ctx, "mock mode: 内嵌 IdP 已启动",
		"authorize", fmt.Sprintf("http://%s/authorize", addr),
		"discovery", fmt.Sprintf("http://%s/.well-known/openid-configuration", addr))

	return func(ctx context.Context) error {
		shutdownCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	}, nil
}
