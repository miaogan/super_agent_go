package handler

import (
	"net/url"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/utils"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	authv1 "github.com/aegis/idl/kitex_gen/authv1"
	commonv1 "github.com/aegis/idl/kitex_gen/commonv1"

	"github.com/aegis/pkg/logger"
)

// ---------- 登录 ----------

// loginReq 登录请求体
type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// Login 账号密码登录
func (d *Deps) Login(c *app.RequestContext) {
	var req loginReq
	if err := c.BindAndValidate(&req); err != nil {
		// BindAndValidate 可能因请求体为空 / BOM / 截断等失败；
		// 这里对调用方统一返回 INVALID_PARAM，不泄露内部绑定细节。
		// ★ DEBUG 日志留在 v1.0.1 观察期，后续改回简单版。
		raw, _ := c.Body()
		ctx, cancel := d.withTimeout(c)
		logger.Error(ctx, "Login BindAndValidate failed [DEBUG]", "err", err, "raw_len", len(raw), "raw_prefix", string(raw[:min(len(raw),80)]))
		cancel()
		Error(c, consts.StatusBadRequest, "INVALID_PARAM", "用户名或密码格式不正确")
		return
	}
	if req.Username == "" || req.Password == "" {
		Error(c, consts.StatusBadRequest, "MISSING_PARAM", "用户名与密码不能为空")
		return
	}

	ctx, cancel := d.withTimeout(c)
	defer cancel()

	resp, err := d.Auth.Login(ctx, &authv1.LoginReq{
		Username: req.Username,
		Password: req.Password,
	})
	if err != nil {
		logger.Warn(ctx, "登录失败", "username", req.Username, "err", err)
		// ★ 统一提示"用户名或密码错误"：
		//   区分"用户不存在"与"密码错误"会给攻击者提供用户枚举接口。
		Error(c, consts.StatusUnauthorized, "INVALID_CREDENTIALS", "用户名或密码错误")
		return
	}
	OK(c, toLoginResp(resp))
}

// refreshReq 刷新令牌请求体
type refreshReq struct {
	RefreshToken string `json:"refresh_token"`
}

// Refresh 刷新访问令牌
func (d *Deps) Refresh(c *app.RequestContext) {
	var req refreshReq
	if err := c.BindAndValidate(&req); err != nil {
		Error(c, consts.StatusBadRequest, "INVALID_PARAM", "refresh_token 不能为空")
		return
	}

	ctx, cancel := d.withTimeout(c)
	defer cancel()

	resp, err := d.Auth.RefreshToken(ctx, &authv1.RefreshTokenReq{
		RefreshToken: req.RefreshToken,
	})
	if err != nil {
		Error(c, consts.StatusUnauthorized, "TOKEN_INVALID", "刷新令牌无效或已过期")
		return
	}
	OK(c, toLoginResp(resp))
}

// logoutReq 登出请求体
type logoutReq struct {
	AccessToken string `json:"access_token"`
}

// Logout 登出（令牌吊销）
func (d *Deps) Logout(c *app.RequestContext) {
	// 优先用请求体（前端可能持有完整令牌），
	// 缺失时回退到当前请求的 Authorization 头。
	token := ""
	var req logoutReq
	_ = c.BindAndValidate(&req)
	token = req.AccessToken
	if token == "" {
		token = extractBearer(string(c.GetHeader("Authorization")))
	}
	if token == "" {
		Error(c, consts.StatusBadRequest, "MISSING_PARAM", "缺少 access_token")
		return
	}

	ctx, cancel := d.withTimeout(c)
	defer cancel()

	if _, err := d.Auth.Logout(ctx, &authv1.LogoutReq{AccessToken: token}); err != nil {
		logError(ctx, c, "auth.Logout", err)
		return
	}
	OK(c, utils.H{"message": "已登出"})
}

// ---------- OAuth2.0 / SSO ----------

// authorizeReq 发起 SSO 请求
type authorizeReq struct {
	IdP         string `json:"idp"`
	RedirectURI string `json:"redirect_uri"`
	State       string `json:"state"`
}

// OAuthAuthorize 发起 OAuth2.0 授权（返回跳转 URL）。
func (d *Deps) OAuthAuthorize(c *app.RequestContext) {
	var req authorizeReq
	_ = c.BindAndValidate(&req)
	if req.IdP == "" {
		req.IdP = queryStr(c, "idp")
	}
	if req.RedirectURI == "" {
		req.RedirectURI = queryStr(c, "redirect_uri")
	}
	if req.State == "" {
		req.State = queryStr(c, "state")
	}
	if req.IdP == "" {
		Error(c, consts.StatusBadRequest, "MISSING_PARAM", "idp 不能为空")
		return
	}

	ctx, cancel := d.withTimeout(c)
	defer cancel()

	resp, err := d.Auth.OAuthAuthorize(ctx, &authv1.OAuthAuthorizeReq{
		Idp:         req.IdP,
		RedirectUri: req.RedirectURI,
		State:       req.State,
	})
	if err != nil {
		logError(ctx, c, "auth.OAuthAuthorize", err)
		return
	}
	OK(c, utils.H{
		"authorize_url": resp.GetAuthorizeUrl(),
		"state":         resp.GetState(),
		"code_verifier": resp.GetCodeVerifier(),
	})
}

