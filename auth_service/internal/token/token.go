// Package token 负责本系统 JWT 的签发与校验。
package token

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/aegis/pkg/conf"
	"github.com/aegis/pkg/errno"
	"github.com/aegis/pkg/jwtx"
	"github.com/aegis/pkg/mockx"
)

// revokedEntry 吊销记录
type revokedEntry struct {
	uid int64
	exp time.Time
}

// Manager 令牌管理器
type Manager struct {
	signer *jwtx.Signer

	issuer     string
	accessTTL  time.Duration
	refreshTTL time.Duration
	jwtSecret  []byte // HS256 兜底

	// 吊销名单：登出后即使 JWT 未过期也不可用。
	//
	// 这是"登出即失效"的实现方式，比单纯依赖过期时间更安全。
	mu      sync.RWMutex
	revoked map[string]revokedEntry // jti -> 吊销记录

	// issued 记录每个用户当前在用的全部令牌（uid -> jti -> 过期时间）。
	//
	// ★ 为什么需要它：
	//   LogoutReq 只带 access_token 与 uid，refresh token 并不在请求里，
	//   要让"登出"真正彻底，就必须能按用户找到全部令牌一并吊销。
	//   没有这张索引就只能全表扫描 revoked —— 而没被吊销过的令牌
	//   根本不在里面，扫了也找不到。
	issued map[int64]map[string]time.Time
}

// Options 构造参数
type Options struct {
	Signer     *jwtx.Signer
	Issuer     string
	AccessTTL  time.Duration
	RefreshTTL time.Duration
	JWTSecret  string
}

// New 构造 Manager
func New(opt Options) (*Manager, error) {
	if opt.Signer == nil {
		return nil, errno.New(errno.ErrInvalidParam)
	}
	ttl := opt.AccessTTL
	if ttl <= 0 {
		ttl = 2 * time.Hour
	}
	refreshTTL := opt.RefreshTTL
	if refreshTTL <= 0 {
		refreshTTL = 7 * 24 * time.Hour
	}
	return &Manager{
		signer:     opt.Signer,
		issuer:     opt.Issuer,
		accessTTL:  ttl,
		refreshTTL: refreshTTL,
		jwtSecret:  []byte(opt.JWTSecret),
		revoked:    make(map[string]revokedEntry),
		issued:     make(map[int64]map[string]time.Time),
	}, nil
}

// UserInfo 令牌主体信息
type UserInfo struct {
	UID      int64
	Username string
	Nickname string
	Email    string
	Avatar   string
	Tenant   string
	Roles    []string
	Scopes   []string
	// IDP 来源（SSO 登录时记录，如 "keycloak"；本地登录为空）
	IDP string
}

// TokenPair 签发结果
type TokenPair struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64
	TokenType    string
}

// Issue 签发一对令牌
//
// access 与 refresh 都是 JWT，但通过 token_use 区分用途。
// refresh token 有效期更长，且必须匹配 token_use=refresh 才能换新 access。
// 这样即使用户拿 refresh token 直接调业务接口也会被拒。
func (m *Manager) Issue(u UserInfo) (*TokenPair, error) {
	access, err := m.sign(u, "access", m.accessTTL)
	if err != nil {
		return nil, err
	}
	refresh, err := m.sign(u, "refresh", m.refreshTTL)
	if err != nil {
		return nil, err
	}
	return &TokenPair{
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresAt:    time.Now().Add(m.accessTTL).Unix(),
		TokenType:    "Bearer",
	}, nil
}

func (m *Manager) sign(u UserInfo, use string, ttl time.Duration) (string, error) {
	exp := time.Now().Add(ttl)
	claims := &jwtx.Claims{
		Issuer:    m.issuer,
		Subject:   fmt.Sprintf("%d", u.UID),
		ExpiresAt: exp.Unix(),
		IssuedAt:  time.Now().Unix(),
		UID:       u.UID,
		Username:  u.Username,
		Tenant:    u.Tenant,
		Roles:     u.Roles,
		Scopes:    u.Scopes,
		TokenUse:  use,
		Issuer2:   u.IDP,
	}
	token, err := m.signer.Sign(claims, ttl)
	if err != nil {
		return "", err
	}
	// 登记到在用令牌索引：登出时要靠它找到该用户的全部令牌
	m.trackIssued(claims.ID, u.UID, claims.ExpiresAt)
	return token, nil
}

