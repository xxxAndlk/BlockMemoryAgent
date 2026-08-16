# firecrawl-mcp 的容器封装：把官方 MCP server 预装进镜像，
# 避免插件 enable 时 npx 现场下载（冷启动曾卡 13 分钟）。
# 构建：docker build -t bma/firecrawl-mcp:local docker/firecrawl-mcp
# 运行（由 mcpbridge docker transport 拉起）：
#   docker run -i --rm -e FIRECRAWL_API_URL -e FIRECRAWL_API_KEY bma/firecrawl-mcp:local
# 注：FROM 走 daocloud 镜像加速（直连 Docker Hub 在本网络不通）；
# 海外环境可改回 node:20-alpine。
FROM docker.m.daocloud.io/library/node:20-alpine

RUN npm install -g firecrawl-mcp && npm cache clean --force

# stdio MCP server：FIRECRAWL_API_URL 指向自托管栈（默认云端 https://api.firecrawl.dev），
# FIRECRAWL_API_KEY 自托管模式可任意非空值。
ENTRYPOINT ["firecrawl-mcp"]
