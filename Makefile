# Aegis —— 构建与开发入口
#
# 设计原则：
#   1. 所有 Go 命令都经过 scripts/goenv.sh 统一环境，
#      避免"在我机器上能跑"—— GOROOT/GOTOOLCHAIN 必须一致。
#   2. 每个服务独立 go.mod，但依赖由 go.work 解析，
#      所以在仓库根执行即可，不必逐模块 cd。
#   3. 目标名对齐动词（build/test/lint/gen），不搞花哨别名。
#
# 常用：
#   make build          编译全部服务到 .run/bin
#   make test           全量测试（含 -race）
#   make cover          生成覆盖率报告 coverage.html
#   make dev            本地 mock 模式一键启停
#   make smoke          端到端冒烟（需先 make dev up）
#   make lint           gofmt + go vet
#   make check          CI 门禁：lint + test + vet
SHELL := /bin/bash
.DEFAULT_GOAL := help

ROOT    := $(shell pwd)
RUN_DIR := $(ROOT)/.run
BIN_DIR := $(RUN_DIR)/bin
LOG_DIR := $(RUN_DIR)/logs

# 服务清单：与 scripts/dev.sh 保持一致
SERVICES := gateway auth_service user_service llm_service rag_service agent_service

# 引入统一 Go 环境
GO_ENV := $(ROOT)/scripts/goenv.sh

.PHONY: help
help: ## 显示本帮助
	@echo "Aegis —— 可用目标："
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'
	@echo ""

# ---------- 环境 ----------

.PHONY: env
env: ## 打印 Go 环境（排查版本问题用）
	@source $(GO_ENV) && go version && go env GOROOT GOPATH GOTOOLCHAIN GOFLAGS

# ---------- 编译 ----------

.PHONY: build
build: ## 编译全部服务到 .run/bin
	@source $(GO_ENV) && mkdir -p $(BIN_DIR)
	@source $(GO_ENV) && for svc in $(SERVICES); do \
		if [ ! -d "$(ROOT)/$$svc/cmd" ]; then \
			printf "  \033[0;90m-\033[0m %-16s (未实现，跳过)\n" "$$svc"; continue; \
		fi; \
		if go build -o $(BIN_DIR)/$$svc ./$$svc/cmd 2>/tmp/aegis_build_err_$$svc; then \
			printf "  \033[0;32m✓\033[0m %s\n" "$$svc"; \
		else \
			printf "  \033[0;31m✗\033[0m %s\n" "$$svc"; \
			cat /tmp/aegis_build_err_$$svc | sed 's/^/      /'; rm -f /tmp/aegis_build_err_$$svc; exit 1; \
		fi; \
	done

.PHONY: build-tools
build-tools: ## 编译命令行工具
	@source $(GO_ENV) && mkdir -p $(BIN_DIR) && \
		go build -o $(BIN_DIR)/smokeclient ./tools/smokeclient && \
		printf "  \033[0;32m✓\033[0m smokeclient\n"

.PHONY: clean
clean: ## 清理编译产物与本地日志
	@rm -rf $(RUN_DIR) coverage.html coverage.out
	@source $(GO_ENV) && go clean -testcache
	@echo "  \033[0;32m✓\033[0m 已清理"

# ---------- Sonic 兼容构建 ----------
#
# Sonic 是构建期通过 -tags sonic 编译进去的，不是运行期开关。
# 因此这条路径必须与默认构建一起进 CI：
# 只测默认构建的话，「加了 -tags sonic 就编不过」这类问题
# 要等到真正要上高性能的那天才暴露。

# sonic 版本号注入：让运行期自检能报出实际版本，便于排障
SONIC_VER := $(shell bash $(ROOT)/scripts/sonic_version.sh 2>/dev/null)
SONIC_LDFLAGS := $(if $(SONIC_VER),-X github.com/aegis/pkg/jsonx.sonicVer=$(SONIC_VER))
ifeq ($(SONIC_VER),)
$(warning 未解析到 sonic 版本，build-sonic 将不带版本注入（不影响功能，仅自检显示 unknown）)
endif

