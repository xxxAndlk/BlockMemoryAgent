#!/usr/bin/env bash
# bma-plugin：BMA 插件 docker 一键集成命令。
# 执行一次即可备齐 config/plugins.yaml 全部插件的 docker 依赖：
#   本地构建 3 个 MCP 桥镜像（bma/*:local，docker/*/Dockerfile）。
# 注意：MCP stdio 容器（computer_use/ui_design/ui_preview）由后端在插件
# enable 时按需拉起/回收（/api/plugins/enable|disable|reload），
# 本命令只负责"镜像就绪"。
# open_design 长驻服务已迁移为 docker compose 统一管理
#（docker/docker-compose.yml open_design 服务，up -d 时缺镜像自动构建），
# 不再由本脚本构建、也不再经后端插件按需拉起。
# web_search 已切 firecrawl 官方云端（config/plugins.yaml，http transport），
# 不再构建/拉起本地 firecrawl 栈。
#
# 用法：
#   docker/bma-plugin.sh           # = all（build + status）
#   docker/bma-plugin.sh build     # 只构建/拉取镜像
#   docker/bma-plugin.sh status    # 查看插件镜像与容器状态
#   docker/bma-plugin.sh media     # 应用媒体生成单点配置（仓库根 .env 媒体段 →
#                                  #   重建 open_design 容器重写 media-config.json + 换 ui_design keeper）
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(dirname "$SCRIPT_DIR")"
cd "$REPO_ROOT"

# 镜像清单（与 config/plugins.yaml 的 settings.image 一一对应，改动需同步）。
BUILD_IMAGES=(
  "bma/computer-use-mcp:local|docker/computer-use"
  "bma/open-design-mcp:local|docker/open-design-mcp"
  "bma/ui-preview-mcp:local|docker/ui-preview-mcp"
)

cmd_build() {
  echo "=== 构建插件镜像（MCP 桥） ==="
  for entry in "${BUILD_IMAGES[@]}"; do
    tag="${entry%%|*}"; dir="${entry##*|}"
    echo "--- docker build -t $tag $dir"
    docker build -t "$tag" "$dir"
  done
}

cmd_status() {
  echo "=== 插件镜像 ==="
  docker images --format '{{.Repository}}:{{.Tag}}  {{.Size}}' \
    | grep -E '^bma/' || echo "(无 bma/* 镜像，先执行 build)"
  echo
  echo "=== 后端拉起的插件容器（bma-plugin-<id>[-<目录哈希>]） ==="
  docker ps --filter "name=bma-plugin-" --format '{{.Names}}  {{.Image}}  {{.Status}}'
  echo
  echo "=== compose 统一管理的长驻服务（open_design / minimax-media） ==="
  docker ps --filter "name=bma-open-design" --filter "name=bma-minimax-media" --format '{{.Names}}  {{.Image}}  {{.Status}}'
}

# cmd_media 媒体生成配置单点应用（2026-10-14）：唯一手改点 = 仓库根 .env 媒体段。
# daemon 侧：重建容器让 entrypoint 重写 media-config.json（env_file 仅容器创建时读取）；
# 桥侧：重建镜像（server.js 改动才需要，层缓存幂等）+ 换 keeper（共享容器按 image tag
# 字符串比较复用，不换新会继续跑旧代码/旧 env）。
cmd_media() {
  echo "=== 媒体生成配置单点应用（源 = 仓库根 .env 媒体段） ==="
  grep -E '^(IMAGE_PROVIDER|IMAGE_URL|IMAGE_MODEL|IMAGE_KEY|VIDEO_PROVIDER|VIDEO_URL|VIDEOS_URL|VIDEO_MODEL|VIDEOS_MODEL|VIDEO_KEY|MINIMAX_[A-Z_]+)=' "$REPO_ROOT/.env" \
    | sed -E 's/((_KEY|_TOKEN)=).*/\1***/' || echo "(媒体段未声明任何变量——将保持容器内既有配置)"
  echo "--- 重建桥镜像（层缓存幂等）"
  docker build -t bma/open-design-mcp:local docker/open-design-mcp >/dev/null
  echo "--- 拉起 minimax-media 适配桥（通道 D 用；未选通道时空转，无副作用）"
  docker compose -f docker/docker-compose.yml up -d --build minimax-media
  echo "--- 重建 open_design 容器（entrypoint 重写 media-config.json，排他写入按 PROVIDER 选择）"
  docker compose -f docker/docker-compose.yml up -d --build --force-recreate open_design
  sleep 5
  echo "--- 生效中的 media-config.json"
  # MSYS_NO_PATHCONV=1：Git Bash 会把容器内路径 /app/... 误转成 Windows 路径（Linux 上无害）。
  MSYS_NO_PATHCONV=1 docker exec bma-open-design cat /app/.od/media-config.json
  echo "--- 换 ui_design keeper（共享容器按 image tag 复用不换新；会掐断在途 MCP 会话，后端下次调用自动重建）"
  docker rm -f $(docker ps -aq --filter name=bma-plugin-ui_design) 2>/dev/null || true
  echo "--- entrypoint 生效摘要（容器日志）"
  MSYS_NO_PATHCONV=1 docker logs bma-open-design 2>&1 | grep 'bma-entrypoint' | tail -3 || true
  echo "BMA_MEDIA_DONE"
}

case "${1:-all}" in
  all)    cmd_build; echo; cmd_status; echo; echo "BMA_PLUGIN_DONE" ;;
  build)  cmd_build ;;
  status) cmd_status ;;
  media)  cmd_media ;;
  *) echo "用法: $0 [all|build|status|media]" >&2; exit 1 ;;
esac
