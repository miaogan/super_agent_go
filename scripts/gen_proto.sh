#!/usr/bin/env bash
# 由 .proto 生成 Kitex 代码。
#
# 用法：
#   scripts/gen_proto.sh              # 生成全部
#   scripts/gen_proto.sh agentv1      # 只生成 agentv1
#
# 产物：idl/kitex_gen/<pkg>/
#   xxx.pb.go        —— 由 protoc-gen-go 生成的消息结构
#   <pkg>service/    —— 由 kitex 生成的 client / server 骨架
#
# ★ 为什么要脚本而不是手敲：
#   kitex 的生成参数（-I、-module、handler 模板）一旦记错，
#   产物会和别的服务不一致，且很难一眼看出。所以固定在这里。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="$ROOT/idl/kitex_gen"

# shellcheck source=/dev/null
source "$ROOT/scripts/goenv.sh"

# ---- 依赖检查 ----
# protoc 与 kitex 是外部二进制，缺失时给出可执行的修复命令，
# 而不是让人对着 "command not found" 猜。
need() {
  command -v "$1" >/dev/null 2>&1 && return 0
  printf '\033[0;31m[gen]\033[0m 缺少 %s\n' "$1"
  case "$1" in
    protoc)
      echo "      安装："
      echo "        # Debian/Ubuntu"
      echo "        apt-get update && apt-get install -y protobuf-compiler"
      echo "        # 或从 https://github.com/protocolbuffers/protobuf/releases 下载二进制"
      ;;
    kitex)
      echo "      安装：go install github.com/cloudwego/kitex/tool/cmd/kitex@latest"
      ;;
    protoc-gen-go)
      echo "      安装：go install google.golang.org/protobuf/cmd/protoc-gen-go@latest"
      ;;
  esac
  exit 1
}

need protoc
need protoc-gen-go
need kitex

PROTOC_VERSION="$(protoc --version)"
KITEX_VERSION="$(kitex -version 2>&1 | head -1)"
printf '\033[0;36m[gen]\033[0m protoc: %s\n' "$PROTOC_VERSION"
printf '\033[0;36m[gen]\033[0m kitex: %s\n' "$KITEX_VERSION"

# ---- 待生成的 proto ----
cd "$ROOT/idl"

if [ $# -gt 0 ]; then
  PROTOS=()
  for pkg in "$@"; do
    if [ ! -f "$pkg/$pkg.proto" ]; then
      printf '\033[0;31m[gen]\033[0m 找不到 idl/%s/%s.proto\n' "$pkg" "$pkg"
      exit 1
    fi
    PROTOS+=("$pkg/$pkg.proto")
  done
else
  PROTOS=()
  # commonv1 必须最先生成：其他 proto 都 import 它
  PROTOS+=("commonv1/common.proto")
  for f in $(ls -1 ./*/*.proto | sort); do
    case "$f" in
      ./commonv1/common.proto) ;;
      *) PROTOS+=("${f#./}") ;;
    esac
  done
fi

printf '\033[0;36m[gen]\033[0m 待生成 %d 个 proto\n' "${#PROTOS[@]}"

# ---- 逐个生成 ----
# 分两步而不是一步到位：
#   kitex 内部会调 protoc，但它对 -I 的相对路径处理容易踩坑，
#   先用 kitex 生成 service 骨架，再单独确认消息结构可用。
for proto in "${PROTOS[@]}"; do
  pkg="$(dirname "$proto")"
  name="$(basename "$pkg")"

  # service 名从 proto 里读，而不是手写列表 ——
  # 手写会在新增服务时静默漏掉，表现为"生成成功但没有 client 包"，
  # 这种错误要到编译期才暴露。
  #
  # ★ 管道末尾加 `|| true`：common.proto 这类纯消息定义文件没有 service，
  #   grep 无匹配返回 1，在 set -e 下会让整个脚本静默退出 ——
  #   表现为"打印了待生成数量然后什么都没有"，极难定位。
  svc="$( { grep -oE '^service[[:space:]]+[A-Za-z0-9_]+' "$proto" || true; } \
    | awk '{print $2}' | head -1)"
  if [ -z "$svc" ]; then
    printf '  \033[0;90m·\033[0m %s (纯消息定义，无 service)\n' "$proto"
    continue
  fi

  printf '  \033[0;36m→\033[0m %s (service: %s)\n' "$proto" "$svc"

  # -type protobuf  kitex 不会从扩展名猜 IDL 类型，必须显式指定
  # -I .            以 idl/ 为 protobuf 根，import 路径才能对上
  # -module         必须是 idl 模块自身的名字（github.com/aegis/idl）
  # -gen-path       kitex_gen 的落盘位置。★ 必须是**相对**路径：
  #                 传绝对路径会被 kitex 拼成 "module + 绝对路径"
  #                 的怪路径，直接 panic 在 index out of range。
  # -service        生成 client/server 骨架
  #
  # ★ 最后一个参数必须是**文件路径**（./agentv1/agent.proto），
  #   传目录（./agentv1）时 kitex 找不到 entry file，
  #   会误报 "option go_package not set" —— 明明 proto 里写了。
  ( cd "$ROOT/idl" && kitex \
      -type protobuf \
      -I . \
      -module "github.com/aegis/idl" \
      -gen-path "kitex_gen" \
      -service "$svc" \
      "./$proto" )

  if [ ! -d "$OUT/$name" ]; then
    printf '\033[0;31m[gen]\033[0m %s 生成失败：未产出 %s\n' "$name" "$OUT/$name"
    exit 1
  fi
done

# ---- 清掉 kitex 的示例脚手架 ----
#
# kitex 的 -service 除了生成 <pkg>service/ 包，还会在**当前工作目录**
# 吐出一对 package main 的 main.go + handler.go（全是 TODO 桩）。
# 它们落在 idl/ 根目录会直接破坏 idl 这个库模块：
#
#	idl/handler.go:94:10: undefined: AuthServiceImpl
#
# ——idl/ 里没有、也没必要有 main 包。
rm -f "$ROOT/idl/main.go" "$ROOT/idl/handler.go"

# ---- 编译校验 ----
# 生成成功 ≠ 能编译。Kitex 模板与 protoc 版本不匹配时，
# 产物能生成出来但 go build 会失败 —— 在这里挡住比等到 CI 好。
printf '\033[0;36m[gen]\033[0m 编译校验…\n'
if ! go build ./... 2>&1; then
  printf '\033[0;31m[gen]\033[0m 生成产物无法编译，请检查上面的错误\n'
  exit 1
fi

printf '\033[0;32m[gen]\033[0m 完成，产物在 idl/kitex_gen\n'
ls -1 "$OUT" | sed 's/^/  /'