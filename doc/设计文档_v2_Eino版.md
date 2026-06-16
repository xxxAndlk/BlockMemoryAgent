# 多Agent协同智能体记忆管理系统技术设计文档 (Eino Edition)

> 版本: v2.0
> 底座框架: [CloudWeGo Eino](https://github.com/cloudwego/eino) (Go)
> 迭代焦点: 记忆模块深度优化、Eino Graph 原生融合、工程可落地性

---

## 1. 项目背景与目标

### 1.1 问题起源
LLM Agent 项目面临两大核心挑战：
- **上下文窗口爆炸**: 多步任务、工具调用、反复修改导致无关信息堆积，模型注意力稀释，出现"迷失在中间"和"伪遗忘"。
- **跨模块/跨Agent协作混乱**: 多子 Agent 频繁切换时，推理日志交叉污染，任务边界模糊。

### 1.2 设计目标
构建**记忆管理中间层**，使多 Agent 系统具备：
- 每个 Agent 工作在**干净、聚焦、无污染**的上下文中。
- 跨 Agent 通过受控**共享工作区**交换精炼信息，非全量历史。
- 主 Agent 快速路由任务，精准分配上下文，维持全局话题连贯性。
- 支持话题隔离、模块上下文切换、长程记忆压缩与遗忘。

---

## 2. 架构总览: Eino Graph × 记忆层

```
┌─────────────────────────────────────────────────────────────────────┐
│                         Eino Graph (编排层)                          │
│  ┌──────────┐    ┌──────────┐    ┌──────────┐    ┌──────────┐      │
│  │  Router  │───→│ Agent A  │───→│ Agent B  │───→│  Sinker  │      │
│  │  (主控)   │←───│  (Node)  │←───│  (Node)  │←───│ (归档)   │      │
│  └──────────┘    └──────────┘    └──────────┘    └──────────┘      │
│       ↑                                              │              │
│       └──────────────────────────────────────────────┘              │
│                          (循环/条件分支)                              │
└─────────────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────────────┐
│                      Memory Controller (记忆控制器)                    │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐              │
│  │   写入处理器   │  │ 检索与评分器  │  │ 上下文构建器  │              │
│  │  (WriteHook) │  │ (Retriever)  │  │ (Assembler)  │              │
│  └──────────────┘  └──────────────┘  └──────────────┘              │
│  ┌──────────────┐  ┌──────────────┐                                  │
│  │ 遗忘与压缩器   │  │  快照管理器   │                                  │
│  │ (Compressor) │  │  (Snapshot)  │                                  │
│  └──────────────┘  └──────────────┘                                  │
└─────────────────────────────────────────────────────────────────────┘
                              │
          ┌───────────────────┼───────────────────┐
          ▼                   ▼                   ▼
┌─────────────────┐ ┌─────────────────┐ ┌─────────────────┐
│  私有记忆层      │ │ 话题共享工作区   │ │   全局知识库     │
│ (Private Store) │ │ (Topic Workspace)│ │ (Global KB)     │
│   PostgreSQL    │ │     Redis       │ │  Vector DB      │
└─────────────────┘ └─────────────────┘ └─────────────────┘
```

### 2.1 Eino 原生融合点

| 记忆层组件 | Eino 对应机制 | 融合方式 |
|-----------|--------------|---------|
| 主 Agent 路由 | `compose.Graph` + `Branch` 节点 | Router Node 作为 Graph 入口，Branch 实现条件分发 |
| Agent 节点执行 | `compose.Chain` / 自定义 `Node` | 每个子 Agent 封装为独立 Node，内部可嵌套 Chain |
| 状态传递 | `state.State` (Graph 共享状态) | 记忆快照挂载于 State，跨 Node 只传递引用 ID |
| 上下文组装 | `prompt.ChatTemplate` | 构建器输出 `[]*schema.Message`，直接注入 ChatModel |
| 记忆写入 | `callback.Handler` (OnEnd) | 利用 Eino Callback 在 Node 执行后异步触发记忆写入 |
| 知识检索 | `retriever.Retriever` | 全局知识库实现 Eino Retriever 接口，无缝接入 RAG 管线 |
| 工具调用 | `tool.BaseTool` | Agent 内部工具统一实现 Eino Tool 接口 |

---

## 3. 三层记忆体系 (Eino State 映射)

### 3.1 私有记忆层 (Agent Private Memory)

**存储内容**: 每个 Agent 在本话题中的完整执行轨迹。
- 结构化情节记录 `Episode`，含: StepID、Timestamp、Action、ObservationSummary、FullObservation(可选)、Reflection、ImportanceScore。

**Eino State 映射**: 
- 运行时不常驻 State（防污染），仅传递 `agentID + topicID`。
- Agent Node 切入时，由 `SnapshotManager` 从 PostgreSQL 加载快照，注入 Node 局部 State。
- Agent Node 切出时，Callback `OnEnd` 触发写入处理器，生成 Episode 并持久化。

**存储 Schema (PostgreSQL)**:
```sql
CREATE TABLE agent_private_memory (
    id BIGSERIAL PRIMARY KEY,
    agent_id VARCHAR(64) NOT NULL,
    topic_id VARCHAR(64) NOT NULL,
    episode JSONB NOT NULL,           -- Episode 结构
    snapshot_ref VARCHAR(128),       -- 指向快照文件/对象
    importance_score FLOAT DEFAULT 0,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    CONSTRAINT uniq_agent_topic_step UNIQUE (agent_id, topic_id, (episode->>'stepID'))
);

CREATE INDEX idx_apm_agent_topic ON agent_private_memory(agent_id, topic_id, created_at DESC);
CREATE INDEX idx_apm_importance ON agent_private_memory(importance_score DESC, created_at DESC);

-- GIN 索引加速 JSONB 内部字段查询
CREATE INDEX idx_apm_episode_gin ON agent_private_memory USING GIN (episode);
```

**Snapshot 结构**:
```go
type AgentSnapshot struct {
    AgentID       string            `json:"agent_id"`
    TopicID       string            `json:"topic_id"`
    LastStepID    string            `json:"last_step_id"`
    KeySummaries  []SummaryBlock    `json:"key_summaries"`   // 最近 K 步关键摘要
    OpenIssues    []Issue           `json:"open_issues"`     // 未解决问题
    LocalVars     map[string]any    `json:"local_vars"`      // 本地变量
    PublishedVer  int               `json:"published_ver"`   // 上次发布到工作区的版本
    UpdatedAt     time.Time         `json:"updated_at"`
}
```

### 3.2 话题共享工作区 (Topic Workspace)

**定位**: 跨 Agent "公共白板"，由主 Agent 维护。

**存储内容**:
- 话题元数据: ID、目标、当前状态、创建时间、过期时间
- 全局约束: 单位、环境设定、硬性规则
- Agent 公开输出: `AgentOutput` 数组，含版本号、数据引用、已知问题
- 待处理事件队列: `Event` 数组
- 决策日志

**Eino State 映射**:
- 工作区作为 Graph 全局 State 的一部分，所有 Agent Node 可读（经主 Agent 过滤后注入）。
- 写入通过主 Agent Router Node 统一调度，子 Agent 通过 `publish()` 回调请求写入，不直接修改。

**存储 Schema (Redis)**:
```
Key 设计:
- topic:{topic_id}:meta          -> Hash (话题元数据)
- topic:{topic_id}:constraints   -> Hash (全局约束)
- topic:{topic_id}:outputs:{agent_id}  -> Sorted Set (按版本号排序的 AgentOutput)
- topic:{topic_id}:events        -> Stream (待处理事件队列)
- topic:{topic_id}:decisions     -> List (决策日志)
- topic:{topic_id}:deps_graph    -> String (JSON, 依赖关系图)
```

**AgentOutput 结构**:
```go
type AgentOutput struct {
    AgentID      string         `json:"agent_id"`
    Version      int            `json:"version"`
    Summary      string         `json:"summary"`       // 精炼摘要，限 500 字
    DataRefs     []string       `json:"data_refs"`     // 引用数据 ID/链接
    KnownIssues  []string       `json:"known_issues"`
    Timestamp    time.Time      `json:"timestamp"`
    Validated    bool           `json:"validated"`     // 主 Agent 已验证
}
```

**Event 结构**:
```go
type Event struct {
    ID           string         `json:"id"`
    Type         EventType      `json:"type"`          // CrossModify, DependencyMet, Escalation
    SourceAgent  string         `json:"source_agent"`
    TargetAgent  string         `json:"target_agent"`  // 空表示广播
    Payload      map[string]any `json:"payload"`
    Priority     int            `json:"priority"`
    CreatedAt    time.Time      `json:"created_at"`
    Status       EventStatus    `json:"status"`        // Pending, Processing, Done
}
```

### 3.3 全局知识库 (Global Knowledge Base)

**存储内容**: 跨话题持久知识 — 用户偏好、项目规范、历史成功案例、Agent 能力注册表。

**技术方案**: 向量数据库 + PostgreSQL 元数据存储。

**Eino 集成**: 实现 Eino `retriever.Retriever` 接口。

```go
// 实现 Eino Retriever 接口
type GlobalKnowledgeRetriever struct {
    vectorDB    VectorDB        // Milvus / FAISS / pgvector
    metaDB      *sql.DB         // PostgreSQL
    embedder    embedding.Embedder // Eino 嵌入接口
}

func (r *GlobalKnowledgeRetriever) Retrieve(ctx context.Context, query string, opts ...retriever.Option) ([]*schema.Document, error) {
    // 1. 向量化 query
    // 2. 向量检索 Top-K
    // 3. 元数据过滤 (用户ID、项目ID、时间范围)
    // 4. 时间衰减加权重排
    // 5. 返回 Document 切片
}
```

**存储 Schema (PostgreSQL 元数据 + pgvector)**:
```sql
CREATE TABLE global_knowledge (
    id BIGSERIAL PRIMARY KEY,
    knowledge_type VARCHAR(32) NOT NULL,  -- user_preference, project_spec, case_study, agent_capability
    topic_id VARCHAR(64),                  -- 可为空 (跨话题知识)
    content TEXT NOT NULL,
    embedding VECTOR(768),                 -- pgvector, 维度根据嵌入模型调整
    meta JSONB,                            -- 扩展元数据
    access_count INT DEFAULT 0,
    last_accessed TIMESTAMPTZ,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    archived BOOL DEFAULT FALSE
);

CREATE INDEX idx_gk_type ON global_knowledge(knowledge_type);
CREATE INDEX idx_gk_embedding ON global_knowledge USING ivfflat (embedding vector_cosine_ops);
CREATE INDEX idx_gk_access ON global_knowledge(last_accessed, access_count);
```

---

## 4. 记忆控制器: Eino Callback + 独立服务

### 4.1 组件架构

```
┌────────────────────────────────────────────────────────────┐
│                    MemoryController                        │
├────────────────────────────────────────────────────────────┤
│  ┌────────────────┐                                       │
│  │  WriteHandler   │  Eino callback.Handler 接口实现        │
│  │  (OnStart/OnEnd)│  挂载于 Graph 全局，每个 Node 触发      │
│  └────────────────┘                                       │
│  ┌────────────────┐  ┌────────────────┐  ┌──────────────┐ │
│  │ WriteProcessor │  │ SearchScorer   │  │ Assembler    │ │
│  │   写入处理器    │  │  检索评分器     │  │ 上下文构建器  │ │
│  └────────────────┘  └────────────────┘  └──────────────┘ │
│  ┌────────────────┐  ┌────────────────┐                   │
│  │  Compressor    │  │ SnapshotMgr    │                   │
│  │ 遗忘与压缩器    │  │   快照管理器    │                   │
│  └────────────────┘  └────────────────┘                   │
└────────────────────────────────────────────────────────────┘
```

### 4.2 写入处理器 (WriteProcessor)

**触发时机**: Eino Callback `OnEnd` 后异步执行（不阻塞 Graph）。

**处理流程**:
```go
func (wp *WriteProcessor) Process(ctx context.Context, nodeInfo *NodeInfo, state *GraphState) error {
    // 1. 提取 Node 输出
    output := extractOutput(nodeInfo)
    
    // 2. 生成结构化摘要 (轻量模型或规则)
    summary := wp.summarizer.Summarize(output)
    
    // 3. 提取关键事实
    facts := wp.extractor.ExtractFacts(output)
    
    // 4. 重要性评分 (0-1)
    importance := wp.scorer.ScoreImportance(summary, facts)
    
    // 5. 话题边界检测
    topicBound := wp.detector.DetectBoundary(state.CurrentTopic, output)
    
    // 6. 构建 Episode
    episode := &Episode{
        StepID:      generateStepID(),
        Timestamp:   time.Now(),
        Action:      nodeInfo.Action,
        ObservationSummary: summary,
        FullObservation:    output.Raw,  // 可选
        Facts:       facts,
        Reflection:  "",  // 可由反思 Node 后续填充
        Importance:  importance,
        TopicBound:  topicBound,
    }
    
    // 7. 持久化到私有记忆层
    return wp.privateStore.SaveEpisode(ctx, state.AgentID, state.TopicID, episode)
}
```

**重要性评分维度**:
- 用户显式关注/确认 (+0.4)
- 涉及系统状态变更 (+0.3)
- 包含错误/异常 (+0.2)
- 包含关键决策 (+0.25)
- 被后续步骤引用 (+0.15)
- 时间衰减因子: exp(-λ * hours_ago)

### 4.3 检索与评分器 (SearchScorer)

**多信号相关性评分**:
```go
type RelevanceScore struct {
    SemanticSim    float64  // 语义向量相似度 (0.4 权重)
    EntityOverlap  float64  // 实体重叠度 (0.25 权重)
    TemporalDecay  float64  // 时间衰减 (0.15 权重)
    CausalChain    float64  // 因果链匹配 (0.2 权重)
    FinalScore     float64  // 加权总和
}
```

**检索流程**:
1. 将当前任务文本向量化
2. 对私有记忆执行向量相似搜索 (Top-N 候选)
3. 对候选记录计算多信号评分
4. 按 FinalScore 降序排列
5. 返回评分结果给 Assembler

### 4.4 上下文构建器 (Assembler)

**核心算法: 配额分配制**
```go
func (a *Assembler) BuildContext(ctx context.Context, req *BuildRequest) (*ContextPack, error) {
    // 1. 基础信息
    topicSummary := a.workspace.GetTopicSummary(req.TopicID)
    sharedState := a.workspace.GetFilteredOutputs(req.TopicID, req.AgentID, req.DependsOn)
    
    // 2. 检索私有记忆
    candidates := a.searcher.Search(ctx, req.AgentID, req.TopicID, req.TaskQuery)
    
    // 3. 全局知识检索 (Eino Retriever)
    globalDocs := a.globalKB.Retrieve(ctx, req.TaskQuery, retriever.WithTopK(3))
    
    // 4. 配额分配 (以 128k 上下文为例)
    budget := &TokenBudget{
        SystemRole:     2048,
        TopicGlobal:    4096,   // 话题全局摘要
        SharedState:    8192,   // 相关共享状态
        GlobalKB:       4096,   // 全局知识
        PrivateMemory:  8192,   // 私有记忆 (主导话题 ≥70%)
        TaskQuery:      2048,
        Reserve:        4096,   // 响应预留
    }
    
    // 5. 按相关性裁剪私有记忆
    selected := a.allocateByRelevance(candidates, budget.PrivateMemory)
    
    // 6. 组装为 Eino Message 格式
    messages := []*schema.Message{
        {Role: schema.System, Content: buildSystemPrompt(topicSummary, globalDocs)},
        {Role: schema.User, Content: buildSharedPrompt(sharedState)},
        {Role: schema.User, Content: buildMemoryPrompt(selected)},
        {Role: schema.User, Content: req.TaskQuery},
    }
    
    return &ContextPack{Messages: messages, TokenBudget: budget}, nil
}
```

**裁剪规则**:
- 主导话题 (FinalScore > 0.7): 保留详细摘要，分配 ≥70% 私有记忆配额
- 中等相关 (0.3 < FinalScore ≤ 0.7): 仅保留一句一句话摘要
- 低相关 (FinalScore ≤ 0.3): 完全丢弃
- 主动检索: Agent 可输出 "我需要回忆..."，系统据此二次检索注入

### 4.5 遗忘与压缩器 (Compressor)

**压缩策略 (分层)**:
```
Level 0 (原始): 完整 Episode 含 FullObservation
Level 1 (标准): Episode 保留 Summary + Facts, 去除 FullObservation
Level 2 (精简): 仅保留 Summary 一句话 + Importance + 关键 Facts
Level 3 (标记): 仅保留索引标记 (存在性记录，无内容)
```

**压缩触发条件**:
- 私有记忆条数超过阈值 (如 100 条/话题)
- 单话题总 Token 超过预算
- 话题结束归档时

**压缩算法**:
```go
func (c *Compressor) Compress(ctx context.Context, agentID, topicID string) error {
    episodes := c.store.GetEpisodes(ctx, agentID, topicID)
    
    // 按重要性 + 时间排序
    sorted := sortByImportanceAndTime(episodes)
    
    for _, ep := range sorted {
        if ep.Importance > 0.8 && ep.Age < 24h {
            continue // 高重要且新鲜: 不压缩
        }
        if ep.Importance > 0.5 {
            ep.CompressTo(Level1)
        } else if ep.Importance > 0.2 {
            ep.CompressTo(Level2)
        } else if ep.Age > 7*24h {
            ep.CompressTo(Level3)
        }
    }
    
    return c.store.BatchUpdate(ctx, sorted)
}
```

**全局知识库淘汰**:
- 访问频率低于阈值且超过 90 天: 降权
- 超过 180 天无访问: 归档 (archived=true)
- 归档超过 1 年: 物理删除

### 4.6 快照管理器 (SnapshotMgr)

**切入流程**:
1. 从 Redis 获取 `snapshot:{agent_id}:{topic_id}`
2. 若不存在，初始化空快照
3. 从工作区拉取与 `depends_on` 相关的上游输出
4. 组装 `AgentActivateContext`

**切出流程**:
1. 提取最近 K 步关键摘要
2. 提取未解决问题列表
3. 提取本地变量状态
4. 序列化写入 Redis (TTL = 话题活跃期 + 7天)
5. 同时异步写入 PostgreSQL 持久化

---

## 5. Eino Graph 编排设计

### 5.1 Graph 拓扑

```
                    ┌─────────────┐
                    │   Start     │
                    └──────┬──────┘
                           ▼
                    ┌─────────────┐
              ┌────→│   Router    │←────┐
              │     │  (主 Agent)  │     │ (循环)
              │     └──────┬──────┘     │
              │            │            │
              │     ┌──────┴──────┐     │
              │     ▼             ▼     │
              │  ┌─────┐      ┌─────┐   │
              │  │Branch│      │Branch│  │
              │  │Cond A│      │Cond B│  │
              │  └──┬───┘      └──┬───┘  │
              │     ▼             ▼      │
              │ ┌───────┐     ┌───────┐  │
              │ │Agent A│     │Agent B│  │
              │ │ Node  │     │ Node  │  │
              │ └───┬───┘     └───┬───┘  │
              │     │             │      │
              │     └──────┬──────┘      │
              │            ▼              │
              │     ┌─────────────┐       │
              │     │  Validator  │       │
              │     │   Node      │       │
              │     └──────┬──────┘       │
              │            ▼               │
              │     ┌─────────────┐       │
              └─────│  Workspace  │───────┘
                    │  Updater    │
                    └──────┬──────┘
                           ▼
                    ┌─────────────┐
                    │   Sinker    │
                    │  (归档/结束) │
                    └─────────────┘
```

### 5.2 State 定义

```go
type GraphState struct {
    // 话题级 (全图共享)
    TopicID       string                 `json:"topic_id"`
    TopicGoal     string                 `json:"topic_goal"`
    Constraints   map[string]string      `json:"constraints"`
    EventQueue    []*Event               `json:"event_queue"`
    
    // Agent 级 (当前激活)
    CurrentAgent  string                 `json:"current_agent"`
    AgentOutputs  map[string]*AgentOutput `json:"agent_outputs"` // agent_id -> 最新输出
    
    // 记忆引用 (不存实际内容)
    SnapshotRefs  map[string]string      `json:"snapshot_refs"` // agent_id -> snapshot_key
    
    // 控制信号
    NextAction    ActionType             `json:"next_action"`   // Continue, Switch, Escalate, Finish
    TargetAgent   string                 `json:"target_agent"`  // Switch 目标
    Reason        string                 `json:"reason"`
}
```

### 5.3 Router Node (主 Agent)

```go
func RouterNode(ctx context.Context, state *GraphState) (*GraphState, error) {
    // 1. 扫描事件队列
    events := workspace.PollEvents(state.TopicID)
    
    // 2. 决定下一步
    switch {
    case len(events) > 0 && events[0].Type == Escalation:
        state.NextAction = Escalate
        state.Reason = events[0].Payload["reason"]
        
    case state.CurrentAgent == "":
        // 初始路由: 根据话题目标匹配 Agent
        state.NextAction = Switch
        state.TargetAgent = registry.MatchAgent(state.TopicGoal)
        
    case hasCrossModifyEvent(events, state.CurrentAgent):
        // 需要切换 Agent 处理交叉修改
        state.NextAction = Switch
        state.TargetAgent = findTargetFromEvent(events)
        
    case agentTaskComplete(state):
        // 当前 Agent 完成，检查是否全部完成
        if allAgentsComplete(state) {
            state.NextAction = Finish
        } else {
            state.NextAction = Switch
            state.TargetAgent = findNextAgent(state)
        }
        
    default:
        // 当前 Agent 继续执行
        state.NextAction = Continue
    }
    
    return state, nil
}
```

### 5.4 Branch 条件

```go
branch := compose.NewGraphBranch(func(ctx context.Context, state *GraphState) (string, error) {
    return string(state.NextAction), nil
}, map[string]bool{
    string(Continue): true,
    string(Switch):   true,
    string(Escalate): true,
    string(Finish):   true,
})

graph.AddBranch("Router", branch)
graph.AddEdge("Router", "AgentExecutor", string(Continue))
graph.AddEdge("Router", "AgentExecutor", string(Switch))   // 同节点，不同上下文
graph.AddEdge("Router", "EscalationHandler", string(Escalate))
graph.AddEdge("Router", "Sinker", string(Finish))
```

### 5.5 Agent Node 封装

```go
type AgentNode struct {
    AgentID      string
    ChatModel    model.ChatModel        // Eino ChatModel
    Tools        []tool.BaseTool        // Eino Tool 列表
    Assembler    *Assembler             // 上下文构建器
    SnapshotMgr  *SnapshotMgr           // 快照管理
}

func (n *AgentNode) Invoke(ctx context.Context, state *GraphState) (*GraphState, error) {
    // 1. 加载快照
    snapshot := n.SnapshotMgr.Load(ctx, n.AgentID, state.TopicID)
    
    // 2. 构建上下文
    ctxPack := n.Assembler.BuildContext(ctx, &BuildRequest{
        AgentID:    n.AgentID,
        TopicID:    state.TopicID,
        TaskQuery:  state.TopicGoal,
        Snapshot:   snapshot,
        DependsOn:  registry.GetDependencies(n.AgentID),
    })
    
    // 3. 注入工具
    messages := append(ctxPack.Messages, &schema.Message{
        Role: schema.User,
        Content: "可用工具: " + formatTools(n.Tools),
    })
    
    // 4. 调用 Eino ChatModel
    resp, err := n.ChatModel.Generate(ctx, messages)
    if err != nil {
        return nil, err
    }
    
    // 5. 处理工具调用 (Eino 自动处理 Tool Call 循环)
    
    // 6. 更新状态
    state.AgentOutputs[n.AgentID] = &AgentOutput{
        AgentID: n.AgentID,
        Summary: extractSummary(resp),
        Version: getNextVersion(state, n.AgentID),
    }
    
    return state, nil
}
```

### 5.6 Callback 挂载 (记忆写入)

```go
func main() {
    graph := compose.NewGraph[*GraphState](&compose.GraphConfig[*GraphState]{
        StateType: reflect.TypeOf(&GraphState{}),
    })
    
    // 注册全局 Callback
    handler := &MemoryCallbackHandler{
        writeProcessor: NewWriteProcessor(privateStore, summarizer, scorer),
        snapshotMgr:    snapshotMgr,
    }
    
    // Eino 支持全局 callback 注册
    callback.RegisterGlobalHandler(handler)
    
    // 编译并运行
    runnable, err := graph.Compile(ctx)
    if err != nil {
        log.Fatal(err)
    }
    
    result, err := runnable.Invoke(ctx, initialState)
}

// MemoryCallbackHandler 实现
type MemoryCallbackHandler struct {
    writeProcessor *WriteProcessor
    snapshotMgr    *SnapshotMgr
}

func (h *MemoryCallbackHandler) OnStart(ctx context.Context, info *callback.RunInfo, input *callback.CallbackInput) context.Context {
    return ctx
}

func (h *MemoryCallbackHandler) OnEnd(ctx context.Context, info *callback.RunInfo, output *callback.CallbackOutput) context.Context {
    // 仅在 Agent Node 结束时触发
    if info.NodeType == callback.NodeTypeAgent {
        go h.writeProcessor.Process(ctx, info, output.State.(*GraphState))
        
        // 同时触发快照保存
        go h.snapshotMgr.SaveSnapshot(ctx, info.AgentID, output.State.(*GraphState))
    }
    return ctx
}

func (h *MemoryCallbackHandler) OnError(ctx context.Context, info *callback.RunInfo, err error) context.Context {
    // 错误 Episode 记录
    return ctx
}
```

---

## 6. 关键机制设计 (优化版)

### 6.1 话题块与模块帧隔离

**模块 (Module)**: 第一级隔离单元，每个子 Agent 绑定模块 ID。
**话题 (Topic)**: 跨模块协作单元，多模块可共同参与。

**隔离规则**:
- 默认按模块隔离，内部日志不混合
- 跨模块交换必须通过话题工作区 `AgentOutput`
- Eino State 中 `CurrentAgent` 切换时，前一 Agent 的完整中间状态不进入下一 Agent 上下文

### 6.2 相关性判断与上下文配比 (优化)

**话题分割算法**:
- TextTiling: 基于词汇链的文本切分
- 语义边界检测: 相邻段落向量相似度骤降点
- Agent 边界: 每次 Agent 切换天然构成话题边界

**配额分配算法 (动态)**:
```go
func allocateBudget(totalTokens int, relevanceMap map[string]float64) map[string]int {
    budget := make(map[string]int)
    
    // 主导话题识别
    dominant := findDominant(relevanceMap) // score > 0.7
    
    if dominant != "" {
        budget[dominant] = int(float64(totalTokens) * 0.7)
        remaining := totalTokens - budget[dominant]
        
        // 其余按相关性比例分配
        for topic, score := range relevanceMap {
            if topic == dominant { continue }
            if score > 0.3 {
                budget[topic] = int(float64(remaining) * score / sumScores(relevanceMap, dominant))
            } else {
                budget[topic] = 0 // 丢弃
            }
        }
    }
    
    return budget
}
```

### 6.3 模块切换与快照机制 (Eino 集成)

```
切换前 (Agent A 切出):
┌─────────────────────────────────────┐
│ 1. Eino Node A OnEnd Callback 触发   │
│ 2. 提取最近 K 步关键摘要              │
│ 3. 提取未解决问题列表                 │
│ 4. 保存本地变量状态                   │
│ 5. Agent A 调用 publish() -> 工作区   │
│ 6. 序列化快照 -> Redis + PostgreSQL   │
└─────────────────────────────────────┘
           ↓
┌─────────────────────────────────────┐
│ Router Node 决策: Switch to Agent B  │
└─────────────────────────────────────┘
           ↓
切换后 (Agent B 切入):
┌─────────────────────────────────────┐
│ 1. Eino Node B OnStart Callback 触发 │
│ 2. 从 Redis 加载 Agent B 上次快照     │
│ 3. 从工作区拉取与 B 依赖相关的上游输出 │
│ 4. Assembler 构建 B 专用上下文        │
│ 5. 注入任务指令 -> ChatModel          │
└─────────────────────────────────────┘
```

**交叉修改事件传递**:
- 不将修改请求放入对方上下文
- 通过工作区 `Event` 对象传递
- 目标 Agent 切入时，Assembler 将相关 Event 注入 `SharedState` 部分

### 6.4 上下文纯净性保障

每个 Agent 输入上下文固定四段式:
```
[System]      角色定义 + 全局约束
[TopicGlobal] 话题目标 + 当前状态摘要
[SharedState] 与本 Agent 相关的上游输出 + 待处理 Event
[Private]     私有记忆快照 (最近 K 步 + 未解决问题)
[Task]        当前具体任务指令
```

**严禁项**:
- 其他 Agent 完整思考链
- 重试日志、调试信息
- 与当前任务无关的历史对话

**推送式更新**:
- 工作区数据变更时，主 Agent Router 主动识别受影响的下游 Agent
- 在下游 Agent 下次切入时，变更摘要自动注入 SharedState
- 不依赖 Agent 被动发现

**上下文验证回路**:
```go
func ValidatorNode(ctx context.Context, state *GraphState) (*GraphState, error) {
    output := state.AgentOutputs[state.CurrentAgent]
    
    // 结构验证
    if !validateSchema(output) {
        return nil, fmt.Errorf("output schema invalid")
    }
    
    // 一致性检查
    if conflicts := checkConflicts(output, state); len(conflicts) > 0 {
        // 退回 Agent 要求修正，或标记为 Event 仲裁
        state.EventQueue = append(state.EventQueue, newEscalationEvent(conflicts))
    }
    
    // 完整性检查
    if !checkCompleteness(output, state.TopicGoal) {
        state.NextAction = Continue // 让当前 Agent 继续完善
    }
    
    return state, nil
}
```

### 6.5 长期记忆与遗忘 (优化)

**话题结束归档**:
```go
func ArchiveTopic(ctx context.Context, topicID string) error {
    // 1. 归档共享工作区
    workspaceData := workspace.Export(topicID)
    
    // 2. 生成话题级摘要 (轻量模型)
    topicSummary := summarizer.SummarizeTopic(workspaceData)
    
    // 3. 写入全局知识库
    kb.Save(ctx, &KnowledgeRecord{
        Type:      "topic_archive",
        TopicID:   topicID,
        Content:   topicSummary,
        Embedding: embedder.Embed(topicSummary),
        Meta: map[string]any{
            "agent_outputs": workspaceData.Outputs,
            "decisions":     workspaceData.Decisions,
        },
    })
    
    // 4. 清理 Redis (保留 7 天后删除)
    workspace.SetTTL(topicID, 7*24*time.Hour)
    
    return nil
}
```

**私有记忆压缩管线**:
```
原始 Episode → [规则摘要] → Level 1 → [模型压缩] → Level 2 → [归档标记] → Level 3
                ↑触发条件:               ↑触发条件:              ↑触发条件:
                100+ 条 / 超 Token       30+ Level1 / 过期      系统标记 / 过期
```

---

## 7. 实现路线图

### Phase 1: 基础设施 (Week 1-2)

**数据层**:
- [ ] PostgreSQL 表创建 (`agent_private_memory`, `global_knowledge`, `agent_snapshots`)
- [ ] Redis Key 设计实现
- [ ] pgvector / 独立向量数据库部署

**Eino 集成**:
- [ ] 定义 `GraphState` 结构体
- [ ] 搭建基础 Graph 骨架 (Router + Branch)
- [ ] 实现空的 Agent Node 模板
- [ ] 注册 Callback Handler 框架

### Phase 2: 记忆核心 (Week 3-4)

**写入管线**:
- [ ] 实现 `WriteProcessor` (摘要生成、事实提取、重要性评分)
- [ ] 实现话题边界检测 (规则版)
- [ ] PostgreSQL 写入器

**检索管线**:
- [ ] 嵌入模型接入 (Eino `embedding.Embedder`)
- [ ] 向量检索实现 (pgvector / Milvus)
- [ ] 多信号评分器

**上下文构建器**:
- [ ] `Assembler.BuildContext` 完整实现
- [ ] Token 预算分配算法
- [ ] 四段式 Prompt 模板

### Phase 3: 编排与快照 (Week 5-6)

**主 Agent 路由**:
- [ ] Agent 能力注册表 (支持语义搜索)
- [ ] Router Node 决策逻辑
- [ ] Event 队列处理

**快照机制**:
- [ ] `SnapshotManager` 切入/切出
- [ ] Redis 序列化/反序列化
- [ ] 交叉修改 Event 传递

**验证回路**:
- [ ] Validator Node
- [ ] 结构/一致性/完整性检查

### Phase 4: 压缩与优化 (Week 7-8)

**压缩器**:
- [ ] 三级压缩策略实现
- [ ] 定期压缩任务 (Cron / Timer)
- [ ] 全局知识库淘汰策略

**监控与自愈**:
- [ ] Agent 切换频率监控
- [ ] "震荡纠缠"检测算法
- [ ] 自动仲裁 / 人工升级

### Phase 5: TUI 观测台与集成 (Week 9-10)

**终端 TUI (bubbletea)**:
- [ ] 五面板实时观测台 (`cmd/memory-console`)
- [ ] SSE 实时数据推送管道
- [ ] Graph 执行流时序可视化
- [ ] Agent 记忆详情查看器 (Page 弹窗)
- [ ] 工作区 Event 交互式操作
- [ ] 记忆检索调试面板

**集成测试**:
- [ ] 端到端多 Agent 协作场景
- [ ] 长话题记忆压力测试
- [ ] 上下文纯净性验证

---

## 8. 典型场景推演 (Eino Graph 视角)

### 场景: 修复商城主页穿模 Bug

```
User: "商城主页有穿模，修复它。"
   │
   ▼
┌─────────────────────────────────────────────────────────────────────┐
│ Eino Graph 执行流                                                    │
├─────────────────────────────────────────────────────────────────────┤
│                                                                     │
│  [Start]                                                            │
│     │ TopicGoal="修复商城主页穿模"                                    │
│     │ TopicID=T-fix-overlap                                         │
│     ▼                                                               │
│  [Router] --初始路由--> registry.Match("修复商城主页穿模")            │
│     │ 匹配到 ui_agent_homepage                                       │
│     │ NextAction=Switch, TargetAgent=ui_agent_homepage              │
│     ▼                                                               │
│  [Agent Node: ui_agent_homepage]                                    │
│     │ Assembler 构建上下文:                                          │
│     │   - System: 前端修复专家角色                                    │
│     │   - TopicGlobal: T-fix-overlap 目标摘要                         │
│     │   - SharedState: 最近首页相关 commit 摘要 (从 KB 检索)           │
│     │   - Private: 空 (首次激活)                                      │
│     │   - Task: 分析、定位、修复穿模问题                               │
│     │                                                               │
│     │ Agent 执行 → 发现根因: cart_agent 错误修改全局导航栏高度          │
│     │                                                               │
│     │ OnEnd Callback 触发:                                           │
│     │   - WriteProcessor: 保存 Episode (分析证据、根因定位)            │
│     │   - SnapshotMgr: 保存快照 (关键证据、未解决: 需回滚)             │
│     │   - publish() -> 工作区 Event: CrossModify                      │
│     │       {Type: CrossModify, Source: ui_agent_homepage,           │
│     │        Target: cart_agent, Payload: {action:"rollback_nav_height"}}│
│     ▼                                                               │
│  [Validator] --验证通过--> output 结构合法                           │
│     ▼                                                               │
│  [Workspace Updater] --Event 入队--> Redis Stream                    │
│     ▼                                                               │
│  [Router] --扫描 Event--> 发现待处理 CrossModify                     │
│     │ NextAction=Switch, TargetAgent=cart_agent                     │
│     ▼                                                               │
│  [Agent Node: cart_agent]                                           │
│     │ Assembler 构建上下文:                                          │
│     │   - System: 购物车模块专家角色                                  │
│     │   - TopicGlobal: T-fix-overlap 目标摘要                         │
│     │   - SharedState: Event{回滚请求} + cart_agent 相关 commit 日志   │
│     │   - Private: cart_agent 上次快照 (如有)                         │
│     │   - Task: 回滚导航栏高度修改                                     │
│     │                                                               │
│     │ Agent 执行 → 完成回滚                                           │
│     │                                                               │
│     │ OnEnd Callback 触发:                                           │
│     │   - WriteProcessor: 保存 Episode (回滚操作)                      │
│     │   - SnapshotMgr: 保存快照                                      │
│     │   - publish() -> 工作区: AgentOutput{cart_agent, v1, "已回滚"}   │
│     ▼                                                               │
│  [Validator]                                                        │
│     ▼                                                               │
│  [Router] --ui_agent_homepage 待验证-->                              │
│     │ NextAction=Switch, TargetAgent=ui_agent_homepage              │
│     ▼                                                               │
│  [Agent Node: ui_agent_homepage]                                    │
│     │ Assembler 构建上下文:                                          │
│     │   - Private: 上次快照 (含分析证据) 加载成功                     │
│     │   - SharedState: "导航栏高度已恢复" + 相关 commit                │
│     │   - Task: 验证修复                                              │
│     │                                                               │
│     │ Agent 执行 → 验证通过                                           │
│     │                                                               │
│     │ OnEnd Callback: 保存 Episode, Snapshot, publish 完成输出        │
│     ▼                                                               │
│  [Validator]                                                        │
│     ▼                                                               │
│  [Router] --全部完成--> NextAction=Finish                            │
│     ▼                                                               │
│  [Sinker]                                                           │
│     │ - 归档话题到全局知识库                                          │
│     │ - 生成经验摘要: "购物车导航栏高度冲突检测"                        │
│     │ - 清理 Redis (TTL 7天)                                         │
│     ▼                                                               │
│  [End]                                                              │
│                                                                     │
└─────────────────────────────────────────────────────────────────────┘
```

**上下文纯净性验证点**:
- `ui_agent_homepage` 从未看到 `cart_agent` 的思考链
- `cart_agent` 只看到回滚 Event，没看到 ui_agent 的完整分析过程
- 双方上下文始终紧凑，无历史污染

---

## 9. Eino 接口适配清单

| 自定义组件 | 需实现的 Eino 接口 | 说明 |
|-----------|------------------|------|
| GlobalKnowledgeRetriever | `retriever.Retriever` | `Retrieve(ctx, query, opts...) -> []*schema.Document` |
| MemoryCallbackHandler | `callback.Handler` | `OnStart`, `OnEnd`, `OnError` |
| AgentNode | `compose.GraphNode` | `Invoke(ctx, state) -> (newState, error)` |
| RouterNode | `compose.GraphNode` | 返回带 NextAction 的 state |
| 嵌入服务 | `embedding.Embedder` | 文本向量化 |
| 工具集 | `tool.BaseTool` | Agent 内部工具统一接口 |

---

## 10. 技术栈

| 层级 | 技术选型 |
|-----|---------|
| Agent 框架 | [Eino](https://github.com/cloudwego/eino) (Go) |
| 语言 | Go 1.22+ |
| 关系数据库 | PostgreSQL 15+ (pgvector 扩展) |
| 缓存 / 消息 | Redis 7+ (Stream 用于 Event 队列) |
| 向量检索 | pgvector / Milvus 2.3+ |
| 嵌入模型 | all-MiniLM-L6-v2 / BGE 系列 |
| 前端 (Web) | Vue 3 + TypeScript (可选) |
| 终端 TUI | bubbletea + lipgloss + bubbles (Go) |
| 微服务框架 | Kratos (可选，如需独立部署) |
| 可观测性 | Prometheus + Grafana / Eino 内置 Callback Metrics |

---

## 11. 附录: 核心数据结构

### 11.1 Episode

```go
type Episode struct {
    StepID             string         `json:"step_id"`
    Timestamp          time.Time      `json:"timestamp"`
    Action             string         `json:"action"`              // Agent 执行的动作
    ObservationSummary string         `json:"observation_summary"`  // 观察摘要
    FullObservation    string         `json:"full_observation,omitempty"`
    Facts              []string       `json:"facts"`               // 提取的关键事实
    Reflection         string         `json:"reflection,omitempty"` // 反思
    Importance         float64        `json:"importance"`          // 0-1
    TopicBound         bool           `json:"topic_bound"`         // 是否为话题边界
    ToolCalls          []ToolCall     `json:"tool_calls,omitempty"`
}

type ToolCall struct {
    Name      string `json:"name"`
    Input     string `json:"input"`
    Output    string `json:"output"`
    Duration  int64  `json:"duration_ms"`
}
```

### 11.2 记忆层级枚举

```go
type CompressionLevel int

const (
    LevelRaw       CompressionLevel = iota // 完整原始记录
    LevelStandard                           // 摘要 + Facts，去除 FullObservation
    LevelCompact                            // 仅保留 Summary 一句话
    LevelMarker                             // 仅存在性标记
)
```

### 11.3 TokenBudget

```go
type TokenBudget struct {
    SystemRole     int `json:"system_role"`
    TopicGlobal    int `json:"topic_global"`
    SharedState    int `json:"shared_state"`
    GlobalKB       int `json:"global_kb"`
    PrivateMemory  int `json:"private_memory"`
    TaskQuery      int `json:"task_query"`
    Reserve        int `json:"reserve"`
}

func (b *TokenBudget) Total() int {
    return b.SystemRole + b.TopicGlobal + b.SharedState + b.GlobalKB + b.PrivateMemory + b.TaskQuery + b.Reserve
}
```

---

## 12. 终端 TUI 实时观测台 (Memory Console)

### 12.1 设计目标

构建一个**开发者专用终端观测台**，实时可视化多 Agent 系统的记忆状态与 Graph 执行流：
- **零配置启动**: 独立二进制，直接连接后端 SSE 端点
- **实时性**: 端到端延迟 < 500ms (SSE 长连接推送)
- **交互性**: 键盘驱动，支持查看详情、手动检索、标记事件
- **信息密度**: 单屏展示 Graph 流、Agent 状态、Event 队列、记忆详情

### 12.2 界面布局

```
┌─ Memory Console ── Topic: T-fix-overlap ── Uptime: 12m34s ───────┐
│                                                                   │
│ ┌─ Agents ───────────┐ ┌─ Graph Execution Flow ───────────────┐  │
│ │ ● ui_homepage      │ │                                    │  │
│ │   [ACTIVE] Step#4  │ │  [Start]                           │  │
│ │ ○ cart_agent       │ │    ↓                               │  │
│ │   [WAITING] v1     │ │  [Router] → Switch(ui_homepage)    │  │
│ │ ○ order_agent      │ │    ↓                               │  │
│ │   [IDLE]           │ │  [ui_homepage] ●─── 2.3s           │  │
│ │                    │ │    ↓ [publish CrossModify]         │  │
│ │                    │ │  [Router] → Switch(cart_agent)     │  │
│ │                    │ │    ↓                               │  │
│ │                    │ │  [cart_agent] ●─── 1.1s            │  │
│ │                    │ │    ↓ [publish Output v1]           │  │
│ │                    │ │  [Router] → ...                    │  │
│ └────────────────────┘ └────────────────────────────────────┘  │
│                                                                   │
│ ┌─ Workspace Events ───────────────┐ ┌─ Memory Stats ─────────┐  │
│ │ ! CrossModify (P0)               │ │ Private Episodes: 12   │  │
│ │   src:ui_homepage → tgt:cart     │ │ Compressed L1:  4      │  │
│ │   Payload: rollback_nav_height   │ │ Compressed L2:  1      │  │
│ │   [Enter] to resolve             │ │ Global KB Hits: 3      │  │
│ │                                  │ │ Token Budget Used: 62% │  │
│ │ ✓ Output v1 (cart_agent)         │ │                        │  │
│ │   "已回滚导航栏高度"              │ │                        │  │
│ └──────────────────────────────────┘ └────────────────────────┘  │
│                                                                   │
│ ┌─ Episode Detail (F3 to toggle) ──────────────────────────────┐  │
│ │ Step: ui_homepage#4  |  Importance: 0.92  |  14:32:01        │  │
│ │ Summary: 定位穿模根因为购物车Agent修改全局导航栏高度...       │  │
│ │ Facts: [nav_height=64→48], [cart_agent_commit=abc123]       │  │
│ │ ToolCalls: GitDiff(1.2s) → ReadFile(0.3s)                   │  │
│ └──────────────────────────────────────────────────────────────┘  │
│                                                                   │
│ [Q]uit  [Tab]Switch Panel  [Enter]Action  [R]etrieve  [P]ause   │
└───────────────────────────────────────────────────────────────────┘
```

**面板说明**:
| 面板 | 位置 | 内容 | 交互 |
|-----|------|------|------|
| Agents | 左上 | Agent 列表、状态、当前步 | ↑↓ 切换, Enter 查看详情 |
| Graph Flow | 右上 | Graph Node 执行时序 | 自动滚动, ←→ 缩放时间轴 |
| Events | 左下 | 工作区 Event 队列 | ↑↓ 选择, Enter 标记处理 |
| Stats | 右下 | 记忆统计、Token 预算 | 只读 |
| Episode | 底部 | 选中 Episode 详情 | F3 展开/折叠 |

### 12.3 数据流与通信协议

```
┌──────────────┐      SSE (text/event-stream)      ┌─────────────────┐
│   TUI Client │ ←─────────────────────────────────│  Memory Server  │
│  (bubbletea) │      events: graph.step,          │   (Go HTTP)     │
│              │              agent.status,         │                 │
│              │              workspace.event,      │   GraphState    │
│              │              episode.new           │      │          │
│              │                                    │      ▼          │
│              │      HTTP POST /api/tui/cmd       │   Broadcaster   │
│              │ ─────────────────────────────────→ │  (fan-out SSE)  │
└──────────────┘      body: {action, payload}      └─────────────────┘
```

**SSE Event 类型**:
```go
type UIEvent struct {
    Type      string    `json:"type"`      // graph.step | agent.status | workspace.event | episode.new | stats.tick
    Timestamp time.Time `json:"timestamp"`
    Payload   any       `json:"payload"`
}

type GraphStepPayload struct {
    NodeName   string        `json:"node_name"`
    AgentID    string        `json:"agent_id,omitempty"`
    Duration   time.Duration `json:"duration"`
    InputSize  int           `json:"input_tokens"`
    OutputSize int           `json:"output_tokens"`
    Action     string        `json:"action,omitempty"` // Switch target / Continue / Finish
}

type AgentStatusPayload struct {
    AgentID    string `json:"agent_id"`
    State      string `json:"state"`      // IDLE | ACTIVE | WAITING | ERROR
    CurrentStep int   `json:"current_step,omitempty"`
    LastOutput string `json:"last_output,omitempty"`
}
```

**HTTP 命令端点 (TUI → Server)**:
- `POST /api/tui/retrieve` — 手动触发记忆检索，返回结果推送 SSE
- `POST /api/tui/event/resolve` — 标记 Event 为已处理
- `POST /api/tui/snapshot/inspect` — 查看某 Agent 快照详情
- `POST /api/tui/graph/pause` / `/resume` — 暂停/恢复 Graph 执行

### 12.4 核心代码框架

**项目结构**:
```
cmd/memory-console/
├── main.go              // 入口: 解析 flag, 初始化 tea.Program
├── model.go             // bubbletea Model 定义
├── update.go            // Update(msg) 消息处理
├── view.go              // View() 渲染逻辑
├── layout.go            // 分屏尺寸计算
├── client/
│   ├── sse_client.go    // SSE 连接管理、重连、解析
│   └── api_client.go    // HTTP 命令客户端
└── components/
    ├── agent_list.go    // Agents 面板组件
    ├── graph_flow.go    // Graph 执行流可视化
    ├── event_queue.go   // Event 队列交互组件
    ├── memory_stats.go  // 统计信息面板
    └── episode_detail.go // Episode 详情页
```

**Model 定义**:
```go
package main

import (
    tea "github.com/charmbracelet/bubbletea"
    "github.com/charmbracelet/lipgloss"
)

// Panel 枚举
type Panel int

const (
    PanelAgents Panel = iota
    PanelGraph
    PanelEvents
    PanelStats
    PanelCount
)

type Model struct {
    width  int
    height int

    // 数据层 (SSE 推送更新)
    agents      []AgentView       // Agent 列表
    graphSteps  []GraphStepView   // Graph 执行步骤
    events      []EventView       // Event 队列
    episodes    map[string]*EpisodeView // agent#step -> 详情
    stats       StatsView         // 统计面板

    // 交互状态
    focusPanel  Panel             // 当前焦点面板
    agentCursor int               // Agents 面板光标
    eventCursor int               // Events 面板光标
    showDetail  bool              // F3 展开 Episode 详情
    paused      bool              // P 暂停

    // 客户端
    sseClient   *client.SSEClient
    apiClient   *client.APIClient

    // 样式
    styles      *Styles
}

type Styles struct {
    Title       lipgloss.Style
    ActiveAgent lipgloss.Style
    WaitingAgent lipgloss.Style
    IdleAgent   lipgloss.Style
    ErrorAgent  lipgloss.Style
    FocusBorder lipgloss.Style
    BlurBorder  lipgloss.Style
    EventHigh   lipgloss.Style
    EventNormal lipgloss.Style
    EventDone   lipgloss.Style
    StepNode    lipgloss.Style
    StepArrow   lipgloss.Style
    HelpBar     lipgloss.Style
}

func NewStyles() *Styles {
    return &Styles{
        Title:       lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7AA2F7")),
        ActiveAgent: lipgloss.NewStyle().Foreground(lipgloss.Color("#9ECE6A")),
        WaitingAgent: lipgloss.NewStyle().Foreground(lipgloss.Color("#E0AF68")),
        IdleAgent:   lipgloss.NewStyle().Foreground(lipgloss.Color("#565F89")),
        ErrorAgent:  lipgloss.NewStyle().Foreground(lipgloss.Color("#F7768E")),
        FocusBorder: lipgloss.NewStyle().BorderStyle(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("#7AA2F7")),
        BlurBorder:  lipgloss.NewStyle().BorderStyle(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("#414868")),
        EventHigh:   lipgloss.NewStyle().Foreground(lipgloss.Color("#F7768E")),
        EventNormal: lipgloss.NewStyle().Foreground(lipgloss.Color("#E0AF68")),
        EventDone:   lipgloss.NewStyle().Foreground(lipgloss.Color("#9ECE6A")),
        StepNode:    lipgloss.NewStyle().Background(lipgloss.Color("#414868")).Foreground(lipgloss.Color("#C0CAF5")).Padding(0, 1),
        StepArrow:   lipgloss.NewStyle().Foreground(lipgloss.Color("#565F89")),
        HelpBar:     lipgloss.NewStyle().Background(lipgloss.Color("#1F2335")).Foreground(lipgloss.Color("#A9B1D6")),
    }
}
```

**消息类型**:
```go
// SSE 收到新数据
type SSEMsg struct {
    Event UIEvent
}

// 窗口大小变化
type ResizeMsg struct {
    Width  int
    Height int
}

// 初始化完成
type InitDoneMsg struct{}

// 连接断开/重连
type ConnStatusMsg struct {
    Connected bool
    Err       error
}
```

**Update 处理**:
```go
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
    switch msg := msg.(type) {
    case tea.KeyMsg:
        switch msg.String() {
        case "q", "ctrl+c":
            return m, tea.Quit
        case "tab":
            m.focusPanel = Panel((int(m.focusPanel) + 1) % int(PanelCount))
        case "shift+tab":
            m.focusPanel = Panel((int(m.focusPanel) - 1 + int(PanelCount)) % int(PanelCount))
        case "up":
            if m.focusPanel == PanelAgents && m.agentCursor > 0 {
                m.agentCursor--
            } else if m.focusPanel == PanelEvents && m.eventCursor > 0 {
                m.eventCursor--
            }
        case "down":
            if m.focusPanel == PanelAgents && m.agentCursor < len(m.agents)-1 {
                m.agentCursor++
            } else if m.focusPanel == PanelEvents && m.eventCursor < len(m.events)-1 {
                m.eventCursor++
            }
        case "enter":
            return m.handleEnter()
        case "f3":
            m.showDetail = !m.showDetail
        case "r":
            return m, m.triggerRetrieve()
        case "p":
            m.paused = !m.paused
            return m, m.sendPauseCmd(m.paused)
        }

    case tea.WindowSizeMsg:
        m.width = msg.Width
        m.height = msg.Height

    case SSEMsg:
        m.applyEvent(msg.Event)

    case ConnStatusMsg:
        if !msg.Connected {
            // 显示断连提示，触发重连
            return m, m.reconnectCmd()
        }
    }

    return m, nil
}

func (m *Model) applyEvent(ev UIEvent) {
    switch ev.Type {
    case "agent.status":
        p := ev.Payload.(AgentStatusPayload)
        for i := range m.agents {
            if m.agents[i].ID == p.AgentID {
                m.agents[i].State = p.State
                m.agents[i].CurrentStep = p.CurrentStep
                m.agents[i].LastOutput = p.LastOutput
                break
            }
        }
    case "graph.step":
        p := ev.Payload.(GraphStepPayload)
        m.graphSteps = append(m.graphSteps, GraphStepView{
            NodeName: p.NodeName,
            AgentID:  p.AgentID,
            Duration: p.Duration,
            Action:   p.Action,
        })
        // 保持最近 50 步
        if len(m.graphSteps) > 50 {
            m.graphSteps = m.graphSteps[len(m.graphSteps)-50:]
        }
    case "workspace.event":
        p := ev.Payload.(EventPayload)
        m.events = append([]EventView{{Event: p}}, m.events...)
    case "episode.new":
        p := ev.Payload.(EpisodePayload)
        key := fmt.Sprintf("%s#%s", p.AgentID, p.StepID)
        m.episodes[key] = &EpisodeView{Episode: p}
    case "stats.tick":
        m.stats = ev.Payload.(StatsView)
    }
}

func (m Model) handleEnter() (tea.Model, tea.Cmd) {
    switch m.focusPanel {
    case PanelEvents:
        if m.eventCursor < len(m.events) {
            ev := m.events[m.eventCursor]
            return m, m.resolveEventCmd(ev.ID)
        }
    case PanelAgents:
        if m.agentCursor < len(m.agents) {
            ag := m.agents[m.agentCursor]
            return m, m.inspectSnapshotCmd(ag.ID)
        }
    }
    return m, nil
}
```

**View 渲染**:
```go
func (m Model) View() string {
    if m.width == 0 || m.height == 0 {
        return "Initializing..."
    }

    // 分屏尺寸计算
    topH := m.height * 3 / 5
    bottomH := m.height - topH - 1 // 留 1 行给 HelpBar
    leftW := m.width / 3
    rightW := m.width - leftW

    // 顶部左: Agents 面板
    agentsPanel := m.renderAgents(leftW, topH)
    // 顶部右: Graph Flow 面板
    graphPanel := m.renderGraphFlow(rightW, topH)
    topRow := lipgloss.JoinHorizontal(lipgloss.Top, agentsPanel, graphPanel)

    // 底部左: Events 面板
    eventsPanel := m.renderEvents(leftW, bottomH)
    // 底部右: Stats 面板
    statsPanel := m.renderStats(rightW, bottomH)
    bottomRow := lipgloss.JoinHorizontal(lipgloss.Top, eventsPanel, statsPanel)

    // Episode Detail (F3 展开时覆盖底部)
    content := lipgloss.JoinVertical(lipgloss.Left, topRow, bottomRow)
    if m.showDetail {
        detail := m.renderEpisodeDetail(m.width, m.height/3)
        content = lipgloss.JoinVertical(lipgloss.Left, topRow, detail, bottomRow)
    }

    // 标题栏
    title := m.styles.Title.Render(fmt.Sprintf(" Memory Console │ Topic: %s │ %s ", m.stats.TopicID, m.connStatus()))
    help := m.styles.HelpBar.Render(" [Q]uit [Tab]Focus [↑↓]Navigate [Enter]Action [F3]Detail [R]etrieve [P]ause ")

    return lipgloss.JoinVertical(lipgloss.Left, title, content, help)
}

func (m Model) renderAgents(w, h int) string {
    var b strings.Builder
    for i, ag := range m.agents {
        cursor := "  "
        if m.focusPanel == PanelAgents && i == m.agentCursor {
            cursor = "▸ "
        }

        var style lipgloss.Style
        switch ag.State {
        case "ACTIVE":
            style = m.styles.ActiveAgent
        case "WAITING":
            style = m.styles.WaitingAgent
        case "ERROR":
            style = m.styles.ErrorAgent
        default:
            style = m.styles.IdleAgent
        }

        line := fmt.Sprintf("%s%s %s (Step#%d)", cursor, ag.State[:1], ag.ID, ag.CurrentStep)
        b.WriteString(style.Render(line) + "\n")
    }

    border := m.styles.BlurBorder
    if m.focusPanel == PanelAgents {
        border = m.styles.FocusBorder
    }
    return border.Width(w).Height(h).Render(b.String())
}

func (m Model) renderGraphFlow(w, h int) string {
    var b strings.Builder
    // 逆向遍历，最新的在底部
    start := len(m.graphSteps) - h + 2
    if start < 0 {
        start = 0
    }

    for i := start; i < len(m.graphSteps); i++ {
        step := m.graphSteps[i]
        node := m.styles.StepNode.Render(step.NodeName)
        arrow := m.styles.StepArrow.Render("→")
        info := fmt.Sprintf(" %s %s", arrow, step.Duration.Round(time.Millisecond))
        if step.Action != "" {
            info += fmt.Sprintf(" [%s]", step.Action)
        }
        b.WriteString(node + info + "\n")
    }

    border := m.styles.BlurBorder
    if m.focusPanel == PanelGraph {
        border = m.styles.FocusBorder
    }
    return border.Width(w).Height(h).Render(b.String())
}

func (m Model) renderEvents(w, h int) string {
    var b strings.Builder
    for i, ev := range m.events {
        if i >= h-2 {
            break
        }
        cursor := "  "
        if m.focusPanel == PanelEvents && i == m.eventCursor {
            cursor = "▸ "
        }

        var style lipgloss.Style
        switch ev.Status {
        case "Pending":
            if ev.Priority >= 8 {
                style = m.styles.EventHigh
            } else {
                style = m.styles.EventNormal
            }
        default:
            style = m.styles.EventDone
        }

        line := fmt.Sprintf("%s%s %s", cursor, ev.Type[:1], ev.Summary)
        b.WriteString(style.Render(line) + "\n")
    }

    border := m.styles.BlurBorder
    if m.focusPanel == PanelEvents {
        border = m.styles.FocusBorder
    }
    return border.Width(w).Height(h).Render(b.String())
}

func (m Model) renderStats(w, h int) string {
    lines := []string{
        fmt.Sprintf("Private Episodes: %d", m.stats.PrivateEpisodes),
        fmt.Sprintf("Compressed L1:    %d", m.stats.CompressedL1),
        fmt.Sprintf("Compressed L2:    %d", m.stats.CompressedL2),
        fmt.Sprintf("Global KB Hits:   %d", m.stats.GlobalKBHits),
        fmt.Sprintf("Token Budget:     %d%%", m.stats.TokenBudgetUsed),
        fmt.Sprintf("Active Agents:    %d/%d", m.stats.ActiveAgents, m.stats.TotalAgents),
    }
    content := strings.Join(lines, "\n")

    border := m.styles.BlurBorder
    if m.focusPanel == PanelStats {
        border = m.styles.FocusBorder
    }
    return border.Width(w).Height(h).Render(content)
}

func (m Model) renderEpisodeDetail(w, h int) string {
    // 根据当前选中 Agent 查找对应 Episode
    key := ""
    if m.focusPanel == PanelAgents && m.agentCursor < len(m.agents) {
        ag := m.agents[m.agentCursor]
        key = fmt.Sprintf("%s#%d", ag.ID, ag.CurrentStep)
    }

    ep, ok := m.episodes[key]
    if !ok {
        return m.styles.BlurBorder.Width(w).Height(h).Render(" No episode selected ")
    }

    lines := []string{
        fmt.Sprintf("Step: %s  |  Importance: %.2f  |  %s", key, ep.Importance, ep.Timestamp.Format("15:04:05")),
        fmt.Sprintf("Summary: %s", truncate(ep.Summary, w-12)),
        fmt.Sprintf("Facts: %v", ep.Facts),
    }
    if len(ep.ToolCalls) > 0 {
        lines = append(lines, "ToolCalls:")
        for _, tc := range ep.ToolCalls {
            lines = append(lines, fmt.Sprintf("  - %s(%s) → %s [%dms]", tc.Name, tc.Input, tc.Output, tc.Duration))
        }
    }

    return m.styles.FocusBorder.Width(w).Height(h).Render(strings.Join(lines, "\n"))
}

func truncate(s string, max int) string {
    if len(s) <= max {
        return s
    }
    return s[:max-3] + "..."
}
```

**SSE 客户端**:
```go
package client

import (
    "bufio"
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "net/http"
    "time"

    tea "github.com/charmbracelet/bubbletea"
)

type SSEClient struct {
    endpoint string
    topicID  string
    client   *http.Client
}

func NewSSEClient(endpoint, topicID string) *SSEClient {
    return &SSEClient{
        endpoint: endpoint,
        topicID:  topicID,
        client:   &http.Client{Timeout: 0}, // SSE 长连接无超时
    }
}

func (c *SSEClient) Subscribe() tea.Cmd {
    return func() tea.Msg {
        url := fmt.Sprintf("%s/api/tui/stream?topic_id=%s", c.endpoint, c.topicID)
        req, err := http.NewRequest("GET", url, nil)
        if err != nil {
            return ConnStatusMsg{Connected: false, Err: err}
        }
        req.Header.Set("Accept", "text/event-stream")
        req.Header.Set("Cache-Control", "no-cache")

        resp, err := c.client.Do(req)
        if err != nil {
            return ConnStatusMsg{Connected: false, Err: err}
        }
        defer resp.Body.Close()

        scanner := bufio.NewScanner(resp.Body)
        var buf bytes.Buffer

        for scanner.Scan() {
            line := scanner.Text()
            if line == "" {
                // 空行表示一个 event 结束
                data := bytes.TrimPrefix(buf.Bytes(), []byte("data: "))
                var ev UIEvent
                if err := json.Unmarshal(data, &ev); err == nil {
                    return SSEMsg{Event: ev} // tea.Cmd 单次返回，需递归调用保持连接
                }
                buf.Reset()
                continue
            }
            if bytes.HasPrefix([]byte(line), []byte("data: ")) {
                buf.WriteString(line)
                buf.WriteByte('\n')
            }
        }

        return ConnStatusMsg{Connected: false, Err: scanner.Err()}
    }
}

// 递归保持 SSE 连接
func (c *SSEClient) RecurrentSubscribe() tea.Cmd {
    return tea.Batch(c.Subscribe(), func() tea.Msg {
        time.Sleep(100 * time.Millisecond)
        return InitDoneMsg{}
    })
}
```

### 12.5 快捷键

| 按键 | 功能 |
|-----|------|
| `Q` / `Ctrl+C` | 退出 |
| `Tab` | 下一面板 |
| `Shift+Tab` | 上一面板 |
| `↑` / `↓` | 面板内导航 |
| `Enter` | 执行面板动作 (Agent 查看快照 / Event 标记处理) |
| `F3` | 展开/折叠 Episode 详情 |
| `R` | 手动触发记忆检索 (弹出输入框) |
| `P` | 暂停 / 恢复 Graph 执行 |
| `G` | 跳转到 Graph Flow 面板底部 (最新) |

### 12.6 部署方式

```bash
# 1. 编译独立二进制
cd cmd/memory-console
go build -o memory-console .

# 2. 启动 (连接本地后端)
./memory-console --endpoint http://localhost:8080 --topic T-fix-overlap

# 3. 在 tmux/screen 中长期运行
./memory-console --endpoint $API_HOST --topic $TOPIC_ID --retry-interval 5s
```

**Flag 说明**:
| Flag | 默认值 | 说明 |
|-----|--------|------|
| `--endpoint` | `http://localhost:8080` | 后端 HTTP/SSE 端点 |
| `--topic` | 必填 | 订阅的话题 ID |
| `--retry-interval` | `5s` | SSE 断连重试间隔 |
| `--refresh-rate` | `30fps` | TUI 刷新率 |

### 12.7 与后端的集成

在 Memory Server 中增加 SSE Broadcaster:

```go
// server/tui_broadcaster.go
type TUIBroadcaster struct {
    mu      sync.RWMutex
    clients map[string][]chan UIEvent // topic_id -> clients
}

func (b *TUIBroadcaster) Subscribe(topicID string) <-chan UIEvent {
    ch := make(chan UIEvent, 100)
    b.mu.Lock()
    b.clients[topicID] = append(b.clients[topicID], ch)
    b.mu.Unlock()
    return ch
}

func (b *TUIBroadcaster) Broadcast(topicID string, ev UIEvent) {
    b.mu.RLock()
    defer b.mu.RUnlock()
    for _, ch := range b.clients[topicID] {
        select {
        case ch <- ev:
        default: // 客户端阻塞则丢弃，防背压
        }
    }
}

// 在 Callback Handler 中埋点
func (h *MemoryCallbackHandler) OnEnd(ctx context.Context, info *callback.RunInfo, output *callback.CallbackOutput) context.Context {
    state := output.State.(*GraphState)
    h.broadcaster.Broadcast(state.TopicID, UIEvent{
        Type: "graph.step",
        Payload: GraphStepPayload{
            NodeName: info.NodeName,
            AgentID:  state.CurrentAgent,
            Duration: info.Duration,
            Action:   string(state.NextAction),
        },
    })
    // ... 原有逻辑
    return ctx
}
```

---

## 13. 变更记录

| 版本 | 日期 | 变更内容 |
|-----|------|---------|
| v1.0 | - | 原始设计文档 |
| v2.0 | 2026-06-15 | 全面 Eino 化: Graph/State/Callback 原生融合; 记忆模块增加配额分配、三级压缩、Eino Retriever 接口; 增加工程级 Schema 定义、实现路线图、Eino 接口适配清单 |
| v2.1 | 2026-06-15 | 增加终端 TUI 观测台完整方案 (bubbletea): 五面板布局、SSE 实时通信、交互式快捷键、核心代码框架、部署方式; 更新技术栈与实现路线图 |

---

> 本文档以 Eino 为技术底座，将记忆管理从"外挂中间件"升级为"与编排框架原生融合的有机层"。所有组件均围绕 Eino 的 Graph/State/Callback/Retriever 模型设计，确保理论设计与代码实现零距离。