// Parse 解析并校验令牌
func (m *Manager) Parse(tokenStr string) (*jwtx.Claims, error) {
	if tokenStr == "" {
		return nil, errno.New(errno.ErrUnauthorized)
	}
	claims, err := m.signer.Verify(tokenStr)
	if err != nil {
		switch {
		case errors.Is(err, jwtx.ErrTokenExpired):
			return nil, errno.Wrap(errno.ErrTokenExpired, err)
		default:
			return nil, errno.Wrap(errno.ErrUnauthorized, err)
		}
	}
	if m.isRevoked(claims.ID) {
		return nil, errno.New(errno.ErrUnauthorized)
	}
	return claims, nil
}

// ParseAccess 解析 access token 并校验用途
func (m *Manager) ParseAccess(tokenStr string) (*jwtx.Claims, error) {
	claims, err := m.Parse(tokenStr)
	if err != nil {
		return nil, err
	}
	if claims.TokenUse != "access" {
		return nil, errno.New(errno.ErrUnauthorized)
	}
	return claims, nil
}

// ParseRefresh 解析 refresh token 并校验用途
func (m *Manager) ParseRefresh(tokenStr string) (*jwtx.Claims, error) {
	claims, err := m.Parse(tokenStr)
	if err != nil {
		return nil, err
	}
	if claims.TokenUse != "refresh" {
		return nil, errno.New(errno.ErrUnauthorized)
	}
	return claims, nil
}

// Revoke 加入吊销名单。登出后即使 JWT 未过期也不再可用。
func (m *Manager) Revoke(tokenStr string) {
	claims, err := m.signer.Verify(tokenStr)
	if err != nil {
		return
	}
	m.trackRevoked(claims.ID, claims.UID, claims.ExpiresAt)
}

// RevokeAllOf 吊销某用户的全部在用令牌。
//
// 登出场景用：LogoutReq 只带 access_token 与 uid，
// 但 refresh token 必须一并失效 —— 否则用户点了退出登录，
// 攻击者手上的 refresh token 仍能换出新令牌，登出形同虚设。
func (m *Manager) RevokeAllOf(uid int64) {
	if uid == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	// 先扫在用令牌索引：从没被吊销过的令牌也要作废。
	// 注意不能边遍历边删 —— Go 的 range 语义下删当前键会中断本轮循环。
	var expired []string
	for jti, exp := range m.issued[uid] {
		if now.After(exp) {
			// 已自然过期，索引里就是垃圾，顺手清掉
			expired = append(expired, jti)
			continue
		}
		m.putRevokedLocked(jti, uid, exp)
	}
	for _, jti := range expired {
		m.dropIssuedLocked(uid, jti)
	}
	// 再兜一遍吊销名单：覆盖"签发索引已被清理但令牌仍在吊销期"的边界
	var dead []string
	for jti, e := range m.revoked {
		if e.uid == uid && now.After(e.exp) {
			dead = append(dead, jti)
		}
	}
	for _, jti := range dead {
		m.dropRevokedLocked(jti)
	}
}

// trackIssued 把签发的令牌登记到"在用令牌"索引
func (m *Manager) trackIssued(jti string, uid int64, exp int64) {
	if jti == "" || uid == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.issued[uid] == nil {
		m.issued[uid] = make(map[string]time.Time)
	}
	m.issued[uid][jti] = time.Unix(exp, 0)
	m.gcLocked()
}

// trackRevoked 把 jti 记入吊销名单
func (m *Manager) trackRevoked(jti string, uid int64, exp int64) {
	if jti == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.putRevokedLocked(jti, uid, time.Unix(exp, 0))
}

