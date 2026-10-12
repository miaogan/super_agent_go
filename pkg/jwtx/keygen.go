package jwtx

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
)

// GenerateRSAKeyPair 生成 RS256 密钥对并可选写入磁盘。
//
// 用途：本地开发与冒烟测试免去手工 openssl 命令。
// 生产环境应由 KMS / K8s Secret 提供 PEM，密钥绝不入库不入仓。
//
// 返回值可直接喂给 NewSigner：
//
//	signer, err := jwtx.NewSigner(jwtx.Options{
//	    PrivateKeyPEM: privPEM,
//	    PublicKeyPEM:  pubPEM,
//	})
func GenerateRSAKeyPair(bits int) (privPEM, pubPEM []byte, err error) {
	if bits == 0 {
		bits = 2048
	}
	if bits < 2048 {
		return nil, nil, fmt.Errorf("jwtx: RSA 密钥位数不得低于 2048，当前 %d", bits)
	}
	key, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		return nil, nil, err
	}

	privBlock := &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, nil, err
	}
	pubBlock := &pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: pubDER,
	}
	return pem.EncodeToMemory(privBlock), pem.EncodeToMemory(pubBlock), nil
}

// EnsureKeyPair 若 PEM 文件不存在则生成并落盘，存在则读回。
//
// 这样本地 `go run ./cmd` 首次启动会自动准备好密钥，重启复用同一密钥
// （否则重启后已签发的 JWT 全部失效）。
func EnsureKeyPair(dir string, bits int) (privPEM, pubPEM []byte, err error) {
	privPath := filepath.Join(dir, "jwt_private.pem")
	pubPath := filepath.Join(dir, "jwt_public.pem")

	privPEM, err = os.ReadFile(privPath)
	if err == nil {
		pubPEM, err = os.ReadFile(pubPath)
		if err == nil {
			return privPEM, pubPEM, nil
		}
	}

	privPEM, pubPEM, err = GenerateRSAKeyPair(bits)
	if err != nil {
		return nil, nil, err
	}
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, err
	}
	if err = os.WriteFile(privPath, privPEM, 0o600); err != nil {
		return nil, nil, err
	}
	if err = os.WriteFile(pubPath, pubPEM, 0o644); err != nil {
		return nil, nil, err
	}
	return privPEM, pubPEM, nil
}
