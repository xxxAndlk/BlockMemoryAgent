# BlockMemoryAgent

> 一个基于 **上下文外部化 + 记忆隔离** 的本地编程 Agent 系统。解决你在使用 Claude Code 等工具时遇到的真实问题：上下文膨胀导致模型变笨、无关对话污染当前任务、每次新对话从零开始。

---

## 为什么需要这个项目

使用 Claude Code 等编程助手时，你会遇到三个无法回避的问题：

### 1. 上下文一膨胀，模型就"变笨"

Claude 3.5 Sonnet 标称 200K 上下文，但实际操作中，当对话历史、工具输出、文件内容堆积超过一定阈值后，模型对中间信息的提取准确率急剧下降。不是模型不支持长上下文，而是**长上下文中的有效注意力会衰减**。这导致：
- 明明刚才讨论过的方案，模型突然"忘了"
- 中间穿插的代码审查、文件读取结果，在后期被忽略
- 代码修改时遗漏之前已经确认过的约束

**BlockMemoryAgent 的解法**：不依赖 LLM 记住一切。所有历史对话、工具输出、文件快照都写入外部记忆库（PostgreSQL + Redis），LLM 的上下文永远只包含"当前任务 + 按需召回的 relevant 记忆"。无论运行多久，输入 LLM 的上下文总量保持稳定。

### 2. 多轮对话中夹杂不相干内容，污染当前任务

你在修复 CSS 问题时，中途问了一句"帮我查一下这个函数的文档"，然后又回到 CSS 修复。但 LLM 的上下文里有 50% 是函数文档查询的内容，这些无关信息会干扰后续的 CSS 修复决策。这导致：
- 修复方案中莫名其妙出现了不相关的代码
- 模型把之前无关话题的假设带入当前任务
- 需要手动不断提示"回到 CSS 问题"

**BlockMemoryAgent 的解法**：会话块隔离（SessionBlock）。每个话题/任务拥有独立的上下文块。当你切换话题时，旧话题的记忆被"归档"（压缩为摘要 + 写入向量库），新话题只加载自己的上下文 + 从归档中按需召回相关片段。不同话题的记忆互不污染。

### 3. 使用期限很短，需要频繁开新对话重置上下文

Claude Code 的对话一旦超过几百轮，质量明显下降。你被迫开启新对话，但新对话里：
- 项目的所有上下文、代码风格、你的偏好都丢失了
- 需要重新描述项目结构、技术栈、编码规范
- 之前的讨论结论、决策原因需要重新解释

**BlockMemoryAgent 的解法**：跨会话记忆持久化。每次对话结束后，系统自动提取关键知识（用户偏好、技术决策、项目约定、成功案例）写入长期记忆库。新对话启动时，自动检索并注入相关历史知识。项目约定、代码风格、你的偏好——这些只会在第一次讨论时建立，之后永久可用。

**一句话总结**：BlockMemoryAgent 不是另一个 Claude Code 的替代品，而是**在 Claude Code 的痛点上，做它做不到的事情——长时稳定运行、话题隔离、跨会话记忆**。

---

## 核心特性

- **上下文恒定控制**：无论运行多久、多少轮对话，输入 LLM 的上下文始终控制在配置阈值内（默认 4K-8K token），通过外部化记忆 + 按需召回实现
- **会话块隔离（SessionBlock）**：每个话题/任务拥有独立的上下文块，切换话题时旧话题归档，新话题只加载 relevant 记忆，互不污染
- **跨会话记忆**：关键知识（用户偏好、技术决策、项目约定）在会话结束后自动提取摘要，写入 pgvector 向量库，新会话自动召回
- **智能压缩与遗忘**：热点记忆（频繁访问）保留完整细节，冷记忆（长期不用）自动降级为摘要，超期冷记忆删除
- **Agent 分级编排**：简单任务直接回答（不走 Agent 循环），复杂任务进入 Plan-Execute-Reflect，避免不必要的开销
- **TUI 编程界面**：参考 Claude Code 的简洁终端交互，主对话 + 底部状态栏 + 可折叠 Agent 面板
- **记忆可观测**：Web UI 和 TUI 均可查看记忆检索过程、压缩决策、热点/冷记忆分布

---

## 架构