// putRevokedLocked 写入吊销记录（调用方必须持写锁）
func (m *Manager) putRevokedLocked(jti string, uid int64, exp time.Time) {
	m.revoked[jti] = revokedEntry{uid: uid, exp: exp}
}

// dropRevokedLocked 删除吊销记录（调用方必须持写锁）
func (m *Manager) dropRevokedLocked(jti string) {
	delete(m.revoked, jti)
}

// dropIssuedLocked 从在用索引移除（调用方必须持写锁）
func (m *Manager) dropIssuedLocked(uid int64, jti string) {
	set, ok := m.issued[uid]
	if !ok {
		return
	}
	delete(set, jti)
	if len(set) == 0 {
		delete(m.issued, uid)
	}
}

// gcLocked 清理已过期的索引条目，防止长期运行后无限增长。
//
// 调用方必须持写锁。成本与在用会话数成正比 —— 本系统会话数是
// "用户数 × 设备数" 量级，扫一遍可以忽略，但它换来了不需要额外
// janitor goroutine 与停机协作的 simplicity。
func (m *Manager) gcLocked() {
	now := time.Now()
	// ★ 先收集再删除，不能边遍历边删：
	//   Go 的 range 语义下删除当前键会让本轮循环提前终止，
	//   同一 map 里剩下的过期条目会被漏掉，GC 永远清不干净。
	var deadUIDs []int64
	for uid, set := range m.issued {
		for jti, exp := range set {
			if now.After(exp) {
				m.dropIssuedLocked(uid, jti)
			}
		}
		if len(set) == 0 {
			deadUIDs = append(deadUIDs, uid)
		}
	}
	for _, uid := range deadUIDs {
		delete(m.issued, uid)
	}

	var deadJTIs []string
	for jti, e := range m.revoked {
		if now.After(e.exp) {
			deadJTIs = append(deadJTIs, jti)
		}
	}
	for _, jti := range deadJTIs {
		m.dropRevokedLocked(jti)
	}
}

func (m *Manager) isRevoked(jti string) bool {
	if jti == "" {
		return false
	}
	m.mu.RLock()
	e, ok := m.revoked[jti]
	m.mu.RUnlock()
	if !ok {
		return false
	}
	if time.Now().After(e.exp) {
		// 已过期，惰性清理
		m.mu.Lock()
		m.dropRevokedLocked(jti)
		m.mu.Unlock()
		return false
	}
	return true
}

// AccessTTL 返回 access token 有效期（供 LoginResp 填充）
func (m *Manager) AccessTTL() time.Duration { return m.accessTTL }

// ---------- 登录失败锁定 ----------

// LoginGuard 控制登录失败次数与锁定
//
// 为什么要自己维护而不是靠 Redis：
//
//	mock 模式无 Redis；且锁定状态短生命周期，本地 map 足够。
//	生产多实例部署时必须换成 Redis，见 README 的部署说明。
type LoginGuard struct {
	mu       sync.Mutex
	failures map[string]int
	lockedAt map[string]time.Time

	maxFailures  int
	lockFor      time.Duration
	captchaAfter int
}

// NewLoginGuard 构造登录守卫
func NewLoginGuard(maxFailures int, lockFor time.Duration, captchaAfter int) *LoginGuard {
	if maxFailures <= 0 {
		maxFailures = 5
	}
	if lockFor <= 0 {
		lockFor = 15 * time.Minute
	}
	if captchaAfter <= 0 {
		captchaAfter = 3
	}
	return &LoginGuard{
		failures:     make(map[string]int),
		lockedAt:     make(map[string]time.Time),
		maxFailures:  maxFailures,
		lockFor:      lockFor,
		captchaAfter: captchaAfter,
	}
}

// ErrLocked 账号被临时锁定
type ErrLocked struct {
	Until time.Time
}

func (e *ErrLocked) Error() string {
	return fmt.Sprintf("账号已锁定，请于 %s 后重试", e.Until.Format("15:04:05"))
}

// ErrCaptcha 连续失败需要验证码
type ErrCaptcha struct{ Failures int }

func (e *ErrCaptcha) Error() string {
	return fmt.Sprintf("连续失败 %d 次，需要验证码", e.Failures)
}

