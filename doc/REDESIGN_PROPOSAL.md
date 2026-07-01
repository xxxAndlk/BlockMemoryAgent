# BlockMemoryAgent 重新设计方案

> 基于对现有系统的问题分析，设计一个更务实、更完善、效果更好的 Agent 编排系统。

---

## 一、对现有系统的核心诊断

| 问题 | 根本原因 | 设计方向 |
|------|----------|----------|
| 四层架构 overhead 过大 | 每次请求 3-4 次 LLM 调用，简单任务也走全链路 | 扁平化为三层，减少中间层 |
| 伪嵌入语义检索失效 | 哈希 bag-of-tokens 无法捕捉语义 | 接入真实 embedding 模型 |
| 记忆写入链路断裂 | 回调触发机制不可靠 | 明确写入触发点，同步写入 |
| 模型未分层选型 | 全部用 deepseek-v4-flash | 按角色分层选型，小任务用小模型 |
| 缺少 Plan-Execute-Reflect | 只有 ReAct，无顶层规划 | 引入计划-执行-反思循环 |
| 工具生态贫瘠 | 仅 7 个基础工具 | 接入 MCP 协议，对接外部工具 |
| 会话与消息混淆 | 每轮消息都开新会话 | 会话=容器，消息=轮次，结构清晰 |

---

## 二、新架构总览：三层扁平化

```
┌─────────────────────────────────────────────────────────────┐
│  Layer 1: Session & Router（会话与路由层）                   │
│  - 维护会话状态，管理消息历史                                 │
│  - 用轻量模型（或规则）判断任务类型：直答/工具调用/复杂任务     │
│  - 简单任务直接回答，不进入 Agent 循环                        │
│  - 复杂任务进入 Agent Loop，路由到 Plan 模块                   │
├─────────────────────────────────────────────────────────────┤
│  Layer 2: Agent Core（核心执行层）                           │
│  - Plan-Execute-Reflect 循环（3-5 轮 Max）                    │
│  - 执行前检索记忆，注入相关上下文                             │
│  - 工具调用走 MCP 协议，生态开放                             │
│  - 每轮执行后同步写入记忆库                                   │
├─────────────────────────────────────────────────────────────┤
│  Layer 3: Memory & Tools（记忆与工具层）                       │
│  - 短期记忆：Redis 热缓存（当前会话上下文）                    │
│  - 中期记忆：PostgreSQL 执行轨迹（7 天保留）                 │
│  - 长期记忆：pgvector + 摘要（跨会话知识，语义检索）         │
│  - 工具集：MCP 客户端 + 本地沙箱工具                          │
└─────────────────────────────────────────────────────────────┘
```

---

## 三、核心设计决策

### 3.1 扁平化：从四层到三层

**不要** MetaAgent → DomainAgent → SubDomainAgent → Assistant 的层层委派。

**改为** Router（调度）→ Agent（执行）→ Tools（工具）。

理由：
- 现代 LLM（128K+ 上下文）足够处理一个中等复杂度的任务规划
- 额外的分层没有带来“更智能”的决策，只是增加了延迟和成本
- 如果确实需要领域隔离，用**路由规则**而非动态 LLM 拆分来实现

### 3.2 路由层：规则优先，LLM 兜底

```go
type TaskType int

const (
    TaskDirectAnswer TaskType = iota  // 寒暄/自我介绍/简单事实 → 轻量模型直接答
    TaskToolCall                       // 查天气/搜代码/读文件 → 直接执行工具，不规划
    TaskComplex                        // 写项目/重构代码/多步分析 → 进入 Plan-Execute-Reflect
)

func ClassifyTask(input string, history []Message) TaskType {
    // 第一步：规则匹配（零成本）
    if isGreeting(input) || isSimpleFact(input) {
        return TaskDirectAnswer
    }
    if isToolPattern(input) { // "查一下" / "读文件" / "运行命令"
        return TaskToolCall
    }
    // 第二步：轻量模型 LLM 判断（单次调用，<100ms）
    return llmClassify(input, history)
}
```

