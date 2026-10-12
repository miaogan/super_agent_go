# ---- 构建阶段 ----
#
# ★ 为什么单阶段而非多阶段：
#   多阶段（builder + runtime）镜像更小，但 Go workspace 有 9 个模块，
#   每个模块独立 go.mod，依赖要按 9 套依赖树处理，
#   交叉编译时还得额外拷 go.work 相关文件。
#   我们的镜像只在测试环境跑（生产走 K8s 直接用 distroless），
#   因此选单阶段 + alpine 省掉这些复杂度。
#
#   若要上生产，把最后 FROM 换成 gcr.io/distroless/static-debian12 即可，
#   构建逻辑一行不用改。
FROM golang:1.26-alpine

# ★ 必须显式设置 GOPROXY —— 这是实测踩出来的坑，不是"可选优化"。
#
#   golang:alpine 的默认 GOPROXY 是 https://proxy.golang.org,direct。
#   在国内网络（含本项目的开发沙箱）访问 proxy.golang.org 会**长时间
#   挂起直到超时**，表现为 docker build 卡在
#     RUN go mod download all
#   这一步一动不动，日志里没有任何错误信息 —— 极难定位。
#   实测对照：
#     proxy.golang.org            → 25s 超时无响应（curl exit 124）
#     goproxy.cn                  → 200，0.29s
#     mirrors.aliyun.com/goproxy   → 200，4.8s
#
#   做成 ARG 而不是写死，是为了留出后门：
#   内网有私有 Athens/Nexus 代理时，用 --build-arg GOPROXY=... 覆盖即可，
#   不用改 Dockerfile。
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY} \
    GOSUMDB=sum.golang.google.cn

# ★ 只装git，不装 gcc / musl-dev / make。
#
#   本项目零 cgo 代码（已核实：无 import "C"，依赖里无 sqlite3 等 cgo 库），
#   构建一律 CGO_ENABLED=0，因此 gcc 与 musl-dev 完全用不上。
#
#   实测装齐 gcc+musl-dev+make 需要拉26 个包（约 700MB 下载），
#   在网络受限环境里单这一步就要十几分钟 —— 而它对产物毫无贡献。
#
#   若将来引入了 cgo 依赖（如某些 SQLite 驱动），再把
#   `gcc musl-dev` 加回来，并同步把 CGO_ENABLED 改为 1。
#
# git 是必需的：go mod download 从 VCS 直取依赖时不带 git 会失败。
#
# APK_REPO 做成 ARG 同理：alpine 官方源 dl-cdn.alpinelinux.org 国内可达
# 但慢（实测 APKINDEX 拉取 50s+），换清华源 0.28s。需要用官方源时传
# --build-arg APK_REPO= 留空即可。
ARG APK_REPO=https://mirrors.tuna.tsinghua.edu.cn/alpine
RUN if [ -n "${APK_REPO}" ]; then \
        sed -i "s|https://dl-cdn.alpinelinux.org/alpine|${APK_REPO}|g" \
            /etc/apk/repositories; \
    fi \
    && apk add --no-cache git

WORKDIR /src

# ---- 依赖层缓存 ----
#
# 先只拷 go.work / go.work.sum / 各 go.mod / go.sum，
# 让 Docker 在"只改业务代码"时命中依赖层，不必重新拉依赖。
# 这是 Go 多模块项目最有效的一处优化。
COPY go.work go.work.sum ./
COPY agent_service/go.mod    ./agent_service/
COPY auth_service/go.mod     ./auth_service/
COPY gateway/go.mod          ./gateway/
COPY idl/go.mod              ./idl/
COPY llm_service/go.mod      ./llm_service/
COPY pkg/go.mod              ./pkg/
COPY rag_service/go.mod      ./rag_service/
COPY user_service/go.mod     ./user_service/
COPY tools/smokeclient/go.mod ./tools/smokeclient/

RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download all

# ---- 源码层 ----
COPY . .

# SERVICE 由 docker-compose 的 build.args 传入
ARG SERVICE=gateway

