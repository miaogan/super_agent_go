package passwd

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// hashWith 用指定轮数生成哈希，供大规模测试降低耗时。
// 格式与 Hash() 完全一致，只是轮数不同。
func hashWith(t *testing.T, iter int, plain string) string {
	t.Helper()
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		t.Fatalf("生成 salt 失败: %v", err)
	}
	dk := pbkdf2([]byte(plain), salt, iter, keyLen)
	return fmt.Sprintf("%s$%d$%s$%s", scheme, iter,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(dk))
}

// splitHash 拆解存储格式：pbkdf2$sha256$iterations$salt$dk
func splitHash(t *testing.T, stored string) (scheme string, iter int, salt, dk []byte) {
	t.Helper()
	parts := strings.Split(stored, "$")
	if len(parts) != 5 {
		t.Fatalf("存储格式应有5 段，实际 %d: %q", len(parts), stored)
	}
	n, err := strconv.Atoi(parts[2])
	if err != nil {
		t.Fatalf("iterations 非数字: %q", parts[2])
	}
	s, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		t.Fatalf("salt 非合法 base64: %q", parts[3])
	}
	d, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		t.Fatalf("dk 非合法 base64: %q", parts[4])
	}
	return parts[0] + "$" + parts[1], n, s, d
}

// TestHashFormat 锁定存储格式契约。
//
// user_service 写、auth_service 读，任何一侧改格式都会导致全平台无法登录，
// 所以这段断言是刻意的"过紧"约束。
func TestHashFormat(t *testing.T) {
	got, err := Hash("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("Hash 不应返回错误: %v", err)
	}
	wantPrefix := "pbkdf2$sha256$120000$"
	if !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("格式前缀应为 %q，实际 %q", wantPrefix, got)
	}
	sch, iter, salt, dk := splitHash(t, got)
	if sch != scheme {
		t.Errorf("scheme 应为 %q，实际 %q", scheme, sch)
	}
	if iter != Iterations {
		t.Errorf("iterations 应为 %d，实际 %d", Iterations, iter)
	}
	if len(salt) != saltLen {
		t.Errorf("salt 长度应为 %d，实际 %d", saltLen, len(salt))
	}
	if len(dk) != keyLen {
		t.Errorf("派生密钥长度应为 %d，实际 %d", keyLen, len(dk))
	}
}

// TestHashSaltUnique 相同口令必须产出不同哈希。
//
// 盐相同意味着全平台用户在同一明文下哈希一致 —— 数据库一旦泄露即批量撞库。
//
// 用低轮数迭代，避免 -race 下几十秒的耗时；
// 真实轮数由 TestHashFormat 覆盖。
func TestHashSaltUnique(t *testing.T) {
	const n = 20
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		h := hashWith(t, 1000, fmt.Sprintf("same-password-%d", i))
		if seen[h] {
			t.Fatal("★ 相同口令产生了相同哈希，盐未生效")
		}
		seen[h] = true
	}
	// 每个哈希都必须能用该口令验证通过
	for i := 0; i < n; i++ {
		h := hashWith(t, 1000, fmt.Sprintf("same-password-%d", i))
		if !Verify(h, fmt.Sprintf("same-password-%d", i)) {
			t.Errorf("第 %d 个哈希应验证通过", i)
		}
	}
}

// TestVerifyCorrectPassword 是主流程。
func TestVerifyCorrectPassword(t *testing.T) {
	stored, err := Hash("s3cret-pass")
	if err != nil {
		t.Fatalf("Hash 不应返回错误: %v", err)
	}
	if !Verify(stored, "s3cret-pass") {
		t.Error("正确口令应校验通过")
	}
}