规则覆盖 80% 的输入，只有 20% 需要 LLM 判断。这比“每次都让 LLM 分析领域、拆分 DomainAgent”高效得多。

### 3.3 Agent 核心：Plan-Execute-Reflect

取代纯 ReAct 循环，引入三层循环：

```
┌──────────┐    ┌──────────┐    ┌──────────┐    ┌──────────┐
│   Plan   │───→│ Execute  │───→│ Observe  │───→│ Reflect  │
│ 生成计划  │    │ 执行一步  │    │ 观察结果  │    │ 评估调整  │
└──────────┘    └──────────┘    └──────────┘    └────┬─────┘
     ↑──────────────────────────────────────────────────┘
```

- **Plan**：分析任务，生成 1-5 步的执行计划（Plan 是结构化的，不是自由文本）
- **Execute**：执行当前步骤（LLM 调用或工具调用）
- **Observe**：收集执行结果（成功/失败/输出）
- **Reflect**：评估是否偏离目标，是否需要调整计划，是否继续/终止

**与 ReAct 的区别**：
- ReAct 是“走一步看一步”，容易陷入局部最优
- Plan-Execute-Reflect 是“有全局规划”，每步后都有反思调整

**最大轮数**：3-5 轮（配置可覆盖），超过则终止并返回当前结果，不无限循环。

### 3.4 记忆系统：三层存储，真实语义

#### 短期记忆（Redis）
- 当前会话的最近 N 轮对话
- TTL：1 小时
- 用途：让 Agent 看到当前对话上下文

#### 中期记忆（PostgreSQL）
- 每条执行轨迹：输入 → 计划 → 执行步骤 → 结果 → 反思
- 保留：7 天
- 用途：同一会话内断点续传、错误排查、审计日志

#### 长期记忆（pgvector + 真实 Embedding）
- 内容：跨会话的知识、成功经验、用户偏好、项目文档
- 存储格式：
  ```json
  {
    "id": "...",
    "content": "用户偏好用 Vue 3 + TypeScript 写前端，不使用 Options API",
    "embedding": [0.12, -0.34, ...],  // 真实向量，由 text-embedding-3 生成
    "session_id": "session-42",
    "topic": "user_preference",
    "created_at": "2025-01-15T10:00:00Z",
    "importance": 0.9
  }
  ```
- 检索方式：向量相似度（cosine）+ 关键词过滤 + 时间衰减
- 自动摘要：每轮会话结束后，用轻量模型自动生成摘要写入长期记忆

**写入触发点**（明确、不可跳过）：
```go
func (a *Agent) Run(ctx context.Context, input string) (*Result, error) {
    // 1. 检索长期记忆
    memories := a.memory.Search(ctx, input, 5)
    // 2. 执行 Plan-Execute-Reflect
    result := a.loop(ctx, input, memories)
    // 3. 同步写入中期记忆（执行轨迹）
    a.memory.SaveTrace(ctx, result.Trace)
    // 4. 异步生成摘要并写入长期记忆
    go a.memory.SaveSummary(ctx, result.Summary, result.Embedding)
    return result, nil
}
```

**关键改进**：写入不是通过回调，而是 Agent 执行后的**显式调用**。回调容易遗漏，显式调用不会。

### 3.5 工具层：MCP 协议 + 本地沙箱

**接入 Model Context Protocol (MCP)**，这是当前最务实的工具集成方案：

```go
type ToolSet struct {
    LocalTools   []Tool      // 本地工具：ReadFile, WriteFile, RunCommand, SearchInFiles
    MCPClients   []MCPClient // MCP 客户端：可对接 GitHub, Browser, Database, Slack, etc.
}
```

- 本地工具：7-10 个基础文件/命令/搜索工具
- MCP 工具：通过 `mcp.json` 配置对接外部 MCP Server，生态无限扩展
- 工具调用格式：OpenAI function-calling 格式（兼容所有主流模型）

