// Package jwtx 实现自签 JWT（RS256）。
//
// 为什么自己实现而不用 golang-jwt：
//  1. 需求只要 RS256 + 少量 claims，自己实现可控且零依赖膨胀
//  2. 认证是全平台安全底座，依赖越少攻击面越小
//  3. 与 ADR 中"不自行建设完整 OIDC Provider"的边界一致：
//     我们只做"签发 + 校验"，不做发现文档、动态注册等 IdP 职责
//
// 生产用 RS256（非对称：auth_service 持私钥，其余服务只拿公钥）。
// 本地开发允许 HS256 兜底，此时所有服务共享同一 secret。
package jwtx

import (
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aegis/pkg/jsonx"
)

// 错误定义
var (
	ErrTokenMalformed = errors.New("jwt: token 格式非法")
	ErrTokenSignature = errors.New("jwt: 签名校验失败")
	ErrTokenExpired   = errors.New("jwt: 已过期")
	ErrTokenNotYet    = errors.New("jwt: 尚未生效")
	ErrKeyUnavailable = errors.New("jwt: 签名密钥不可用")
	ErrUnsupportedAlg = errors.New("jwt: 不支持的签名算法")
	// ErrWrongIssuer 签发方不匹配：常见于外部 IdP 的令牌被误当作本平台令牌
	ErrWrongIssuer = errors.New("jwt: 签发方不匹配")
	// ErrWrongAudience 受众不匹配：常见于把为 A 服务签发的令牌用于 B 服务
	ErrWrongAudience = errors.New("jwt: 受众不匹配")
)

// Algorithm 签名算法
type Algorithm string

const (
	RS256 Algorithm = "RS256"
	HS256 Algorithm = "HS256"
)

// Claims 自定义负载。
//
// 注意：标准 claims（iss/sub/aud/exp/iat/nbf/jti）由本包统一管理，
// 业务方只填 Extra，避免各处手写时间戳出错。
type Claims struct {
	Issuer    string   `json:"iss,omitempty"`
	Subject   string   `json:"sub,omitempty"`
	Audience  []string `json:"aud,omitempty"`
	ExpiresAt int64    `json:"exp,omitempty"`
	NotBefore int64    `json:"nbf,omitempty"`
	IssuedAt  int64    `json:"iat,omitempty"`
	ID        string   `json:"jti,omitempty"`

	// 业务字段
	UID      int64    `json:"uid,omitempty"`
	Username string   `json:"username,omitempty"`
	Tenant   string   `json:"tenant,omitempty"`
	Roles    []string `json:"roles,omitempty"`
	Scopes   []string `json:"scopes,omitempty"`
	TokenUse string   `json:"token_use,omitempty"` // access / refresh
	Issuer2  string   `json:"idp,omitempty"`       // 来源 IdP（SSO 登录时记录）

	Extra map[string]any `json:"extra,omitempty"`
}

type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid,omitempty"`
}

// Signer 负责签发与校验。
//
// 并发安全：内部只读，无状态。
type Signer struct {
	alg Algorithm
	rsa *rsa.PrivateKey
	pub *rsa.PublicKey
	// hs 用于 HS256 兜底（本地开发）
	hs  []byte
	kid string

	// 校验期的期望值。Verify 时强制比对，防止令牌跨服务/跨 IdP 复用。
	// 零值表示不校验（仅建议在测试中这样用）。
	wantIssuer   string
	wantAudience []string
}

// Options 构造参数
type Options struct {
	// Algorithm 缺省时按是否有私钥自动选 RS256 / HS256
	Algorithm Algorithm

	// RS256：PEM 内容或文件路径（传路径时自动读取）
	PrivateKeyPEM []byte
	PublicKeyPEM  []byte

	// HS256：共享密钥（长度需 >= 32 字节）
	HMACSecret []byte

	// KeyID 写入 JWT header，便于将来轮换多把密钥
	KeyID string

	// Issuer 期望的签发方。Verify 时令牌 iss 不匹配则拒绝。
	// 生产环境必须配置 —— 它是防止"外部 IdP 的令牌被当成本平台令牌"的关键。
	Issuer string

	// Audience 期望的受众。Verify 时令牌 aud 与之无交集则拒绝。
	// 用于防止把为 A 服务签发的令牌拿去访问 B 服务。
	Audience []string
}