```
┌─────────────────────────────────────────────────────────────┐
│  User Input → Router（智能路由）                              │
│  - 简单任务（寒暄/直答）→ 直接回答，不进入 Agent 循环           │
│  - 工具调用（查文件/跑命令）→ 直接执行，不生成计划               │
│  - 复杂任务（写代码/重构）→ 进入 Agent Loop                     │
├─────────────────────────────────────────────────────────────┤
│  Agent Core（Plan-Execute-Reflect）                          │
│  - Plan：生成结构化执行计划（1-5 步）                          │
│  - Execute：执行当前步骤（LLM 调用或工具调用）                  │
│  - Observe：收集执行结果                                     │
│  - Reflect：评估是否偏离，是否调整计划，是否继续/终止           │
│  - 最大 3-5 轮后强制终止，防止无限循环                          │
├─────────────────────────────────────────────────────────────┤
│  Memory Layer（记忆层）                                       │
│  - 短期记忆：Redis 热缓存（当前会话最近 N 轮对话）               │
│  - 中期记忆：PostgreSQL 执行轨迹（保留 7 天，用于调试/审计）      │
│  - 长期记忆：pgvector + 摘要（跨会话知识，热点保留/冷记忆降级）   │
│  - 写入触发：Agent 执行后显式同步写入，非回调，确保不丢          │
├─────────────────────────────────────────────────────────────┤
│  Tools Layer（工具层）                                        │
│  - 本地工具：ReadFile / WriteFile / RunCommand / SearchInFiles  │
│  - MCP 协议：对接外部工具生态（GitHub / Browser / Database 等） │
└─────────────────────────────────────────────────────────────┘
```

### 记忆层详解

```
User Input
    │
    ▼
┌────────────────────┐    ┌────────────────────┐    ┌────────────────────┐
│  短期记忆 (Redis)    │───→│  中期记忆 (PG)      │───→│  长期记忆 (pgvector) │
│  当前会话上下文      │    │  执行轨迹 (7 天)     │    │  知识摘要 + 向量     │
│  TTL: 1 小时       │    │  用于调试/审计       │    │  热点: 完整细节      │
│  容量: N 轮对话      │    │  容量: 无限制        │    │  冷记忆: 摘要降级    │
│  用途: 当前对话     │    │  用途: 断点续传     │    │  超期: 自动删除      │
│  写入: 每轮同步     │    │  写入: 每轮同步     │    │  写入: 会话结束异步  │
└────────────────────┘    └────────────────────┘    └────────────────────┘
    │                           │                          │
    └───────────────────────────┴──────────────────────────┘
                              │
                              ▼
                    ┌────────────────────┐
                    │  Context Assembler  │
                    │  按任务召回相关记忆  │
                    │  注入 LLM 上下文     │
                    │  总量控制在阈值内     │
                    └────────────────────┘
                              │
                              ▼
                         LLM 调用
```

**记忆写入机制**（关键改进）：
- 不是通过回调触发，而是 Agent 执行后的**最后一行显式调用**
- `SaveTrace()` 同步写入中期记忆（确保不丢，失败立即报错）
- `SaveSummary()` 异步写入长期记忆（不阻塞用户响应）
- 写入失败可立即重试，日志清晰，不再"数据可能写入了也可能没写入"

**记忆压缩机制**（两级简化）：
- **原始保留**：执行轨迹保留 7 天，用于调试、审计、断点续传
- **摘要降级**：7 天后自动删除原始轨迹，只保留 LLM 生成的摘要 + 向量嵌入
- **热点识别**：频繁访问的记忆标记为热点，保留更长时间
- **超期删除**：冷记忆超过配置期限（默认 30 天）自动删除

---

## 快速开始

### 依赖

- Go 1.22+
- PostgreSQL 14+（含 pgvector 插件，记忆持久化）
- Redis 7+（短期记忆热缓存）
- OpenAI API Key（或兼容供应商，如 DeepSeek）

### 安装

```bash
go mod download
```

### 配置

编辑 `config/roles.yaml`：

```yaml
meta_agent:
  model_config:
    provider: openai
    model: deepseek-v4-flash
    api_key: ${OPENAI_API_KEY}
    base_url: ${OPENAI_BASE_URL}
    temperature: 0.3
  max_blocks: 10
  summary_interval: 5

domain_agent:
  model_config:
    provider: openai
    model: deepseek-v4-flash
    api_key: ${OPENAI_API_KEY}
    base_url: ${OPENAI_BASE_URL}
    temperature: 0.5

fixed_roles:
  - id: code_assistant
    name: 代码助手
    model_config:
      provider: openai
      model: deepseek-v4-flash
      api_key: ${OPENAI_API_KEY}
      base_url: ${OPENAI_BASE_URL}
    can_be_called: true
```

### 运行

```bash
# 启动 PostgreSQL + Redis
docker compose -f docker/docker-compose.yml up -d

# 应用数据库迁移
psql "$POSTGRES_DSN" -f migrations/001_init.sql

# 运行 TUI 终端界面（编程主入口）
go run ./backend/cmd/tui/main.go

# 或运行 HTTP 服务（Web UI）
go run ./backend
```

