# 测试 Demo

本文档列出验证 BlockMemoryAgent 核心能力的标准方式，替代早期不相关的游戏开发示例。

## 1. 单元测试

```bash
cd D:/data/project/BlockMemoryAgent/backend
GOTOOLCHAIN=local go test ./... -count=1
```

关键包覆盖（现行，2026-09-24 复核）：`internal/agent`、`internal/domain/subagent`、`internal/domain/tool`、`internal/domain/memory`、`internal/domain/orchestrator`、`internal/board`、`internal/mailbox`、`internal/skill`、`internal/soul`、`internal/config`、`internal/model`、`internal/server`、`internal/tui`、`internal/bootstrap`、`internal/store`、`internal/retriever`、`internal/plugins`、`pkg/textutil` 等 37 个 `_test.go`。**历史包已删**：`internal/watchdog`、`internal/memory`、`internal/graph`、`internal/runtime` 死重（ReAct 重构期清理，见 `doc/变更.md`）。

## 2. 集成测试

集成测试位于独立模块 `test/`（`module .../backend/test`，`replace` 指向 `../backend`；根 `go.work` 当前**不含** `./test`，在 `test/` 目录内独立跑）。31 个文件带 `//go:build integration` 标签，用 mock LLM/SSE 驱动完整链路（`test/coding/` 编程场景、`test/api/` HTTP 端点、`fixtures/` 共享测试基建）。

```bash
cd D:/data/project/BlockMemoryAgent/test
# 纯单测（无 tag，如 role_config_sanity_test.go）：
GOTOOLCHAIN=local go test ./... -count=1
# 集成套件（需 PG + Redis：fixtures 经 docker-compose.test.yml 起共享容器，
# 隔离端口 PG 55432 / Redis 56380，每包独立库；表结构由服务启动期 ensureSchemas 幂等自举）
GOTOOLCHAIN=local go test -tags integration ./... -count=1
```

> 注：PG/Redis 集成套件曾在 commit b35f87e 移除，其后按新端点逐步重建（现行 37 个 `_test.go`，与 `doc/TODO.md` #1 口径一致）；**无需手工执行 `migrations/*.sql`**——表结构由服务启动期 `ensureSchemas` 幂等自举（见 `doc/技术逻辑/10-存储层与数据表.md`）。

### 2.1 编程主场景（`test/coding/`）

- `snake_test.go`：写贪吃蛇小游戏并运行，验证会话到达终态、goal 传入 LLM、记忆落库。
- `css_refactor_test.go`：CSS 重构场景，验证 DomainAgent 任务拆分与助手执行。
- `bug_fix_test.go`：bug 修复场景，验证工具调用与结果汇总。

### 2.2 HTTP API 测试（`test/api/`）

覆盖会话 CRUD、SSE 流、错误路径、404/空 body 等边界。

### 2.3 TUI 测试（`test/tui/`）

模拟按键流验证 TUI 状态机、命令模式、话题切换等交互。

## 3. 端到端启动验证

```bash
# 1. 起基础设施
docker compose -f docker/docker-compose.yml up -d

# 2. 应用全部迁移（按顺序）
for f in migrations/*.sql; do psql "$POSTGRES_DSN" -f "$f"; done

# 3. 构建前端
cd web && npm install && npm run build && cd ..

# 4. 启动服务
cd backend && GOTOOLCHAIN=local go run .
```

访问 `http://localhost:10010` 创建会话并发送消息，观察 SSE 事件流与 `/api/sessions/{id}/board` 任务看板。

## 4. 记忆层可观测

```bash
# 查看压缩分布（需先跑完 coding/api 测试）
curl "http://localhost:10010/api/memory/eval"

# 查看会话日志
curl "http://localhost:10010/api/sessions/{id}/logs"
```

## 5. 本地无数据库时

仅跑不依赖 PG/Redis 的单元测试：

```bash
GOTOOLCHAIN=local go test ./backend/internal/config/... -count=1
```
