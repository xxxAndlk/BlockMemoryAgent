# BlockMemoryAgent 项目文档

> 最后更新: 2026-06-16

## 1. 项目概述

BlockMemoryAgent 是一个基于 CloudWeGo Eino 框架的多Agent智能编排系统。核心思想是"分块记忆"——将Agent的私有记忆按会话块（Block）隔离管理，通过分层架构实现任务自动拆解、角色动态创建和记忆压缩。

**技术栈**: Go 1.25 / CloudWeGo Eino / PostgreSQL + pgvector / Redis / bubbletea TUI

---

## 2. 系统架构

### 2.1 四层Agent架构

```
┌─────────────────────────────────────────┐
│           Layer 1: MetaAgent            │  会话调度器
│  - 分析用户目标，确定业务领域            │  - 拆分领域
│  - 创建DomainAgent（SessionBlock）       │  - 监控会话块
│  - 处理跨领域协作与升级                  │  - 会话总结
├─────────────────────────────────────────┤
│         Layer 2: DomainAgent            │  领域上下文管理器
│  - 管理领域内的上下文信息                │  - 一个Domain = 一个SessionBlock
│  - 分析任务，分发给助手或子领域           │  - 自动拆分为SubDomain
│  - 汇总助手结果                          │
├─────────────────────────────────────────┤
│       Layer 2.5: SubDomainAgent         │  子领域执行器
│  - 细粒度任务拆解                        │  - 商城页面→头部/列表/底部
│  - 创建助手执行具体任务                   │  - 完成后返回父DomainAgent
│  - 汇总子领域结果                        │
├─────────────────────────────────────────┤
│         Layer 3: Assistant              │  任务执行者
│  - Fixed: 配置文件预定义，长期存在        │  - 代码/UI/测试/文档/Prompt助手
│  - Dynamic: 大模型动态创建，任务后消亡    │  - 由Domain/SubDomain调度
│  - 执行具体任务并返回结果                 │
└─────────────────────────────────────────┘
```

### 2.2 图执行模型

系统使用 `ThreeLayerGraph` 实现状态机式执行：

```
MetaAgent ──Switch──→ DomainAgent ──Switch──→ SubDomainAgent ──Switch──→ Assistant
    ↑                     │                      │                      │
    │    Continue/Finish  │     Continue/Finish   │    Switch(Parent)    │
    └─────────────────────┘──────────────────────┘──────────────────────┘
                                         ↓
                                  EscalationHandler (升级仲裁)
                                         ↓
                                      Sinker (终止)
```

**核心数据流**:
1. `ThreeLayerGraph.Invoke()` 驱动状态机循环（max 200 steps）
2. 每个Node通过 `state.NextAction` 控制流向：Continue/Switch/Escalate/Finish
3. `CallStack` 实现嵌套调用：DomainAgent→SubDomainAgent→Assistant
4. `SessionBlock` 隔离领域上下文，`TaskResults` 跟踪任务完成

### 2.3 记忆系统架构

```
┌─────────────────────────────────────────────────┐
│                 记忆系统三层存储                    │
├─────────────────────────────────────────────────┤
│  PostgreSQL (持久层)                              │
│  - Episodes: 结构化情节记录                       │
│  - Snapshots: Agent快照（KeySummaries + Issues） │
│  - Knowledge: 全局知识库 + pgvector向量检索       │
├─────────────────────────────────────────────────┤
│  Redis (热数据层)                                 │
│  - Snapshots: Agent热快照 (TTL 7天)              │
│  - AgentOutput: 最新输出                          │
│  - Event: 工作区事件                              │
├─────────────────────────────────────────────────┤
│  pgvector (向量检索层)                             │
│  - 768维嵌入 + IVFFlat/HNSW索引                   │
│  - cosine/l2/inner_product距离度量                │
└─────────────────────────────────────────────────┘
```

**记忆处理管线**:

