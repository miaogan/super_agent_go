package service

import "github.com/aegis/pkg/passwd"

// HashPasswordForSeed 供 seed 包生成演示账号的口令哈希。
// seed 场景下熵源异常直接 panic —— 演示数据生成失败比偷偷降级更安全。
func HashPasswordForSeed(plain string) string {
	h, err := passwd.Hash(plain)
	if err != nil {
		panic(err)
	}
	return h
}
