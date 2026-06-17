# BlockMemoryAgent 部署教程

## 目录

1. [环境要求](#1-环境要求)
2. [快速开始（5分钟）](#2-快速开始5分钟)
3. [配置详解](#3-配置详解)
4. [Docker 部署（推荐）](#4-docker-部署推荐)
5. [二进制部署](#5-二进制部署)
6. [生产环境部署](#6-生产环境部署)
7. [常见问题](#7-常见问题)

---

## 1. 环境要求

| 组件 | 最低版本 | 用途 |
|------|----------|------|
| Go | 1.21+ | 编译运行（推荐 1.25+） |
| PostgreSQL | 14+ | 持久化存储 + pgvector |
| pgvector | 0.5+ | 向量检索扩展 |
| Redis | 7+ | 热数据缓存 |
| OpenAI 兼容 API | - | LLM 模型调用 |

---

## 2. 快速开始（5分钟）

### 2.1 克隆项目

```bash
git clone <repo-url>
cd BlockMemoryAgent
```

### 2.2 启动基础设施

```bash
# 使用 Docker Compose 启动 PostgreSQL + Redis
cd docker
docker compose up -d

# 验证服务状态
docker compose ps
# 两个服务都应该是 healthy 状态
```

### 2.3 配置环境变量

```bash
# 回到项目根目录
cd ..

# 从模板创建 .env 文件
cp .env.example .env
```

编辑 `.env` 文件，填入你的实际配置：

```bash
# 必填: 你的 OpenAI 兼容 API Key
OPENAI_API_KEY=sk-your-actual-api-key

# PostgreSQL（与 docker-compose 中配置一致）
POSTGRES_DSN=postgres://blockmemory:blockmemory_dev@localhost:5432/blockmemory?sslmode=disable

# Redis（与 docker-compose 中配置一致）
REDIS_ADDR=localhost:6379
REDIS_PASSWORD=blockmemory_dev

# HTTP 服务端口
HTTP_ADDR=:10010
```

### 2.4 初始化数据库

连接 PostgreSQL 并启用 pgvector 扩展：

```bash
# 使用 psql 连接
docker exec -it blockmemory-postgres psql -U blockmemory -d blockmemory

# 在 psql 中执行
CREATE EXTENSION IF NOT EXISTS vector;
\q
```

### 2.5 编译运行

```bash
# 编译
go build -o blockmemory-agent ./backend

# 运行
./blockmemory-agent
```

看到以下输出表示启动成功：

```
Loaded environment variables from .env
Starting session: session-001, goal: 修复商城主页穿模问题
```

---

## 3. 配置详解

BlockMemoryAgent 使用三层配置体系：

### 3.1 环境变量文件（`.env`）

**优先级最高**。系统启动时首先加载 `.env` 文件，将变量注入到进程环境。已有的系统环境变量不会被覆盖。

```bash
# .env 文件格式说明

# 注释以 # 开头
# 支持引号包裹值（可选）
OPENAI_API_KEY=sk-abc123

# 引号包裹（值中包含特殊字符时使用）
POSTGRES_DSN="postgres://user:p@ss@localhost:5432/blockmemory?sslmode=disable"

# 空值
REDIS_PASSWORD=
```

**优先级规则**：
```
系统环境变量 > .env 文件 > config.yaml 默认值
```

### 3.2 基础设施配置（`config/config.yaml`）

使用 `${VAR:"default"}` 语法引用环境变量：

```yaml
postgres:
  # ${POSTGRES_DSN} 读取环境变量，:"..." 为默认值
  dsn: ${POSTGRES_DSN:"postgres://user:pass@localhost:5432/blockmemory?sslmode=disable"}
  max_open_conns: 25
  max_idle_conns: 5
  conn_max_lifetime: 300

pgvector:
  enabled: true
  dimensions: 768
  index_type: ivfflat
  distance_metric: cosine

redis:
  addr: ${REDIS_ADDR:"localhost:6379"}
  password: ${REDIS_PASSWORD:""}
  db: 5
  pool_size: 10
  snapshot_ttl_days: 7

http:
  addr: ${HTTP_ADDR:":10010"}

memory:
  write_batch_size: 100
  write_flush_interval: 5
  snapshot_interval: 300
```

### 3.3 角色配置（`config/roles.yaml`）

定义 Agent 角色和模型配置，API Key 也支持环境变量：

```yaml
meta_agent:
  model_config:
    provider: openai
    model: deepseek-v4-flash
    api_key: ${OPENAI_API_KEY}    # 引用 .env 中的变量
    temperature: 0.3
    max_tokens: 2048
  max_blocks: 10
  summary_interval: 5

domain_agent:
  model_config:
    provider: openai
    model: deepseek-v4-flash
    api_key: ${OPENAI_API_KEY}
    temperature: 0.5
    max_tokens: 4096

fixed_roles:
  - id: code_assistant
    name: 代码助手
    type: fixed
    lifecycle: permanent
    model_config:
      provider: openai
      model: deepseek-v4-flash
      api_key: ${OPENAI_API_KEY}
      temperature: 0.4
      max_tokens: 4096
    # ... 更多角色配置
```

---

## 4. Docker 部署（推荐）

### 4.1 启动基础设施

```bash
cd docker
docker compose up -d
```

这将启动：
- **PostgreSQL 16 + pgvector**（端口 5432）
- **Redis 7**（端口 6379）

### 4.2 验证基础设施

```bash
# 检查 PostgreSQL
docker exec -it blockmemory-postgres psql -U blockmemory -d blockmemory -c "SELECT version();"

# 检查 pgvector 扩展
docker exec -it blockmemory-postgres psql -U blockmemory -d blockmemory -c "CREATE EXTENSION IF NOT EXISTS vector;"

# 检查 Redis
docker exec -it blockmemory-redis redis-cli -a blockmemory_dev ping
# 应返回 PONG
```

### 4.3 配置并启动应用

```bash
cd ..

# 创建 .env
cp .env.example .env
# 编辑 .env 填入实际值

# 编译并运行
go build -o blockmemory-agent ./backend
./blockmemory-agent
```

---

## 5. 二进制部署

### 5.1 编译

```bash
# Linux
GOOS=linux GOARCH=amd64 go build -o blockmemory-agent ./backend

# Windows
GOOS=windows GOARCH=amd64 go build -o blockmemory-agent.exe ./backend

# macOS (Apple Silicon)
GOOS=darwin GOARCH=arm64 go build -o blockmemory-agent ./backend
```

### 5.2 部署文件结构

```
/opt/blockmemory/
├── blockmemory-agent       # 二进制文件
├── .env                    # 环境变量配置
├── config/
│   ├── config.yaml         # 基础设施配置
│   └── roles.yaml          # 角色配置
└── logs/                   # 日志目录（可选）
```

### 5.3 运行

```bash
cd /opt/blockmemory

# 前台运行（调试用）
./blockmemory-agent

# 指定配置路径
./blockmemory-agent -config config/config.yaml -roles config/roles.yaml -env .env

# 后台运行
nohup ./blockmemory-agent > logs/app.log 2>&1 &
```

---

## 6. 生产环境部署

### 6.1 环境变量安全

**不要将 `.env` 文件提交到 Git！** `.gitignore` 已包含 `.env`。

生产环境推荐：
- 使用系统环境变量或密钥管理服务
- `.env` 文件权限设为 `600`：`chmod 600 .env`
- 定期轮换 API Key

### 6.2 PostgreSQL 生产配置

修改 `docker-compose.yml` 或使用托管数据库：

```yaml
postgres:
  environment:
    POSTGRES_PASSWORD: <强密码>
  # 生产环境不要暴露端口到公网
  ports:
    - "127.0.0.1:5432:5432"
```

`.env` 中对应的 DSN：
```bash
POSTGRES_DSN=postgres://blockmemory:<强密码>@localhost:5432/blockmemory?sslmode=require
```

### 6.3 Redis 生产配置

```yaml
redis:
  command: redis-server --requirepass <强密码> --appendonly yes
  ports:
    - "127.0.0.1:6379:6379"
```

### 6.4 使用自定义 LLM 提供商

项目使用 OpenAI 兼容协议，支持任何兼容 API：

```yaml
# roles.yaml 中配置
meta_agent:
  model_config:
    provider: openai
    model: your-model-name
    api_key: ${YOUR_API_KEY}
    base_url: https://your-api-endpoint/v1    # 自定义端点
    temperature: 0.3
    max_tokens: 2048
```

`.env` 中：
```bash
YOUR_API_KEY=your-api-key
```

### 6.5 Systemd 服务（Linux）

创建 `/etc/systemd/system/blockmemory.service`：

```ini
[Unit]
Description=BlockMemoryAgent
After=network.target docker.service

[Service]
Type=simple
User=blockmemory
WorkingDirectory=/opt/blockmemory
ExecStart=/opt/blockmemory/blockmemory-agent
Restart=on-failure
RestartSec=5

# 安全限制
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=/opt/blockmemory/logs

# 环境变量
EnvironmentFile=/opt/blockmemory/.env

[Install]
WantedBy=multi-user.target
```

启动服务：
```bash
sudo systemctl daemon-reload
sudo systemctl enable blockmemory
sudo systemctl start blockmemory

# 查看日志
sudo journalctl -u blockmemory -f
```

---

## 7. 常见问题

### Q: 启动时提示 "No .env file found"

正常提示。如果没有 `.env` 文件，程序会使用系统环境变量和 `config.yaml` 中的默认值。

### Q: 启动时提示 "Warning: postgres not available"

PostgreSQL 未启动或连接配置错误：
1. 检查 Docker 容器是否运行：`docker compose ps`
2. 检查 `.env` 中 `POSTGRES_DSN` 是否正确
3. 等待 PostgreSQL healthcheck 通过后重试

### Q: 启动时提示 "Warning: redis not available"

Redis 未启动或连接配置错误，同上排查。

### Q: LLM 模型调用失败，返回模拟响应

检查：
1. `.env` 中 `OPENAI_API_KEY` 是否正确
2. 网络是否能访问 LLM API 端点
3. `roles.yaml` 中 `model_config.base_url` 是否正确（如使用自定义端点）

### Q: 如何使用 DeepSeek / 通义千问 / 其他国产模型？

在 `roles.yaml` 中设置 `base_url` 和 `model`：

```yaml
meta_agent:
  model_config:
    provider: openai
    model: deepseek-chat
    api_key: ${DEEPSEEK_API_KEY}
    base_url: https://api.deepseek.com/v1
```

### Q: 如何修改 HTTP 服务端口？

方式一：修改 `.env`
```bash
HTTP_ADDR=:8080
```

方式二：修改 `config/config.yaml`
```yaml
http:
  addr: ${HTTP_ADDR:":8080"}
```

方式三：命令行环境变量
```bash
HTTP_ADDR=:8080 ./blockmemory-agent
```

### Q: 如何查看调试信息？

设置日志级别：
```bash
# Go 运行时调试
GODEBUG=http2debug=1 ./blockmemory-agent
```
