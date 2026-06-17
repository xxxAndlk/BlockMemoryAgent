# BlockMemoryAgent Web 展示面板需求文档

> 基于 v3 设计文档 + 已实现后端能力，梳理 web 端应展示的全部数据与交互。

---

## 0. 设计原则

- **实时优先**：所有状态变化通过 SSE 推送，无需手动刷新。
- **分层展示**：用户只关心当前会话；运维/开发者关心全局健康与历史。
- **操作闭环**：展示的数据应能触发操作（如暂停 Graph、标记事件已处理、手动检索记忆）。
- **暗黑主题**：与现有 TUI 保持一致（#0d1117 背景 + 语义化彩色边框）。

---

## 1. 全局导航栏（Header）

| 元素 | 说明 |
|------|------|
| Logo + 标题 | BlockMemoryAgent |
| 系统健康指示器 | Postgres ● / Redis ● / LLM API ●（绿/红） |
| 当前人格 | 显示 `config/soul.md` 加载的人格名（如"严谨工程师"） |
| 快速操作 | 暂停/恢复当前会话 Graph（调用 `/api/graph/pause` `/resume`） |

---

## 2. 首页 / 会话大厅（Dashboard）

### 2.1 快速创建区
- 大输入框（goal）+ Run 按钮（已有）
- 快捷模板按钮："分析代码" / "写文件" / "运行命令" / "搜索知识"

### 2.2 会话列表（左侧，已有）
- 卡片：goal 截断、时间、状态 badge（running 带脉冲动画）
- 点击展开详情

### 2.3 统计概览（新增）
- 今日会话数 / 完成率
- LLM 调用总次数 / 超时率
- 平均响应时间趋势（最近 10 次）

---

## 3. 会话详情页（核心，多面板布局）

采用 **3 列布局**（左 280px / 中弹性 / 右 320px）：

```
┌─────────────────┬──────────────────────────────┬─────────────────┐
│  左：角色树      │  中：主内容区（Tab 切换）     │  右：辅助面板    │
│  + 任务看板      │                              │  + 实时指标      │
├─────────────────┤  Tab: 执行日志 / 记忆 / Skill  ├─────────────────┤
│                 │                              │  邮箱通知        │
│  MetaAgent      │                              │  上下文监控      │
│  └── DomainA    │                              │  系统状态        │
│      ├── SubA   │                              │                 │
│      └── Asst1  │                              │                 │
│                 │                              │                 │
└─────────────────┴──────────────────────────────┴─────────────────┘
```

---

## 4. 左侧面板

### 4.1 角色树（Role Hierarchy）

数据来源：`RoleRegistry.GetInstancesBySession(sessionID)`

展示：
- 树形结构：MetaAgent → DomainAgent[] → SubDomainAgent[] / Assistant[]
- 每个节点显示：角色名、类型 badge（meta/domain/subdomain/fixed/dynamic）、状态（active/done/error）
- 点击节点：右侧显示该 Agent 的 Skill 装配列表 + 记忆快照

### 4.2 任务看板（Task Board）

数据来源：`board.Manager.Get(sessionID).Snapshot()`

展示：
- 看板标题 = 全局 Goal
- 看板状态 badge：NEW / IN_PROGRESS / DONE / FAILED
- 子任务列表（Kanban 风格或列表）：
  - Title + Assignee（Agent 名）+ Status badge
  - Status：pending / in_progress / blocked / done / failed
  - blocked 任务显示阻塞原因
- 约束条件（Constraints）key-value 展示

实时：子任务状态变化通过 SSE 事件推送到前端。

---

## 5. 中间主内容区（Tab 切换）

### Tab 1: 执行日志（Execution Log）— 已有，增强

数据来源：`/api/sessions/{id}/stream` SSE

现有功能保留：
- 事件列表，时间戳、Agent、消息
- kind badge（think/intend/llm/tool_call/tool_result/wait/error）
- 工具执行结果折叠/展开
- markdown 渲染（代码块、粗体）

新增：
- **按 Agent 过滤**：只看某个 DomainAgent 或 Assistant 的事件
- **按 kind 过滤**：只看 tool_call / 只看 error
- **进度条**：当前会话整体进度（已完成子任务数 / 总子任务数）

### Tab 2: 记忆浏览器（Memory Explorer）— 新增

数据来源：
- `POST /api/snapshot` — 查看 Agent 快照
- `POST /api/retrieve` — 手动检索记忆

展示：
- Agent 选择器（下拉选择当前会话中的 Agent）
- **快照卡片**：
  - KeySummaries（最近 5 步摘要）
  - OpenIssues（未解决问题列表）
  - LocalVars（键值对）
- **记忆检索**：输入 query，调用 `/api/retrieve`，展示相关 episodes
- **压缩级别分布**：该 Agent 的 memory 中各压缩级别占比（饼图/条形）

### Tab 3: Skill 装配（Skill Set）— 新增

数据来源：`skill.Registry.GetForAgent(agentInstID)`

展示：
- 当前选中的 Agent 装配了哪些 Skill
- Skill 卡片：ID、名称、描述、ToolRef、Cost
- 领域过滤：该 Agent 的 Domain 对应的 Skill 候选池