```
WriteProcessor → Episode写入 → 重要性评分 → 主题绑定
                                    ↓
                            Compressor (遗忘压缩)
                            Level 0 (完整) → Level 1 (摘要+Facts)
                            → Level 2 (仅Summary) → Level 3 (标记)
                                    ↓
SearchScorer (多信号检索评分)
  - 语义相似度 (pgvector)
  - 实体重叠 (关键词)
  - 时间衰减
  - 因果链关联
                                    ↓
ContextAssembler (上下文组装)
  - TokenBudget: SystemRole/TopicGlobal/SharedState/GlobalKB/PrivateMemory/TaskQuery/Reserve
  - 按相关性分配配额，裁剪私有记忆
```

### 2.4 角色管理系统

```
RoleConfigFile (roles.yaml)
├── MetaAgent配置 (model, system_prompt, max_blocks)
├── DomainAgent配置 (共享模型配置)
├── FixedRoles[] (5个固定助手: 代码/UI/Prompt审查/测试/文档)
└── DynamicTemplates[] (领域/助手模板)

RoleRegistry (运行时)
├── RoleDefinitions: 固定 + 动态角色定义
├── RoleInstances: 运行时实例
├── CanCall() 权限矩阵
└── CleanupExpired() 生命周期管理

RoleFactory (动态创建)
├── CreateDomainAgent() → LLM生成角色定义 → 注册 → 实例化
├── CreateAssistant() → LLM生成 → JSON解析 → 模板回退 → 实例化
└── CreateSubDomainAgent() → 模板创建 → 实例化
```

---

## 3. 目录结构

```
BlockMemoryAgent/
├── main.go                          # 程序入口
├── config/
│   ├── config.yaml                  # 基础设施配置 (Postgres/Redis/HTTP/Memory)
│   └── roles.yaml                   # 角色配置 (Meta/Domain/Fixed/Dynamic)
├── cmd/
│   ├── demo/main.go                 # 交互式演示 (3层图执行)
│   ├── tui/main.go                  # bubbletea TUI前端 (4面板)
│   └── memory-console/main.go       # 记忆控制台 (SSE + TUI)
├── internal/
│   ├── config/config.go             # 基础设施配置加载 + env解析
│   ├── graph/
│   │   ├── three_layer_graph.go     # 核心图执行引擎
│   │   ├── meta_agent.go            # Layer 1: MetaAgent
│   │   ├── domain_agent.go          # Layer 2: DomainAgent
│   │   ├── subdomain_agent.go       # Layer 2.5: SubDomainAgent
│   │   ├── assistant.go             # Layer 3: Assistant
│   │   ├── escalation.go            # 升级仲裁节点
│   │   ├── role_registry.go         # 角色注册表 + 权限矩阵
│   │   ├── role_factory.go          # 动态角色工厂 (LLM生成 + JSON解析)
│   │   └── util.go                  # 共享类型 (WorkspaceWriter, BuildRequest等)
│   ├── memory/
│   │   ├── write.go                 # Episode写入 + 重要性评分 + 主题绑定
│   │   ├── compress.go              # 4级压缩 (Raw→Standard→Compact→Marker)
│   │   ├── search.go                # 多信号检索评分
│   │   ├── assembler.go             # 上下文组装 (TokenBudget分配)
│   │   ├── snapshot.go              # 快照管理 (Redis热 + PG冷)
│   │   ├── callback.go              # Eino回调 (OnStart/OnEnd/OnError)
│   │   └── monitor.go               # 切换监控 + 自愈 + 自动仲裁
│   ├── model/
│   │   ├── eino_client.go           # Eino ChatModel封装 (OpenAI兼容)
│   │   └── factory.go               # 模型工厂 (per-role缓存 + mock回退)
│   ├── retriever/global_kb.go       # 全局知识库检索
│   ├── server/
│   │   ├── api.go                   # HTTP API
│   │   └── tui_broadcaster.go       # SSE广播
│   └── store/
│       ├── postgres.go              # PostgreSQL存储 (pgvector格式)
│       └── redis.go                 # Redis存储
├── pkg/
│   ├── config/
│   │   ├── role_config.go           # 角色配置加载 + env解析 + 权限检查
│   │   └── util.go                  # resolveEnv()
│   └── types/
│       ├── types.go                 # 核心类型 (Episode/Event/TokenBudget等)
│       └── role.go                  # 角色类型 (RoleDefinition/Instance/SessionBlock等)
└── doc/
    ├── ARCHITECTURE.md
    ├── AGENT_MODEL_CONFIG.md
    ├── 设计文档.md
    └── 设计文档_v2_Eino版.md
```

