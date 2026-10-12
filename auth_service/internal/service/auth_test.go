package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cloudwego/kitex/client/callopt"

	authv1 "github.com/aegis/idl/kitex_gen/authv1"
	commonv1 "github.com/aegis/idl/kitex_gen/commonv1"
	userv1 "github.com/aegis/idl/kitex_gen/userv1"
	"github.com/aegis/idl/kitex_gen/userv1/userservice"

	"github.com/aegis/auth_service/internal/token"
	"github.com/aegis/pkg/errno"
	"github.com/aegis/pkg/jwtx"
	"github.com/aegis/pkg/mockx"
	"github.com/aegis/pkg/passwd"
)

// ---------- 可编程 userservice 桩 ----------

type fakeUserClient struct {
	credResp *userv1.GetUserCredentialResp
	credErr  error
	userResp *userv1.GetUserResp
	userErr  error
	create   *userv1.CreateUserResp
}

var _ userservice.Client = (*fakeUserClient)(nil)

func (f *fakeUserClient) GetUserCredential(_ context.Context, _ *userv1.GetUserCredentialReq,
	_ ...callopt.Option) (*userv1.GetUserCredentialResp, error) {
	if f.credErr != nil {
		return nil, f.credErr
	}
	return f.credResp, nil
}

func (f *fakeUserClient) GetUser(_ context.Context, _ *userv1.GetUserReq,
	_ ...callopt.Option) (*userv1.GetUserResp, error) {
	if f.userErr != nil {
		return nil, f.userErr
	}
	return f.userResp, nil
}

func (f *fakeUserClient) CreateUser(_ context.Context, _ *userv1.CreateUserReq,
	_ ...callopt.Option) (*userv1.CreateUserResp, error) {
	return f.create, nil
}

func (f *fakeUserClient) Health(context.Context, *commonv1.HealthReq, ...callopt.Option) (*commonv1.HealthResp, error) {
	return nil, errors.New("stub 未实现")
}
func (f *fakeUserClient) BatchGetUsers(context.Context, *userv1.BatchGetUsersReq, ...callopt.Option) (*userv1.BatchGetUsersResp, error) {
	return nil, errors.New("stub 未实现")
}
func (f *fakeUserClient) UpdateUser(context.Context, *userv1.UpdateUserReq, ...callopt.Option) (*userv1.UpdateUserResp, error) {
	return nil, errors.New("stub 未实现")
}
func (f *fakeUserClient) DeleteUser(context.Context, *userv1.DeleteUserReq, ...callopt.Option) (*commonv1.OpResult, error) {
	return nil, errors.New("stub 未实现")
}
func (f *fakeUserClient) ListUsers(context.Context, *userv1.ListUsersReq, ...callopt.Option) (*userv1.ListUsersResp, error) {
	return nil, errors.New("stub 未实现")
}
func (f *fakeUserClient) GetPreference(context.Context, *userv1.GetPreferenceReq, ...callopt.Option) (*userv1.GetPreferenceResp, error) {
	return nil, errors.New("stub 未实现")
}
func (f *fakeUserClient) SetPreference(context.Context, *userv1.SetPreferenceReq, ...callopt.Option) (*commonv1.OpResult, error) {
	return nil, errors.New("stub 未实现")
}

// ---------- 夹具 ----------

type authFixture struct {
	svc   *AuthService
	users *fakeUserClient
	mgr   *token.Manager
}