---

## 项目结构

```
.
├── backend/
│   ├── cmd/
│   │   ├── tui/              # TUI 终端界面（编程主入口）
│   │   └── memory-console/   # 记忆检查控制台
│   ├── internal/
│   │   ├── graph/            # Agent 编排（Router + Plan-Execute-Reflect）
│   │   ├── memory/           # 记忆模块（写入、压缩、检索、组装）
│   │   ├── model/            # 模型工厂（按角色分层选型）
│   │   ├── server/           # HTTP API + SSE 事件流
│   │   └── store/            # PostgreSQL + Redis 存储
│   ├── pkg/
│   │   ├── config/           # 配置加载
│   │   └── types/            # 核心类型定义
│   ├── go.mod
│   └── main.go               # HTTP 服务入口
├── config/
│   ├── config.yaml           # 基础设施配置（PG/Redis/HTTP）
│   ├── roles.yaml            # 角色定义 + 模型配置
│   ├── skills.yaml           # Skill 池（可选）
│   └── soul.md               # 人格定义（可选）
├── web/                      # Web UI（Vue 3 + Vite）
├── doc/                      # 设计文档
├── migrations/               # 数据库 Schema
└── docker/
    └── docker-compose.yml    # PG + Redis 一键启动
```

---

## 核心模块

### 1. 智能路由（`internal/graph/router.go`）

三层路由策略：

| 任务类型 | 判断方式 | 处理方式 | 延迟 |
|---------|---------|---------|------|
| 简单任务（寒暄/自我介绍/事实查询） | 规则匹配（零成本） | 轻量模型直接回答，不进入 Agent 循环 | ~200ms |
| 工具调用（查文件/读代码/跑命令） | 关键词匹配（零成本） | 直接执行工具，不生成计划 | ~500ms |
| 复杂任务（写代码/重构/多步分析） | LLM 判断（兜底） | 进入 Plan-Execute-Reflect 循环 | 1-3s |

**效果**：80% 的请求不走 Agent 循环，直接响应；只有 20% 的复杂任务进入完整编排。

### 2. Agent 核心（`internal/graph/agent.go`）

Plan-Execute-Reflect 循环：

```
Plan  → 生成结构化计划（1-5 步 JSON）
  │
  ▼
Execute → 执行当前步骤（LLM 调用或工具调用）
  │
  ▼
Observe → 收集执行结果（成功/失败/输出）
  │
  ▼
Reflect → 评估是否偏离目标，是否调整计划
  │
  └─→ 继续 / 调整计划 / 终止
```

- 最大 **3-5 轮**后强制终止（配置可覆盖），防止无限循环
- 每轮执行后**同步写入**中期记忆（执行轨迹），确保可审计、可续传
- 计划是结构化的（JSON 格式），不是自由文本，LLM 可以看到全局

### 3. 记忆模块（`internal/memory/`）

| 组件 | 文件 | 职责 |
|------|------|------|
| Write | `write.go` | Episode 重要性评分 + 话题绑定 |
| Compress | `compress.go` | 两级压缩：原始（7 天）→ 摘要（永久） |
| Search | `search.go` | 多信号相关性：关键词匹配 + 时间衰减 |
| Assemble | `assembler.go` | 上下文组装：按 TokenBudget 召回相关记忆 |
| Snapshot | `snapshot.go` | Redis 热加载 + Postgres 持久化 |

**记忆写入机制**（关键改进）：
```go
func (a *Agent) Run(ctx context.Context, input string) (*Result, error) {
    result := a.loop(ctx, input, memories)       // 执行 Plan-Execute-Reflect
    a.memory.SaveTrace(ctx, result.Trace)        // 同步写入中期记忆（确保不丢）
    go a.memory.SaveSummary(ctx, result.Summary) // 异步写入长期记忆（不阻塞）
    return result, nil
}
```

**不是回调，是显式调用。写入失败立即报错，不再"可能写入了也可能没写入"。**

### 4. 存储（`internal/store/`）

- `postgres.go` — 中期记忆（执行轨迹）、长期记忆（摘要+向量）、会话元数据
- `redis.go` — 短期记忆（当前会话上下文）、任务看板、会话快照

---

## TUI 界面

参考 Claude Code 的简洁终端交互，设计为编程场景优化：

