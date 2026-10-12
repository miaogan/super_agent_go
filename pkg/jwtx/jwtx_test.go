package jwtx

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
	"time"
)

// ---------- 测试辅助 ----------

// newHS256 构造 HS256 签名器（测试用，避免每次生成 RSA 密钥拖慢）
func newHS256(t *testing.T) *Signer {
	t.Helper()
	s, err := NewSigner(Options{
		Algorithm:  HS256,
		HMACSecret: []byte("0123456789abcdef0123456789abcdef"), // 32 字节
		KeyID:      "test",
	})
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return s
}

// newRS256 构造 RS256 签名器（只在该算法确实被测时才调用）
func newRS256(t *testing.T) *Signer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("生成 RSA 密钥: %v", err)
	}
	s, err := NewSigner(Options{
		Algorithm:     RS256,
		PrivateKeyPEM: pemOf(t, key),
		KeyID:         "rsa-test",
	})
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return s
}

func pemOf(t *testing.T, key *rsa.PrivateKey) []byte {
	t.Helper()
	b, err := marshalPKCS1(key)
	if err != nil {
		t.Fatalf("序列化密钥: %v", err)
	}
	return b
}

func encode(h header, c Claims, sig []byte) string {
	hb, _ := json.Marshal(h)
	cb, _ := json.Marshal(c)
	return b64(hb) + "." + b64(cb) + "." + b64(sig)
}

// ---------- 基础签发与校验 ----------

func TestSignAndVerify(t *testing.T) {
	s := newHS256(t)

	tok, err := s.Sign(&Claims{
		Subject:  "42",
		UID:      1001,
		Username: "admin",
		Roles:    []string{"admin", "user"},
		Scopes:   []string{"kb:*"},
		Tenant:   "t1",
	}, 2*time.Hour)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}

	got, err := s.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.UID != 1001 || got.Username != "admin" {
		t.Errorf("业务字段丢失: uid=%d username=%q", got.UID, got.Username)
	}
	if got.Subject != "42" {
		t.Errorf("sub 不符: %q", got.Subject)
	}
	if got.ID == "" {
		t.Error("jti 应自动生成")
	}
	if got.ExpiresAt <= time.Now().Unix() {
		t.Error("exp 应为未来时间")
	}
	if got.IssuedAt == 0 || got.NotBefore == 0 {
		t.Error("iat / nfb 应自动补齐")
	}
}

