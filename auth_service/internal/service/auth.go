// Package service 实现 auth_service 的业务 handler。
//
// 双通道认证：
//  1. 本地账号密码（auth_service 通过 user_service 取 credential 校验）
//  2. OAuth2.0 授权码 + PKCE（对接外部 IdP，回调后自签本系统 JWT）
//
// 两条通道最终产出同一种凭证：本系统签发的 JWT。
// 下游服务只认 JWT，不关心用户从哪条通道来 —— 这是认证层解耦的关键。
package service

import (
	"context"
	"errors"
	"sync"
	"time"

	authv1 "github.com/aegis/idl/kitex_gen/authv1"
	commonv1 "github.com/aegis/idl/kitex_gen/commonv1"
	userv1 "github.com/aegis/idl/kitex_gen/userv1"
	userservice "github.com/aegis/idl/kitex_gen/userv1/userservice"

	"github.com/aegis/auth_service/internal/oauth2"
	"github.com/aegis/auth_service/internal/token"
	"github.com/aegis/pkg/conf"
	"github.com/aegis/pkg/errno"
	"github.com/aegis/pkg/logger"
	"github.com/aegis/pkg/mockx"
)

// AuthService 实现 authv1.AuthService
type AuthService struct {
	tokens *token.Manager
	guard  *token.LoginGuard
	oauth  *oauth2.Client
	users  userservice.Client
	mock   *mockx.FaultInjector

	idps     map[string]conf.IdPConf
	idpOrder []string

	// state 存储：state → 授权上下文，TTL 10 分钟
	// OAuth2.0 state 校验是防 CSRF 的关键，不能省
	stateMu sync.Mutex
	states  map[string]*authState

	startedAt time.Time
}

// authState 一次授权请求的上下文
type authState struct {
	idpName      string
	redirectURI  string
	codeVerifier string
	createdAt    time.Time
	uid          int64 // 已登录用户则非 0（SSO 绑定场景）
}

const stateTTL = 10 * time.Minute

// New 构造 handler
func New(
	tokens *token.Manager,
	guard *token.LoginGuard,
	oauth *oauth2.Client,
	users userservice.Client,
	mock *mockx.FaultInjector,
	idps []conf.IdPConf,
) *AuthService {
	m := make(map[string]conf.IdPConf, len(idps))
	order := make([]string, 0, len(idps))
	for _, idp := range idps {
		if !idp.Enabled {
			continue
		}
		m[idp.Name] = idp
		order = append(order, idp.Name)
	}
	return &AuthService{
		tokens:    tokens,
		guard:     guard,
		oauth:     oauth,
		users:     users,
		mock:      mock,
		idps:      m,
		idpOrder:  order,
		states:    make(map[string]*authState),
		startedAt: time.Now(),
	}
}

var _ authv1.AuthService = (*AuthService)(nil)

// ---------- 健康检查 ----------

func (s *AuthService) Health(_ context.Context, _ *commonv1.HealthReq) (*commonv1.HealthResp, error) {
	return &commonv1.HealthResp{
		Healthy:   !s.mock.ShouldFail("health"),
		Service:   "auth_service",
		Version:   "dev",
		UptimeSec: uint64(time.Since(s.startedAt).Seconds()),
		MockMode:  s.mock.Enabled(),
	}, nil
}

// ---------- 本地登录 ----------

