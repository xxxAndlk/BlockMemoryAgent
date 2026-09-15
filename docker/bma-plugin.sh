#!/usr/bin/env bash
# bma-plugin：BMA 插件 docker 一键集成命令。
# 执行一次即可备齐 config/plugins.yaml 全部插件的 docker 依赖：
#   本地构建 4 个镜像（3 个 MCP 桥 bma/*:local + open_design 补丁守护进程
#   bma/open-design-daemon:local，docker/*/Dockerfile）。
# 注意：MCP stdio 容器（computer_use/ui_design/ui_preview）与 open_design service
# 容器由后端在插件 enable 时按需拉起/回收（/api/plugins/enable|disable|reload），
# 本命令只负责"镜像就绪"。
# web_search 已切 firecrawl 官方云端（config/plugins.yaml，http transport），
# 不再构建/拉起本地 firecrawl 栈；需要回退自托管时按 plugins.yaml 备选 1 注释
# 手动 docker compose -f docker/docker-compose.firecrawl.yml up -d + 自 build。
#
# 用法：
#   docker/bma-plugin.sh           # = all（build + status）
#   docker/bma-plugin.sh build     # 只构建/拉取镜像
#   docker/bma-plugin.sh status    # 查看插件镜像与容器状态
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(dirname "$SCRIPT_DIR")"
cd "$REPO_ROOT"

# 镜像清单（与 config/plugins.yaml 的 settings.image 一一对应，改动需同步）。
BUILD_IMAGES=(
  "bma/computer-use-mcp:local|docker/computer-use"
  "bma/open-design-mcp:local|docker/open-design-mcp"
  "bma/ui-preview-mcp:local|docker/ui-preview-mcp"
  # open_design service（kind: service，:7456）：上游 ghcr.io/nexu-io/od 的
  # 本地补丁镜像（volcengine 生图 size 适配 Agent Plan），FROM 自动拉上游。
  "bma/open-design-daemon:local|docker/open-design-daemon"
)

cmd_build() {
  echo "=== 构建插件镜像（MCP 桥 + open_design 补丁守护进程） ==="
  for entry in "${BUILD_IMAGES[@]}"; do
    tag="${entry%%|*}"; dir="${entry##*|}"
    echo "--- docker build -t $tag $dir"
    docker build -t "$tag" "$dir"
  done
}

cmd_status() {
  echo "=== 插件镜像 ==="
  docker images --format '{{.Repository}}:{{.Tag}}  {{.Size}}' \
    | grep -E '^(bma/|ghcr\.io/nexu-io/od)' || echo "(无 bma/* 镜像，先执行 build)"
  echo
  echo "=== 后端拉起的插件容器（bma-plugin-<id>-<pid> / bma-plugin-svc-<id>-<pid>） ==="
  docker ps --filter "name=bma-plugin-" --format '{{.Names}}  {{.Image}}  {{.Status}}'
}

case "${1:-all}" in
  all)    cmd_build; echo; cmd_status; echo; echo "BMA_PLUGIN_DONE" ;;
  build)  cmd_build ;;
  status) cmd_status ;;
  *) echo "用法: $0 [all|build|status]" >&2; exit 1 ;;
esac