// LoadPEM 支持直接传 PEM 内容或文件路径，方便本地与 K8s Secret 两种投放方式。
func LoadPEM(v string) ([]byte, error) {
	if v == "" {
		return nil, nil
	}
	if strings.Contains(v, "-----BEGIN") {
		return []byte(v), nil
	}
	return os.ReadFile(v)
}

// NewSigner 构造 Signer
func NewSigner(opt Options) (*Signer, error) {
	s := &Signer{alg: opt.Algorithm, kid: opt.KeyID}
	s.wantIssuer = opt.Issuer
	s.wantAudience = append([]string(nil), opt.Audience...)

	privPEM, err := LoadPEM(string(opt.PrivateKeyPEM))
	if err != nil {
		return nil, fmt.Errorf("jwt: 读取私钥: %w", err)
	}
	pubPEM, err := LoadPEM(string(opt.PublicKeyPEM))
	if err != nil {
		return nil, fmt.Errorf("jwt: 读取公钥: %w", err)
	}

	if len(privPEM) > 0 {
		key, err := parseRSAPrivateKey(privPEM)
		if err != nil {
			return nil, err
		}
		s.rsa = key
		s.pub = &key.PublicKey
	}

	// 校验服务可独立部署：只持公钥也能校验
	if len(pubPEM) > 0 && s.pub == nil {
		key, err := parseRSAPublicKey(pubPEM)
		if err != nil {
			return nil, err
		}
		s.pub = key
	}

	if len(opt.HMACSecret) > 0 {
		if len(opt.HMACSecret) < 32 {
			return nil, errors.New("jwt: HS256 密钥长度不足 32 字节")
		}
		s.hs = opt.HMACSecret
	}

	// ★ 只有公钥时也必须能推断出 RS256：
	//   校验服务（gateway / 各业务服务）按设计只拿公钥，
	//   若alg 仅在有私钥时才推断，校验方将无法构造 Signer，
	//   "校验可独立部署"就成了一句空话。
	if s.alg == "" {
		if s.rsa != nil {
			s.alg = RS256
		} else if s.pub != nil {
			s.alg = RS256
		} else if s.hs != nil {
			s.alg = HS256
		}
	}

	switch s.alg {
	case RS256:
		// 校验只需公钥，签发才需要私钥
		if s.pub == nil {
			return nil, ErrKeyUnavailable
		}
	case HS256:
		if s.hs == nil {
			return nil, ErrKeyUnavailable
		}
	default:
		return nil, ErrUnsupportedAlg
	}

	if s.kid == "" {
		s.kid = string(s.alg)
	}
	return s, nil
}

// Alg 返回当前签名算法
func (s *Signer) Alg() Algorithm { return s.alg }

func parseRSAPrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("jwt: 私钥 PEM 解析失败")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	anyKey, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("jwt: 私钥解析失败: %w", err)
	}
	key, ok := anyKey.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("jwt: 私钥不是 RSA 类型")
	}
	return key, nil
}

func parseRSAPublicKey(pemBytes []byte) (*rsa.PublicKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("jwt: 公钥 PEM 解析失败")
	}
	if key, err := x509.ParsePKCS1PublicKey(block.Bytes); err == nil {
		return key, nil
	}
	anyKey, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("jwt: 公钥解析失败: %w", err)
	}
	key, ok := anyKey.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("jwt: 公钥不是 RSA 类型")
	}
	return key, nil
}