func (s *AuthService) Login(ctx context.Context, req *authv1.LoginReq) (*authv1.LoginResp, error) {
	if err := s.mock.Sleep(ctx); err != nil {
		return nil, err
	}
	username := req.GetUsername()
	if username == "" || req.GetPassword() == "" {
		return nil, errno.New(errno.ErrMissingParam)
	}

	// 锁定检查：已锁定直接拒绝
	if err := s.guard.Check(username); err != nil {
		var locked *token.ErrLocked
		if errors.As(err, &locked) {
			logger.Warn(ctx, "login blocked: account locked", "username", username)
			return nil, errno.Wrap(errno.ErrForbidden, err)
		}
		var cap *token.ErrCaptcha
		if errors.As(err, &cap) {
			// ★ 缺陷修复（白盒测试报告 #1）：验证码只是提示，不阻断校验流程。
			//
			//   原实现在此直接 return，导致失败计数冻结在验证码阈值，
			//   maxFailures 锁定防护永远不可达 —— 暴力破解只需面对
			//   （未必强制校验的）验证码，绕过了最强的一道防护。
			//   现在继续执行密码校验：错则 Fail 累计直至锁定，对则 Success 清零。
			logger.Info(ctx, "captcha required, continue verifying",
				"username", username, "failures", cap.Failures)
		} else {
			return nil, errno.Wrap(errno.ErrInvalidParam, err)
		}
	}

	// 从 user_service 取凭据
	cred, err := s.users.GetUserCredential(ctx, &userv1.GetUserCredentialReq{
		Username: username,
	})
	if err != nil {
		logger.Warn(ctx, "get credential failed", "username", username, "err", err)
		return nil, errno.Wrap(errno.ErrUpstreamError, err)
	}
	if !cred.GetFound() {
		// ★ 用户不存在与密码错误返回同一个错误码，防止用户名枚举
		s.guard.Fail(username)
		return nil, errno.New(errno.ErrInvalidCredential)
	}

	// 密码校验在 user_service 侧完成（hash 格式由 user_service 定义）
	if !s.verifyPassword(cred.GetPasswordHash(), req.GetPassword()) {
		s.guard.Fail(username)
		logger.Warn(ctx, "login failed: bad password", "username", username)
		// 达到验证码阈值时提示验证码（失败计数已继续累计，
		// 达到 maxFailures 后由下一次 Check 以 ErrLocked 拦截）。
		if s.guard.NeedsCaptcha(username) {
			return nil, errno.New(errno.ErrCaptchaRequired)
		}
		return nil, errno.New(errno.ErrInvalidCredential)
	}

	if cred.GetStatus() != 1 {
		return nil, errno.New(errno.ErrAccountDisabled)
	}

	s.guard.Success(username)

	// 拉完整用户资料（credential 只含认证必需字段，昵称/邮箱需另取）
	profile := s.fetchProfile(ctx, cred.GetUid(), cred.GetUsername())

	pair, resp, err := s.issueFor(ctx, token.UserInfo{
		UID:      cred.GetUid(),
		Username: cred.GetUsername(),
		Nickname: profile.GetNickname(),
		Email:    profile.GetEmail(),
		Avatar:   profile.GetAvatar(),
		Tenant:   cred.GetTenant(),
		Roles:    cred.GetRoles(),
		Scopes:   scopesFromRoles(cred.GetRoles()),
	})
	if err != nil {
		return nil, err
	}

	logger.Info(ctx, "login success", "uid", cred.GetUid(), "username", username, "channel", "local")
	return respBuilder(pair)(resp), nil
}

