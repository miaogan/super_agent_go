package mockx

import (
	"crypto/rand"
	"encoding/base64"
	"math/big"
	"time"
)

func timeNow() time.Time { return time.Now() }

// fillRandom 用 crypto/rand 填充缓冲区。
// OAuth state / PKCE code_verifier 属安全敏感值，必须用密码学随机源。
func fillRandom(b []byte) {
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败属系统级异常，此处退化到时间派生仅用于避免 panic，
		// 生产环境不会出现。
		for i := range b {
			b[i] = byte(timeNow().UnixNano() >> (i % 8 * 8))
		}
	}
}

// RandomURLSafe 生成 URL 安全的随机串（OAuth2.0 PKCE / state）
func RandomURLSafe(n int) string {
	b := make([]byte, n)
	fillRandom(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// RandomInt 返回 [0, max) 的密码学随机整数
func RandomInt(max int64) int64 {
	if max <= 0 {
		return 0
	}
	n, err := rand.Int(rand.Reader, big.NewInt(max))
	if err != nil {
		return 0
	}
	return n.Int64()
}
