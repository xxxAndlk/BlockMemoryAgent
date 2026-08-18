# image-gen-mcp 镜像：独立免费文生图 stdio MCP server（BMA ui_design 插件）。
# 无本地守护进程依赖，直连免费 provider（智谱 CogView-3-Flash / pollinations.ai）。
# 由 mcpbridge docker transport 以 `docker run -i --rm` 拉起（stdio 桥接）。
# 构建：docker build -t bma/image-gen-mcp:local docker/image-gen-mcp
# 注：FROM 走 daocloud 镜像加速（直连 Docker Hub 在本网络不通）；海外环境可改回 node:20-alpine。
FROM docker.m.daocloud.io/library/node:20-alpine

WORKDIR /app
COPY package.json ./
RUN npm install --omit=dev && npm cache clean --force
COPY server.js ./

# stdio MCP server：stdout 只承载 JSON-RPC 帧，日志一律 stderr。
ENTRYPOINT ["node", "server.js"]
