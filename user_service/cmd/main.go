// Command user_service 用户与租户管理服务。
//
// 职责：用户 CRUD、角色权限、租户隔离、用户偏好。
// 认证域所需的最小信息通过 GetUserCredential 提供给 auth_service。
package main

import (
	"context"
	"os"
	"strings"

	userservice "github.com/aegis/idl/kitex_gen/userv1/userservice"
	"github.com/cloudwego/kitex/server"

	"github.com/aegis/pkg/logger"
	"github.com/aegis/pkg/serverx"
	"github.com/aegis/user_service/internal/repository"
	"github.com/aegis/user_service/internal/seed"
	"github.com/aegis/user_service/internal/service"
)

func main() {
	ctx := context.Background()

	if err := serverx.Start(serverx.Options{
		ServiceName:     "user_service",
		DefaultRPCPort:  8882,
		DefaultHTTPPort: 8092,
		Register: func(svr server.Server, app *serverx.App) (func(context.Context) error, error) {
			var (
				repo    repository.Repository
				cleanup func(context.Context) error
			)

			// ★ mock 模式：内存仓储 + 种子用户（离线冒烟）
			if app.MockMode() {
				mr := repository.NewMemoryRepo()
				mr.Seed(seed.Users()...)
				repo = mr
				cleanup = func(context.Context) error { return nil }
				logger.Info(ctx, "user_service: mock 模式，MemoryRepo + 种子用户", "count", mr.Count())
			} else {
				// ★ 生产/联调模式：优先 PG，DSN 未配时回退 MemoryRepo（不 seed）
				pg, err := repository.NewPgRepo(ctx, app.Cfg.Postgres)
				if err != nil {
					logger.Warn(ctx, "user_service: PG 不可用，回退 MemoryRepo",
						"dsn", app.Cfg.Postgres.DSN, "err", err)
					repo = repository.NewMemoryRepo()
					cleanup = func(context.Context) error { return nil }
				} else {
					repo = pg
					cleanup = func(context.Context) error { pg.Close(); return nil }
					logger.Info(ctx, "user_service: PG 仓储已连接",
						"dsn_masked", maskDSN(app.Cfg.Postgres.DSN))
				}
			}

			userservice.RegisterService(svr, service.New(repo, app.FaultInjector()))
			return cleanup, nil
		},
	}); err != nil {
		logger.Error(ctx, "user_service 启动失败", "err", err)
		os.Exit(1)
	}
}

// maskDSN 把 DSN 里的 password 替换为 ****，仅用于日志
func maskDSN(dsn string) string {
	if i := strings.Index(dsn, ":"); i >= 0 {
		if j := strings.Index(dsn[i:], "@"); j >= 0 {
			return dsn[:i+1] + "****" + dsn[i+j:]
		}
	}
	return dsn
}