.PHONY: build-sonic
build-sonic: ## 用 Sonic 引擎编译全部服务到 .run/bin（高性能路径）
	@source $(GO_ENV) && mkdir -p $(BIN_DIR)
	@source $(GO_ENV) && for svc in $(SERVICES); do \
		if [ ! -d "$(ROOT)/$$svc/cmd" ]; then continue; fi; \
		if go build -tags sonic -ldflags "$(SONIC_LDFLAGS) -s -w" \
			-o $(BIN_DIR)/$$svc ./$$svc/cmd 2>/tmp/aegis_build_err_$$svc; then \
			printf "  \033[0;32m✓\033[0m %s\n" "$$svc"; \
		else \
			printf "  \033[0;31m✗\033[0m %s\n" "$$svc"; \
			cat /tmp/aegis_build_err_$$svc | sed 's/^/      /'; rm -f /tmp/aegis_build_err_$$svc; exit 1; \
		fi; \
	done
	@printf "  \033[0;90mengine=sonic version=%s\033[0m\n" "$(or $(SONIC_VER),unknown)"

.PHONY: test-sonic
test-sonic: ## 用 Sonic 引擎跑测试（验证跨引擎行为一致）
	@source $(GO_ENV) && FAILED=0; \
	for m in $(SERVICES) idl pkg tools/smokeclient; do \
		[ -d "$(ROOT)/$$m" ] || continue; \
		printf "\033[0;36m── %s (-tags sonic)\033[0m\n" "$$m"; \
		if go test -tags sonic -race ./$$m/...; then :; else FAILED=1; fi; \
	done; \
	exit $$FAILED

.PHONY: engine-info
engine-info: ## 打印当前 JSON 引擎（两种构建各跑一次，便于对照）
	@source $(GO_ENV) && printf "  \033[0;90m默认构建：\033[0m\n" && \
		go test ./pkg/jsonx/ -run 'TestPrintEngineInfo' -v -count=1 2>&1 | grep -E 'json engine=' || true
	@source $(GO_ENV) && printf "  \033[0;90msonic 构建：\033[0m\n" && \
		go test -tags sonic ./pkg/jsonx/ -run 'TestPrintEngineInfo' -v -count=1 2>&1 | grep -E 'json engine=' || true

.PHONY: model-info
model-info: ## 打印实际生效的模型清单（密钥脱敏），用于确认配置是否加载
	@source $(GO_ENV) && go test ./pkg/conf/ -run 'TestPrintModelConfig' -v -count=1 2>&1 \
		| grep -E 'model_config_test.go|--- (PASS|FAIL)' | sed 's/.*model_config_test.go:[0-9]*: //'

.PHONY: model-check
model-check: ## 校验模型配置文件合法且维度自洽
	@source $(GO_ENV) && go test ./pkg/conf/ -run 'TestLoadExampleConfig' -count=1 && \
		echo "  \033[0;32m✓\033[0m 模型配置合法"

.PHONY: model-probe
model-probe: ## 真实探测模型连通性（会调外部 API；仅探测向量模型，几乎不产生费用）
	@source $(GO_ENV) && \
	if [ -f $(ROOT)/.env ]; then set -a; . $(ROOT)/.env; set +a; fi; \
	go run scripts/model_probe.go

.PHONY: live-check
live-check: ## ★ 用项目 Provider 真实调用各对话模型（产生少量费用，部署前必跑）
	@source $(GO_ENV) && \
	if [ -f $(ROOT)/.env ]; then set -a; . $(ROOT)/.env; set +a; fi; \
	go run scripts/live_chat_check.go

.PHONY: rerank-probe
rerank-probe: ## ★真实探测重排模型（产生少量费用，配置 rerank 后必跑）##
	@source $(GO_ENV) && \
	if [ -f $(ROOT)/.env ]; then set -a; . $(ROOT)/.env; set +a; fi; \
	go run scripts/rerank_probe.go

.PHONY: milvus-probe
milvus-probe: ## ★真实探测 Milvus 适配层（需 Milvus 已启动，不产生费用）##
	@source $(GO_ENV) && \
	if [ -f $(ROOT)/.env ]; then set -a; . $(ROOT)/.env; set +a; fi; \
	go run scripts/milvus_probe.go

.PHONY: milvus-diag
milvus-diag: ## ★诊断 Milvus 一致性行为（写后多久可检索 / Sync 代价，需 Milvus 运行）##
	@source $(GO_ENV) && \
	if [ -f $(ROOT)/.env ]; then set -a; . $(ROOT)/.env; set +a; fi; \
	go run scripts/milvus_diag.go

.PHONY: tenant-audit
tenant-audit: ## 只读巡检：统计存量向量里 tenant 为空的行数（回填决策依据）
	@source $(GO_ENV) && \
	if [ -f $(ROOT)/.env ]; then set -a; . $(ROOT)/.env; set +a; fi; \
	go run scripts/tenant_backfill_audit.go

