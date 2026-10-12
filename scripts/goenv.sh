#!/usr/bin/env bash
# 统一 Go 环境。
# 沙箱里 /usr/local/go 是 1.21，而 go.work 要求 1.26，
# 且 GOROOT/GOPATH 被预置成 1.21 的路径 —— 不覆盖会报
# "version go1.21.4 does not match go tool version go1.26.0"。
export GOROOT=/usr/local/go1.26
export GOPATH=/usr/local/go-packages
export PATH="$GOROOT/bin:$GOPATH/bin:$PATH"
export GOTOOLCHAIN=local