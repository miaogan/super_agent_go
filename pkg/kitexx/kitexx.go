// Package kitexx 收纳 Kitex 直连客户端的公共装配逻辑。
//
// 目前只有一项：WaitHostPort —— 直连 client 创建前的 DNS 就绪等待。
package kitexx

import (
	"fmt"
	"net"
	"time"
)

// WaitHostPort 等待 addr（host:port）的主机名可被解析为 TCP 地址。
//
// # 为什么需要它
//
// Kitex 的 client.WithHostPorts 在 NewClient 时对每个地址做一次
// net.ResolveTCPAddr 以判定网络类型：解析失败并不会报错，而是退
// 而使用 net.ResolveUnixAddr（对任意字符串都成功），地址被永久
// 标记为 unix 网络 —— 之后每次拨号都报
//
//	dial unix host:port: connect: no such file or directory
//
// 且无法自愈（网络类型随 instance 固化）。Compose 冷启动时下游
// 容器的 DNS 记录可能尚未注册，服务启动即建连正好踩中（实测：
// auth_service 启动即连 user_service，登录持续失败直到重启）。
//
// 因此创建直连 client 前必须先确认 DNS 就绪：解析成功立即返回；
// 失败每 500ms 重试，直至 timeout。host 是 IP 字面量时无需等待，
// 直接返回 nil。
func WaitHostPort(addr string, timeout time.Duration) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("下游地址格式非法（期望 host:port）: %q: %w", addr, err)
	}
	if net.ParseIP(host) != nil {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for {
		// 与 kitex client/option.go WithHostPorts 内部的判定逻辑一致，
		// 这里成功即保证 NewClient 不会误判为 unix。
		if _, err := net.ResolveTCPAddr("tcp", addr); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("等待下游 %s DNS 就绪超时（%.0fs），请检查地址与服务是否启动", addr, timeout.Seconds())
		}
		time.Sleep(500 * time.Millisecond)
	}
}
