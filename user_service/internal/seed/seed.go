// Package seed 提供 mock 模式的种子数据。
//
// 冒烟测试需要可预测的登录账号，因此内存仓储启动时注入固定用户。
// 仅在 mock.enabled=true 时装载 —— 生产环境绝不会写入这些账号。
package seed

import (
	"time"

	userv1 "github.com/aegis/idl/kitex_gen/userv1"

	"github.com/aegis/user_service/internal/repository"
	"github.com/aegis/user_service/internal/service"
)

// DemoPassword 演示账号的统一密码。
//
// ★ 仅 mock 模式使用。文档与前端演示均以此密码登录。
const DemoPassword = "Aegis@2026"

// Users 返回演示用户列表
func Users() []*repository.UserRecord {
	now := time.Now().Unix()
	hash := service.HashPasswordForSeed(DemoPassword)

	return []*repository.UserRecord{
		{
			PasswordHash: hash,
			User: &userv1.User{
				Uid:       1001,
				Username:  "admin",
				Nickname:  "平台管理员",
				Email:     "admin@aegis.dev",
				Avatar:    "",
				Status:    1,
				Roles:     []string{"admin", "user"},
				Tenant:    "default",
				CreatedAt: now,
				UpdatedAt: now,
				Source:    "local",
			},
		},
		{
			PasswordHash: hash,
			User: &userv1.User{
				Uid:       1002,
				Username:  "alice",
				Nickname:  "爱丽丝",
				Email:     "alice@aegis.dev",
				Status:    1,
				Roles:     []string{"user"},
				Tenant:    "default",
				CreatedAt: now,
				UpdatedAt: now,
				Source:    "local",
			},
		},
		{
			PasswordHash: hash,
			User: &userv1.User{
				Uid:       1003,
				Username:  "bob",
				Nickname:  "鲍勃",
				Email:     "bob@aegis.dev",
				Status:    1,
				Roles:     []string{"user"},
				Tenant:    "default",
				CreatedAt: now,
				UpdatedAt: now,
				Source:    "local",
			},
		},
		{
			// 禁用账号：用于验证登录被拒
			PasswordHash: hash,
			User: &userv1.User{
				Uid:       1004,
				Username:  "disabled",
				Nickname:  "已禁用用户",
				Email:     "disabled@aegis.dev",
				Status:    2,
				Roles:     []string{"user"},
				Tenant:    "default",
				CreatedAt: now,
				UpdatedAt: now,
				Source:    "local",
			},
		},
	}
}
