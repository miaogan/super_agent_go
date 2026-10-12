#!/usr/bin/env bash
# 统一 Go 环境。
#
# 历史包袱：沙箱里 /usr/local/go 是 1.21，而 go.work 要求 1.26，
# 且 GOROOT/GOPATH 被预置成 1.21 的路径 —— 不覆盖会报
# "version go1.21.4 does not match go tool version go1.26.0"。
#
# 但硬编码路径只对沙箱成立：本机 Go 装在别处时（例如 Windows 的
# E:\go），写死的 /usr/local/go1.26 会让每一条 make 目标都挂在
# `go: cannot find GOROOT directory`。因此这里改成“显式指定优先，
# 否则从实际使用的 go 二进制反推”，沙箱与本机都能跑。

# aegis_root_from_go：从 PATH 里的 go 反推 GOROOT。
# go 通常在 <GOROOT>/bin/go，符号链接场景先解析真实路径。
_aegis_goroot_from_go() {
    local g
    g="$(command -v go 2>/dev/null)" || return 1
    # 逐层展开软链（Linux），Windows 上 readlink 不存在则原样返回
    while [ -L "$g" ]; do
        local t
        t="$(readlink "$g" 2>/dev/null)" || break
        case "$t" in
            /*) g="$t" ;;
            *)  g="$(dirname "$g")/$t" ;;
        esac
    done
    # 去掉末尾的 /bin/go(.exe)
    local dir
    dir="$(dirname "$g")"
    [ "$(basename "$dir")" = "bin" ] || return 1
    dirname "$dir"
}

# 仅当给定路径确实存在时才采纳，避免把不存在的目录塞进 GOROOT。
_aegis_dir_exists() { [ -n "$1" ] && [ -d "$1" ]; }

if ! _aegis_dir_exists "${GOROOT:-}"; then
    if _aegis_dir_exists /usr/local/go1.26; then
        export GOROOT=/usr/local/go1.26
    elif _aegis_goroot_from_go >/dev/null 2>&1; then
        export GOROOT="$(_aegis_goroot_from_go)"
    fi
fi

if _aegis_dir_exists /usr/local/go-packages; then
    export GOPATH=/usr/local/go-packages
fi

# 只有在拿到有效 GOROOT 时才改写 PATH，否则保留系统默认。
if _aegis_dir_exists "${GOROOT:-}"; then
    export PATH="$GOROOT/bin:$GOPATH/bin:$PATH"
fi

# GOTOOLCHAIN=local：禁止 go 自动下载其他工具链（沙箱无网）。
# 代价是本机工具链必须 ≥ go.work 声明版本；不满足时给出可操作的报错，
# 而不是让 go 抛出一句看不出根因的版本不匹配。
export GOTOOLCHAIN=local