// Check 登录前检查锁定状态与是否需要验证码
func (g *LoginGuard) Check(username string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	key := normalizeKey(username)
	if until, ok := g.lockedAt[key]; ok {
		if time.Now().Before(until) {
			return &ErrLocked{Until: until}
		}
		// 锁定已过期，清理
		delete(g.lockedAt, key)
		delete(g.failures, key)
	}

	if n := g.failures[key]; n >= g.captchaAfter {
		return &ErrCaptcha{Failures: n}
	}
	return nil
}

// Fail 记录一次失败
func (g *LoginGuard) Fail(username string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	key := normalizeKey(username)
	g.failures[key]++
	n := g.failures[key]

	if n >= g.maxFailures {
		until := time.Now().Add(g.lockFor)
		g.lockedAt[key] = until
		return &ErrLocked{Until: until}
	}
	return nil
}

// Success 登录成功，清空计数
func (g *LoginGuard) Success(username string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	key := normalizeKey(username)
	delete(g.failures, key)
	delete(g.lockedAt, key)
}

// FailCount 返回当前失败次数（供测试断言）
func (g *LoginGuard) FailCount(username string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.failures[normalizeKey(username)]
}

// NeedsCaptcha 判断该用户名是否已达到验证码阈值（且尚未锁定）。
//
// 供调用方在密码校验失败后决定响应码：达到阈值应提示验证码，
// 但失败计数**不冻结** —— 由 Fail 继续累计直至 maxFailures 触发锁定。
func (g *LoginGuard) NeedsCaptcha(username string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	key := normalizeKey(username)
	if _, ok := g.lockedAt[key]; ok {
		return false // 已锁定：走 ErrLocked 路径，不再提示验证码
	}
	return g.failures[key] >= g.captchaAfter
}

// normalizeKey 把用户名归一化为稳定的 map key。
//
// 用哈希而非明文：避免明文用户名在内存 map 中长期驻留，
// 也避免 map key 出现在内存 dump 或调试日志中。
func normalizeKey(s string) string {
	return strconv.FormatInt(mockx.HashID("", s), 10)
}

// ---------- 配置文件密钥加载 ----------

// LoadSigner 依据配置装载签名器。
//
// 三种情形：
//  1. 配置了 RS256 私钥 → 生产路径
//  2. 只配了 HS256 secret → 本地开发路径
//  3. 都没配 → 自动生成 RS256 密钥并缓存到磁盘（仅本地/冒烟）
//
// LoadSigner 构造 JWT 签名器。
//
// ★ iss / aud 期望值必须在这里注入Signer，
//
//	否则 Verify 无从校验（历史上正是漏了这一步，
//	导致外部 IdP 的令牌与跨服务令牌都能通过校验）。
func LoadSigner(ctx context.Context, c conf.AuthConf, keyDir string) (*jwtx.Signer, error) {
	// 本平台所有服务的受众
	audience := c.Audience
	if len(audience) == 0 {
		audience = []string{"aegis"}
	}

	opts := jwtx.Options{
		Issuer:   c.Issuer,
		Audience: audience,
	}

	if c.JWTPrivateKey != "" {
		opts.PrivateKeyPEM = []byte(c.JWTPrivateKey)
		opts.PublicKeyPEM = []byte(c.JWTPublicKey)
		opts.KeyID = "primary"
		return jwtx.NewSigner(opts)
	}
	if c.JWTSecret != "" {
		opts.Algorithm = jwtx.HS256
		opts.HMACSecret = []byte(c.JWTSecret)
		opts.KeyID = "dev-hs256"
		return jwtx.NewSigner(opts)
	}

	// 自动生成并落盘：重启后已签发的 JWT 仍有效
	priv, pub, err := jwtx.EnsureKeyPair(keyDir, 2048)
	if err != nil {
		return nil, err
	}
	opts.PrivateKeyPEM = priv
	opts.PublicKeyPEM = pub
	opts.KeyID = "auto"
	return jwtx.NewSigner(opts)
}