// Sign 签发令牌。
//
// 调用方只需填业务字段，标准 claims 由本方法补齐。
func (s *Signer) Sign(c *Claims, ttl time.Duration) (string, error) {
	now := time.Now()
	if c.IssuedAt == 0 {
		c.IssuedAt = now.Unix()
	}
	if c.ExpiresAt == 0 {
		c.ExpiresAt = now.Add(ttl).Unix()
	}
	if c.NotBefore == 0 {
		c.NotBefore = c.IssuedAt
	}
	if c.ID == "" {
		c.ID = randHex(12)
	}
	// ★ 默认填入本平台的 iss / aud。
	//   否则"签发时按平台身份、校验时按平台身份"的两端永远对不上，
	//   轻则全部令牌被拒，重则校验形同虚设（调用方会索性不校验）。
	//   调用方仍可显式覆盖，但不能是"忘了填"这种默认值。
	if c.Issuer == "" {
		c.Issuer = s.wantIssuer
	}
	if len(c.Audience) == 0 && len(s.wantAudience) > 0 {
		c.Audience = append([]string(nil), s.wantAudience...)
	}

	// ★ 这里必须走 jsonx（统一门面）而不是直接 import encoding/json。
	//
	//   原因：header 与 claims 序列化后的字节是签名的输入，
	//   任何字节差异都会导致签名变化。若签发侧用 sonic、校验侧用标准库
	//   （或反之，且配置导致 map 序/escape 行为不同），
	//   就会出现「本服务签的令牌被自己拒绝」。
	//
	//   jsonx 内部已把 sonic 配置为 ConfigStd（EscapeHTML + SortMapKeys），
	//   保证两个引擎产出字节一致 —— 见 pkg/jsonx 的包注释。
	hdr := header{Alg: string(s.alg), Typ: "JWT", Kid: s.kid}
	hb, err := jsonx.Marshal(hdr)
	if err != nil {
		return "", err
	}
	cb, err := jsonx.Marshal(c)
	if err != nil {
		return "", err
	}
	signing := b64(hb) + "." + b64(cb)

	sig, err := s.sign(signing)
	if err != nil {
		return "", err
	}
	return signing + "." + b64(sig), nil
}

func (s *Signer) sign(payload string) ([]byte, error) {
	switch s.alg {
	case RS256:
		// 只持公钥的服务可以校验，但不能签发
		if s.rsa == nil {
			return nil, ErrKeyUnavailable
		}
		sum := sha256.Sum256([]byte(payload))
		return rsa.SignPKCS1v15(rand.Reader, s.rsa, crypto.SHA256, sum[:])
	case HS256:
		mac := hmac.New(sha256.New, s.hs)
		mac.Write([]byte(payload))
		return mac.Sum(nil), nil
	default:
		return nil, ErrUnsupportedAlg
	}
}