.PHONY: milvus-clean
milvus-clean: ## 清理探测/诊断残留集合（白名单，只删 aegis_probe_* / aegis_diag_*）##
	@source $(GO_ENV) && \
	if [ -f $(ROOT)/.env ]; then set -a; . $(ROOT)/.env; set +a; fi; \
	go run scripts/drop_probe_colls.go

.PHONY: bench-json
bench-json: ## JSON 引擎性能对比（标准库 vs Sonic）
	@source $(GO_ENV) && echo "  \033[1m── encoding/json ──\033[0m" && \
		go test ./pkg/jsonx/ -run '^$$' -bench 'Benchmark' -benchtime 200000x -count 1 | grep Benchmark
	@source $(GO_ENV) && echo "  \033[1m── sonic ──\033[0m" && \
		go test -tags sonic ./pkg/jsonx/ -run '^$$' -bench 'Benchmark' -benchtime 200000x -count 1 | grep Benchmark

# ---------- 质量 ----------

.PHONY: fmt
fmt: ## 格式化（gofmt -w）
	@source $(GO_ENV) && gofmt -l -w \
		$(foreach svc,$(SERVICES),./$(svc)) ./idl ./pkg ./tools 2>/dev/null
	@echo "  \033[0;32m✓\033[0m 已格式化"

.PHONY: fmt-check
fmt-check: ## 检查格式（CI 用，不改文件）
	@source $(GO_ENV) && UNFORMATTED=$$(gofmt -l \
		$(foreach svc,$(SERVICES),./$(svc)) ./idl ./pkg ./tools 2>/dev/null); \
	if [ -n "$$UNFORMATTED" ]; then \
		echo "  \033[0;31m✗\033[0m 以下文件未格式化："; \
		echo "$$UNFORMATTED" | sed 's/^/      /'; exit 1; \
	fi
	@echo "  \033[0;32m✓\033[0m 格式正确"

.PHONY: vet
vet: ## 静态检查
	@source $(GO_ENV) && FAILED=0; \
	for svc in $(SERVICES); do \
		[ -d "$(ROOT)/$$svc" ] || continue; \
		if go vet ./$$svc/... 2>&1 | sed 's/^/      /' | grep -q .; then \
			printf "  \033[0;31m✗\033[0m %s\n" "$$svc"; go vet ./$$svc/... 2>&1 | sed 's/^/      /'; FAILED=1; \
		else \
			printf "  \033[0;32m✓\033[0m %s\n" "$$svc"; \
		fi; \
	done; \
	go vet ./idl/... ./pkg/... 2>&1 | sed 's/^/      /' || FAILED=1; \
	exit $$FAILED

.PHONY: lint
lint: fmt-check vet env-audit ## 格式化检查 + vet + 环境变量审计

# ---------- 环境变量审计 ----------
#
# ★ 为什么要单独设一个目标：
#   pkg/conf 的 applyEnv 是**显式逐字段声明**的（不用反射）。
#   好处是行为可预测，代价是——写错变量名不会被任何机制拦住：
#   不报错、不警告、不生效，只是安静地被忽略。
#   本项目已经因此踩坑两次（部署文档整批 RAG_*、compose 的
#   AEGIS_LLM__API_KEY），症状都是「配置看着配了，运行时却不认」。
#
#   这个目标把该类问题变成 CI 可捕获的失败，而不是靠人肉比对。
.PHONY: env-audit
env-audit: ## 审计 env 变量名是否真能被 conf.go 读取（CI用）
	@python3 $(ROOT)/scripts/env_audit.py

# ---------- 测试 ----------

.PHONY: test
test: ## 全量测试（-race）
	@source $(GO_ENV) && FAILED=0; \
	for m in $(SERVICES) idl pkg tools/smokeclient; do \
		[ -d "$(ROOT)/$$m" ] || continue; \
		printf "\033[0;36m── %s\033[0m\n" "$$m"; \
		if go test -race ./$$m/...; then :; else FAILED=1; fi; \
	done; \
	exit $$FAILED

.PHONY: test-short
test-short: ## 快速测试（跳过 -race）
	@source $(GO_ENV) && FAILED=0; \
	for m in $(SERVICES) idl pkg tools/smokeclient; do \
		[ -d "$(ROOT)/$$m" ] || continue; \
		printf "\033[0;36m── %s\033[0m\n" "$$m"; \
		if go test ./$$m/...; then :; else FAILED=1; fi; \
	done; \
	exit $$FAILED

