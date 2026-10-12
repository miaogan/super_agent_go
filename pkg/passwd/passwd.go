// Package passwd 提供跨服务共享的口令哈希与校验。
//
// 为什么放在 pkg 而不是各自实现：
//
//	user_service 负责"存哈希"，auth_service 负责"校验哈希"。
//	两边必须使用完全一致的格式与算法，否则任何一侧改动都会导致
//	全平台无法登录。把实现收敛到一处是唯一可靠的防漂移手段。
//
// 当前算法：PBKDF2-HMAC-SHA256，120000 轮（OWASP 2023 推荐）。
// 存储格式：
//
//	pbkdf2$sha256$<iterations>$<salt-base64>$<dk-base64>
//
// 迁移路径：上线前替换为 bcrypt 时，只需改本文件两个函数，
// 并在 Verify 中保留旧格式分支（当前已支持识别 $2a$/$2b$/$2y$ 前缀）。
package passwd

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"hash"
	"strconv"
	"strings"
)

const (
	// Iterations PBKDF2 迭代轮数
	Iterations = 120_000
	saltLen    = 16
	keyLen     = 32
	scheme     = "pbkdf2$sha256"
)

// Hash 生成口令哈希。空串原样返回("", nil)（OAuth 账号无口令）。
//
// 错误仅在 crypto/rand 熵源异常时返回 —— 这是极端系统级故障，
// 调用方（user_service 创建用户、auth_service 注册）必须向上传递错误，
// 让请求 fail 而不是静默降级到弱随机或 panic。
func Hash(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		// crypto/rand 失败意味着系统熵源异常，绝不能静默降级到弱随机
		return "", fmt.Errorf("passwd: 生成 salt 失败（系统熵源异常）: %w", err)
	}
	dk := pbkdf2([]byte(plain), salt, Iterations, keyLen)
	return fmt.Sprintf("%s$%d$%s$%s",
		scheme, Iterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(dk),
	), nil
}

// Verify 校验口令。
//
// 格式不识别时返回 false 而非 error：避免通过错误信息泄露内部细节。
func Verify(stored, plain string) bool {
	if stored == "" || plain == "" {
		return false
	}
	switch {
	case strings.HasPrefix(stored, scheme):
		return verifyPBKDF2(stored, plain)
	case strings.HasPrefix(stored, "$2"):
		// bcrypt：接入 golang.org/x/crypto 后在此调用 CompareHashAndPassword
		return verifyBcrypt(stored, plain)
	default:
		return false
	}
}

// NeedsRehash 判断存量哈希是否需要升级到当前参数。
// 用户登录成功后调用，必要时后台重算并回写。
func NeedsRehash(stored string) bool {
	if !strings.HasPrefix(stored, scheme) {
		return true
	}
	parts := strings.Split(stored, "$")
	if len(parts) != 5 {
		return true
	}
	iter, err := strconv.Atoi(parts[2])
	if err != nil {
		return true
	}
	return iter < Iterations
}

func verifyPBKDF2(stored, plain string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 5 {
		return false
	}
	iter, err := strconv.Atoi(parts[2])
	if err != nil || iter <= 0 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(want) == 0 {
		return false
	}
	got := pbkdf2([]byte(plain), salt, iter, len(want))
	return hmac.Equal(got, want)
}

func verifyBcrypt(stored, plain string) bool {
	// 无 bcrypt 依赖时不得误判为通过
	return bcryptHook != nil && bcryptHook(stored, plain)
}

// BcryptVerifier 由接入 golang.org/x/crypto/bcrypt 的层注入
var bcryptHook func(stored, plain string) bool

// RegisterBcrypt 注册 bcrypt 校验实现
func RegisterBcrypt(fn func(stored, plain string) bool) {
	if fn != nil {
		bcryptHook = fn
	}
}

// pbkdf2 实现 PBKDF2-HMAC-SHA256（RFC 8018 §5.2）
func pbkdf2(password, salt []byte, iter, keyLen int) []byte {
	prf := func() hash.Hash { return sha256.New() }
	hLen := prf().Size()
	blocks := (keyLen + hLen - 1) / hLen

	out := make([]byte, 0, blocks*hLen)
	buf := make([]byte, 4)
	for block := 1; block <= blocks; block++ {
		buf[0] = byte(block >> 24)
		buf[1] = byte(block >> 16)
		buf[2] = byte(block >> 8)
		buf[3] = byte(block)

		mac := hmac.New(prf, password)
		mac.Write(salt)
		mac.Write(buf)
		u := mac.Sum(nil)

		t := make([]byte, len(u))
		copy(t, u)
		for i := 1; i < iter; i++ {
			mac.Reset()
			mac.Write(u)
			u = mac.Sum(u[:0])
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}