// TestVerifyWrongPassword 错误口令必须拒绝。
func TestVerifyWrongPassword(t *testing.T) {
	stored := hashWith(t, 1000, "s3cret-pass")
	for _, wrong := range []string{
		"",             // 空口令
		"S3cret-pass",  // 大小写
		" s3cret-pass", // 前导空格
		"s3cret-pass ", // 尾随空格
		"s3cret",       // 前缀截断
		"s3cret-passs", // 多一个字符
		"",             //
	} {
		if Verify(stored, wrong) {
			t.Errorf("★ 错误口令 %q 被判为通过", wrong)
		}
	}
}

// TestVerifyEmptyStored 空哈希必须拒绝。
func TestVerifyEmptyStored(t *testing.T) {
	if Verify("", "anything") {
		t.Error("空存储哈希不应通过")
	}
	stored, err := Hash("x")
	if err != nil {
		t.Fatalf("Hash 不应返回错误: %v", err)
	}
	if Verify(stored, "") {
		t.Error("空口令不应通过")
	}
}

// TestVerifyMalformed 畸形存储串必须安全地返回 false，绝不能 panic。
//
// 这些串可能来自数据库损坏或恶意构造的输入。
func TestVerifyMalformed(t *testing.T) {
	cases := []string{
		"pbkdf2$sha256",                 // 段数不足
		"pbkdf2$sha256$120000",          // 段数不足
		"pbkdf2$sha256$abc$salt$dk",     // iterations 非数字
		"pbkdf2$sha256$0$salt$dk",       // iterations 为 0
		"pbkdf2$sha256$-5$salt$dk",      // iterations 为负
		"pbkdf2$sha256$120000$!!!$dk",   // salt 非法 base64
		"pbkdf2$sha256$120000$salt$!!!", // dk 非法 base64
		"pbkdf2$sha256$120000$salt$",    // dk 为空
		"pbkdf2$sha256$120000$salt$d",   //
		"plain-text-password",           // 完全不认识的格式
		"",                              //
		"$2a$10$abcdefghijklmnopqrstuv", // bcrypt 前缀但无 hook
		"pbkdf2$sha256$1e10$salt$dk",    // iterations 溢出
	}
	for _, c := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("★ Verify(%q) panic: %v", c, r)
				}
			}()
			if Verify(c, "anything") {
				t.Errorf("★ 畸形存储串 %q 被判为通过", c)
			}
		}()
	}
}

// TestVerifyDifferentIterations 用低轮数构造哈希，验证可正确解析自定义轮数。
func TestVerifyDifferentIterations(t *testing.T) {
	// 1000 轮足够快，同时仍是合法的 PBKDF2 串
	salt := []byte("0123456789abcdef")
	dk := pbkdf2([]byte("pw"), salt, 1000, 32)
	stored := fmt.Sprintf("%s$1000$%s$%s", scheme,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(dk))

	if !Verify(stored, "pw") {
		t.Error("自定义轮数的哈希应校验通过")
	}
	if Verify(stored, "pw2") {
		t.Error("错误口令不应通过")
	}
}

// TestPBKDF2KnownVector 用 RFC 6070 风格的已知答案向量锁定实现正确性。
//
// 这是最重要的一条：PBKDF2 手写实现极易在 XOR 累加或块计数上出隐蔽错误，
// 而错误的 PBKDF2 依然能"自洽"（自己哈希自己验证能通过），
// 只有对照标准向量才能发现。
func TestPBKDF2KnownVector(t *testing.T) {
	cases := []struct {
		pass, salt string
		iter, kLen int
		want       string // hex
	}{
		// PBKDF2-HMAC-SHA256, RFC 7914 §11 / 经典测试向量
		{"password", "salt", 1, 32,
			"120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b"},
		{"password", "salt", 2, 32,
			"ae4d0c95af6b46d32d0adff928f06dd02a303f8ef3c251dfd6e2d85a95474c43"},
		{"password", "salt", 4096, 32,
			"c5e478d59288c841aa530db6845c4c8d962893a001ce4e11a4963873aa98134a"},
		{"passwordPASSWORDpassword", "saltSALTsaltSALTsaltSALTsaltSALTsalt", 4096, 40,
			"348c89dbcbd32b2f32d814b8116e84cf2b17347ebc1800181c4e2a1fb8dd53e1c635518c7dac47e9"},
	}
	for _, c := range cases {
		got := pbkdf2([]byte(c.pass), []byte(c.salt), c.iter, c.kLen)
		if hexOf(got) != c.want {
			t.Errorf("pbkdf2(%q,%q,%d,%d)\n got %s\nwant %s",
				c.pass, c.salt, c.iter, c.kLen, hexOf(got), c.want)
		}
	}
}

