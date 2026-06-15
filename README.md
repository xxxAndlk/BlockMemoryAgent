# BlockMemoryAgent

基于 [CloudWeGo Eino](https://github.com/cloudwego/eino) 的多 Agent 智能编排系统，支持可配置角色、三层（可扩展至四层）架构调度、记忆模块优化与终端 TUI 可视化。

---

## 特性

- **三层 + 扩展架构**：MetaAgent（调度）→ DomainAgent（领域管理）→ Assistant（任务执行），复杂领域自动拆分为 SubDomainAgent
- **配置驱动角色**：固定角色（代码助手 / UI 助手 / 测试助手等）在 `config/roles.yaml` 定义；业务角色（商城页面负责人等）由大模型动态创建
- **独立模型配置**：每个角色可绑定独立模型（gpt-4o / gpt-4o-mini / claude 等），DomainAgent 层共用同一配置
- **记忆模块优化**：三层记忆（PostgreSQL 私有记忆 + Redis 工作区 + Vector DB 全局知识库）+ 四级压缩 + 多信号相关性评分
- **TUI 终端可视化**：基于 bubbletea 的 4 面板实时状态展示（角色树 / 会话状态 / 事件日志 / 统计）
- **Eino 原生集成**：基于 Eino Graph 编排与 ChatModel 接口实现 Agent 节点

---

## 架构

```
┌─────────────────────────────────────────────────────────────┐
│  Layer 1: MetaAgent (调度中枢)                               │
│  - 分析用户目标，创建/调度 DomainAgent                        │
│  - 处理跨领域协作与升级                                       │
│  - 模型：轻量 (gpt-4o-mini)                                   │
├─────────────────────────────────────────────────────────────┤
│  Layer 2: DomainAgent (领域负责人)                           │
│  - 管理领域上下文，拆分任务                                    │
│  - 复杂领域自动拆分为 SubDomainAgent                           │
│  - 模型：共用配置 (gpt-4o)                                   │
├─────────────────────────────────────────────────────────────┤
│  Layer 2.5: SubDomainAgent (子领域负责人)                    │
│  - 自动拆分：如商城页面 → 首页头部 / 商品列表 / 底部导航       │
│  - 模型：继承 DomainAgent 配置                               │
├─────────────────────────────────────────────────────────────┤
│  Layer 3: Assistant (助手)                                   │
│  - 固定助手：code/ui/test/doc/prompt_reviewer（配置文件）      │
│  - 临时助手：大模型动态创建，任务级生命周期                      │
│  - 每个助手可独立配置模型                                     │
└─────────────────────────────────────────────────────────────┘
```

### 调用关系

| 调用者 \ 被调用者 | MetaAgent | DomainAgent | SubDomainAgent | 固定助手 | 临时助手 |
|---|---|---|---|---|---|
| **MetaAgent** | — | 创建/调度 | — | — | — |
| **DomainAgent** | 返回结果 | — | 创建/调用 | 调用 | 创建/调用 |
| **SubDomainAgent** | — | — | — | 调用 | 创建/调用 |
| **固定/临时助手** | — | — | — | — | — |

---

## 快速开始

### 依赖

- Go 1.22+
- PostgreSQL 14+（可选，记忆持久化）
- Redis 7+（可选，工作区缓存）
- OpenAI API Key（或配置其他兼容供应商）

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
    model: gpt-4o-mini
    api_key: ${OPENAI_API_KEY}
    temperature: 0.3
  max_blocks: 10

domain_agent:
  model_config:
    provider: openai
    model: gpt-4o
    api_key: ${OPENAI_API_KEY}
    temperature: 0.5

fixed_roles:
  - id: code_assistant
    name: 代码助手
    model_config:
      provider: openai
      model: gpt-4o
      api_key: ${OPENAI_API_KEY}
    can_be_called: true
```

### 运行

```bash
# 命令行演示
go run cmd/demo/main.go

# TUI 终端可视化
go run cmd/tui/main.go
```

---

## 项目结构

```
.
├── cmd/
│   ├── demo/          # 三层架构命令行演示
│   └── tui/           # bubbletea 终端可视化
├── config/
│   └── roles.yaml     # 角色与模型配置
├── internal/
│   ├── graph/         # 三层/四层图编排（Meta/Domain/SubDomain/Assistant）
│   ├── memory/        # 记忆模块（压缩、检索、组装、快照）
│   ├── model/         # Eino ChatModel 工厂与客户端
│   ├── retriever/     # 全局知识库检索
│   ├── server/        # API 与 TUI 事件广播
│   └── store/         # PostgreSQL + Redis 存储
├── migrations/        # 数据库 Schema
├── pkg/
│   ├── config/        # 配置加载
│   └── types/         # 核心类型定义
├── ARCHITECTURE.md    # 三层架构详细设计
├── AGENT_MODEL_CONFIG.md  # 模型配置设计
└── README.md
```

---

## 核心模块

### 1. 角色系统（`internal/graph/`）

- `role_registry.go` — 固定角色 + 动态角色注册表，权限矩阵
- `role_factory.go` — 大模型动态创建 DomainAgent / SubDomainAgent / Assistant
- `meta_agent.go` — Layer 1：会话调度、领域分析、事件处理
- `domain_agent.go` — Layer 2：任务拆解、助手匹配、自动拆分判断
- `subdomain_agent.go` — Layer 2.5：子领域执行、结果聚合
- `assistant.go` — Layer 3：具体任务执行、调用栈管理

### 2. 模型工厂（`internal/model/`）

- `eino_client.go` — Eino ChatModel 包装器，支持 Generate / GenerateWithSystem
- `factory.go` — 按角色缓存模型实例，自动回退 Mock（无 API Key）

### 3. 记忆模块（`internal/memory/`）

- `assembler.go` — 四段式上下文组装（System + TopicGlobal + SharedState + PrivateMemory）
- `compress.go` — 四级压缩（Raw → Standard → Compact → Marker）
- `search.go` — 多信号相关性评分（语义相似 + 实体重叠 + 因果链 + 时间衰减）
- `snapshot.go` — Redis 热加载 + PostgreSQL 持久化
- `write.go` — 重要性判断与写入决策

### 4. 存储（`internal/store/`）

- `postgres.go` — 私有记忆、快照、话题元数据、知识库 CRUD
- `redis.go` — 工作区 Hash、SortedSet、Stream、TTL 管理

---

## 配置说明

### 角色配置（`config/roles.yaml`）

| 字段 | 说明 |
|---|---|
| `meta_agent.model_config` | MetaAgent 专用模型（建议轻量） |
| `domain_agent.model_config` | 所有 DomainAgent / SubDomainAgent 共用 |
| `fixed_roles[].model_config` | 每个固定助手独立模型配置 |
| `fixed_roles[].can_be_called` | 是否允许上层 Agent 调用 |
| `fixed_roles[].skills` | 技能关键词，用于任务匹配 |
| `dynamic_templates` | 动态角色生成模板（LLM Prompt） |

### 环境变量

| 变量 | 说明 |
|---|---|
| `OPENAI_API_KEY` | OpenAI API Key（配置文件中 `${OPENAI_API_KEY}` 引用） |
| `ANTHROPIC_API_KEY` | Claude 系列模型 Key（可选） |

---

## 运行示例

### 命令行演示

```bash
$ go run cmd/demo/main.go

=== 角色配置加载完成 ===
固定角色数量: 5
  - [code_assistant] 代码助手 (类型: fixed, 生命周期: permanent, 模型: gpt-4o)
  - [ui_assistant] UI助手 (类型: fixed, 生命周期: permanent, 模型: gpt-4o)
  ...
MetaAgent 模型: gpt-4o-mini
DomainAgent 模型: gpt-4o

=== 启动三层架构会话 ===
会话ID: demo-session-001
用户目标: 修复商城主页穿模问题

=== 会话执行完成 ===
最终动作: Finish
会话总结: 会话[demo-session-001]已执行2步...

=== 会话中创建的角色实例 ===
  - [domain_商城页面_1] 商城页面负责人 (类型: domain, 状态: done)
  - [subdomain_首页头部_2] 首页头部子领域负责人 (类型: subdomain, 状态: done)
  - [subdomain_商品列表_6] 商品列表子领域负责人 (类型: subdomain, 状态: done)
  - [subdomain_底部导航_10] 底部导航子领域负责人 (类型: subdomain, 状态: done)
  - [assistant_3] 临时助手 (类型: dynamic, 状态: done)
```

### TUI 终端

```bash
$ go run cmd/tui/main.go
```

界面布局：

```
┌─────────────────────────────────────────────────────────┐
│  BlockMemory Agent Console ● │ Session: tui-session-001 │
├──────────────────────┬──────────────────────────────────┤
│ Role Hierarchy       │ Session State                     │
│ ◆ MetaAgent active   │ Summary: ...                     │
│   └── ◆ 商城页面      │ Active Blocks: 0                 │
│       └── ◇ 首页头部  │ Call Stack: (empty)              │
│           └── ▸ 助手  │ Next Action: Finish              │
│       └── ◇ 商品列表  │                                   │
│           └── ▸ 助手  │                                   │
│       └── ◇ 底部导航  │                                   │
│           └── ▸ 助手  │                                   │
├──────────────────────┼──────────────────────────────────┤
│ Event Log            │ Statistics                        │
│  17:29:05 会话完成   │ Total Roles: 16                   │
│  17:29:05 头部完成   │   DomainAgent: 1                  │
│  17:29:05 列表完成   │   SubDomainAgent: 3               │
│  17:29:05 底部完成   │   Assistants: 12                  │
│  17:29:05 助手完成   │ Completed: 16/16                  │
│  17:29:05 助手完成   │ Steps: 2                          │
└──────────────────────┴──────────────────────────────────┘
 [Q]uit [Tab]Focus [↑↓]Navigate [D]etail
```

---

## 技术栈

- **编排框架**：CloudWeGo Eino（`compose.Graph`, `Branch`, `State`, `Callback`）
- **模型适配**：Eino OpenAI Adapter（`eino-ext/components/model/openai`）
- **存储**：PostgreSQL（持久化）+ Redis（工作区缓存）
- **TUI**：bubbletea + lipgloss
- **配置解析**：gopkg.in/yaml.v3

---

## 扩展方向

- **Meta-MetaAgent**：多会话协调层
- **助手自治化**：Assistant 间直接调用（无需 DomainAgent 中转）
- **Tool Calling**：集成 Eino `ToolCallingChatModel` 实现工具调用闭环
- **Stream 输出**：TUI 支持 SSE 实时流式状态更新

---

## 文档索引

| 文档 | 内容 |
|---|---|
| `ARCHITECTURE.md` | 三层架构详细设计、状态机、调用权限矩阵 |
| `AGENT_MODEL_CONFIG.md` | 多 Agent 模型配置与 Eino ChatModel 工厂设计 |
| `设计文档.md` | 原始设计文档（记忆模块、Graph 编排） |
| `设计文档_v2_Eino版.md` | 基于 Eino 的完整系统设计（含 SQL Schema、TUI 设计） |

---

## License

MIT
