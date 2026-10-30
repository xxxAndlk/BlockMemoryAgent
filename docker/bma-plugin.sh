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
#   docker/bma-plugin.sh clean     # 清理 docker 残留（已停止的插件容器 + 悬空镜像）
#                                  #   clean --all 连运行中的 keeper 也删（会掐断在途会话）
#                                  #   clean --images 额外删已退役架构的旧镜像（firecrawl 自托管栈/上游 od）
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
  docker ps -a --filter "name=bma-plugin-" --format '{{.Names}}  {{.Image}}  {{.Status}}'
  echo
  echo "=== compose 统一管理的长驻服务（open_design / minimax-media） ==="
  docker ps --filter "name=bma-open-design" --filter "name=bma-minimax-media" --format '{{.Names}}  {{.Image}}  {{.Status}}'
  echo
  echo "提示：已停止的插件容器 / 悬空镜像 / 已退役架构的旧镜像占空间，可执行 bash docker/bma-plugin.sh clean --images"
}

# cmd_clean 清理 docker 残留（2026-10-28）：
#   插件 keeper 容器按 (插件, 工作目录哈希) 命名，换项目目录就新增一套，旧容器 stop/孤立后
#   永久残留；镜像重建（同名 tag 重打）后旧镜像变悬空。后端启动时会自动做同一套清理，
#   这里给手动入口（换镜像、磁盘告警、装机回收时用）。
#   默认只删**已停止**的 bma-plugin-* 容器——运行中的可能正被另一个 BMA 实例（tui+server
#   多开）使用；--all 才连运行中的一起删（会掐断在途 MCP 会话，下次调用自动重建）。
#   --images 额外删已退役架构的旧镜像（本地 firecrawl 自托管栈、上游 od 基础镜像；
#   docker rmi 不用 -f，被其他项目引用时自动拒绝，不会误删）。
cmd_clean() {
  local all="" with_images=""
  for arg in "$@"; do
    case "$arg" in
      --all) all="--all" ;;
      --images) with_images="1" ;;
      *) echo "未知参数: $arg（可用: --all / --images）" >&2; exit 1 ;;
    esac
  done
  echo "=== 清理 Docker 残留 ==="
  local filter=(--filter "name=bma-plugin-" --filter "label=com.docker.compose.project=bma")
  local ids
  if [ "$all" = "--all" ]; then
    ids=$(docker ps -aq "${filter[@]}")
  else
    ids=$(docker ps -aq "${filter[@]}" --filter status=exited --filter status=created --filter status=dead)
  fi
  if [ -n "$ids" ]; then
    echo "--- docker rm -f（$(echo "$ids" | wc -l | tr -d ' ') 个）"
    # shellcheck disable=SC2086
    docker rm -f $ids
  else
    echo "(无需要删除的插件容器)"
  fi
  # 悬空镜像安全清理：containerd 镜像存储下 `docker image prune -f` 会把仍被容器
  # 引用的 untagged 镜像一起删掉（2026-10-30 实测：运行中 keeper 的镜像被清成不存在的
  # ID，容器变"幽灵"）。这里显式排除"任何容器（含已停止）引用的镜像 ID"。
  echo "--- 清理悬空镜像（排除仍被容器引用的）"
  local used short
  used=$(docker ps -aq | xargs -r docker inspect -f '{{.Image}}' 2>/dev/null || true)
  for id in $(docker images -q -f dangling=true); do
    short="${id#sha256:}"
    case "$used" in *"$short"*) continue ;; esac
    docker rmi "$id" >/dev/null 2>&1 || true
  done
  if [ -n "$with_images" ]; then
    # 已退役架构的镜像（不参与当前部署）：本地 firecrawl 自托管栈（2026-09 切官方云端后删除）、
    # 上游 od 基础镜像（现用 docker/open-design-daemon 构建的 bma/open-design-daemon:local）。
    local retired=(
      "bma/firecrawl-mcp:local"
      "ghcr.io/firecrawl/firecrawl:latest"
      "ghcr.io/firecrawl/playwright-service:latest"
      "ghcr.io/firecrawl/nuq-postgres:latest"
      "ghcr.io/nexu-io/od:latest"
    )
    echo "--- 删除已退役镜像（被其他项目引用时 docker 会拒绝，不强制）"
    for img in "${retired[@]}"; do
      if docker image inspect "$img" >/dev/null 2>&1; then
        docker rmi "$img" || echo "(跳过 $img：仍被引用或正在使用)"
      fi
    done
  fi
  echo "--- 清理后状态"
  docker ps -a --filter "name=bma-plugin-" --format '{{.Names}}  {{.Image}}  {{.Status}}' \
    || echo "(无插件容器)"
  echo "BMA_CLEAN_DONE"
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
  clean)  shift; cmd_clean "$@" ;;
  media)  cmd_media ;;
  *) echo "用法: $0 [all|build|status|clean [--all] [--images]|media]" >&2; exit 1 ;;
esac