---

## 4. 核心数据流

### 4.1 一次完整会话流程

```
用户输入: "修复商城主页穿模问题"
    │
    ▼
MetaAgent.handleInitial()
  ├── analyzeDomainsWithLLM() → LLM分析用户目标 → [{Name:"商城页面", Goal:"..."}]
  │   └── LLM不可用时回退到 analyzeDomainsByRules() 关键词匹配
  ├── factory.CreateDomainAgent() → 注册动态角色 + 创建实例
  └── 创建SessionBlock{Domain:"商城页面", Agents:[domain_inst_1]}
    │
    ▼
DomainAgent.Invoke() (domain_inst_1)
  ├── analyzeTasksWithLLM() → LLM拆解任务 → ["分析根因", "定位代码", ...]
  │   └── LLM不可用时回退到 analyzeTasksByRules()
  ├── shouldSplitWithLLM() → LLM判断是否需拆分子领域
  │   └── LLM不可用时回退到规则判断
  └── handleSubDomainSplit()
      ├── inferSubDomainsWithLLM() → LLM推断子领域
      └── 依次调度SubDomainAgent:
          │
          ▼ SubDomainAgent ("首页头部")
          ├── analyzeSubTasksWithLLM() → LLM拆解子任务
          ├── dispatchAssistantsParallel() ← 并行执行核心
          │   ├── goroutine 1: createAssistantForTask → runAssistant → retryWithBackoff
          │   ├── goroutine 2: createAssistantForTask → runAssistant → retryWithBackoff
          │   └── goroutine 3: createAssistantForTask → runAssistant → retryWithBackoff
          │       └── 每个助手: executeAssistantTask()
          │           ├── 优先: modelFactory.GetModel → LLM.Generate (真实执行)
          │           └── 回退: 模拟结果
          │       └── sync.WaitGroup 等待所有 goroutine 完成
          ├── 汇总 results → block.TaskResults
          └── PopCallStack → Switch(parent DomainAgent)
    │
    ▼
DomainAgent 所有子领域完成
  ├── summarizeResults() → 从 TaskResults 收集
  └── NextAction = Continue → 返回 MetaAgent
    │
    ▼
MetaAgent.switchToNextBlock()
  ├── 标记SessionBlock完成
  └── 无更多活跃块 → ActionFinish → Sinker
```

### 4.2 记忆写入流程

```
Agent每步执行 → CallbackHandler.OnEnd()
  → WriteProcessor.Process()
    ├── generateStepID() → 唯一步ID
    ├── Summarizer.Summarize() → ObservationSummary
    ├── FactExtractor.ExtractFacts() → Facts[]
    ├── ImportanceScorer.Score() → Importance (0-1)
    ├── TopicDetector.Detect() → TopicID
    └── PrivateStore.SaveEpisode()
  → SnapshotManager.SaveFromState()
    ├── 提取最近5步关键摘要
    ├── 提取未解决问题 (importance>0.7 && reflection为空)
    ├── Redis.SaveSnapshot (TTL 7天)
    └── PG.SaveSnapshot (异步)
```

---

## 5. 配置系统

### 5.1 基础设施配置 (config/config.yaml)