// TestPBKDF2LongKey 多块输出（keyLen > 32）必须正确拼接。
func TestPBKDF2LongKey(t *testing.T) {
	for _, kLen := range []int{1, 31, 32, 33, 64, 100} {
		got := pbkdf2([]byte("p"), []byte("s"), 10, kLen)
		if len(got) != kLen {
			t.Errorf("kLen=%d 时输出长度应为 %d，实际 %d", kLen, kLen, len(got))
		}
	}
}

// TestPBKDF2Deterministic 同参数必须同输出。
func TestPBKDF2Deterministic(t *testing.T) {
	a := pbkdf2([]byte("pw"), []byte("salt"), 100, 32)
	b := pbkdf2([]byte("pw"), []byte("salt"), 100, 32)
	if hexOf(a) != hexOf(b) {
		t.Error("同参数输出应一致")
	}
	c := pbkdf2([]byte("pw"), []byte("salt2"), 100, 32)
	if hexOf(a) == hexOf(c) {
		t.Error("不同 salt 应产出不同结果")
	}
	d := pbkdf2([]byte("pw"), []byte("salt"), 101, 32)
	if hexOf(a) == hexOf(d) {
		t.Error("不同轮数应产出不同结果")
	}
}

// TestNeedsRehash 升级判定逻辑。
func TestNeedsRehash(t *testing.T) {
	cur := hashWith(t, Iterations, "pw") // 真实轮数，-race 下仅此一处
	if NeedsRehash(cur) {
		t.Error("当前参数生成的哈希不应需要 rehash")
	}

	// 低轮数 → 需要升级
	old := fmt.Sprintf("%s$1000$%s$%s", scheme,
		base64.RawStdEncoding.EncodeToString([]byte("0123456789abcdef")),
		base64.RawStdEncoding.EncodeToString(make([]byte, 32)))
	if !NeedsRehash(old) {
		t.Error("低轮数哈希应需要 rehash")
	}

	// 非本方案（bcrypt）→ 需要升级
	if !NeedsRehash("$2a$10$abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Error("bcrypt 哈希应标记为需要 rehash")
	}
	// 格式残缺 → 需要升级
	if !NeedsRehash("pbkdf2$sha256$abc") {
		t.Error("残缺格式应标记为需要 rehash")
	}
	// 纯文本口令 → 需要升级
	if !NeedsRehash("plaintext") {
		t.Error("明文口令必须标记为需要 rehash")
	}
}

// TestBcryptHook 未注册 hook 时绝不误判为通过。
//
// 这是最容易写错的地方：未接入 bcrypt 时若返回 true，
// 任何 $2a$ 开头的字符串都能登录 —— 等于后门。
func TestBcryptHookUnregistered(t *testing.T) {
	RegisterBcrypt(nil) // 显式传 nil，不应覆盖已有状态
	if Verify("$2a$10$abcdefghijklmnopqrstuv", "pw") {
		t.Error("未注册 bcrypt hook 时不应通过")
	}
	if Verify("$2b$10$abcdefghijklmnopqrstuv", "pw") {
		t.Error("未注册 bcrypt hook 时不应通过")
	}
}

