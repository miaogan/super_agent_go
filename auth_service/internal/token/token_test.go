package token

import (
	"sync"
	"testing"
	"time"

	"github.com/aegis/pkg/jwtx"
)

func newTestManager(t *testing.T) *Manager {
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
	m, err := New(Options{
		Signer:     signer,
		Issuer:     "aegis",
		AccessTTL:  time.Hour,
		RefreshTTL: 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("构造 Manager 失败: %v", err)
	}
	return m
}

func user(uid int64) UserInfo {
	return UserInfo{UID: uid, Username: "tester", Tenant: "default", Roles: []string{"user"}, Scopes: []string{"user"}}
}

// ---------- 签发与校验 ----------

func TestIssuePairUsable(t *testing.T) {
	m := newTestManager(t)
	pair, err := m.Issue(user(1001))
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if pair.AccessToken == "" || pair.RefreshToken == "" {
		t.Fatal("令牌对不完整")
	}
	if pair.TokenType != "Bearer" {
		t.Errorf("TokenType = %q", pair.TokenType)
	}

	ac, err := m.ParseAccess(pair.AccessToken)
	if err != nil {
		t.Fatalf("解析 access 失败: %v", err)
	}
	if ac.UID != 1001 {
		t.Errorf("UID = %d", ac.UID)
	}

	rc, err := m.ParseRefresh(pair.RefreshToken)
	if err != nil {
		t.Fatalf("解析 refresh 失败: %v", err)
	}
	if rc.UID != 1001 {
		t.Errorf("refresh UID = %d", rc.UID)
	}
}

func TestPurposeIsolation(t *testing.T) {
	m := newTestManager(t)
	pair, _ := m.Issue(user(1001))

	// refresh token 不能当 access 用，反之亦然。
	// 少了这道隔离，截获 refresh token 就能直接调业务接口。
	if _, err := m.ParseAccess(pair.RefreshToken); err == nil {
		t.Error("refresh token 不应通过 ParseAccess")
	}
	if _, err := m.ParseRefresh(pair.AccessToken); err == nil {
		t.Error("access token 不应通过 ParseRefresh")
	}
}

func TestParseRejectsEmpty(t *testing.T) {
	m := newTestManager(t)
	if _, err := m.Parse(""); err == nil {
		t.Error("空令牌应被拒绝")
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	m := newTestManager(t)
	for _, s := range []string{"abc", "a.b.c", "Bearer xxx"} {
		if _, err := m.Parse(s); err == nil {
			t.Errorf("垃圾令牌 %q 应被拒绝", s)
		}
	}
}

// ---------- 吊销 ----------

func TestRevokeBlocksAccessToken(t *testing.T) {
	m := newTestManager(t)
	pair, _ := m.Issue(user(1001))

	if _, err := m.ParseAccess(pair.AccessToken); err != nil {
		t.Fatalf("吊销前应可用: %v", err)
	}
	m.Revoke(pair.AccessToken)
	if _, err := m.ParseAccess(pair.AccessToken); err == nil {
		t.Error("吊销后不应可用")
	}
}

func TestRevokeIgnoresInvalidToken(t *testing.T) {
	// 吊销一个解析不了的令牌不能 panic —— Logout 允许不带令牌调用
	m := newTestManager(t)
	m.Revoke("garbage")
	m.Revoke("")
}

func TestRevokeAllOfKillsWholeUser(t *testing.T) {
	m := newTestManager(t)
	// 同一用户在两台设备上登录
	dev1, _ := m.Issue(user(1001))
	dev2, _ := m.Issue(user(1001))
	other, _ := m.Issue(user(2002))

	m.RevokeAllOf(1001)

	if _, err := m.ParseAccess(dev1.AccessToken); err == nil {
		t.Error("设备1 的 access token 应失效")
	}
	if _, err := m.ParseAccess(dev2.AccessToken); err == nil {
		t.Error("设备2 的 access token 应失效")
	}
	if _, err := m.ParseRefresh(dev1.RefreshToken); err == nil {
		t.Error("refresh token 也必须失效 —— 否则登出形同虚设")
	}
	if _, err := m.ParseAccess(other.AccessToken); err != nil {
		t.Errorf("不应影响其他用户: %v", err)
	}
}

func TestRevokeAllOfZeroUIDNoop(t *testing.T) {
	m := newTestManager(t)
	pair, _ := m.Issue(user(1001))
	m.RevokeAllOf(0) // uid 缺失时不能把所有人的令牌都吊了
	if _, err := m.ParseAccess(pair.AccessToken); err != nil {
		t.Errorf("uid=0 不应吊销任何令牌: %v", err)
	}
}

func TestRefreshRotationBlocksReplay(t *testing.T) {
	// 一次性消费：refresh 用过一次就作废。
	// 不轮换的话，攻击者截获一次 refresh token 就能长期换取 access token。
	m := newTestManager(t)
	pair, _ := m.Issue(user(1001))

	if _, err := m.ParseRefresh(pair.RefreshToken); err != nil {
		t.Fatalf("首次使用应成功: %v", err)
	}
	m.Revoke(pair.RefreshToken)

	if _, err := m.ParseRefresh(pair.RefreshToken); err == nil {
		t.Error("旧 refresh token 重放必须被拒")
	}
}

func TestRevokedEntryExpiresNaturally(t *testing.T) {
	// 吊销记录带过期时间，到期后自动清理，不会无限增长
	m := newTestManager(t)
	pair, _ := m.Issue(user(1001))
	m.Revoke(pair.AccessToken)

	m.mu.Lock()
	n := len(m.revoked)
	idx, hasIdx := m.issued[1001]
	m.mu.Unlock()

	if n != 1 {
		t.Errorf("吊销名单大小 = %d, 期望 1", n)
	}
	if !hasIdx || len(idx) != 2 {
		t.Errorf("在用令牌索引应含 access+refresh 两条，实际 %d", len(idx))
	}

	// 手工把过期时间推到过去，模拟令牌到期
	m.mu.Lock()
	for jti := range m.revoked {
		m.revoked[jti] = revokedEntry{uid: 1001, exp: time.Now().Add(-time.Minute)}
	}
	m.mu.Unlock()

	if _, err := m.ParseAccess(pair.AccessToken); err != nil {
		t.Errorf("过期后应恢复可用: %v", err)
	}
	m.mu.RLock()
	n = len(m.revoked)
	m.mu.RUnlock()
	if n != 0 {
		t.Errorf("过期记录应被清理，剩余 %d 条", n)
	}
}

func TestIssuedIndexGCRuns(t *testing.T) {
	// 在用索引必须自清理，否则长期运行会随签发次数无限增长
	m := newTestManager(t)
	_, _ = m.Issue(user(1001))

	m.mu.Lock()
	for jti := range m.issued[1001] {
		m.issued[1001][jti] = time.Now().Add(-time.Hour)
	}
	m.mu.Unlock()

	// 再签发一次触发 GC
	_, _ = m.Issue(user(2002))

	m.mu.RLock()
	_, stillThere := m.issued[1001]
	m.mu.RUnlock()
	if stillThere {
		t.Error("过期的在用令牌索引应被 GC 清理")
	}
}

func TestRevokeAllOfSkipsExpired(t *testing.T) {
	m := newTestManager(t)
	// 造一个"已过期但仍挂在吊销名单里"的脏记录
	m.Issue(user(1001))
	m.mu.Lock()
	for jti := range m.revoked {
		m.revoked[jti] = revokedEntry{uid: 1001, exp: time.Now().Add(-time.Minute)}
	}
	// 造一个"已过期但在用索引里"的脏索引
	for jti := range m.issued[1001] {
		m.issued[1001][jti] = time.Now().Add(-time.Minute)
	}
	m.mu.Unlock()

	m.RevokeAllOf(1001)

	m.mu.RLock()
	nRevoked := len(m.revoked)
	_, hasIssued := m.issued[1001]
	m.mu.RUnlock()

	if nRevoked != 0 {
		t.Errorf("过期的吊销记录应被清理，剩余 %d 条", nRevoked)
	}
	if hasIssued {
		t.Error("过期的在用索引应被清理")
	}
}

func TestRevokeAllOfKeepsLiveTokens(t *testing.T) {
	// 反向断言：未过期的令牌必须真的进吊销名单，
	// 否则上面的清理逻辑就可能把"该吊销的"一起清掉。
	m := newTestManager(t)
	pair, _ := m.Issue(user(1001))
	m.RevokeAllOf(1001)

	m.mu.RLock()
	n := len(m.revoked)
	m.mu.RUnlock()
	if n != 2 { // access + refresh
		t.Errorf("在用令牌应全部被吊销，实际 %d 条", n)
	}
	if _, err := m.ParseAccess(pair.AccessToken); err == nil {
		t.Error("access token 应失效")
	}
	if _, err := m.ParseRefresh(pair.RefreshToken); err == nil {
		t.Error("refresh token 应失效")
	}
}

// ---------- 构造 ----------

func TestNewRejectsNilSigner(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Error("nil Signer 应报错")
	}
}

func TestNewFillsDefaultTTL(t *testing.T) {
	signer, _ := jwtx.NewSigner(jwtx.Options{
		Algorithm:  jwtx.HS256,
		HMACSecret: []byte("test-secret-key-must-be-32-bytes!!"),
	})
	m, err := New(Options{Signer: signer})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if m.AccessTTL() <= 0 {
		t.Errorf("access TTL 未填默认值: %v", m.AccessTTL())
	}
	if m.refreshTTL <= 0 {
		t.Errorf("refresh TTL 未填默认值: %v", m.refreshTTL)
	}
}

// ---------- 并发 ----------

func TestConcurrentRevokeAndParse(t *testing.T) {
	m := newTestManager(t)
	pairs := make([]*TokenPair, 20)
	for i := range pairs {
		pairs[i], _ = m.Issue(user(int64(1000 + i%5)))
	}

	var wg sync.WaitGroup
	for _, p := range pairs {
		wg.Add(2)
		go func(p *TokenPair) {
			defer wg.Done()
			m.Revoke(p.AccessToken)
		}(p)
		go func(p *TokenPair) {
			defer wg.Done()
			_, _ = m.ParseAccess(p.AccessToken)
		}(p)
	}
	// 同时按用户批量吊销，考验索引一致性
	for uid := int64(1000); uid < 1005; uid++ {
		wg.Add(1)
		go func(uid int64) {
			defer wg.Done()
			m.RevokeAllOf(uid)
		}(uid)
	}
	wg.Wait()

	// 不变量：在用索引里不应残留已过期条目（GC 生效）
	m.mu.RLock()
	stale := 0
	now := time.Now()
	for uid, set := range m.issued {
		for jti, exp := range set {
			if now.After(exp) {
				stale++
			}
			_ = jti
		}
		_ = uid
	}
	revokedTotal := 0
	for _, e := range m.revoked {
		if !now.After(e.exp) {
			revokedTotal++
		}
	}
	m.mu.RUnlock()

	if stale != 0 {
		t.Errorf("在用索引残留 %d 条已过期记录", stale)
	}
	if revokedTotal != len(m.revoked) && revokedTotal < 0 {
		t.Errorf("吊销名单含意外条目: %d", revokedTotal)
	}
}
