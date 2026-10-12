package service

import (
	"context"
	"testing"

	commonv1 "github.com/aegis/idl/kitex_gen/commonv1"
	userv1 "github.com/aegis/idl/kitex_gen/userv1"

	"github.com/aegis/pkg/errno"
	"github.com/aegis/pkg/mockx"

	"github.com/aegis/user_service/internal/repository"
)

// newFixture 内存仓储 + 服务。
func newFixture(t *testing.T) (*UserService, *repository.MemoryRepo) {
	t.Helper()
	repo := repository.NewMemoryRepo()
	svc := New(repo, mockx.NewFaultInjector(false, 0, nil))
	return svc, repo
}

// TestHealth 健康检查返回服务标识与 mock 状态。
func TestHealth(t *testing.T) {
	svc, _ := newFixture(t)
	resp, err := svc.Health(context.Background(), &commonv1.HealthReq{})
	if err != nil {
		t.Fatalf("Health 失败: %v", err)
	}
	if !resp.GetHealthy() || resp.GetService() != "user_service" {
		t.Errorf("健康状态不符: %+v", resp)
	}
}

// TestGetUserByUID 按 uid / username / 缺参三分支。
func TestGetUserByUID(t *testing.T) {
	svc, repo := newFixture(t)
	uid, err := repo.Create(context.Background(), &userv1.User{Username: "alice"}, "")
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}

	// 按 uid
	resp, err := svc.GetUser(context.Background(), &userv1.GetUserReq{Uid: uid})
	if err != nil || !resp.GetSuccess() || resp.GetUser().GetUsername() != "alice" {
		t.Errorf("按 uid 查询失败: resp=%+v err=%v", resp, err)
	}
	// 按 username
	resp2, err := svc.GetUser(context.Background(), &userv1.GetUserReq{Username: "alice"})
	if err != nil || resp2.GetUser().GetUid() != uid {
		t.Errorf("按 username 查询失败: resp=%+v err=%v", resp2, err)
	}
	// 缺参
	_, err = svc.GetUser(context.Background(), &userv1.GetUserReq{})
	if errno.CodeOf(err) != errno.ErrInvalidParam {
		t.Errorf("缺参应 ErrInvalidParam, 实际 %v", err)
	}
	// 不存在
	_, err = svc.GetUser(context.Background(), &userv1.GetUserReq{Uid: 99999})
	if errno.CodeOf(err) != errno.ErrUserNotFound {
		t.Errorf("不存在应 ErrUserNotFound, 实际 %v", err)
	}
}

// TestCreateUser 默认字段回填 + 密码哈希存储 + 用户名冲突。
func TestCreateUser(t *testing.T) {
	svc, repo := newFixture(t)

	resp, err := svc.CreateUser(context.Background(), &userv1.CreateUserReq{
		Username: "bob", Password: "pw-123",
	})
	if err != nil {
		t.Fatalf("CreateUser 失败: %v", err)
	}
	if !resp.GetSuccess() || resp.GetUid() == 0 {
		t.Fatalf("创建失败: %+v", resp)
	}

	u, err := repo.GetByUID(context.Background(), resp.GetUid())
	if err != nil {
		t.Fatalf("GetByUID 失败: %v", err)
	}
	if u.GetTenant() != "default" || u.GetStatus() != 1 {
		t.Errorf("默认 tenant/status 不符: %+v", u)
	}
	if len(u.GetRoles()) != 1 || u.GetRoles()[0] != "user" {
		t.Errorf("默认角色应为 user: %v", u.GetRoles())
	}
	// 密码哈希已存储（明文不落库）
	cred, _ := repo.GetCredential(context.Background(), "bob", 0)
	if cred.PasswordHash == "" || cred.PasswordHash == "pw-123" {
		t.Error("密码应以哈希形式存储")
	}
	// 用户名冲突 → Success=false + 提示
	resp2, err := svc.CreateUser(context.Background(), &userv1.CreateUserReq{Username: "bob"})
	if err != nil {
		t.Fatalf("冲突不应是 RPC 错误: %v", err)
	}
	if resp2.GetSuccess() {
		t.Error("重复用户名应返回 Success=false")
	}
	// 缺 username
	_, err = svc.CreateUser(context.Background(), &userv1.CreateUserReq{})
	if errno.CodeOf(err) != errno.ErrInvalidParam {
		t.Errorf("缺 username 应 ErrInvalidParam, 实际 %v", err)
	}
}

// TestUpdateUserMask 掩码更新不误覆盖零值。
func TestUpdateUserMask(t *testing.T) {
	svc, repo := newFixture(t)
	uid, _ := repo.Create(context.Background(), &userv1.User{Username: "carol"}, "")

	// 只更新 email，nickname 保持
	resp, err := svc.UpdateUser(context.Background(), &userv1.UpdateUserReq{
		Uid: uid, Email: "c@x.io", UpdateMask: []string{"email"},
	})
	if err != nil {
		t.Fatalf("UpdateUser 失败: %v", err)
	}
	if resp.GetUser().GetEmail() != "c@x.io" {
		t.Errorf("email 应更新: %q", resp.GetUser().GetEmail())
	}
	if resp.GetUser().GetUsername() != "carol" {
		t.Errorf("username 不应被清空: %q", resp.GetUser().GetUsername())
	}
	// 不存在
	_, err = svc.UpdateUser(context.Background(), &userv1.UpdateUserReq{Uid: 99999})
	if errno.CodeOf(err) != errno.ErrUserNotFound {
		t.Errorf("应 ErrUserNotFound, 实际 %v", err)
	}
	// 缺 uid
	_, err = svc.UpdateUser(context.Background(), &userv1.UpdateUserReq{})
	if errno.CodeOf(err) != errno.ErrInvalidParam {
		t.Errorf("缺 uid 应 ErrInvalidParam, 实际 %v", err)
	}
}

