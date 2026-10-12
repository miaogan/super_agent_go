#!/usr/bin/env bash
# 容器操作的统一入口。
#
# 存在的理由：docker compose 的参数组合（--profile / --build /服务名）
# 太长且容易记错，尤其是 mock 与 sonic 两个 profile 的差别。
# 固化到脚本里，让 make 目标只需一个词。
#
# 用法：
#   scripts/docker.sh build gateway           # 标准镜像
#   scripts/docker.sh build-sonic gateway     # Sonic 镜像
#   scripts/docker.sh up                # 全栈
#   scripts/docker.sh up-mock                # 仅 mock（不连中间件）
#   scripts/docker.sh up-sonic               # Sonic 版业务服务
#   scripts/docker.sh down                    # 停止并移除
#   scripts/docker.sh logs gateway            # 看日志
#   scripts/docker.sh ps                      # 状态
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

c_ok=$'\033[0;32m'; c_dim=$'\033[0;90m'; c_off=$'\033[0m'

# 6 个业务服务（中间件由 compose 的 depends_on 自动拉起）
SERVICES=(gateway auth_service user_service llm_service rag_service agent_service)

need_docker() {
	if ! command -v docker > /dev/null 2>&1; then
		echo "  ✗ 未找到 docker，请先安装 Docker Engine"
		exit 1
	fi
	if ! docker compose version > /dev/null 2>&1; then
		echo "  ✗ docker compose 不可用（需要 Compose v2+，非 docker-compose）"
		exit 1
	fi
}

# sonic 版本：注入二进制供运行期自检报告
sonic_ver() {
	bash "$ROOT/scripts/sonic_version.sh" 2> /dev/null || true
}

cmd_build() {
	local svc="${1:-}" only=""
	[ -n "$svc" ] && only="$svc"
	if [ -n "$only" ]; then
		need_docker
		docker compose build "$only"
	else
		need_docker
		docker compose build "${SERVICES[@]}"
	fi
}

cmd_build_sonic() {
	local svc="${1:-}"
	need_docker
	local ver; ver="$(sonic_ver)"
	printf "  %ssonic 镜像：version=%s%s\n" "$c_dim" "${ver:-unknown}" "$c_off"
	if [ -n "$svc" ]; then
		docker compose --profile sonic build "$svc"
	else
		docker compose --profile sonic build
	fi
}

cmd_up() {
	need_docker
	# 先起中间件并等健康，再起业务服务 —— 业务服务启动时
	# 会连 PG/Milvus/Nacos，顺序错了会启动失败后不断重启
	docker compose up -d postgres etcd minio nacos milvus
	printf "  %s等待中间件健康（Milvus 首启需 60–90s）...%s\n" "$c_dim" "$c_off"
	docker compose up -d --wait --wait-timeout 180 postgres etcd minio nacos milvus 2> /dev/null || true
	docker compose up -d "${SERVICES[@]}"
	docker compose ps
}

cmd_up_mock() {
	need_docker
	docker compose --profile mock build
	docker compose --profile mock up -d
	printf "  %smock 服务已启动（不连任何中间件）%s\n" "$c_dim" "$c_off"
	printf "  冒烟：%sBASE_URL=http://127.0.0.1:18000 bash scripts/smoke.sh%s\n" "$c_dim" "$c_off"
}

cmd_up_sonic() {
	need_docker
	docker compose --profile sonic build
	docker compose --profile sonic up -d
	printf "  %s启动日志中确认引擎：%s\n" "$c_dim" "$c_off"
	printf "  %s  docker compose logs gateway-sonic | grep json_engine%s\n" "$c_dim" "$c_off"
	printf "  %s  期望 json_engine=sonic；若是 encoding/json 因 strict=true 会直接启动失败%s\n" "$c_dim" "$c_off"
}

cmd_down() {
	need_docker
	docker compose --profile mock --profile sonic down -v --remove-orphans
}

cmd_logs() {
	need_docker
	docker compose logs -f --tail=200 "${1:-gateway}"
}

cmd_ps() {
	need_docker
	docker compose ps
}

case "${1:-}" in
	build)         shift; cmd_build "${1:-}" ;;
	build-sonic)   shift; cmd_build_sonic "${1:-}" ;;
	up)            cmd_up ;;
	up-mock)       cmd_up_mock ;;
	up-sonic)      cmd_up_sonic ;;
	down)          cmd_down ;;
	logs)          shift; cmd_logs "${1:-}" ;;
	ps)            cmd_ps ;;
	*)
		cat <<EOF
用法：scripts/docker.sh <命令>

  build [服务]        构建标准镜像（默认全部 6 个业务服务）
  build-sonic [服务]  构建 Sonic 版镜像
  up                  启动全栈（中间件 + 业务服务，按依赖顺序）
  up-mock             仅启动 mock 服务（不连中间件）
  up-sonic            启动 Sonic 版业务服务
  down                停止并移除容器（-v 会清数据卷）
  logs [服务]         跟踪日志（默认 gateway）
  ps                  查看容器状态
EOF
		exit 1
		;;
esac