// Verify 校验并解析令牌。
//
// ★ 安全要点：alg 由服务端配置决定，绝不信任 token header 里的 alg，
// 否则会踩 "alg=none" 与 RS256→HS256 混淆攻击。
func (s *Signer) Verify(token string) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, ErrTokenMalformed
	}

	hb, err := unb64(parts[0])
	if err != nil {
		return nil, ErrTokenMalformed
	}
	var hdr header
	if err := jsonx.Unmarshal(hb, &hdr); err != nil {
		return nil, ErrTokenMalformed
	}
	if hdr.Alg != string(s.alg) {
		return nil, ErrUnsupportedAlg
	}
	// ★ kid 刻意不做硬校验。
	//
	//   kid 的用途是"告诉校验方该用哪把密钥"。多密钥并存时
	//   正确做法是按 kid 选密钥后逐一尝试，而不是因kid 不同就拒绝 ——
	//   否则密钥轮换窗口内新旧令牌会互相失效。
	//
	//   kid 不匹配时这里不报错，交由上层多密钥链处理；
	//   若验签失败，错误信息已足够定位（ErrTokenSignature）。
	_ = hdr.Kid

	sig, err := unb64(parts[2])
	if err != nil {
		return nil, ErrTokenMalformed
	}
	if err := s.verify(parts[0]+"."+parts[1], sig); err != nil {
		return nil, err
	}

	cb, err := unb64(parts[1])
	if err != nil {
		return nil, ErrTokenMalformed
	}
	var claims Claims
	if err := jsonx.Unmarshal(cb, &claims); err != nil {
		return nil, ErrTokenMalformed
	}

	now := time.Now().Unix()
	// 留5s 时钟偏移容忍
	const skew = 5
	if claims.ExpiresAt != 0 && now > claims.ExpiresAt+skew {
		return nil, ErrTokenExpired
	}
	if claims.NotBefore != 0 && now < claims.NotBefore-skew {
		return nil, ErrTokenNotYet
	}

	// ★ iss / aud 必须校验。
	//
	// 不校验的实际风险（本项目曾真的漏掉）：
	//  1. 跨 IdP 复用：外部 SSO（Google/企业 IdP）签发的令牌，
	//     只要持有本平台公钥就能通过校验，被当成本平台用户身份。
	//  2. 跨服务越权：为服务 A 签发的令牌可拿去访问服务 B。
	//  3. 环境串用：测试环境签发的令牌在生产仍有效（若密钥同源）。
	if err := s.checkIssuer(&claims); err != nil {
		return nil, err
	}
	if err := s.checkAudience(&claims); err != nil {
		return nil, err
	}
	return &claims, nil
}

// checkIssuer 校验签发方
func (s *Signer) checkIssuer(c *Claims) error {
	if s.wantIssuer == "" {
		return nil // 未配置期望值则不校验
	}
	if c.Issuer != s.wantIssuer {
		return ErrWrongIssuer
	}
	return nil
}

// checkAudience 校验受众：令牌 aud 与期望列表必须有交集。
//
// 按JWT 规范（RFC 7519 §4.1.3），含 aud 的令牌在 aud 不含自身时必须拒绝。
func (s *Signer) checkAudience(c *Claims) error {
	if len(s.wantAudience) == 0 {
		return nil
	}
	for _, want := range s.wantAudience {
		for _, got := range c.Audience {
			if got == want {
				return nil
			}
		}
	}
	return ErrWrongAudience
}

func (s *Signer) verify(payload string, sig []byte) error {
	switch s.alg {
	case RS256:
		sum := sha256.Sum256([]byte(payload))
		if err := rsa.VerifyPKCS1v15(s.pub, crypto.SHA256, sum[:], sig); err != nil {
			return ErrTokenSignature
		}
		return nil
	case HS256:
		mac := hmac.New(sha256.New, s.hs)
		mac.Write([]byte(payload))
		if !hmac.Equal(mac.Sum(nil), sig) {
			return ErrTokenSignature
		}
		return nil
	default:
		return ErrUnsupportedAlg
	}
}

// HasScope 判断是否具备某权限（支持 "kb:*" 通配）
func (c *Claims) HasScope(required string) bool {
	if required == "" {
		return true
	}
	for _, s := range c.Scopes {
		if s == required {
			return true
		}
		if strings.HasSuffix(s, "*") && strings.HasPrefix(required, strings.TrimSuffix(s, "*")) {
			return true
		}
	}
	return false
}

// HasRole 判断是否具备某角色
func (c *Claims) HasRole(required string) bool {
	for _, r := range c.Roles {
		if r == required {
			return true
		}
	}
	return false
}

// UidString 返回字符串形式的 uid，便于日志
func (c *Claims) UidString() string { return strconv.FormatInt(c.UID, 10) }

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func unb64(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 0, n*2)
	for _, c := range b {
		out = append(out, hexDigits[c>>4], hexDigits[c&0x0f])
	}
	return string(out)
}
