#!/usr/bin/env bash
# 本地一键启停全部服务（mock 模式，零外部依赖）。
#
# 用法：
#   scripts/dev.sh up       启动全部（若已运行则跳过）
#   scripts/dev.sh down     停止全部
#   scripts/dev.sh restart  重启全部
#   scripts/dev.sh status   查看状态
#   scripts/dev.sh logs <svc>  跟踪日志
#   scripts/dev.sh up-sonic 用 Sonic 引擎编译并启动（对比性能）
#
# 为什么用预编译二进制而不是 go run：
# go run 会留下孤儿子进程，端口占用时很难定位到真正 PID。
# build 到 .run/bin 后 PID 一一对应，kill 才可靠。
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$ROOT/.run/bin"
LOG="$ROOT/.run/logs"
mkdir -p "$BIN" "$LOG"

# shellcheck source=/dev/null
source "$ROOT/scripts/goenv.sh"

# ---- 服务清单：name:module:rpc_port:http_port ----
#
# ★ gateway 的 rpc_port 留空：它是纯 HTTP 入口（HTTPOnly 模式），
#   既不暴露也不消费 Kitex 接口。健康检查由 Hertz 自己实现，
#   复用同一个 http_port，不再另起探活端口。
SERVICES=(
  "gateway:gateway::8000"
  "auth_service:auth_service:8881:8090"
  "user_service:user_service:8882:8092"
  "llm_service:llm_service:8884:8094"
  "rag_service:rag_service:8885:8095"
  "agent_service:agent_service:8886:8096"
)

# ---- 统一环境变量 ----
# mock 模式：无 Nacos / 无 PG / 无 Milvus / 无外部模型，全部进程内替身。
export AEGIS_MOCK__ENABLED=true
export AEGIS_NACOS__ENABLED=false
export AEGIS_LOGGER__LEVEL=info
# mock IdP 与 auth_service 共用一台机器
export AEGIS_AUTH__MOCK_IDP__ENABLED=true

svc_field() { # svc_field <svc> <2|3|4>
  local IFS=':'
  local parts=($1)
  echo "${parts[$2]}"
}

log_info()  { printf '\033[0;36m[dev]\033[0m %s\n' "$*"; }
log_ok()   { printf '\033[0;32m[dev]\033[0m %s\n' "$*"; }
log_err()  { printf '\033[0;31m[dev]\033[0m %s\n' "$*"; }

pid_of() { # pid_of <rpc_port> <http_port>；rpc_port 可为空（纯 HTTP 服务）
  local pattern
  if [ -n "${1:-}" ]; then
    pattern=":${1}\b|:${2}\b"
  else
    pattern=":${2}\b"
  fi
  ss -ltnp 2>/dev/null \
    | grep -E "$pattern" \
    | grep -oP 'pid=\K[0-9]+' | sort -u | head -1
}

# ---- 构建引擎选择 ----
#
# ★ 必须在这里（顶层、case 之前）决定，而不是在 case 分支里赋值：
#   顶层代码先于 case 执行完毕，把TAGS=sonic 写在 case 里
#   已经太晚 —— 表现为 `up-sonic` 仍然编译成标准库，且不报错。
#
# 两种启用方式：
#   scripts/dev.sh up-sonic          子命令（下方 case 会提前设置 AEGIS_BUILD_TAGS）
#   AEGIS_BUILD_TAGS=sonic scripts/dev.sh up   环境变量
TAGS="${AEGIS_BUILD_TAGS:-}"
LDFLAGS_EXTRA=""
if [ "$TAGS" = "sonic" ]; then
  # 从 go.mod 解析 sonic 版本，写进二进制供运行期自检报告
  # ★ 不能用 local：这段在顶层，local 仅在函数内有效
  sv="$(bash "$ROOT/scripts/sonic_version.sh" 2>/dev/null || true)"
  [ -n "$sv" ] && LDFLAGS_EXTRA="-X github.com/aegis/pkg/jsonx.sonicVer=$sv"
fi

build_all() {
  if [ "$TAGS" = "sonic" ]; then
    log_info "构建引擎: sonic ${sv:-unknown}（strict 自检已开启）"
  else
    log_info "构建引擎: encoding/json"
  fi
  log_info "编译所有服务…"
  local failed=0
  for entry in "${SERVICES[@]}"; do
    local name; name="$(svc_field "$entry" 0)"
    local mod;  mod="$(svc_field "$entry" 1)"
    if [ ! -d "$ROOT/$mod/cmd" ]; then
      log_info "跳过 $name（尚未实现）"
      continue
    fi
    if (cd "$ROOT" && go build \
          ${TAGS:+-tags "$TAGS"} \
          ${LDFLAGS_EXTRA:+-ldflags "$LDFLAGS_EXTRA"} \
          -o "$BIN/$name" "./$mod/cmd" 2>&1); then
      log_ok "  ✓ $name"
    else
      log_err "  ✗ $name 编译失败"
      failed=1
    fi
  done
  return $failed
}

