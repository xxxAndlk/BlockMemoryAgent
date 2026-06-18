# Web UI 功能缺口清单

`frontend/` 已删除，新前端项目迁移到 `web/`。以下功能当前仍用 mock 数据占位，需后端 / 存储层补齐后替换真实 API。

## 已接入的真实接口

- `GET  /api/sessions` — 会话列表
- `POST /api/sessions` — 创建会话
- `GET  /api/sessions/{id}` — 会话详情
- `GET  /api/sessions/{id}/stream` — SSE 事件流
- `GET  /api/sessions/{id}/board` — 任务看板快照
- `GET  /api/sessions/{id}/agents` — 会话内智能体

## Dashboard 首页 (mock)

| 前端展示 | 状态 | 待实现接口 |
|---|---|---|
| 顶部 Program/Mode/LLM API/Health 状态芯片 | mock | `/api/status` 或 `/api/health` |
| 统计概览 (会话数/完成率/Token/超时率) | 部分由 events 计算，缺省时 mock | 全局聚合接口 |
| 趋势图 | mock 数组 | `/api/metrics/timeline` |
| 最近活动 | mock | `/api/activity` |

## 会话详情页 (mock)

| 前端展示 | 状态 | 待实现接口 |
|---|---|---|
| 执行日志 | 真实 events + markdown 渲染 | 已接入 SSE |
| 角色层级 | 优先用 `/agents` | 已接入 |
| 任务看板 | 优先用 `/board` | 已接入 |
| 约束条件 | 来自 board.constraints，否则 mock | 已接入 board |
| 实时指标 (LLM 调用/超时/耗时/上下文 token) | 由 events 计算，兜底 mock | `/api/metrics/session/{id}` |
| 看门狗状态 | mock | `/api/watchdog` |
| 邮箱通知 | mock | `/api/mailbox` |
| 系统健康 (Postgres/Redis/LLM) | mock | `/api/health` |

## 记忆浏览器 / Skill 装配 / 文件预览

| 前端展示 | 状态 | 待实现接口 |
|---|---|---|
| 记忆快照、检索、压缩级别、实体关系 | 全 mock | `/api/memory/*` |
| Skill 候选池、已装配技能、装配历史 | 全 mock | `/api/skills/*` |
| 文件树、文件内容预览 | 全 mock | `/api/files/*` |

## 配置

- `web/vite.config.ts` 已设置 dev server port `10086` 并代理 `/api` 到 `http://localhost:10010`。
- `backend/main.go` 已改为从 `web/dist` 提供静态文件。
