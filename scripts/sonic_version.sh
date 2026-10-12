#!/usr/bin/env bash
# 解析 sonic 的模块版本，供 -ldflags -X 注入。
#
# 用途：让 jsonx.Info() 在运行时能报出"到底装了哪个版本的 sonic"。
# Sonic 没有导出 Version 常量，只能从 go.mod / go.sum 里查。
#
# 输出：v1.15.0（查不到则输出空串，调用方应自行跳过 -X）
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# 优先级 1：pkg/go.mod 的 require（sonic 实现时 pkg 直接依赖它）
ver="$(grep -oE 'github\.com/bytedance/sonic v[0-9]+\.[0-9]+\.[0-9]+' \
	"$ROOT/pkg/go.mod" 2>/dev/null | head -1 | awk '{print $2}' || true)"

# 优先级 2：go.sum（间接依赖场景，如 pkg 未直接引入时）
if [ -z "$ver" ]; then
	ver="$(grep -oE 'github\.com/bytedance/sonic v[0-9]+\.[0-9]+\.[0-9]+' \
		"$ROOT/go.work.sum" 2>/dev/null | head -1 | awk '{print $2}' || true)"
fi

# 优先级 3：go list 解析（最准，但需要能连模块缓存）
if [ -z "$ver" ] && command -v go > /dev/null 2>&1; then
	ver="$(go list -m -f '{{.Version}}' github.com/bytedance/sonic 2> /dev/null || true)"
	# go list 对未在依赖图中的模块会报错，此时输出空
	case "$ver" in
	v[0-9]*) ;;
	*) ver="" ;;
	esac
fi

echo "$ver"