```yaml
postgres:
  dsn: ${POSTGRES_DSN:"postgres://user:pass@localhost:5432/blockmemory?sslmode=disable"}
  max_open_conns: 25
  max_idle_conns: 5
  conn_max_lifetime: 300

pgvector:
  enabled: true
  dimensions: 768
  index_type: ivfflat        # ivfflat | hnsw
  distance_metric: cosine     # cosine | l2 | inner_product

redis:
  addr: ${REDIS_ADDR:"localhost:6379"}
  password: ${REDIS_PASSWORD:""}
  db: 5
  pool_size: 10
  snapshot_ttl_days: 7

http:
  addr: ${HTTP_ADDR:":8080"}

memory:
  write_batch_size: 100
  write_flush_interval: 5
  snapshot_interval: 300
```

**环境变量解析**: 支持 `${VAR}` 和 `${VAR:"default"}` 语法，通过 `resolveEnvWithDefault()` 实现。

### 5.2 角色配置 (config/roles.yaml)

- **MetaAgent**: 配置模型 + 系统提示词 + 最大会话块数 + 总结间隔
- **DomainAgent**: 共享模型配置（所有DomainAgent使用同一模型）
- **FixedRoles**: 5个预定义助手（代码/UI/Prompt审查/测试/文档）
- **DynamicTemplates**: LLM动态创建角色时的参考模板

**每角色模型配置**: `provider/model/api_key/base_url/temperature/max_tokens`，APIKey支持 `${ENV_VAR}` 解析。

---

## 6. 已修复的Bug清单

| 编号 | 严重度 | 文件 | 问题描述 | 修复方式 |
|------|--------|------|----------|----------|
| 1 | P0 | memory/write.go | `containsAny` 循环体 `_ = lower` 永远不匹配 | `strings.Contains(strings.ToLower(s), strings.ToLower(kw))` |
| 2 | P0 | store/postgres.go | pgVector格式 `[1 2 3]` 非PostgreSQL要求的 `[1,2,3]` | `strings.Join` 格式化浮点数组 |
| 3 | P0 | graph/escalation.go | 使用 `ctx.Value("step")` 无类型context key | 改用 `stepKeyType{}` 类型化key |
| 4 | P0 | graph/role_factory.go | LLM响应被 `_ = resp` 丢弃 | 添加 `extractJSON()` + `json.Unmarshal` + 模板回退 |
| 5 | P0 | memory/compress.go | 压缩后Episode未持久化 | 添加 `c.store.SaveEpisode()` 批量写入 |
| 6 | P0 | main.go | `config.Load` 在 `internal/config` 但导入 `pkg/config` | 添加 `internal/config` 导入，别名 `pkgconfig` |
| 7 | P1 | graph/three_layer_graph.go | `g.nodes` map无并发保护 | 添加 `sync.RWMutex`，读写分离锁 |
| 8 | P1 | internal/config/config.go | `${ENV_VAR:"default"}` 语法未解析 | 添加 `resolveEnvWithDefault()` + `resolveEnvVars()` |
| 9 | P1 | graph/meta_agent.go | `CreateDomainAgent` 失败静默跳过 | 添加 `fmt.Printf` 错误日志 |
| 10 | P1 | graph/domain_agent.go | `CreateInstance`/`CreateAssistant`/`CreateSubDomainAgent` 失败静默跳过 | 添加 `fmt.Printf` 错误日志 |
| 11 | P2 | 3处自定义 `min()` | Go 1.21+ 内置 `min`，自定义版本冲突 | 删除3处自定义实现 |
| 12 | P2 | 7个2层遗留文件 | router.go/validator.go/graph.go/agent_node.go/workspace.go/sinker.go/state.go | 删除遗留代码，迁移类型到 util.go |

---

## 7. 优势

### 7.1 架构设计

- **分层解耦**: MetaAgent/Domain/SubDomain/Assistant 四层职责清晰，每层只关心自己的调度逻辑
- **动态角色创建**: RoleFactory 通过 LLM 生成角色定义 + JSON 解析 + 模板回退，实现了灵活的运行时扩展
- **会话块隔离**: SessionBlock 机制确保不同领域的上下文互不干扰
- **调用栈嵌套**: CallStack 支持多层嵌套调用（Domain→SubDomain→Assistant），且自动回溯
- **Goroutine并行执行**: DomainAgent/SubDomainAgent 使用 `sync.WaitGroup` 并行调度所有助手，充分利用Go并发优势
- **LLM驱动决策**: 领域分析/任务拆解/子领域拆分均优先使用LLM，规则回退保证无LLM时也可运行
- **指数退避重试**: 助手执行失败自动重试（3次，100ms起步倍增），提高容错性

