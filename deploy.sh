#!/usr/bin/env bash
# trae2api 部署脚本：自动解析最新 Release → 生成 CalVer 镜像 tag → 构建并重启容器
#
# 用法：
#   bash deploy.sh            # 自动取 fork 最新 Release
#   bash deploy.sh v2026.09.23-c66d08c   # 指定 Release tag
#
# 设计要点：
#   - 镜像 tag 用 CalVer（vYYYY.MM.DD），从 Release tag 去掉 -<commit> 后缀得到
#   - 二进制 sha256 从 Release 的 SHA256SUMS 实时提取，与版本强绑定
#   - 数据全在 Dropbox 挂载，重建容器不丢数据

set -euo pipefail

REPO="jarvanh/trae2api"
COMPOSE_ENV="/dropbox/self-hosted/trae2api/.env"
APP_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

cd "$APP_DIR"

echo "==> 解析最新 Release"
if [[ $# -ge 1 ]]; then
  RELEASE_TAG="$1"
else
  RELEASE_TAG="$(gh api "repos/${REPO}/releases/latest" --jq '.tag_name')"
fi
echo "    Release tag: ${RELEASE_TAG}"

# 镜像 tag 直接复用 Release 完整 tag（含 -<commit>，同日多构建不撞车）
#   v2026.09.23-1e9ca73 -> v2026.09.23-1e9ca73
IMAGE_TAG="${RELEASE_TAG}"
echo "    镜像 tag    : ${IMAGE_TAG}"

echo "==> 获取二进制 sha256（从 SHA256SUMS 实时提取）"
SHA256="$(curl -fsSL "https://github.com/${REPO}/releases/download/${RELEASE_TAG}/SHA256SUMS" \
  | awk '$2 == "./trae2api-linux-amd64" {print $1}')"
if [[ -z "${SHA256}" ]]; then
  echo "❌ 未能提取 sha256，拒绝构建" >&2
  exit 1
fi
echo "    sha256      : ${SHA256}"

export TRAE2API_IMAGE_VERSION="${IMAGE_TAG}"
export TRAE2API_RELEASE_TAG="${RELEASE_TAG}"
export TRAE2API_SHA256="${SHA256}"
export BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

echo "==> 构建镜像 trae2api:${IMAGE_TAG} + latest"
docker compose --env-file "${COMPOSE_ENV}" build

docker tag "trae2api:${IMAGE_TAG}" "trae2api:latest"

echo "==> 重启容器（数据在 Dropbox 挂载，不丢失）"
docker compose --env-file "${COMPOSE_ENV}" up -d

echo "==> 等待健康检查"
for i in $(seq 1 30); do
  STATUS="$(docker inspect trae2api --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' 2>/dev/null || echo none)"
  if [[ "${STATUS}" == "healthy" ]]; then
    echo "    ✅ healthy (${i}s)"
    break
  fi
  sleep 1
done

echo "==> 最终状态"
docker ps --filter name=trae2api --format 'table {{.Names}}\t{{.Image}}\t{{.Status}}'
curl -fsS -o /dev/null -w '    healthz: HTTP %{http_code}\n' http://127.0.0.1:7864/healthz || echo "    ⚠️ healthz 未通过"