### Tab 4: 代码/文件预览（File Preview）— 新增

数据来源：`ToolExecutor` WriteFile 结果中的 path

展示：
- 当前会话中所有 WriteFile 创建的文件列表
- 点击文件预览内容（代码高亮）
- 文件路径可点击用 OS 打开

---

## 6. 右侧面板

### 6.1 实时指标（Metrics）

数据来源：`MetaAgent.TimeoutStats()` + `watchdog.History()`

展示：
- **LLM 统计**：调用 N 次 / 超时 M 次 / 平均 Xs / 最长 Ys
- **上下文用量**：当前 Agent 的 token 估算值 + 进度条（soft/hard 阈值线）
- **看门狗状态**：最近 3 次决策（OK/Warn/Compress/Evict）

### 6.2 邮箱通知（Mailbox）

数据来源：`mailbox.Mailbox.Peek(agentID)` / `DrainBroadcast()`

展示：
- 未读消息列表（按优先级降序）
- 消息卡片：From → To、Type badge、Subject、时间
- Type 颜色：
  - milestone = 绿
  - request = 蓝
  - info = 灰
  - escalate = 红
  - dependency = 黄
- 点击标记已读（调用 `/api/event/resolve`）
- 广播桶消息特殊标识（待主 Agent 决议）

### 6.3 系统状态

- Postgres：连接状态、表行数（session_history / agent_private_memory）
- Redis：连接状态、key 数量（demo:snake:scores 等）
- LLM：API 延迟（最近一次调用耗时）

---

## 7. 全局功能面板（独立页面或抽屉）

### 7.1 知识库搜索（Knowledge Base）

数据来源：`store.PostgresStore.SearchKnowledge()`

- 搜索框：语义检索（pgvector）
- 结果列表：content + similarity score + access_count
- 可归档/取消归档

### 7.2 会话历史（Session History）

数据来源：`store.PostgresStore.RecentSessionHistories()`

- 时间线：最近 20 个会话
- 每条：Goal + Summary + 工具调用摘要
- 点击恢复上下文：把历史注入新会话的 system prompt

### 7.3 人格配置（Soul Config）

数据来源：`soul.Loader`

- 展示当前加载的 `soul.md` 内容
- Temperature 调节滑块（0~1，影响 LLM 创造性）
- 人格切换：上传新的 soul.md

### 7.4 Skill 库管理（Skill Pool）

数据来源：`skill.Pool.All()`

- 全部 Skill 列表（表格）
- 新增 / 编辑 / 删除 Skill
- 按 Domain 分组展示
- 导入/导出 YAML（`config/skills.yaml` 格式）

---

## 8. 数据流与 API 映射

| 前端展示 | 后端数据源 | 已有 API / 需新增 |
|----------|-----------|-------------------|
| 角色树 | `RoleRegistry` | 需新增 `GET /api/sessions/{id}/agents` |
| 任务看板 | `board.Manager` | 需新增 `GET /api/sessions/{id}/board` |
| 执行日志 | `SessionManager` + SSE | 已有 `/api/sessions/{id}/stream` |
| 记忆快照 | `memory.SnapshotManager` | 已有 `POST /api/snapshot` |
| 记忆检索 | `memory.SearchScorer` | 已有 `POST /api/retrieve` |
| 邮箱通知 | `mailbox.Mailbox` | 需新增 `GET /api/sessions/{id}/mailbox` |
| 看门狗 | `watchdog.Watchdog` | 需新增 `GET /api/sessions/{id}/watchdog` |
| Skill 装配 | `skill.Registry` | 需新增 `GET /api/agents/{id}/skills` |
| 系统健康 | `store.PostgresStore` / `RedisStore` | 需新增 `GET /api/health` |
| 会话历史 | `session_history` 表 | 已有 `RecentSessionHistories` |
| 知识库 | `global_knowledge` 表 | 已有 `SearchKnowledge` |
| 人格 | `soul.Loader` | 需新增 `GET/PUT /api/soul` |
| 文件预览 | `ToolExecutor` 结果 | 需新增 `GET /api/files?session={id}` |

---

## 9. 交互规范

- **SSE 连接**：每个打开详情页的会话独立一个 EventSource，关闭页面时断开。
- **自动刷新**：会话列表每 5s 轮询；详情页依赖 SSE 推送。
- **错误提示**：API 失败时 toast 提示，不阻断界面。
- **移动端**：左侧面板可折叠为抽屉；中间内容区全屏。
- **深色模式唯一**：不设计浅色模式，保持与 TUI 一致的开发者工具风格。

---

## 10. 优先级建议（MVP → 完整）

| 阶段 | 功能 |
|------|------|
| MVP（已有） | 会话创建/列表/详情、执行日志、SSE、工具结果折叠 |
| P1 | 角色树、任务看板、邮箱通知、实时指标 |
| P2 | 记忆浏览器、Skill 装配、文件预览、系统健康 |
| P3 | 知识库搜索、会话历史、人格配置、Skill 库管理 |