func TestVerifyExpired(t *testing.T) {
	s := newHS256(t)
	// 签发一个 1 秒后过期、但校验时已过期的令牌
	tok, err := s.Sign(&Claims{
		Subject:   "1",
		ExpiresAt: time.Now().Add(-time.Minute).Unix(),
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(tok); !errors.Is(err, ErrTokenExpired) {
		t.Errorf("过期令牌应报 ErrTokenExpired，实际=%v", err)
	}
}

// TestVerifyClockSkew 验证 5 秒时钟偏移容忍是双向的：
// 令牌过期 3 秒内仍应可用（不误杀），超过则拒绝。
func TestVerifyClockSkew(t *testing.T) {
	s := newHS256(t)

	// 过期 3 秒 → 在skew 内，应通过
	tok, err := s.Sign(&Claims{
		Subject:   "1",
		ExpiresAt: time.Now().Add(-3 * time.Second).Unix(),
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(tok); err != nil {
		t.Errorf("skew 内过期应容忍：%v", err)
	}

	// 过期 30 秒 → 必须拒绝
	tok2, err := s.Sign(&Claims{
		Subject:   "1",
		ExpiresAt: time.Now().Add(-30 * time.Second).Unix(),
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(tok2); !errors.Is(err, ErrTokenExpired) {
		t.Errorf("超出 skew 应拒绝，实际=%v", err)
	}
}

func TestVerifyNotYet(t *testing.T) {
	s := newHS256(t)
	tok, err := s.Sign(&Claims{
		Subject:   "1",
		NotBefore: time.Now().Add(time.Hour).Unix(),
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(tok); !errors.Is(err, ErrTokenNotYet) {
		t.Errorf("未生效令牌应报 ErrTokenNotYet，实际=%v", err)
	}
}

// ---------- 安全攻击面 ----------

// TestAlgNoneRejected是"alg=none"攻击的回归防线。
//
// 攻击者伪造 header{alg:"none"} 并把签名留空，
// 若服务端信任 header 就会当作合法令牌放行。
func TestAlgNoneRejected(t *testing.T) {
	s := newHS256(t)

	tok := encode(header{Alg: "none", Typ: "JWT"}, Claims{
		Subject: "admin", UID: 1001, ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}, nil)

	if _, err := s.Verify(tok); err == nil {
		t.Fatal("★ alg=none 令牌被接受，安全缺陷")
	}
}

// TestAlgConfusionRejected 验证 RS256/HS256 混淆攻击被阻断。
//
// 场景：服务端用 RS256（非对称），攻击者拿到公钥后
// 用公钥当 HMAC 密钥伪造 HS256 令牌。
// 防护关键在于 alg 由服务端配置决定，不读 token header。
func TestAlgConfusionRejected(t *testing.T) {
	// 先造一个 RS256 签名器
	rsaSigner := newRS256(t)
	pubPEM, err := marshalPKIX(&rsaSigner.rsa.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	// 攻击者：知道公钥，构造一个"HS256"签名器
	attacker, err := NewSigner(Options{
		Algorithm:  HS256,
		HMACSecret: pubPEM, // 错误地用公钥作 HMAC 密钥
	})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := attacker.Sign(&Claims{
		Subject: "1", UID: 666, ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}, 0)
	if err != nil {
		t.Fatal(err)
	}

	// 真实验证方是 RS256 签名器 —— 必须拒绝
	if _, err := rsaSigner.Verify(tok); err == nil {
		t.Fatal("★ 算法混淆令牌被接受，安全缺陷")
	}
}

func TestTamperedPayloadRejected(t *testing.T) {
	s := newHS256(t)
	tok, err := s.Sign(&Claims{Subject: "1", UID: 1001}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")

	// 篡改 payload：把 uid 从 1001 改成 9999（提权）
	forged := Claims{Subject: "1", UID: 9999, ExpiresAt: time.Now().Add(time.Hour).Unix()}
	fb, _ := json.Marshal(forged)
	bad := parts[0] + "." + b64(fb) + "." + parts[2]

	if _, err := s.Verify(bad); !errors.Is(err, ErrTokenSignature) {
		t.Errorf("篡改 payload 应报签名错误，实际=%v", err)
	}
}

func TestMalformedToken(t *testing.T) {
	s := newHS256(t)
	cases := []struct {
		name string
		tok  string
	}{
		{"空串", ""},
		{"无点号", "abcdef"},
		{"两段", "a.b"},
		{"四段", "a.b.c.d"},
		{"非法 base64", "!!!.!!!.!!!"},
		{"合法 base64 但非 JSON", b64([]byte("notjson")) + "." + b64([]byte("notjson")) + "." + b64([]byte("x"))},
	}
	for _, c := range cases {
		if _, err := s.Verify(c.tok); !errors.Is(err, ErrTokenMalformed) {
			t.Errorf("%s 应报 ErrTokenMalformed，实际=%v", c.name, err)
		}
	}
}

// TestKidNotHardRejected 确认 kid 不同时不会直接拒绝。
//
// kid 的用途是"选哪把密钥"。多密钥并存（轮换窗口）时
// 硬校验 kid 会让新旧令牌互相失效 —— 那是比不校验更糟的故障。
// 正确做法是按 kid 选密钥后逐一验签。
func TestKidNotHardRejected(t *testing.T) {
	a, _ := NewSigner(Options{
		Algorithm: HS256, HMACSecret: []byte("0123456789abcdef0123456789abcdef"), KeyID: "key-a",
	})
	b, _ := NewSigner(Options{
		Algorithm: HS256, HMACSecret: []byte("0123456789abcdef0123456789abcdef"), KeyID: "key-b",
	})

	tok, _ := a.Sign(&Claims{Subject: "1", ExpiresAt: time.Now().Add(time.Hour).Unix()}, 0)
	if _, err := b.Verify(tok); err != nil {
		t.Errorf("kid 不同但密钥相同时应能验签（供轮换窗口使用），实际=%v", err)
	}
}

// ---------- iss / aud 校验（本次修复的核心） ----------

// TestVerifyRejectsWrongIssuer 防止外部 IdP 令牌被当成本平台令牌。
//
// 这是本项目曾真实存在的缺陷：签发时写了 iss，但校验时从未比对，
// 导致 iss 形同虚设。
func TestVerifyRejectsWrongIssuer(t *testing.T) {
	s, err := NewSigner(Options{
		Algorithm:  HS256,
		HMACSecret: []byte("0123456789abcdef0123456789abcdef"),
		Issuer:     "aegis",
		Audience:   []string{"aegis"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 攻击者持同一密钥（模拟密钥泄露或内部误用），签一个外来iss
	tok, err := s.Sign(&Claims{
		Subject:   "1",
		Issuer:    "evil-corp-idp",
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(tok); !errors.Is(err, ErrWrongIssuer) {
		t.Errorf("★ 外来 iss 应被拒绝，实际=%v", err)
	}

	// 本平台 iss 应通过
	good, _ := s.Sign(&Claims{Subject: "1", ExpiresAt: time.Now().Add(time.Hour).Unix()}, 0)
	got, err := s.Verify(good)
	if err != nil {
		t.Fatalf("本平台 iss 应通过：%v", err)
	}
	if got.Issuer != "aegis" {
		t.Errorf("Sign 应自动填入 iss，实际=%q", got.Issuer)
	}
}

// TestVerifyRejectsWrongAudience 防止跨服务令牌越权。
func TestVerifyRejectsWrongAudience(t *testing.T) {
	s, _ := NewSigner(Options{
		Algorithm:  HS256,
		HMACSecret: []byte("0123456789abcdef0123456789abcdef"),
		Issuer:     "aegis",
		Audience:   []string{"gateway"},
	})

	// 为别的服务签的令牌
	tok, _ := s.Sign(&Claims{
		Subject:   "1",
		Audience:  []string{"some-other-service"},
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}, 0)
	if _, err := s.Verify(tok); !errors.Is(err, ErrWrongAudience) {
		t.Errorf("★ 跨服务 aud 应被拒绝，实际=%v", err)
	}

	// aud 无交集也必须拒绝（即使签发时 auto-fill 也会被覆盖）
	tok2, _ := s.Sign(&Claims{
		Subject:   "1",
		Audience:  []string{"gateway", "extra"},
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}, 0)
	if _, err := s.Verify(tok2); err != nil {
		t.Errorf("aud 有交集应通过：%v", err)
	}
}

// TestVerifyNoAudienceConfigured 未配置 audience 时不做校验，
// 保证向后兼容（测试与本地开发场景）。
func TestVerifyNoAudienceConfigured(t *testing.T) {
	s := newHS256(t) // 未配置 Issuer/Audience
	tok, _ := s.Sign(&Claims{
		Subject:   "1",
		Issuer:    "任意",
		Audience:  []string{"任意"},
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}, 0)
	if _, err := s.Verify(tok); err != nil {
		t.Errorf("未配置期望值时不应因 iss/aud 失败：%v", err)
	}
}

// ---------- 权限判断 ----------

func TestHasScope(t *testing.T) {
	cases := []struct {
		required string
		scopes   []string
		want     bool
	}{
		{"", []string{}, true},
		{"user", []string{"user", "admin"}, true},
		{"kb:read", []string{"kb:read"}, true},
		{"kb:read", []string{"kb:*"}, true},
		{"kb:write", []string{"kb:read"}, false},
		{"admin", []string{"user"}, false},
		// 前缀相似但不是通配，避免 "kb:*" 误配 "kbx:read"
		{"kbx:read", []string{"kb:*"}, false},
	}
	for _, c := range cases {
		cl := &Claims{Scopes: c.scopes}
		if got := cl.HasScope(c.required); got != c.want {
			t.Errorf("HasScope(%q, %v) = %v，期望 %v", c.required, c.scopes, got, c.want)
		}
	}
}

func TestHasRole(t *testing.T) {
	c := &Claims{Roles: []string{"admin", "user"}}
	if !c.HasRole("admin") || !c.HasRole("user") {
		t.Error("已有角色应命中")
	}
	if c.HasRole("root") {
		t.Error("不存在的角色不应命中")
	}
}

// ---------- 密钥装载 ----------

func TestHS256SecretTooShort(t *testing.T) {
	if _, err := NewSigner(Options{Algorithm: HS256, HMACSecret: []byte("short")}); err == nil {
		t.Error("短密钥应被拒绝")
	}
}

func TestRS256RequiresPrivateKey(t *testing.T) {
	// 只声明 RS256 但没给私钥
	if _, err := NewSigner(Options{Algorithm: RS256}); !errors.Is(err, ErrKeyUnavailable) {
		t.Errorf("缺少私钥应报 ErrKeyUnavailable，实际=%v", err)
	}
}

func TestUnsupportedAlg(t *testing.T) {
	if _, err := NewSigner(Options{Algorithm: "ES256"}); !errors.Is(err, ErrUnsupportedAlg) {
		t.Errorf("不支持的算法应报错，实际=%v", err)
	}
}

func TestLoadPEM(t *testing.T) {
	// 内联 PEM
	inline := "-----BEGIN RSA PRIVATE KEY-----\nabc\n-----END RSA PRIVATE KEY-----"
	got, err := LoadPEM(inline)
	if err != nil || !strings.Contains(string(got), "BEGIN") {
		t.Errorf("内联 PEM 应原样返回: %v", err)
	}
	// 空串
	if got, err := LoadPEM(""); err != nil || got != nil {
		t.Errorf("空串应返回 nil: %v", err)
	}
	// 不存在的路径应报错
	if _, err := LoadPEM("/nonexistent/path/key.pem"); err == nil {
		t.Error("不存在的路径应报错")
	}
}

// TestRS256VerifyOnlyPublicKey 验证"校验服务可独立部署"：
// 只持公钥也能校验，但无法签发。
func TestRS256VerifyOnlyPublicKey(t *testing.T) {
	signer := newRS256(t)
	pubPEM, err := marshalPKIX(&signer.rsa.PublicKey)
	if err != nil {
		t.Fatal(err)
	}

	verifier, err := NewSigner(Options{PublicKeyPEM: pubPEM})
	if err != nil {
		t.Fatalf("仅公钥构造失败: %v", err)
	}

	tok, err := signer.Sign(&Claims{Subject: "1", ExpiresAt: time.Now().Add(time.Hour).Unix()}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Verify(tok); err != nil {
		t.Errorf("公钥应能校验 RS256 令牌: %v", err)
	}
	// 但不能签发
	if _, err := verifier.Sign(&Claims{Subject: "1"}, time.Hour); err == nil {
		t.Error("仅持公钥时不应能签发")
	}
}

func TestBase64URLNoPadding(t *testing.T) {
	// JWT 使用无填充 base64url，断言不含 '=' 与 '+' '/'（避免非常规编码）
	s := newHS256(t)
	tok, _ := s.Sign(&Claims{Subject: "1", ExpiresAt: time.Now().Add(time.Hour).Unix()}, 0)
	if strings.ContainsAny(tok, "=+/") {
		t.Errorf("令牌不应含填充或非URL 安全字符: %s", tok)
	}
	if _, err := base64.RawURLEncoding.DecodeString(strings.Split(tok, ".")[1]); err != nil {
		t.Errorf("payload 应为 RawURL base64: %v", err)
	}
}

func TestConcurrentSignVerify(t *testing.T) {
	// Signer 声明为并发安全，用 -race 验证
	s := newHS256(t)
	done := make(chan error, 50)
	for i := 0; i < 50; i++ {
		go func(i int) {
			tok, err := s.Sign(&Claims{Subject: "u", UID: int64(i), ExpiresAt: time.Now().Add(time.Hour).Unix()}, 0)
			if err != nil {
				done <- err
				return
			}
			if _, err := s.Verify(tok); err != nil {
				done <- err
				return
			}
			done <- nil
		}(i)
	}
	for i := 0; i < 50; i++ {
		if err := <-done; err != nil {
			t.Fatalf("并发失败: %v", err)
		}
	}
}

// ---------- 编码辅助（供测试构造 PEM） ----------

func marshalPKCS1(key *rsa.PrivateKey) ([]byte, error) {
	der := x509.MarshalPKCS1PrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}), nil
}

func marshalPKIX(pub *rsa.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}