### 7.2 记忆系统

- **4级压缩**: 从完整记录到存在性标记，按重要性和时间智能压缩，平衡信息保留与Token消耗
- **Token预算分配**: 明确的TokenBudget分区（System/Topic/Shared/KB/Private/Task/Reserve），防止上下文溢出
- **多信号检索评分**: 语义相似度 + 实体重叠 + 时间衰减 + 因果链，多维度评估记忆相关性
- **冷热分层存储**: Redis热数据 + PG冷数据，快照优先读Redis，异步持久化到PG

### 7.3 工程实践

- **环境变量安全**: `${ENV_VAR:"default"}` 语法避免硬编码凭据
- **线程安全**: ThreeLayerGraph 的 nodes map 使用 RWMutex 保护
- **Mock回退**: ModelFactory 在无API Key时自动降级到MockClient，支持离线开发
- **TUI可视化**: bubbletea 4面板布局，实时展示角色树/会话状态/事件日志/统计信息
- **生命周期管理**: 角色实例有 permanent/session/task 三种生命周期，自动清理过期实例

---

## 8. 不足与改进方向

### 8.1 架构层面

| 问题 | 状态 | 说明 |
|------|------|------|
| ~~领域分析硬编码~~ | ✅ 已修复 | `analyzeDomainsWithLLM()` 优先使用LLM分析，回退到 `analyzeDomainsByRules()` |
| ~~任务拆解硬编码~~ | ✅ 已修复 | `analyzeTasksWithLLM()` Chain-of-Thought拆解，回退到 `analyzeTasksByRules()` |
| ~~SubDomain拆分条件简单~~ | ✅ 已修复 | `shouldSplitWithLLM()` 使用LLM判断是否需要拆分，回退到规则 |
| **助手匹配粗糙** | 待改进 | 关键词匹配 `score>=10` 即选中，无语义理解 → 使用嵌入向量做语义匹配 |
| ~~无重试机制~~ | ✅ 已修复 | `retryWithBackoff()` 指数退避重试（3次，100ms起步倍增） |
| ~~无并发执行~~ | ✅ 已修复 | `dispatchAssistantsParallel()` 使用 `sync.WaitGroup` + goroutine 并行调度所有助手 |

### 8.2 记忆系统

| 问题 | 影响 | 改进方向 |
|------|------|----------|
| **Summarizer/FactExtractor/ImportanceScorer 未实现** | `write.go` 只定义了接口，无实际实现 | 接入LLM做摘要/事实提取/重要性评分 |
| **Embedder 未实现** | `search.go` 的向量搜索依赖嵌入模型 | 对接OpenAI embedding API或本地模型 |
| **压缩Token估算不准确** | `CompressByBudget` 假设每条200 token，误差大 | 基于实际token计数器估算 |
| **快照异步保存可能丢失** | `snapshot.go` 用goroutine异步写PG，无确认 | 改为WAL模式或同步双写 |
| **全局知识库检索未连通** | `retriever/global_kb.go` 接口定义完整但无实现 | 实现pgvector检索 + 嵌入生成 |

### 8.3 工程层面

| 问题 | 影响 | 改进方向 |
|------|------|----------|
| **无单元测试** | 0%测试覆盖，Bug只能手动发现 | 至少覆盖 graph/记忆核心逻辑 |
| **错误处理不统一** | 部分fmt.Printf，部分log.Printf，部分静默忽略 | 统一使用 `log/slog` 结构化日志 |
| **配置校验不足** | roles.yaml 加载后不校验字段合法性 | 添加配置校验（必填字段、模型名白名单等） |
| **无优雅关闭** | 收到SIGINT后直接cancel，不等待进行中的任务 | 添加 graceful shutdown（等待当前Invoke完成） |
| **HTTP API 未完成** | `server/api.go` 有框架但无实际端点 | 实现会话创建/查询/控制API |
| **无分布式支持** | 所有状态在内存，单进程运行 | SessionState外置到Redis，支持多实例 |