func newAuthFixture(t *testing.T) *authFixture {
	t.Helper()
	signer, err := jwtx.NewSigner(jwtx.Options{
		Algorithm:  jwtx.HS256,
		HMACSecret: []byte("test-secret-key-must-be-32-bytes!!"),
		Issuer:     "aegis",
		Audience:   []string{"aegis"},
	})
	if err != nil {
		t.Fatalf("构造 Signer 失败: %v", err)
	}
	mgr, err := token.New(token.Options{
		Signer:     signer,
		Issuer:     "aegis",
		AccessTTL:  time.Hour,
		RefreshTTL: 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("构造 Manager 失败: %v", err)
	}
	users := &fakeUserClient{}
	svc := New(mgr, token.NewLoginGuard(3, time.Minute, 2), nil, users,
		mockx.NewFaultInjector(false, 0, nil), nil)
	return &authFixture{svc: svc, users: users, mgr: mgr}
}

// credResp 构造一个已启用账号的凭据响应。
func credResp(uid int64, hash string) *userv1.GetUserCredentialResp {
	return &userv1.GetUserCredentialResp{
		Found:        true,
		Uid:          uid,
		Username:     "alice",
		PasswordHash: hash,
		Status:       1,
		Tenant:       "default",
		Roles:        []string{"user"},
	}
}

// ---------- 本地登录 ----------

// TestLoginSuccess 正确凭据签发令牌对。
func TestLoginSuccess(t *testing.T) {
	f := newAuthFixture(t)
	hash, _ := passwd.Hash("secret-123")
	f.users.credResp = credResp(1001, hash)
	f.users.userResp = &userv1.GetUserResp{
		Success: true,
		User:    &userv1.User{Uid: 1001, Username: "alice", Nickname: "爱丽丝"},
	}

	resp, err := f.svc.Login(context.Background(), &authv1.LoginReq{
		Username: "alice", Password: "secret-123",
	})
	if err != nil {
		t.Fatalf("Login 失败: %v", err)
	}
	if resp.GetAccessToken() == "" || resp.GetRefreshToken() == "" {
		t.Fatal("令牌对不完整")
	}
	if resp.GetUser().GetNickname() != "爱丽丝" {
		t.Errorf("应回传展示资料: %+v", resp.GetUser())
	}
}

// TestLoginMissingParams 缺用户名或密码 → ErrMissingParam。
func TestLoginMissingParams(t *testing.T) {
	f := newAuthFixture(t)
	_, err := f.svc.Login(context.Background(), &authv1.LoginReq{Username: "", Password: "x"})
	if errno.CodeOf(err) != errno.ErrMissingParam {
		t.Errorf("应返回 ErrMissingParam, 实际 %v", err)
	}
	_, err = f.svc.Login(context.Background(), &authv1.LoginReq{Username: "alice", Password: ""})
	if errno.CodeOf(err) != errno.ErrMissingParam {
		t.Errorf("应返回 ErrMissingParam, 实际 %v", err)
	}
}

// TestLoginUserNotFound 用户不存在与密码错误同码（防用户名枚举）。
func TestLoginUserNotFound(t *testing.T) {
	f := newAuthFixture(t)
	f.users.credResp = &userv1.GetUserCredentialResp{Found: false}

	_, err := f.svc.Login(context.Background(), &authv1.LoginReq{Username: "ghost", Password: "x"})
	if errno.CodeOf(err) != errno.ErrInvalidCredential {
		t.Errorf("应返回 ErrInvalidCredential, 实际 %v", err)
	}
}

// TestLoginBadPassword 密码错误 → ErrInvalidCredential 且累计失败计数。
func TestLoginBadPassword(t *testing.T) {
	f := newAuthFixture(t)
	hash, _ := passwd.Hash("right")
	f.users.credResp = credResp(1001, hash)

	_, err := f.svc.Login(context.Background(), &authv1.LoginReq{Username: "alice", Password: "wrong"})
	if errno.CodeOf(err) != errno.ErrInvalidCredential {
		t.Errorf("应返回 ErrInvalidCredential, 实际 %v", err)
	}
	if f.svc.guard.FailCount("alice") != 1 {
		t.Errorf("失败计数应为 1, 实际 %d", f.svc.guard.FailCount("alice"))
	}
}

// TestLoginAccountDisabled 账号禁用 → ErrAccountDisabled。
func TestLoginAccountDisabled(t *testing.T) {
	f := newAuthFixture(t)
	hash, _ := passwd.Hash("secret")
	resp := credResp(1001, hash)
	resp.Status = 2
	f.users.credResp = resp

	_, err := f.svc.Login(context.Background(), &authv1.LoginReq{Username: "alice", Password: "secret"})
	if errno.CodeOf(err) != errno.ErrAccountDisabled {
		t.Errorf("应返回 ErrAccountDisabled, 实际 %v", err)
	}
}

// TestLoginLocked 连续失败达到阈值后锁定（即使密码正确）。
func TestLoginLocked(t *testing.T) {
	f := newAuthFixture(t)
	hash, _ := passwd.Hash("secret")
	f.users.credResp = credResp(1001, hash)

	// captchaAfter=2 会先于 maxFailures=3 拦截，因此直接灌满失败计数触发锁定
	for i := 0; i < 3; i++ {
		f.svc.guard.Fail("alice")
	}
	// 锁定后即使密码正确也拒绝
	_, err := f.svc.Login(context.Background(), &authv1.LoginReq{Username: "alice", Password: "secret"})
	if errno.CodeOf(err) != errno.ErrForbidden {
		t.Errorf("锁定后应返回 ErrForbidden, 实际 %v", err)
	}
}

// TestLoginCaptchaRequired 连续失败达验证码阈值 → ErrCaptchaRequired，
// 且验证码提示不阻断失败累计（锁定仍可达）。
func TestLoginCaptchaRequired(t *testing.T) {
	f := newAuthFixture(t)
	hash, _ := passwd.Hash("secret")
	f.users.credResp = credResp(1001, hash)

	// 第 1 次错：普通密码错误
	_, err := f.svc.Login(context.Background(), &authv1.LoginReq{Username: "alice", Password: "bad"})
	if errno.CodeOf(err) != errno.ErrInvalidCredential {
		t.Fatalf("第 1 次失败应返回 ErrInvalidCredential, 实际 %v", err)
	}
	// 第 2 次错（captchaAfter=2）：提示验证码
	_, err = f.svc.Login(context.Background(), &authv1.LoginReq{Username: "alice", Password: "bad"})
	if errno.CodeOf(err) != errno.ErrCaptchaRequired {
		t.Fatalf("达到验证码阈值应返回 ErrCaptchaRequired, 实际 %v", err)
	}
	// 验证码提示后失败计数仍在累计
	if f.svc.guard.FailCount("alice") != 2 {
		t.Errorf("验证码提示后计数不应冻结, 实际 %d", f.svc.guard.FailCount("alice"))
	}
}

// TestLoginCaptchaThenLock 验证码提示后继续失败最终触发锁定（缺陷#1 回归）。
//
// 修复前：Check 在验证码阈值直接 return，Fail 不再被调用，
// 计数冻结在 captchaAfter，maxFailures 锁定永远不可达。
func TestLoginCaptchaThenLock(t *testing.T) {
	f := newAuthFixture(t)
	hash, _ := passwd.Hash("secret")
	f.users.credResp = credResp(1001, hash)

	// 连续错 3 次（maxFailures=3）：第 2、3 次起提示验证码，同时累计
	for i := 0; i < 3; i++ {
		_, err := f.svc.Login(context.Background(), &authv1.LoginReq{Username: "alice", Password: "bad"})
		if errno.CodeOf(err) != errno.ErrInvalidCredential && errno.CodeOf(err) != errno.ErrCaptchaRequired {
			t.Fatalf("第 %d 次失败响应不符: %v", i+1, err)
		}
	}
	// 第 3 次失败已触发锁定：第 4 次即使密码正确也拒绝
	_, err := f.svc.Login(context.Background(), &authv1.LoginReq{Username: "alice", Password: "secret"})
	if errno.CodeOf(err) != errno.ErrForbidden {
		t.Errorf("验证码提示后继续失败应能触发锁定(ErrForbidden), 实际 %v", err)
	}
}

// TestLoginCaptchaThenCorrectPassword 验证码提示后密码正确 → 放行并清零计数。
func TestLoginCaptchaThenCorrectPassword(t *testing.T) {
	f := newAuthFixture(t)
	hash, _ := passwd.Hash("secret")
	f.users.credResp = credResp(1001, hash)

	for i := 0; i < 2; i++ {
		f.svc.Login(context.Background(), &authv1.LoginReq{Username: "alice", Password: "bad"})
	}
	// 第 3 次：验证码提示不阻断，密码正确应放行
	resp, err := f.svc.Login(context.Background(), &authv1.LoginReq{Username: "alice", Password: "secret"})
	if err != nil {
		t.Fatalf("验证码提示后密码正确应放行: %v", err)
	}
	if resp.GetAccessToken() == "" {
		t.Error("应签发令牌")
	}
	if f.svc.guard.FailCount("alice") != 0 {
		t.Errorf("登录成功后计数应清零, 实际 %d", f.svc.guard.FailCount("alice"))
	}
}

// TestLoginUpstreamError user_service 不可达 → ErrUpstreamError。
func TestLoginUpstreamError(t *testing.T) {
	f := newAuthFixture(t)
	f.users.credErr = errors.New("rpc down")

	_, err := f.svc.Login(context.Background(), &authv1.LoginReq{Username: "alice", Password: "x"})
	if errno.CodeOf(err) != errno.ErrUpstreamError {
		t.Errorf("应返回 ErrUpstreamError, 实际 %v", err)
	}
}

// ---------- VerifyToken ----------

// TestVerifyToken 签发 → 校验有效；伪造 → 无效；缺 scope → 无效。
func TestVerifyToken(t *testing.T) {
	f := newAuthFixture(t)
	pair, err := f.mgr.Issue(token.UserInfo{
		UID: 1001, Username: "alice", Tenant: "default",
		Roles: []string{"user"}, Scopes: []string{"user"},
	})
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}

	resp, err := f.svc.VerifyToken(context.Background(), &authv1.VerifyTokenReq{
		AccessToken: pair.AccessToken,
	})
	if err != nil {
		t.Fatalf("VerifyToken 失败: %v", err)
	}
	if !resp.GetValid() || resp.GetUid() != 1001 {
		t.Errorf("有效令牌应校验通过: %+v", resp)
	}

	// 伪造令牌
	fake, _ := f.svc.VerifyToken(context.Background(), &authv1.VerifyTokenReq{
		AccessToken: "not-a-real-token",
	})
	if fake.GetValid() {
		t.Error("伪造令牌不应通过")
	}

	// 缺 admin scope
	admin, _ := f.svc.VerifyToken(context.Background(), &authv1.VerifyTokenReq{
		AccessToken:   pair.AccessToken,
		RequiredScope: "admin",
	})
	if admin.GetValid() {
		t.Error("无 admin scope 的令牌要求 admin 时应拒绝")
	}
}