**为什么用 MCP？**
- 不需要自己维护工具实现，对接社区生态即可
- 工具描述和 Schema 由 MCP Server 自动生成
- 同一个 MCP Server 可被多个 Agent 复用

### 3.6 模型分层选型

| 角色 | 模型 | 理由 |
|------|------|------|
| 路由判断 | 规则 + 轻量模型（如 deepseek-v3 / gpt-4o-mini） | 简单分类，成本最低 |
| 计划生成 | 中等模型（如 gpt-4o / claude-3.5-sonnet） | 需要结构化推理 |
| 代码执行 | 强模型（如 o3-mini / claude-3.5-sonnet） | 代码质量要求高 |
| 摘要生成 | 轻量模型 | 不需要强推理 |
| Embedding | 专用模型（text-embedding-3 / bge-m3） | 语义检索质量决定记忆效果 |

### 3.7 会话设计：清晰的容器模型

```go
type Session struct {
    ID        string    // 会话 ID
    Topic     string    // 主题（如"商城页面重构"）
    Status    Status    // active / completed / error
    Messages  []Message // 消息历史（多轮）
    Traces    []Trace   // 执行轨迹
    CreatedAt time.Time
    UpdatedAt time.Time
}

type Message struct {
    ID      string
    Role    Role      // user / assistant / system
    Content string
    Metadata Metadata // 关联的 trace_id, tool_calls 等
}
```

- **会话**是主题容器，一个会话可以有多轮消息
- 用户发送新消息 → 加入当前会话的 Messages
- 只有用户明确“新建会话”或主题完全不同，才创建新会话
- 会话结束时，自动生成摘要，提取关键知识写入长期记忆

---

## 四、系统架构图

```
                           User Input
                              │
                              ▼
                    ┌──────────────────┐
                    │  Session Manager │  ← 维护会话状态，消息历史
                    │  (Redis 热缓存)   │
                    └────────┬─────────┘
                             │
                             ▼
                    ┌──────────────────┐
                    │  Router (Classify) │  ← 规则 + 轻量模型，输出 TaskType
                    └────────┬─────────┘
                             │
              ┌──────────────┼──────────────┐
              │              │              │
              ▼              ▼              ▼
        ┌─────────┐  ┌──────────┐  ┌──────────────┐
        │ Direct  │  │ ToolCall │  │  Agent Loop  │
        │ Answer  │  │ (1-step) │  │  (3-5轮)     │
        │ 轻量模型 │  │ 直接执行  │  │ Plan-Execute-Reflect │
        └────┬────┘  └────┬─────┘  └──────┬───────┘
             │            │               │
             └────────────┴───────────────┘
                             │
                             ▼
                    ┌──────────────────┐
                    │  Result Formatter  │  ← 格式化结果，写入消息历史
                    └────────┬─────────┘
                             │
                             ▼
              ┌──────────────┼──────────────┐
              │              │              │
              ▼              ▼              ▼
        ┌─────────┐  ┌──────────┐  ┌──────────────┐
        │ 用户输出 │  │ 中期记忆  │  │ 长期记忆摘要  │
        │         │  │ (PG, 7天) │  │ (pgvector)    │
        └─────────┘  └──────────┘  └──────────────┘
```

---

## 五、数据模型