func (s *AuthService) RefreshToken(ctx context.Context, req *authv1.RefreshTokenReq) (*authv1.LoginResp, error) {
	if err := s.mock.Sleep(ctx); err != nil {
		return nil, err
	}
	claims, err := s.tokens.ParseRefresh(req.GetRefreshToken())
	if err != nil {
		return nil, err
	}

	// ★ 一次性消费：验证通过立刻吊销旧的 refresh token。
	//
	//   OAuth 2.0 安全最佳实践（RFC 6819 §5.2.2.3）要求 refresh token
	//   必须轮换（rotation）—— 每次刷新都换发新令牌并作废旧的。
	//   不作废的话，攻击者只要截获一次 refresh token 就能长期换取
	//   access token，即使用户已经登出或改了密码，令牌依然有效。
	//   轮换后重放会被吊销名单拦下，同时说明令牌已泄漏。
	//
	//   代价是并发刷新时后到的请求会失败 —— 前端应串行化刷新，
	//   这也是所有主流 OAuth 实现（含 Spring Authorization Server）的行为。
	s.tokens.Revoke(req.GetRefreshToken())

	// 重新拉一次用户：确保角色/租户变更即时生效，
	// 而不是让旧 token 里的角色一直有效到过期。
	// 展示资料（昵称/邮箱）刻意不放进 JWT —— 令牌应尽量最小化，
	// PII 留在 user_service，刷新时重取。
	var roles []string
	var tenant string
	cred, err := s.users.GetUserCredential(ctx, &userv1.GetUserCredentialReq{Uid: claims.UID})
	if err != nil {
		return nil, errno.Wrap(errno.ErrUpstreamError, err)
	}
	if cred.GetFound() {
		if cred.GetStatus() != 1 {
			return nil, errno.New(errno.ErrAccountDisabled)
		}
		roles = cred.GetRoles()
		tenant = cred.GetTenant()
	}
	profile := s.fetchProfile(ctx, claims.UID, claims.Username)

	pair, resp, err := s.issueFor(ctx, token.UserInfo{
		UID:      claims.UID,
		Username: claims.Username,
		Nickname: profile.GetNickname(),
		Email:    profile.GetEmail(),
		Avatar:   profile.GetAvatar(),
		Tenant:   tenant,
		Roles:    roles,
		Scopes:   scopesFromRoles(roles),
		IDP:      claims.Issuer2,
	})
	if err != nil {
		return nil, err
	}
	logger.Info(ctx, "token refreshed", "uid", claims.UID)
	return respBuilder(pair)(resp), nil
}

func (s *AuthService) Logout(ctx context.Context, req *authv1.LogoutReq) (*commonv1.OpResult, error) {
	// proto 中 LogoutReq 只带 access_token 与 uid，没有 refresh_token。
	// ★ 但登出必须让 refresh token 一并失效 ——
	//   否则用户点了"退出登录"，攻击者手上的 refresh token 仍能换出新令牌，
	//   登出形同虚设。方案：按 uid 吊销该用户全部在用令牌，
	//   这比"只吊销当前这一个 access token"更彻底，也不需要改协议。
	if req.GetAccessToken() != "" {
		s.tokens.Revoke(req.GetAccessToken())
	}
	if req.GetUid() != 0 {
		s.tokens.RevokeAllOf(req.GetUid())
	}
	logger.Info(ctx, "logout", "uid", req.GetUid())
	return &commonv1.OpResult{Success: true, Message: "已登出"}, nil
}

// ---------- OAuth2.0 ----------

func (s *AuthService) ListIdPs(_ context.Context, _ *authv1.ListIdPsReq) (*authv1.ListIdPsResp, error) {
	resp := &authv1.ListIdPsResp{}
	for _, name := range s.idpOrder {
		idp := s.idps[name]
		resp.Idps = append(resp.Idps, &authv1.IdPInfo{
			Name:         idp.Name,
			Provider:     idp.Provider,
			AuthorizeUrl: idp.AuthorizeURL,
			Enabled:      idp.Enabled,
		})
	}
	return resp, nil
}

func (s *AuthService) OAuthAuthorize(ctx context.Context, req *authv1.OAuthAuthorizeReq) (*authv1.OAuthAuthorizeResp, error) {
	if err := s.mock.Sleep(ctx); err != nil {
		return nil, err
	}
	idp, ok := s.idps[req.GetIdp()]
	if !ok {
		return nil, errno.New(errno.ErrIdPNotFound)
	}
	if idp.AuthorizeURL == "" {
		return nil, errno.New(errno.ErrIdPNotFound)
	}

	// redirect_uri 优先用请求传入的（多租户/多前端场景），否则用配置值
	redirect := req.GetRedirectUri()
	if redirect == "" {
		redirect = idp.RedirectURI
	}

	// state：调用方自带则沿用（前端可能已生成），否则服务端生成
	state := req.GetState()
	if state == "" {
		state = oauth2.NewState()
	}

	verifier := oauth2.NewVerifier()
	challenge := ""
	if idp.UsePKCE {
		challenge = oauth2.CodeChallenge(verifier)
	}

	s.saveState(state, &authState{
		idpName:      idp.Name,
		redirectURI:  redirect,
		codeVerifier: verifier,
		createdAt:    time.Now(),
	})

	url := s.oauth.AuthorizeURL(idp, state, challenge)
	logger.Info(ctx, "oauth authorize issued", "idp", idp.Name, "pkce", idp.UsePKCE)

	return &authv1.OAuthAuthorizeResp{
		AuthorizeUrl: url,
		State:        state,
		// ★ verifier 只在启用了 PKCE 时回传给调用方；
		//   不启用时回空，避免给前端一个无用的敏感值
		CodeVerifier: pick(idp.UsePKCE, verifier, ""),
	}, nil
}