// TestRefreshTokenRotation 刷新后旧 refresh token 立即失效（一次性消费）。
func TestRefreshTokenRotation(t *testing.T) {
	f := newAuthFixture(t)
	pair, err := f.mgr.Issue(token.UserInfo{UID: 1001, Username: "alice"})
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	oldRefresh := pair.RefreshToken
	f.users.userResp = &userv1.GetUserResp{
		Success: true,
		User:    &userv1.User{Uid: 1001, Username: "alice"},
	}

	_, err = f.svc.RefreshToken(context.Background(), &authv1.RefreshTokenReq{
		RefreshToken: oldRefresh,
	})
	if err != nil {
		t.Fatalf("首次刷新应成功: %v", err)
	}
	// 重放旧 refresh token 应失败
	_, err = f.svc.RefreshToken(context.Background(), &authv1.RefreshTokenReq{
		RefreshToken: oldRefresh,
	})
	if err == nil {
		t.Error("refresh token 轮换后重放应失败")
	}
}

// TestLogoutRevokesAll 登出按 uid 吊销全部令牌。
func TestLogoutRevokesAll(t *testing.T) {
	f := newAuthFixture(t)
	pair, err := f.mgr.Issue(token.UserInfo{UID: 1001, Username: "alice"})
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}

	if _, err := f.svc.Logout(context.Background(), &authv1.LogoutReq{
		AccessToken: pair.AccessToken,
		Uid:         1001,
	}); err != nil {
		t.Fatalf("Logout 失败: %v", err)
	}

	resp, _ := f.svc.VerifyToken(context.Background(), &authv1.VerifyTokenReq{
		AccessToken: pair.AccessToken,
	})
	if resp.GetValid() {
		t.Error("登出后令牌应失效")
	}
}