// TestDeleteUser 软删与硬删。
func TestDeleteUser(t *testing.T) {
	svc, repo := newFixture(t)
	uid, _ := repo.Create(context.Background(), &userv1.User{Username: "dave"}, "")

	// 软删：记录保留但不可查
	if _, err := svc.DeleteUser(context.Background(), &userv1.DeleteUserReq{Uid: uid, Soft: true}); err != nil {
		t.Fatalf("软删失败: %v", err)
	}
	if _, err := repo.GetByUID(context.Background(), uid); err == nil {
		t.Error("软删后不应可查询")
	}
	// 硬删不存在
	if _, err := svc.DeleteUser(context.Background(), &userv1.DeleteUserReq{Uid: 99999}); errno.CodeOf(err) != errno.ErrUserNotFound {
		t.Errorf("应 ErrUserNotFound, 实际 %v", err)
	}
	// 缺 uid
	if _, err := svc.DeleteUser(context.Background(), &userv1.DeleteUserReq{}); errno.CodeOf(err) != errno.ErrInvalidParam {
		t.Errorf("缺 uid 应 ErrInvalidParam, 实际 %v", err)
	}
}

// TestGetUserCredential 不存在时返回 found=false（防用户名枚举）。
func TestGetUserCredential(t *testing.T) {
	svc, repo := newFixture(t)
	repo.Create(context.Background(), &userv1.User{Username: "eve"}, "hash-x")

	resp, err := svc.GetUserCredential(context.Background(), &userv1.GetUserCredentialReq{Username: "eve"})
	if err != nil {
		t.Fatalf("GetUserCredential 失败: %v", err)
	}
	if !resp.GetFound() || resp.GetPasswordHash() != "hash-x" {
		t.Errorf("凭据不符: %+v", resp)
	}
	// 不存在 → found=false，不报错
	nf, err := svc.GetUserCredential(context.Background(), &userv1.GetUserCredentialReq{Username: "ghost"})
	if err != nil || nf.GetFound() {
		t.Errorf("不存在应返回 found=false: resp=%+v err=%v", nf, err)
	}
	// 缺参
	_, err = svc.GetUserCredential(context.Background(), &userv1.GetUserCredentialReq{})
	if errno.CodeOf(err) != errno.ErrInvalidParam {
		t.Errorf("缺参应 ErrInvalidParam, 实际 %v", err)
	}
}

// TestPreference 偏好读写往返。
func TestPreference(t *testing.T) {
	svc, repo := newFixture(t)
	uid, _ := repo.Create(context.Background(), &userv1.User{Username: "frank"}, "")

	if _, err := svc.SetPreference(context.Background(), &userv1.SetPreferenceReq{
		Uid: uid, Items: map[string]string{"theme": "dark", "lang": "zh"},
	}); err != nil {
		t.Fatalf("SetPreference 失败: %v", err)
	}
	resp, err := svc.GetPreference(context.Background(), &userv1.GetPreferenceReq{Uid: uid})
	if err != nil {
		t.Fatalf("GetPreference 失败: %v", err)
	}
	if resp.GetItems()["theme"] != "dark" || resp.GetItems()["lang"] != "zh" {
		t.Errorf("偏好不符: %+v", resp.GetItems())
	}
	// 无偏好返回空 map 而非 nil
	empty, _ := svc.GetPreference(context.Background(), &userv1.GetPreferenceReq{Uid: 88888})
	if empty.GetItems() == nil {
		t.Error("无偏好应返回空 map")
	}
}

// TestListUsers 分页与过滤。
func TestListUsers(t *testing.T) {
	svc, repo := newFixture(t)
	for i := 0; i < 5; i++ {
		repo.Create(context.Background(), &userv1.User{
			Username: "user" + string(rune('0'+i)),
			Tenant:   "t1",
		}, "")
	}
	resp, err := svc.ListUsers(context.Background(), &userv1.ListUsersReq{
		Page: &commonv1.PageReq{Page: 1, PageSize: 2},
	})
	if err != nil {
		t.Fatalf("ListUsers 失败: %v", err)
	}
	if len(resp.GetUsers()) != 2 || int(resp.GetMeta().GetTotal()) != 5 {
		t.Errorf("分页不符: n=%d total=%d", len(resp.GetUsers()), resp.GetMeta().GetTotal())
	}
	// 按租户过滤
	t1, _ := svc.ListUsers(context.Background(), &userv1.ListUsersReq{Tenant: "t1"})
	if int(t1.GetMeta().GetTotal()) != 5 {
		t.Errorf("租户过滤不符: %d", t1.GetMeta().GetTotal())
	}
}