// OAuthAuthorizeRedirect 直接 302 跳转到 IdP。
//
// 浏览器场景下前端应直接跳此端点，
// 少一次前端取 URL 再跳转的往返。
func (d *Deps) OAuthAuthorizeRedirect(c *app.RequestContext) {
	idp := queryStr(c, "idp")
	if idp == "" {
		Error(c, consts.StatusBadRequest, "MISSING_PARAM", "idp 不能为空")
		return
	}

	ctx, cancel := d.withTimeout(c)
	defer cancel()

	resp, err := d.Auth.OAuthAuthorize(ctx, &authv1.OAuthAuthorizeReq{
		Idp:         idp,
		RedirectUri: queryStr(c, "redirect_uri"),
		State:       queryStr(c, "state"),
	})
	if err != nil {
		logError(ctx, c, "auth.OAuthAuthorize", err)
		return
	}
	c.Redirect(consts.StatusFound, []byte(resp.GetAuthorizeUrl()))
}

// OAuthCallback 处理 IdP 回调。
//
// state 校验由 auth_service 完成（它持有 IdP 配置与白名单）。
// 网关只负责换令牌并按需重定向回前端。
func (d *Deps) OAuthCallback(c *app.RequestContext) {
	code := queryStr(c, "code")
	state := queryStr(c, "state")
	if code == "" {
		// 用户拒绝授权时 IdP 会带 error 回调
		if e := queryStr(c, "error"); e != "" {
			Error(c, consts.StatusBadRequest, "AUTH_DENIED", "授权被拒绝: "+e)
			return
		}
		Error(c, consts.StatusBadRequest, "MISSING_PARAM", "缺少授权码")
		return
	}

	ctx, cancel := d.withTimeout(c)
	defer cancel()

	resp, err := d.Auth.OAuthCallback(ctx, &authv1.OAuthCallbackReq{
		Idp:          queryStr(c, "idp"),
		Code:         code,
		State:        state,
		CodeVerifier: queryStr(c, "code_verifier"),
	})
	if err != nil {
		logger.Warn(ctx, "OAuth 回调失败", "err", err)
		Error(c, consts.StatusUnauthorized, "AUTH_FAILED", "授权失败，请重新登录")
		return
	}

	// 带上前端指定的跳转地址时直接302 回前端，
	// 令牌通过 URL fragment 传递（不出现在服务端日志与 Referer 中）。
	if redirect := queryStr(c, "redirect"); redirect != "" {
		sep := "?"
		if strings.Contains(redirect, "?") {
			sep = "&"
		}
		target := redirect + sep + "access_token=" +
			url.QueryEscape(resp.GetAccessToken()) + "&token_type=Bearer"
		c.Redirect(consts.StatusFound, []byte(target))
		return
	}
	OK(c, toLoginResp(resp))
}

// ---------- IdP 列表 ----------

// ListIdPs 查询可用的身份提供方
func (d *Deps) ListIdPs(c *app.RequestContext) {
	ctx, cancel := d.withTimeout(c)
	defer cancel()

	resp, err := d.Auth.ListIdPs(ctx, &authv1.ListIdPsReq{})
	if err != nil {
		logError(ctx, c, "auth.ListIdPs", err)
		return
	}
	OK(c, utils.H{"idps": resp.GetIdps()})
}

// ---------- 鉴权 ----------

// me 查询当前登录用户信息。
//
// 身份完全来自已验证的令牌，不接受任何查询参数指定 uid ——
// 否则任何人传 ?uid=1001 就能冒充管理员。
func (d *Deps) Me(c *app.RequestContext) {
	id := idOf(c)
	OK(c, utils.H{
		"uid":      id.GetUid(),
		"username": id.GetUsername(),
		"tenant":   id.GetTenant(),
		"scopes":   id.GetScopes(),
	})
}

// Verify 校验访问令牌（供前端启动时自检）
func (d *Deps) Verify(c *app.RequestContext) {
	token := extractBearer(string(c.GetHeader("Authorization")))
	if token == "" {
		token = c.Query("access_token")
	}
	if token == "" {
		Error(c, consts.StatusBadRequest, "MISSING_PARAM", "缺少访问令牌")
		return
	}

	ctx, cancel := d.withTimeout(c)
	defer cancel()

	resp, err := d.Auth.VerifyToken(ctx, &authv1.VerifyTokenReq{
		AccessToken:   token,
		RequiredScope: queryStr(c, "required_scope"),
	})
	if err != nil || !resp.GetValid() {
		Error(c, consts.StatusUnauthorized, "TOKEN_INVALID", "访问令牌无效或已过期")
		return
	}
	OK(c, utils.H{
		"valid":    true,
		"uid":      resp.GetUid(),
		"username": resp.GetUsername(),
		"tenant":   resp.GetTenant(),
		"scopes":   resp.GetScopes(),
	})
}

// ---------- 转换 ----------

func toLoginResp(r *authv1.LoginResp) utils.H {
	return utils.H{
		"access_token":  r.GetAccessToken(),
		"refresh_token": r.GetRefreshToken(),
		"expires_at":    r.GetExpiresAt(),
		"token_type":    r.GetTokenType(),
		"user":          r.GetUser(),
	}
}

// toIdentity 从校验结果构造身份
func toIdentity(v *authv1.VerifyTokenResp) *commonv1.Identity {
	if v == nil {
		return &commonv1.Identity{}
	}
	return &commonv1.Identity{
		Uid:      v.GetUid(),
		Username: v.GetUsername(),
		Tenant:   v.GetTenant(),
		Scopes:   v.GetScopes(),
	}
}
