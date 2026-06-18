# UI 功能缺口清单

为匹配 `doc/image/` 设计稿，前端已按新布局实现 Dashboard、Sessions、Memory 三页。以下功能当前用 mock 数据占位，需后端 / 模型 / 存储层补齐后替换真实 API。

## Dashboard 首页

| 前端展示 | 当前实现 | 缺口 |
|---|---|---|
| Program / Mode / LLM API / Health 状态芯片 | 硬编码 mock | 需要 `/api/status` 或 `/api/health` 返回运行模式、LLM 供应商、健康状态 |
| Token 趋势图、延迟趋势图 | mock 数组 | 需要会话维度或全局聚合的时序接口，返回 `[{timestamp, tokens, latency}]` |
| 领域分布条形图 | mock 数据 | 需要按 DomainAgent 聚合事件/调用统计的接口 |
| 最近活动流 | mock | 需要全局事件流或审计日志接口 |
| 平均耗时 | 硬编码 `1.2s` | 需要 `started_at` / `ended_at` 聚合计算 |

## 会话详情页 (Sessions)

| 前端展示 | 当前实现 | 缺口 |
|---|---|---|
| 任务看板 (Task Board) | mock 任务列表 | 需要 `/api/sessions/{id}/board` 返回真实 TaskBoardData |
| 智能体参与卡片 | 从 `agent_created` 事件解析，无事件时 fallback mock | 需要 `/api/sessions/{id}/agents` 返回 AgentNode 列表及实时状态 |
| 概览页最终结果 | 从 `会话完成` 系统事件解析 | 需保证 MetaAgent 稳定输出该事件；或提供 `/api/sessions/{id}/result` |
| Metrics Token/事件分布 | 由当前 events 派生 | 数据稀疏时仍为 mock，需要后端聚合 |
| 运行时长 | 当前时间 - started_at (running) / ended_at - started_at | 已可从现有字段计算，无需新接口 |

## 记忆浏览页 (Memory)

| 前端展示 | 当前实现 | 缺口 |
|---|---|---|
| 智能体选择器 | 硬编码列表 | 需要 `/api/agents` 列出可浏览记忆的智能体 |
| 记忆搜索 | 前端本地过滤 mock | 需要 `/api/memory/search?agent=...&q=...&level=...` 返回 MemoryItem |
| 记忆统计 (总数/主题/实体/压缩率) | mock | 需要 `/api/memory/stats?agent=...` |
| 压缩层级分布 | mock | 需要 `/api/memory/levels?agent=...` |
| 记忆写入时间线 | mock | 需要 `/api/memory/timeline?agent=...` |
| 最近访问记录 | mock | 需要审计/访问日志接口 |
| 实体关系图谱 | mock 实体列表 | 需要 `/api/memory/entities?agent=...` 及关系边数据 |

## 通用

- 设计稿中的顶部导航（首页、会话管理、知识库等）当前由 `Sidebar.vue` 完成，无需新增。
- 深色主题色值、卡片、表格、时间线已统一使用 CSS 变量，后续若换肤只需改 `:root`。
- 图表目前为 SVG 自绘，若需要复杂交互可引入 ECharts / Chart.js。