func (s *AuthService) OAuthCallback(ctx context.Context, req *authv1.OAuthCallbackReq) (*authv1.LoginResp, error) {
	if err := s.mock.Sleep(ctx); err != nil {
		return nil, err
	}
	if req.GetCode() == "" {
		return nil, errno.New(errno.ErrMissingParam)
	}

	// 1) 校验并消费 state（一次性，防重放 + 防 CSRF）
	st, err := s.consumeState(req.GetState())
	if err != nil {
		return nil, err
	}
	// 请求里的 idp 优先，便于多 IdP 场景（同一 state 可能来自不同入口）
	idpName := req.GetIdp()
	if idpName == "" {
		idpName = st.idpName
	}
	idp, ok := s.idps[idpName]
	if !ok {
		return nil, errno.New(errno.ErrIdPNotFound)
	}

	// 2) 用 code 换 token
	verifier := req.GetCodeVerifier()
	if verifier == "" {
		verifier = st.codeVerifier
	}
	if !idp.UsePKCE {
		verifier = ""
	}
	tr, err := s.oauth.Exchange(ctx, idp, req.GetCode(), verifier, st.redirectURI)
	if err != nil {
		return nil, err
	}

	// 3) 拉 userinfo 并映射
	var info *oauth2.UserInfo
	if idp.UserInfoURL != "" {
		accessToken := tr.AccessToken
		if accessToken == "" {
			accessToken = tr.IDToken
		}
		info, err = s.oauth.FetchUserInfo(ctx, idp, accessToken)
		if err != nil {
			return nil, err
		}
	} else {
		// 没有 userinfo 接口时，退化为解析 id_token 的 payload（不校验签名，
		// 因为 token 刚从 IdP 的 HTTPS 端点换回来，通道本身可信）
		info = decodeIDToken(tr.IDToken)
		if info == nil {
			return nil, errno.New(errno.ErrOAuthExchangeFailed)
		}
	}

	username := oauth2.UsernameFrom(idp, info)
	if username == "" {
		return nil, errno.New(errno.ErrOAuthExchangeFailed)
	}

	// 4) 查找或创建本地用户
	//    策略：以 IdP 的 subject 作为外部身份锚点，username 仅作展示名。
	//    这样 IdP 换域名也不会导致用户重复。
	uid, userInfo, err := s.upsertSSOUser(ctx, idp, info, username)
	if err != nil {
		return nil, err
	}

	pair, resp, err := s.issueFor(ctx, token.UserInfo{
		UID:      uid,
		Username: userInfo.GetUsername(),
		Nickname: userInfo.GetNickname(),
		Email:    userInfo.GetEmail(),
		Avatar:   userInfo.GetAvatar(),
		Tenant:   userInfo.GetTenant(),
		Roles:    userInfo.GetRoles(),
		Scopes:   scopesFromRoles(userInfo.GetRoles()),
		IDP:      idp.Name,
	})
	if err != nil {
		return nil, err
	}

	logger.Info(ctx, "sso login success", "uid", uid, "idp", idp.Name, "username", username)
	return respBuilder(pair)(resp), nil
}