### 8.4 Eino 集成

| 问题 | 状态 | 说明 |
|------|------|------|
| **未使用Eino Graph编排** | 待改进 | 自定义 `ThreeLayerGraph` 而非 `compose.Graph` → 迁移到Eino原生Graph |
| ~~Eino ChatModel利用率低~~ | ✅ 已修复 | DomainAgent/SubDomainAgent 的 `executeAssistantTask()` 已接入ChatModel，MetaAgent领域分析也使用LLM |
| **未使用Eino Callback** | 待改进 | 自定义CallbackHandler但未注册到Eino → 使用 `schema.Callback` 机制集成 |

---

## 9. 运行方式

> 详细部署教程见 [doc/DEPLOYMENT.md](DEPLOYMENT.md)

```bash
# 1. 启动基础设施
cd docker && docker compose up -d && cd ..

# 2. 配置环境变量
cp .env.example .env
# 编辑 .env 填入 OPENAI_API_KEY 等

# 3. 编译运行
go build -o blockmemory-agent .
./blockmemory-agent                          # 自动加载 .env
./blockmemory-agent -env /path/to/.env       # 指定 .env 路径
./blockmemory-agent -config config/config.yaml -roles config/roles.yaml

# Demo模式 (无需外部依赖)
go run cmd/demo/main.go

# TUI模式
go run cmd/tui/main.go

# 记忆控制台
go run cmd/memory-console/main.go
```

**配置优先级**: 系统环境变量 > `.env` 文件 > `config.yaml` 默认值

---

## 10. 依赖清单

| 依赖 | 用途 |
|------|------|
| `cloudwego/eino` | Graph编排 + ChatModel接口 |
| `cloudwego/eino-ext/components/model/openai` | OpenAI兼容ChatModel实现 |
| `gopkg.in/yaml.v3` | YAML配置解析 |
| `github.com/lib/pq` | PostgreSQL驱动 |
| `github.com/redis/go-redis/v9` | Redis客户端 |
| `github.com/charmbracelet/bubbletea` | TUI框架 |
| `github.com/pgvector/pgvector-go` | pgvector Go支持 |

---

## 11. 类型系统速查

```
核心类型:
  ThreeLayerState     全局状态机（SessionID/ActiveBlocks/CallStack/NextAction）
  SessionBlock        领域上下文隔离单元（Domain/Agents/Events/TaskResults/SubDomain*）
  RoleDefinition      角色定义（配置层，5种Type × 3种Lifecycle）
  RoleInstance        角色实例（运行时，含Status/ParentID/Children）
  CallRequest/Response 角色间调用协议（CallerID/CalleeID/Task/Context）
  Episode             结构化记忆（Observation/Facts/Reflection/Importance/CompressionLevel）
  AgentSnapshot       Agent快照（KeySummaries/OpenIssues/LocalVars）
  AgentOutput         Agent公开输出（Summary/DataRefs/KnownIssues）
  Event               工作区事件（CrossModify/DependencyMet/Escalation）
  TokenBudget         Token预算分配（7个分区）
  KnowledgeRecord     全局知识库记录（Content/Embedding/AccessCount）

枚举类型:
  ActionType          Continue | Switch | Escalate | Finish
  RoleType            meta | domain | subdomain | fixed | dynamic
  RoleLifecycle       permanent | session | task
  RoleStatus          idle | active | waiting | calling | done | error
  CompressionLevel    LevelRaw | LevelStandard | LevelCompact | LevelMarker
  EventType           CrossModify | DependencyMet | Escalation
  EventStatus         Pending | Processing | Done
```
