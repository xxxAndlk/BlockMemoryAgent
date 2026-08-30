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

# recorder-mcp.js：复合 MCP stdio 代理——spawn 原 computer-use server 作为子进程，
# 合并 screen_record 录屏工具（ffmpeg x11grab 抓本入口起的 :99 桌面）。
# 录的就是 Agent 自己操作的同一个虚拟桌面。
export COMPUTER_USE_SERVER="$(npm root -g)/@zavora-ai/computer-use-mcp/dist/server.js"
exec node /recorder-mcp.js