```
┌────────────────────────────────────────────────────────────┐
│  BlockMemoryAgent > session-001  ●running  3 agents active    │
├────────────────────────────────────────────────────────────┤
│                                                             │
│  > 帮我修复这个 CSS 文件的样式问题                           │
│                                                             │
│  好的，让我先检查文件内容...                                  │
│  [●] ReadFile: styles.css                                  │
│  [✓] ReadFile: styles.css  (1.2KB)                         │
│                                                             │
│  发现 `.header` 的 `padding` 值不正确...                      │
│  [●] WriteFile: styles.css                                 │
│  [✓] WriteFile: styles.css  (+3/-2 lines)                  │
│                                                             │
│  已修复。让我验证一下...                                      │
│  [●] RunCommand: npx stylelint styles.css                   │
│  [✓] RunCommand: npx stylelint styles.css  (exit 0)        │
│                                                             │
│  修复完成。`.header` 的 `padding` 从 `20px` 调整为 `16px`，   │
│  与 `.footer` 保持一致。                                      │
│                                                             │
├────────────────────────────────────────────────────────────┤
│  [plan] 1.ReadFile → 2.WriteFile → 3.RunTest   [2/3] ✓      │
├────────────────────────────────────────────────────────────┤
│  > _                                                        │
└────────────────────────────────────────────────────────────┘
```

- **主对话区**：滚动显示用户输入和 Agent 响应，工具调用用 `[●]` / `[✓]` 标记
- **底部状态栏**：显示当前计划进度（如 `[plan] 1.ReadFile → 2.WriteFile → 3.RunTest [2/3] ✓`）
- **Agent 面板**：按 `Tab` 切换右侧折叠面板，显示当前活跃 Agent 列表、执行计划、工具调用历史
- **记忆指示**：当 Agent 从长期记忆召回知识时，在状态栏显示 `🧠 recalled: 用户偏好用 Vue 3`

---

## 与 Claude Code 的对比

| 能力 | Claude Code | BlockMemoryAgent |
|------|-------------|------------------|
| 终端编程界面 | ✅ 优秀 | ✅ 参考 Claude Code 设计 |
| 代码编辑 | ✅ 成熟 | ✅ 工具调用实现 |
| 命令执行 | ✅ 成熟 | ✅ 工具调用实现 |
| **长时稳定运行** | ❌ 几百轮后质量下降 | ✅ 上下文恒定，理论上无限运行 |
| **话题隔离** | ❌ 所有对话在一个上下文 | ✅ 会话块隔离，互不污染 |
| **跨会话记忆** | ❌ 新对话从零开始 | ✅ 自动提取关键知识，新会话召回 |
| **记忆可观测** | ❌ 黑盒 | ✅ Web/TUI 均可查看记忆检索过程 |
| **自托管** | ❌ 闭源 | ✅ 开源，本地运行 |
| **模型选择** | ❌ 固定 Anthropic | ✅ 多模型，按角色分层选型 |

**定位**：BlockMemoryAgent 不是 Claude Code 的替代品，而是**在 Claude Code 做不到的事情上补强**。你可以把它和 Claude Code 配合使用：Claude Code 负责日常快速编辑，BlockMemoryAgent 负责需要持续多天、多话题并行、跨会话记忆保持的长任务。

---

## 技术栈

- **语言**：Go 1.22+
- **Agent 框架**：自研 Plan-Execute-Reflect 循环
- **模型适配**：OpenAI 兼容 API（DeepSeek / OpenAI / 本地模型）
- **存储**：PostgreSQL 14+（中期/长期记忆）+ Redis 7+（短期记忆）
- **向量检索**：pgvector（后期接入真实 embedding，前期关键词匹配兜底）
- **TUI**：bubbletea + lipgloss
- **Web**：Vue 3 + Vite + Element Plus
- **配置**：YAML + `.env`

---

## 扩展方向

- **真实 Embedding**：接入 text-embedding-3 或 bge-m3，替换关键词匹配，提升跨会话记忆召回质量
- **MCP 工具生态**：对接 filesystem / github / browser / database 等 MCP Server，扩展工具能力
- **Web UI 增强**：记忆可视化、Agent 执行轨迹回放、热点/冷记忆分布图
- **模型分层选型**：Router 用轻量模型，Plan 用中等模型，代码执行用强模型，节省成本

---

## 文档索引

| 文档 | 内容 |
|------|------|
| `doc/设计文档_v3.md` | 核心设计文档（记忆、编排、上下文管理） |
| `doc/项目说明.md` | 项目导览、目录结构、代码阅读顺序 |
| `doc/TUI设计文档.md` | TUI 界面设计与交互规范 |
| `doc/AGENT_FLOW_GUIDE.md` | Agent 执行流程详解 |
| `doc/TODO.md` | 已完成/待完成任务清单 |

---

## License

MIT
