# MiniMax 媒体适配桥（BMA，2026-10-23）。
#
# 单文件零依赖 Node 服务：把 MiniMax 开放平台/M Plan 订阅 Key 的
# /v1/image_generation 与 /v1/video_generation 翻译成 open_design daemon 认识的
# 两种形状（OpenAI 图像 / grok 线视频），从而 **daemon 与 ui_design 桥零改动**。
#
# 构建/启动：docker compose -f docker/docker-compose.yml up -d --build minimax-media
#（`bash docker/bma-plugin.sh media` 会一并拉起）。
# 健康检查：curl http://127.0.0.1:8787/health（不回显 key，只报 key 是否配置）。
# 注：FROM 走 daocloud 镜像加速（直连 Docker Hub 在本网络不通，与 open-design-mcp 同）；
# 海外环境可改回 node:20-alpine（桥只需要 node:18+ 的全局 fetch，无第三方依赖）。
FROM docker.m.daocloud.io/library/node:20-alpine

WORKDIR /app
COPY server.js /app/server.js

ENV MINIMAX_MEDIA_PORT=8787
EXPOSE 8787

# 基础镜像自带 node 用户；桥只做无状态转发，不需要 root。
USER node

CMD ["node", "/app/server.js"]