### 5.1 会话表
```sql
CREATE TABLE sessions (
    id TEXT PRIMARY KEY,
    topic TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('active', 'completed', 'error')),
    summary TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

### 5.2 消息表
```sql
CREATE TABLE messages (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    role TEXT NOT NULL CHECK (role IN ('user', 'assistant', 'system')),
    content TEXT NOT NULL,
    trace_id TEXT,          -- 关联执行轨迹
    metadata JSONB,         -- tool_calls, references 等
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

### 5.3 执行轨迹表（中期记忆）
```sql
CREATE TABLE traces (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id),
    plan JSONB NOT NULL,          -- 初始计划
    steps JSONB NOT NULL,         -- 每步执行记录
    reflection TEXT,              -- 最终反思
    result TEXT,                  -- 最终结果
    status TEXT NOT NULL,         -- success / partial / failure
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

### 5.4 长期记忆表（向量）
```sql
CREATE TABLE long_term_memory (
    id TEXT PRIMARY KEY,
    session_id TEXT REFERENCES sessions(id),
    content TEXT NOT NULL,                    -- 文本内容
    embedding VECTOR(1536) NOT NULL,        -- OpenAI text-embedding-3 维度
    topic TEXT,                              -- 主题分类
    importance FLOAT NOT NULL DEFAULT 0.5,   -- 重要性 0-1
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_ltm_embedding ON long_term_memory USING ivfflat (embedding vector_cosine_ops);
CREATE INDEX idx_ltm_topic ON long_term_memory(topic);
```

---

## 六、与现有系统的关键差异对比

| 维度 | 现有系统 | 重新设计方案 | 改进理由 |
|------|----------|-------------|----------|
| 架构层级 | 4 层（Meta/Domain/Sub/Assistant） | 3 层（Router/Agent/Tools） | 减少 overhead，降低延迟 |
| 路由方式 | LLM 动态拆分领域 | 规则 + 轻量模型分类 | 80% 输入零成本路由 |
| 任务执行 | ReAct 循环 | Plan-Execute-Reflect | 全局规划 + 每步反思，避免局部最优 |
| 嵌入模型 | 伪嵌入（哈希 bag-of-tokens） | 真实 embedding（text-embedding-3） | 语义检索质量从"不可用"到"可用" |
| 记忆写入 | 回调触发（疑似断裂） | 执行后显式同步写入 | 确保 100% 持久化 |
| 工具生态 | 7 个基础工具 | MCP 协议 + 本地工具 | 社区生态无限扩展 |
| 压缩策略 | 四级压缩（Raw→Standard→Compact→Marker） | 原始保留 7 天 + 自动摘要永久保存 | 简单、可靠、可预测 |
| 模型分层 | 全部用同一模型 | 按角色分层选型 | 小任务用小模型，节省成本 |
| TUI | 设计 5 面板，实现 1 列 | 设计 2 面板（聊天 + 侧边栏），完整实现 | 务实、可交付、够用 |
| 会话模型 | 消息=新会话 | 会话=容器，消息=轮次 | 符合用户直觉，支持多轮 |

---

## 七、关键改进点详解

### 7.1 为什么放弃四级压缩？

四级压缩（Raw → Standard → Compact → Marker）的设计意图是渐进式遗忘，但：
- 实现复杂，容易出 bug（现有系统已暴露写入链路问题）
- 压缩决策需要人工设定阈值，不够智能
- 对于大多数场景，**保留原始记录 7 天 + 自动生成高质量摘要永久保存** 已经足够
- 如果用户真的需要历史细节，7 天内可以查原始记录；7 天后只需要摘要

新方案更简单：
```go
func (m *Memory) SaveSummary(ctx context.Context, trace *Trace) error {
    // 1. 用轻量模型生成摘要
    summary := m.summarizer.Summarize(trace)
    // 2. 生成真实 embedding
    embedding := m.embedder.Embed(summary)
    // 3. 写入长期记忆
    return m.pg.Insert(ctx, summary, embedding, trace.Topic, trace.Importance)
}
```

### 7.2 为什么用 MCP 替代自建工具？

现有系统花了大量代码实现 `ReadFile`/`WriteFile`/`ListDir`/`RunCommand`/`SearchInFiles`/`HTTPGet`/`HTTPPost`。

MCP 方案：
- 这些工具 MCP 社区已有成熟实现（如 `filesystem` MCP Server）
- 新的工具（如 GitHub、Jira、浏览器、数据库）通过配置即可接入，无需写代码
- 工具描述和 Schema 由 MCP Server 自动生成，不需要手动维护 JSON Schema
- 工具执行可以走沙箱隔离，安全性更好

### 7.3 为什么 Plan-Execute-Reflect 比纯 ReAct 更好？

纯 ReAct 的问题：
- "走一步看一步"，容易陷入工具调用的局部循环（比如反复 ReadFile 却忘记 WriteFile）
- 没有全局规划，复杂任务容易遗漏关键步骤

Plan-Execute-Reflect 的优势：
- Plan 阶段生成结构化计划（如 JSON 格式的步骤列表），LLM 可以看到全局
- 每步 Execute 后都有 Observe，收集执行结果
- Reflect 阶段评估是否偏离目标，是否需要调整计划（比如跳过已完成的步骤，或补充遗漏的步骤）
- 最大轮数限制（3-5 轮），防止无限循环

### 7.4 为什么显式写入比回调更好？

现有系统的记忆写入是通过回调（`memCallback.OnEnd`）触发的。回调的问题：
- 容易被遗漏：如果执行路径有分支，某条分支没触发回调，数据就丢了
- 难以调试：你不知道回调是否被触发，也无法在回调失败时重试
- 时序不清：回调是异步的，可能 Agent 已经返回了，回调还没执行完

显式写入：
- 在 `Agent.Run()` 的 return 前，明确调用 `memory.SaveTrace()` 和 `memory.SaveSummary()`
- 同步写入中期记忆（确保不丢），异步写入长期记忆（不阻塞返回）
- 写入失败可以立即重试，失败日志清晰

---

## 八、实现优先级

### Phase 1：核心骨架（2-3 周）
1. **Session Manager**：会话容器、消息历史、状态管理
2. **Router**：规则分类 + 轻量模型兜底
3. **Agent Core**：Plan-Execute-Reflect 循环（3-5 轮限制）
4. **本地工具**：ReadFile, WriteFile, RunCommand, SearchInFiles
5. **中期记忆**：PostgreSQL 执行轨迹表，显式写入

### Phase 2：记忆与工具（2-3 周）
1. **真实 Embedding**：接入 OpenAI text-embedding-3（或本地 bge-m3）
2. **长期记忆**：pgvector 向量表，语义检索
3. **自动摘要**：会话结束后轻量模型生成摘要，写入长期记忆
4. **MCP 集成**：接入 filesystem, github, browser 等 MCP Server

### Phase 3：观测与优化（1-2 周）
1. **Web UI**：会话列表 + 聊天页 + 执行轨迹回放 + 记忆检索页
2. **TUI**：简化版（聊天 + 侧边栏显示当前计划/工具调用），完整实现
3. **模型分层**：Router/Plan/Execute/Summary 分别配置不同模型
4. **成本追踪**：记录每轮 LLM 调用的 token 和成本，展示给用户

---

## 九、与现有系统的兼容性

如果需要从现有系统迁移：
- `PostgresStore` 的表结构可以保留，增加新表（messages, traces）
- `agent_private_memory` 表可以保留作为历史数据，新系统用 `traces` 和 `long_term_memory`
- `RoleRegistry` 和 `RoleFactory` 可以废弃，替换为简单的 Router + Agent 配置
- `ThreeLayerGraph` 可以废弃，替换为直接的 `Plan-Execute-Reflect` 循环
- Web UI 的 Vue 组件可以复用，只需调整 API 接口

---

## 十、总结

重新设计的核心思路：**做减法，做可靠，做务实。**

- 去掉过度分层，保留必要的调度
- 用真实 embedding 替代伪嵌入，让记忆检索真正可用
- 用显式写入替代回调，确保数据不丢
- 用 MCP 替代自建工具，接入社区生态
- 用 Plan-Execute-Reflect 替代纯 ReAct，提升复杂任务成功率
- 按角色分层选型模型，节省成本

这套方案在工程上更可控，在效果上更可靠，在成本上更经济。它不会比直接使用大模型 API 更复杂，但会比现有系统更实用。