.PHONY: cover
cover: ## 生成覆盖率报告 → coverage.html
	@source $(GO_ENV) && go test -coverprofile=coverage.out \
		$(foreach svc,$(SERVICES),./$(svc)/...) ./pkg/... ./idl/... > /dev/null
	@source $(GO_ENV) && go tool cover -html=coverage.out -o coverage.html
	@source $(GO_ENV) && go tool cover -func=coverage.out | tail -1
	@echo "  \033[0;32m✓\033[0m 报告：$(ROOT)/coverage.html"

.PHONY: cover-func
cover-func: ## 按函数查看覆盖率（>50% 的单独列出，便于找盲区）
	@source $(GO_ENV) && go test -coverprofile=coverage.out \
		$(foreach svc,$(SERVICES),./$(svc)/...) ./pkg/... > /dev/null
	@source $(GO_ENV) && go tool cover -func=coverage.out | sort -t'	' -k3 -n | head -30

# ---------- 容器 ----------

.PHONY: docker-build
docker-build: ## 构建业务服务镜像（make docker-build SERVICE=gateway）
	@bash scripts/docker.sh build $(or $(SERVICE),gateway)

.PHONY: docker-build-sonic
docker-build-sonic: ## 构建 Sonic 版镜像（JSON 引擎换 JIT）
	@bash scripts/docker.sh build-sonic $(or $(SERVICE),gateway)

.PHONY: docker-up
docker-up: ## 启动全栈（中间件 + 业务服务）
	@bash scripts/docker.sh up

.PHONY: docker-up-mock
docker-up-mock: ## 仅启动 mock 服务（不连中间件）
	@bash scripts/docker.sh up-mock

.PHONY: docker-up-sonic
docker-up-sonic: ## 启动 Sonic 版业务服务
	@bash scripts/docker.sh up-sonic

.PHONY: docker-down
docker-down: ## 停止并移除容器
	@bash scripts/docker.sh down

.PHONY: docker-logs
docker-logs: ## 查看容器日志（make docker-logs S=gateway）
	@bash scripts/docker.sh logs $(or $(S),gateway)

# ---------- 本地开发 ----------
.PHONY: dev
dev: ## 启停本地服务：make dev / make dev down / make dev restart / make dev status
	@bash scripts/dev.sh $(or $(DEV_CMD),up)

.PHONY: dev-sonic
dev-sonic: ## 以 Sonic 引擎启动本地服务（strict 自检，性能对比用）
	@bash scripts/dev.sh up-sonic

.PHONY: smoke
smoke: ## 端到端冒烟（需服务已启动）
	@bash scripts/smoke.sh

# ---------- 代码生成 ----------

.PHONY: gen
gen: gen-proto ## 生成全部代码（当前仅 proto）

.PHONY: gen-proto
gen-proto: ## 由 .proto 生成 kitex_gen
	@bash scripts/gen_proto.sh

# ---------- CI ----------

.PHONY: check
check: lint test ## CI 门禁：格式 + vet + 测试
	@echo ""
	@echo "  \033[0;32m✓\033[0m 全部检查通过"

# ★ 为什么 check 必须包含 sonic 构建：
#   Sonic 由构建标签门控，默认构建完全不碰它，
#   只跑 make check 的话，"加了 -tags sonic 就编不过"要等到
#   真正要上高性能镜像那天才暴露 —— 那时通常已在生产准备阶段。
.PHONY: check-sonic
check-sonic: fmt-check build-sonic test-sonic ## CI 门禁（Sonic 路径）：格式 + 编译 + 测试
	@echo ""
	@echo "  \033[0;32m✓\033[0m Sonic 路径全部检查通过"

.PHONY: verify
verify: build check smoke ## 完整验证：编译 → 检查 → 冒烟
	@echo ""
	@echo "  \033[0;32m✓\033[0m 端到端验证通过"

# ★ 完整验证（含Sonic 路径）
#
#   verify 跑的是标准库引擎 —— 它无法发现「加了 -tags sonic 就编不过」
#   这类问题。verify-sonic 在其之后再编一遍 Sonic 路径，
#   两者共同构成完整门禁。
.PHONY: verify-sonic
verify-sonic: verify build-sonic test-sonic ## 完整验证 + Sonic 路径
	@echo ""
	@source $(GO_ENV) && echo "  当前引擎：" && \
		go test -tags sonic ./pkg/jsonx/ -run TestPrintEngineInfo -v -count=1 2>&1 | grep -oE 'json engine=.*'
	@echo "  \033[0;32m✓\033[0m Sonic 路径验证通过"