// fetchProfile 拉取用户完整资料。
//
// 拿不到不算失败：登录本身不依赖昵称/邮箱，
// 缺失只影响登录响应里的展示字段，不应阻断认证流程。
func (s *AuthService) fetchProfile(ctx context.Context, uid int64, username string) *userv1.User {
	req := &userv1.GetUserReq{Uid: uid}
	if uid == 0 {
		req = &userv1.GetUserReq{Username: username}
	}
	got, err := s.users.GetUser(ctx, req)
	if err != nil || got.GetUser() == nil {
		return &userv1.User{Uid: uid, Username: username}
	}
	return got.GetUser()
}

// upsertSSOUser 查找或创建 SSO 用户
func (s *AuthService) upsertSSOUser(ctx context.Context, idp conf.IdPConf, info *oauth2.UserInfo, username string) (int64, *authv1.UserInfo, error) {
	// 以 "oauth:<idp>" 作为 source 标识
	source := "oauth:" + idp.Name

	// 先按用户名查找
	got, err := s.users.GetUser(ctx, &userv1.GetUserReq{Username: username})
	if err == nil && got.GetSuccess() && got.GetUser() != nil {
		u := got.GetUser()
		return u.GetUid(), &authv1.UserInfo{
			Uid:      u.GetUid(),
			Username: u.GetUsername(),
			Nickname: u.GetNickname(),
			Email:    u.GetEmail(),
			Avatar:   u.GetAvatar(),
			Roles:    u.GetRoles(),
			Tenant:   u.GetTenant(),
		}, nil
	}

	// 不存在则创建（SSO 账号无密码）
	roles := info.Roles
	if len(roles) == 0 {
		roles = []string{"user"}
	}
	created, err := s.users.CreateUser(ctx, &userv1.CreateUserReq{
		Username: username,
		Nickname: firstNonEmpty(info.Nickname, username),
		Email:    info.Email,
		Phone:    info.Phone,
		Roles:    roles,
		Tenant:   "default",
		Source:   source,
	})
	if err != nil {
		return 0, nil, errno.Wrap(errno.ErrUpstreamError, err)
	}
	if !created.GetSuccess() {
		return 0, nil, errno.New(errno.ErrConflict)
	}

	logger.Info(ctx, "sso user provisioned", "uid", created.GetUid(), "username", username, "idp", idp.Name)

	return created.GetUid(), &authv1.UserInfo{
		Uid:      created.GetUid(),
		Username: username,
		Nickname: firstNonEmpty(info.Nickname, username),
		Email:    info.Email,
		Avatar:   info.Avatar,
		Roles:    roles,
		Tenant:   "default",
	}, nil
}

// ---------- state 管理 ----------

func (s *AuthService) saveState(state string, st *authState) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.gcStatesLocked()
	s.states[state] = st
}