# ---- Sonic 构建开关 ----
#
# SONIC_TAGS 留空则走encoding/json（默认，跨平台最稳）；
# 设为 "sonic" 则启用 Sonic JIT（amd64/arm64 上 Marshal 快 ~1.5x、
# Unmarshal 快 ~2.7x，见 make bench-json）。
#
# ★ 为什么用ARG 而不是直接写死：
#   Sonic 依赖 JIT 与平台指令集。在不支持的平台（如 linux/arm64 的部分版本、
#   未来的 Go 版本）虽然能编译通过，但内部会静默回落标准库 ——
#   镜像看起来是"高性能版"，实际没有任何加速。
#   做成可切换参数后，同一份 Dockerfile 能同时产出两种镜像，
#   由部署环境显式选择，而不是靠猜。
ARG SONIC_TAGS=""
ARG SONIC_VER=""

# LDFLAGS 始终带上 -s -w（剥离符号表与调试信息），
# SONIC_VER 存在时再追加版本注入。
#
# ★ 不能写成 ${SONIC_VER:+-ldflags "-X ... -s -w"}：
#   SONIC_VER 为空时整个 -ldflags 都会消失，
#   默认构建就退化成未剥离的二进制（镜像体积多几 MB）。
#   两个变量分开处理，避免这种"参数消失"型故障。
ARG LDFLAGS="-s -w"
ARG SONIC_VER=""

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build \
      ${SONIC_TAGS:+-tags "$SONIC_TAGS"} \
      -trimpath \
      -ldflags "${SONIC_VER:+-X github.com/aegis/pkg/jsonx.sonicVer=$SONIC_VER }${LDFLAGS}" \
      -o /out/app ./${SERVICE}/cmd

# 引擎信息不打在这里 —— 见运行阶段的 LABEL。

# ---- 运行阶段 ----
FROM alpine:3.20

#★ ARG 不会跨 FROM 传递：构建阶段的 ARG SERVICE / SONIC_TAGS
#   在这里必须重新声明才能取值。
#
#   不重新声明的后果（实测踩过）：
#     docker inspect 显示 SERVICE_NAME=   ← 空值
#     docker inspect 显示 Labels=null     ← LABEL 完全丢失
#   前者让 Nacos 注册名退化为空，后者让「排查用的引擎标记」形同虚设。
ARG SERVICE=gateway
ARG SONIC_TAGS=""
ARG APK_REPO=https://mirrors.tuna.tsinghua.edu.cn/alpine
RUN if [ -n "${APK_REPO}" ]; then \
        sed -i "s|https://dl-cdn.alpinelinux.org/alpine|${APK_REPO}|g" \
            /etc/apk/repositories; \
    fi \
    && apk add --no-cache ca-certificates tzdata curl \
    && adduser -D -u 10001 aegis

# 引擎信息打进 OCI label：不进容器也能用 docker inspect 确认，
# 排查「以为用了 sonic 实际没生效」时比进容器 exec 看日志快得多。
#
# ★ 只能写在最后一个 FROM 之后：LABEL 属于镜像最终配置，
#   写在前一个阶段会被后一个 FROM 覆盖丢弃。
LABEL aegis.json.engine="${SONIC_TAGS:-encoding/json}" \
      aegis.service="${SERVICE}"

ENV TZ=Asia/Shanghai \
    GOTOOLCHAIN=local \
    SERVICE_NAME=${SERVICE}

COPY --from=0 /out/app /app/app
# 配置作为默认值，实际部署用环境变量覆盖（见 pkg/conf 的 AEGIS_ 前缀约定）
COPY --from=0 /src/configs /app/configs

WORKDIR /app
USER aegis

# 各服务的 HTTP 健康检查端口（gateway 8000，其余 809x）
ARG HEALTH_PORT=8000
ENV HEALTH_PORT=${HEALTH_PORT}
EXPOSE ${HEALTH_PORT}

# 默认关闭 mock：容器里应连真实依赖。
# 要跑冒烟时用 AEGIS_MOCK__ENABLED=true 打开。
ENV AEGIS_MOCK__ENABLED=false \
    AEGIS_LOG__LEVEL=info \
    AEGIS_LOG__JSON=true

# 优雅停机：先给 SIGTERM 留出处理在途请求的时间
STOPSIGNAL SIGTERM

HEALTHCHECK --interval=10s --timeout=3s --start-period=20s --retries=3 \
  CMD curl -sf "http://127.0.0.1:${HEALTH_PORT}/healthz" || exit 1

ENTRYPOINT ["/app/app"]