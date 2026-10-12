package mockidp

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
)

// codeChallenge 计算 PKCE S256 挑战值
func codeChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// shortHash 生成短稳定标识，用于构造可预测的 token / code
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:6])
}
