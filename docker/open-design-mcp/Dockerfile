# open-design-mcp 桥镜像：把 open_design 守护进程（service 插件，:7456）的
# 媒体生成 REST API 包装成 stdio MCP server（BMA ui_design 插件）。
# 由 mcpbridge docker transport 以 `docker run -i --rm` 拉起（stdio 桥接）。
# 构建：docker build -t bma/open-design-mcp:local docker/open-design-mcp
# 注：FROM 走 daocloud 镜像加速（直连 Docker Hub 在本网络不通）；海外环境可改回 node:20-alpine。
FROM docker.m.daocloud.io/library/node:20-alpine

WORKDIR /app
COPY package.json ./
RUN npm install --omit=dev && npm cache clean --force
COPY server.js ./

# stdio MCP server：stdout 只承载 JSON-RPC 帧，日志一律 stderr。
ENTRYPOINT ["node", "server.js"]