// ---------- OAuth ----------

// TestOAuthAuthorizeUnknownIdp 未配置的 IdP → ErrIdPNotFound。
func TestOAuthAuthorizeUnknownIdp(t *testing.T) {
	f := newAuthFixture(t)
	_, err := f.svc.OAuthAuthorize(context.Background(), &authv1.OAuthAuthorizeReq{Idp: "ghost"})
	if errno.CodeOf(err) != errno.ErrIdPNotFound {
		t.Errorf("应返回 ErrIdPNotFound, 实际 %v", err)
	}
}

// TestOAuthCallbackBadState 无有效 state → ErrOAuthStateMismatch（防 CSRF）。
func TestOAuthCallbackBadState(t *testing.T) {
	f := newAuthFixture(t)
	_, err := f.svc.OAuthCallback(context.Background(), &authv1.OAuthCallbackReq{
		Code: "code-x",
	})
	if errno.CodeOf(err) != errno.ErrOAuthStateMismatch {
		t.Errorf("应返回 ErrOAuthStateMismatch, 实际 %v", err)
	}
}

// ---------- 权限 ----------

// TestCheckPermission 规则：admin 全放行；非 admin 读放行、写拒绝。
func TestCheckPermission(t *testing.T) {
	f := newAuthFixture(t)
	admin := &userv1.GetUserResp{Success: true, User: &userv1.User{Uid: 1, Roles: []string{"admin"}}}
	normal := &userv1.GetUserResp{Success: true, User: &userv1.User{Uid: 2, Roles: []string{"user"}}}

	ctx := context.Background()
	f.users.userResp = admin
	resp, err := f.svc.CheckPermission(ctx, &authv1.CheckPermissionReq{
		Uid: 1, Resource: "kb:1", Action: "write",
	})
	if err != nil || !resp.GetAllowed() {
		t.Errorf("admin 应放行写操作: allowed=%v err=%v", resp.GetAllowed(), err)
	}

	f.users.userResp = normal
	r1, _ := f.svc.CheckPermission(ctx, &authv1.CheckPermissionReq{Uid: 2, Resource: "kb:1", Action: "read"})
	if !r1.GetAllowed() {
		t.Error("非 admin 读操作应放行")
	}
	r2, _ := f.svc.CheckPermission(ctx, &authv1.CheckPermissionReq{Uid: 2, Resource: "kb:1", Action: "write"})
	if r2.GetAllowed() {
		t.Error("非 admin 写操作应拒绝")
	}

	anon, _ := f.svc.CheckPermission(ctx, &authv1.CheckPermissionReq{Uid: 0, Resource: "kb:1", Action: "read"})
	if anon.GetAllowed() {
		t.Error("匿名不应放行")
	}
}

// TestScopesFromRoles 角色推导 scope：基础 user + 去重。
func TestScopesFromRoles(t *testing.T) {
	got := scopesFromRoles([]string{"admin", "admin", "user", ""})
	want := []string{"user", "admin"}
	if len(got) != len(want) {
		t.Fatalf("scope 数量不符: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("scope[%d]=%q 期望 %q: %v", i, got[i], want[i], got)
		}
	}
	// 无角色也有基础 scope
	if g := scopesFromRoles(nil); len(g) != 1 || g[0] != "user" {
		t.Errorf("无角色应只有 user scope: %v", g)
	}
}

// TestHealth 健康检查返回 mock 状态。
func TestHealth(t *testing.T) {
	f := newAuthFixture(t)
	resp, err := f.svc.Health(context.Background(), &commonv1.HealthReq{})
	if err != nil {
		t.Fatalf("Health 失败: %v", err)
	}
	if !resp.GetHealthy() || resp.GetService() != "auth_service" {
		t.Errorf("健康状态不符: %+v", resp)
	}
}