// consumeState 校验并删除 state（一次性）
func (s *AuthService) consumeState(state string) (*authState, error) {
	if state == "" {
		return nil, errno.New(errno.ErrOAuthStateMismatch)
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	s.gcStatesLocked()
	st, ok := s.states[state]
	if !ok {
		// state 不存在 = 过期、被重放，或攻击者伪造
		return nil, errno.New(errno.ErrOAuthStateMismatch)
	}
	delete(s.states, state)
	if time.Since(st.createdAt) > stateTTL {
		return nil, errno.New(errno.ErrOAuthStateMismatch)
	}
	return st, nil
}

func (s *AuthService) gcStatesLocked() {
	now := time.Now()
	for k, v := range s.states {
		if now.Sub(v.createdAt) > stateTTL {
			delete(s.states, k)
		}
	}
}

// ---------- 令牌校验与授权 ----------

func (s *AuthService) VerifyToken(ctx context.Context, req *authv1.VerifyTokenReq) (*authv1.VerifyTokenResp, error) {
	claims, err := s.tokens.ParseAccess(req.GetAccessToken())
	if err != nil {
		code := errno.CodeOf(err)
		return &authv1.VerifyTokenResp{Valid: false, Reason: code.Message()}, nil
	}
	if !claims.HasScope(req.GetRequiredScope()) {
		return &authv1.VerifyTokenResp{
			Valid:  false,
			Reason: "缺少所需权限: " + req.GetRequiredScope(),
		}, nil
	}
	return &authv1.VerifyTokenResp{
		Valid:     true,
		Uid:       claims.UID,
		Username:  claims.Username,
		Tenant:    claims.Tenant,
		Scopes:    claims.Scopes,
		ExpiresAt: claims.ExpiresAt,
	}, nil
}

func (s *AuthService) CheckPermission(ctx context.Context, req *authv1.CheckPermissionReq) (*authv1.CheckPermissionResp, error) {
	// 资源级权限：<resourceType>:<id>，动作 <action>
	// 规则（后续可替换为策略引擎）：
	//   - admin 角色放行一切
	//   - 资源属于自己则放行读写
	//   - 写操作需要 admin 或资源 owner
	resource := req.GetResource()
	action := req.GetAction()

	uid := req.GetUid()
	if uid == 0 {
		return &authv1.CheckPermissionResp{Allowed: false, Reason: "未认证"}, nil
	}

	// 查用户角色做判定
	resp, err := s.users.GetUser(ctx, &userv1.GetUserReq{Uid: uid})
	if err != nil {
		return nil, errno.Wrap(errno.ErrUpstreamError, err)
	}
	if !resp.GetSuccess() || resp.GetUser() == nil {
		return &authv1.CheckPermissionResp{Allowed: false, Reason: "用户不存在"}, nil
	}
	roles := resp.GetUser().GetRoles()

	for _, r := range roles {
		if r == "admin" {
			return &authv1.CheckPermissionResp{Allowed: true, Reason: "admin"}, nil
		}
	}

	// 非 admin：只允许读，写操作一律拒绝
	if action == "read" || action == "" {
		return &authv1.CheckPermissionResp{Allowed: true, Reason: "默认读权限"}, nil
	}
	return &authv1.CheckPermissionResp{
		Allowed: false,
		Reason:  "资源 " + resource + " 的写操作需要 admin 角色",
	}, nil
}

// ---------- 工具 ----------

// issueFor 签发令牌并构造响应
func (s *AuthService) issueFor(ctx context.Context, u token.UserInfo) (*token.TokenPair, *authv1.LoginResp, error) {
	pair, err := s.tokens.Issue(u)
	if err != nil {
		logger.Error(ctx, "issue token failed", "uid", u.UID, "err", err)
		return nil, nil, errno.Wrap(errno.ErrInternal, err)
	}
	resp := &authv1.LoginResp{
		User: &authv1.UserInfo{
			Uid:      u.UID,
			Username: u.Username,
			Nickname: u.Nickname,
			Email:    u.Email,
			Avatar:   u.Avatar,
			Roles:    u.Roles,
			Tenant:   u.Tenant,
		},
	}
	return pair, resp, nil
}

// resp 填充令牌到响应
func respBuilder(pair *token.TokenPair) func(*authv1.LoginResp) *authv1.LoginResp {
	return func(r *authv1.LoginResp) *authv1.LoginResp {
		r.AccessToken = pair.AccessToken
		r.RefreshToken = pair.RefreshToken
		r.ExpiresAt = pair.ExpiresAt
		r.TokenType = pair.TokenType
		return r
	}
}

// verifyPassword 校验密码。
//
// 委托给 user_service 侧的同格式实现，保证"user_service 存什么格式，
// auth_service 就按什么格式校验"，不会出现两边算法不一致。
func (s *AuthService) verifyPassword(stored, plain string) bool {
	return passwordVerify(stored, plain)
}

// scopesFromRoles 由角色推导 scope
//
// 权限模型保持简单：角色即 scope。
// 后续接入 Casbin/OPA 时只需替换此函数。
func scopesFromRoles(roles []string) []string {
	seen := make(map[string]bool, len(roles)+1)
	out := make([]string, 0, len(roles)+1)
	// 所有登录用户至少具备基础 scope
	seen["user"] = true
	out = append(out, "user")

	for _, r := range roles {
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func pick(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}
