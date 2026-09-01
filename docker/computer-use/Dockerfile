# computer-use 沙箱镜像：Xvfb 虚拟桌面 + @zavora-ai/computer-use-mcp（stdio MCP）。
# 由 mcpbridge docker transport 以 `docker run -i` 拉起（stdio 桥接，stdin 由桥保持打开），
# enable/disable 即容器启停；noVNC 端口（settings.ports 6081:6081）供人观察 Agent 操作。
# shared 模式（plugins.yaml 2026-09-01 起）：容器 docker run -d 常驻 keeper
#（entrypoint.sh 收到 BMA_KEEPER=1 只起桌面不 exec MCP），MCP server 由各 BMA
# 实例会话经 `docker exec -i node /recorder-mcp.js` 按需拉起，多开实例复用同一容器。
# 构建：docker build -t bma/computer-use-mcp:local docker/computer-use
# 注：FROM 走 daocloud 镜像加速（直连 Docker Hub 在本网络不通）；海外环境可改回 node:22-trixie。
# 必须 trixie（Debian 13）：预编译的 computer-use-napi.linux-x64.node 要求 GLIBC_2.39+，
# bookworm 只有 2.36（dlopen 失败 ERR_DLOPEN_FAILED）。
FROM docker.m.daocloud.io/library/node:22-trixie

# xdotool/wmctrl/xclip/scrot 是 computer-use-mcp Linux 后端的系统依赖（见项目 README Linux 节）。
# ffmpeg 供 recorder-mcp.js 的 screen_record 工具录屏（x11grab 抓 :99 桌面）+ 抽关键帧。
# deb.debian.org 本网络高丢包（502/EOF），切阿里云镜像 + 重试。
RUN sed -i 's|deb.debian.org|mirrors.aliyun.com|g; s|security.debian.org|mirrors.aliyun.com|g' \
      /etc/apt/sources.list.d/debian.sources \
    && apt-get -o Acquire::Retries=3 update \
    && apt-get -o Acquire::Retries=3 install -y --no-install-recommends --fix-missing \
      xvfb x11vnc xdotool fluxbox xauth novnc websockify wmctrl xclip scrot ffmpeg \
    && rm -rf /var/lib/apt/lists/* \
    && npm install -g @zavora-ai/computer-use-mcp && npm cache clean --force

COPY recorder-mcp.js /recorder-mcp.js
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