start_all() {
  build_all || { log_err "编译失败，中止"; exit 1; }

  for entry in "${SERVICES[@]}"; do
    local name; name="$(svc_field "$entry" 0)"
    local mod;  mod="$(svc_field "$entry" 1)"
    local rpc;  rpc="$(svc_field "$entry" 2)"
    local http; http="$(svc_field "$entry" 3)"

    if [ ! -x "$BIN/$name" ]; then continue; fi

    local existing; existing="$(pid_of "$rpc" "$http")"
    if [ -n "$existing" ]; then
      log_info "$name 已在运行 (pid=$existing)"
      continue
    fi

    AEGIS_MOCK__ENABLED=true nohup "$BIN/$name" > "$LOG/$name.log" 2>&1 &
    local pid=$!
    echo "$pid" > "$LOG/$name.pid"

    # 等待健康检查通过；失败时直接给出日志尾部，不让调用方猜
    local ok=0
    for _ in $(seq 1 30); do
      if curl -sf --max-time 1 "http://127.0.0.1:$http/healthz" >/dev/null 2>&1; then
        ok=1; break
      fi
      # 进程提前退出也算失败
      if ! kill -0 "$pid" 2>/dev/null; then break; fi
      sleep 0.5
    done

    if [ "$ok" = "1" ]; then
      log_ok "$name 已就绪 (rpc=$rpc http=$http pid=$pid)"
    else
      log_err "$name 启动失败，日志尾部："
      tail -n 15 "$LOG/$name.log" | sed 's/^/      /'
      exit 1
    fi
  done
}

stop_all() {
  for entry in "${SERVICES[@]}"; do
    local name; name="$(svc_field "$entry" 0)"
    local rpc;  rpc="$(svc_field "$entry" 2)"
    local http; http="$(svc_field "$entry" 3)"

    local pid=""
    [ -f "$LOG/$name.pid" ] && pid="$(cat "$LOG/$name.pid")"
    # pid 文件可能因重启失效，回退到按端口找
    [ -z "$pid" ] || ! kill -0 "$pid" 2>/dev/null && pid="$(pid_of "$rpc" "$http")"

    if [ -n "$pid" ]; then
      kill "$pid" 2>/dev/null
      for _ in $(seq 1 10); do
        kill -0 "$pid" 2>/dev/null || break
        sleep 0.3
      done
      kill -9 "$pid" 2>/dev/null
      log_ok "$name 已停止 (pid=$pid)"
    fi
    rm -f "$LOG/$name.pid"
  done
}

status_all() {
  printf '%-16s %-8s %-8s %-10s %s\n' 服务 RPC HTTP 状态 PID
  for entry in "${SERVICES[@]}"; do
    local name; name="$(svc_field "$entry" 0)"
    local rpc;  rpc="$(svc_field "$entry" 2)"
    local http; http="$(svc_field "$entry" 3)"
    local pid;  pid="$(pid_of "$rpc" "$http")"
    local state="未运行"
    if [ -n "$pid" ]; then
      if curl -sf --max-time 1 "http://127.0.0.1:$http/healthz" >/dev/null 2>&1; then
        state="健康"
      else
        state="进程在但无响应"
      fi
    fi
    printf '%-16s %-8s %-8s %-10s %s\n' "$name" "$rpc" "$http" "$state" "${pid:--}"
  done
}

# ★ 在引擎初始化之后再处理 up-sonic：
#   子命令需在顶层就转换成 AEGIS_BUILD_TAGS（见上方说明）。
if [ "${1:-up}" = "up-sonic" ]; then
  export AEGIS_BUILD_TAGS=sonic
  # strict：镜像若静默回落到标准库就拒启动，
  # 否则会出现"以为在跑 Sonic、实际没有"的静默降级
  export AEGIS_JSON__ENGINE=sonic
  export AEGIS_JSON__STRICT=true
  # 重新计算（此前 TAGS 还是空）
  TAGS=sonic
  sv="$(bash "$ROOT/scripts/sonic_version.sh" 2>/dev/null || true)"
  LDFLAGS_EXTRA=""
  [ -n "$sv" ] && LDFLAGS_EXTRA="-X github.com/aegis/pkg/jsonx.sonicVer=$sv"
fi

case "${1:-up}" in
  up)      start_all ;;
  up-sonic) start_all ;;
  down)    stop_all ;;
  restart) stop_all; sleep 1; start_all ;;
  status)  status_all ;;
  build)   build_all ;;
  logs)
    name="${2:-}"
    if [ -z "$name" ]; then
      ls -1 "$LOG" | sed 's/\.log$//;s/\.pid$//' | sort -u
      exit 0
    fi
    tail -f "$LOG/$name.log"
    ;;
  *)
    echo "用法: $0 {up|up-sonic|down|restart|status|build|logs <svc>}"
    echo "  up-sonic  用 Sonic 引擎编译并启动（strict 校验，不符即拒启动）"
    exit 1
    ;;
esac