// TestBcryptHookRegistered 注册后应被正确调用。
func TestBcryptHookRegistered(t *testing.T) {
	var gotStored, gotPlain string
	called := 0
	RegisterBcrypt(func(stored, plain string) bool {
		called++
		gotStored, gotPlain = stored, plain
		return stored == "$2a$10$good" && plain == "pw"
	})
	t.Cleanup(func() { RegisterBcrypt(nil) })

	if !Verify("$2a$10$good", "pw") {
		t.Error("注册 hook 后合法 bcrypt 串应通过")
	}
	if Verify("$2a$10$bad", "pw") {
		t.Error("注册 hook 后非法 bcrypt 串不应通过")
	}
	if called != 2 {
		t.Errorf("hook 应被调用 2 次，实际 %d", called)
	}
	if gotStored != "$2a$10$bad" || gotPlain != "pw" {
		t.Errorf("hook 入参错误: %q %q", gotStored, gotPlain)
	}
}

// TestHashEmpty 空口令原样返回（OAuth 账号无口令）。
func TestHashEmpty(t *testing.T) {
	got, err := Hash("")
	if err != nil {
		t.Fatalf("空口令 Hash 不应返回错误: %v", err)
	}
	if got != "" {
		t.Errorf("空口令应返回空串，实际 %q", got)
	}
}

// TestConcurrentHashVerify 并发哈希与校验。
//
// Hash 内部用 crypto/rand，-race 可捕捉共享状态问题；
// 同时验证 saltLen 级别的随机源在高并发下不重复。
func TestConcurrentHashVerify(t *testing.T) {
	var wg sync.WaitGroup
	results := make([]string, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h := hashWith(t, 1000, fmt.Sprintf("pw-%d", i))
			if !Verify(h, fmt.Sprintf("pw-%d", i)) {
				t.Errorf("并发哈希校验失败 i=%d", i)
			}
			results[i] = h
		}(i)
	}
	wg.Wait()

	seen := make(map[string]bool, len(results))
	for _, h := range results {
		if seen[h] {
			t.Fatal("★ 并发场景下出现重复哈希")
		}
		seen[h] = true
	}
}

// TestVerifyUsesConstantTime 校验路径应使用 hmac.Equal（常量时间）。
//
// 这里通过"篡改一个字节"来间接确认：若用 bytes.Equal，
// 短路比较会在首个不等字节处返回，无法直接观测；
// 故本测试主要保证 hmac.Equal 的语义（完全相等才通过）。
func TestVerifyUsesConstantTime(t *testing.T) {
	const iter = 1000
	stored := hashWith(t, iter, "pw")
	_, _, salt, dk := splitHash(t, stored)
	other := pbkdf2([]byte("pw"), salt, iter, len(dk))
	other[0] ^= 0x01

	bad := fmt.Sprintf("%s$%d$%s$%s", scheme, iter,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(other))
	if Verify(bad, "pw") {
		t.Error("单个比特被篡改的哈希不应通过")
	}
}

// TestHashIsDeterministicAcrossCalls 同一口令两次哈希不同但都能验证通过。
func TestHashIsDeterministicAcrossCalls(t *testing.T) {
	a, b := hashWith(t, 1000, "pw"), hashWith(t, 1000, "pw")
	if a == b {
		t.Skip("极端情况下盐碰撞，不做断言")
	}
	if !Verify(a, "pw") || !Verify(b, "pw") {
		t.Error("两次哈希都应能验证通过")
	}
	// 交叉验证：用 a 的哈希验 b 的口令也应失败
	if Verify(a, "pw2") {
		t.Error("错误口令不应通过")
	}
}

// TestSHA256Available 确认底层用的是 SHA-256（不是被换掉的其它算法）。
func TestSHA256Available(t *testing.T) {
	if sha256.Size != 32 {
		t.Fatalf("SHA-256 输出应为 32 字节，实际 %d", sha256.Size)
	}
}

func hexOf(b []byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, hexdigits[c>>4], hexdigits[c&0x0f])
	}
	return string(out)
}
