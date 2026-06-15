#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

if ! command -v docker >/dev/null 2>&1; then
  echo "未检测到 docker，请先安装 Docker Engine / Docker Compose。" >&2
  exit 1
fi

if ! docker compose version >/dev/null 2>&1; then
  echo "未检测到 docker compose，请先安装 Docker Compose v2。" >&2
  exit 1
fi

if [ ! -f .env ]; then
  cp .env.example .env
  echo "已生成 deploy/.env。请先编辑域名和管理员密码："
  echo "  LANQIN_PUBLIC_HOSTNAME"
  echo "  LANQIN_PUBLIC_BASE_URL"
  echo "  LANQIN_ADMIN_EMAIL"
  echo "  LANQIN_ADMIN_PASSWORD"
  echo
  echo "编辑完成后再次执行：./install.sh"
  exit 0
fi

echo "拉取镜像..."
docker compose pull

echo "启动服务..."
docker compose up -d

echo "完成。查看日志：docker compose logs -f lanqin-email"
