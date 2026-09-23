# syntax=docker/dockerfile:1
# 单阶段构建：直接下载 Release 静态二进制 + SHA256 校验（不再本机编译 Go）。
#
# 本 Dockerfile 是自包含的：不 COPY 任何源码，构建上下文只需这一个文件。
#   mkdir build && cp Dockerfile build/ && cd build && docker build -t trae2api:v2026.09.23 .
#
# 版本命名约定（CalVer）：镜像 tag 直接复用 Release 完整 tag，
#   形如 v2026.09.23-1e9ca73（日期 + 短 commit），与上游 Release 一一对应，
#   同日多次构建不会撞车；OCI label 另存二进制 sha256 供校验追溯。
#   查上游版本：
#     docker inspect --format '{{index .Config.Labels "trae2api.upstream.tag"}}' trae2api:v2026.09.23-1e9ca73
#
# 构建示例：
#   docker build \
#     --build-arg IMAGE_VERSION=v2026.09.23-1e9ca73 \
#     --build-arg TRAE2API_VERSION=v2026.09.23-1e9ca73 \
#     --build-arg TRAE2API_SHA256=a95a013a...3f0d \
#     -t trae2api:v2026.09.23-1e9ca73 -t trae2api:latest .
#
ARG IMAGE_VERSION=v2026.09.23-1e9ca73
ARG TRAE2API_VERSION=v2026.09.23-1e9ca73
ARG TRAE2API_SHA256=a95a013aee185b03c2377b282027b55318ba932a094b2f5bdd50c8461c103f0d

FROM alpine:3.20

# 注意：ARG 不会跨 stage 继承，凡在 stage 内使用的都必须在 FROM 之后重新声明，
# 否则 LABEL/RUN 里的 ${VAR} 会触发 UndefinedVar warning 并解析为空字符串。
ARG IMAGE_VERSION
ARG TRAE2API_VERSION
ARG TRAE2API_SHA256
ARG BUILD_DATE
ARG TRAE2API_REPO=https://github.com/jarvanh/trae2api

LABEL org.opencontainers.image.title="trae2api" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.version="${IMAGE_VERSION}" \
      org.opencontainers.image.source="${TRAE2API_REPO}" \
      trae2api.image.version="${IMAGE_VERSION}" \
      trae2api.upstream.tag="${TRAE2API_VERSION}" \
      trae2api.upstream.repo="${TRAE2API_REPO}" \
      trae2api.binary.sha256="${TRAE2API_SHA256}" \
      trae2api.build.mode="release-binary"

RUN apk add --no-cache wget ca-certificates tzdata \
 && adduser -D -u 10001 app \
 && mkdir -p /app/auths /app/data \
 && chown -R app:app /app

# 下载 Release 二进制并校验 SHA256：校验失败即构建失败，杜绝脏版本上线
RUN set -eux; \
    wget -qO /app/trae2api \
      "${TRAE2API_REPO}/releases/download/${TRAE2API_VERSION}/trae2api-linux-amd64"; \
    echo "${TRAE2API_SHA256}  /app/trae2api" | sha256sum -c -; \
    chmod +x /app/trae2api

USER app
WORKDIR /app
EXPOSE 7864
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:7864/healthz || exit 1
ENTRYPOINT ["/app/trae2api", "-config", "/app/config.json"]
