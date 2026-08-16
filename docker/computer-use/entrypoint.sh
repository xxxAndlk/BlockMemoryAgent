#!/usr/bin/env bash
# computer-use 沙箱入口：先起 Xvfb 虚拟桌面（+ noVNC 观察口），
# 再把 MCP server exec 到 stdio —— stdin/stdout 由 docker run -i 桥接到后端插件桥。
# 注意：stdout 必须只承载 MCP JSON-RPC 帧，所有后台服务的输出一律转走。
set -e
export DISPLAY=:99

Xvfb :99 -screen 0 "${RESOLUTION:-1280x800x24}" >/var/log/xvfb.log 2>&1 &
sleep 2
fluxbox >/var/log/fluxbox.log 2>&1 &
x11vnc -display :99 -forever -nopw -shared >/var/log/x11vnc.log 2>&1 &
websockify --web /usr/share/novnc 6081 localhost:5900 >/var/log/websockify.log 2>&1 &

exec node "$(npm root -g)/@zavora-ai/computer-use-mcp/dist/server.js"
