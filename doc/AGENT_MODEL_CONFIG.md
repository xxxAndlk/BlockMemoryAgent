# 多Agent模型配置与层级结构说明

## 1. 当前Agent层级结构 (2层)

```
┌─────────────────────────────────────────────┐
│  Layer 1: 主Agent (Router / 编排者)          │
│  - 职责: 任务路由、状态管理、上下文组装        │
│  - 不执行业务逻辑，只决策下一个激活谁           │
│  - 全局唯一，每个Topic一个Router实例           │
├─────────────────────────────────────────────┤
│  Layer 2: 业务子Agent (Agent Executor)         │
│  - 职责: 执行具体业务逻辑 (UI修复/购物车回滚等) │
│  - 每个Agent绑定一个ModuleID，上下文隔离        │
│  - 多个子Agent，按需动态切换激活               │
└─────────────────────────────────────────────┘
```

### 当前代码中的Agent示例

| AgentID | Name | ModuleID | Keywords | 职责 |
|---------|------|----------|----------|------|
| `ui_agent_homepage` | UI Homepage Agent | `ui` | ui, homepage, frontend, 界面, 首页 | 处理首页UI相关问题 |
| `cart_agent` | Cart Agent | `cart` | cart, shopping, 购物车, 购买 | 处理购物车相关问题 |

### 区分机制

1. **ModuleID 隔离**: 每个Agent绑定一个模块ID（如`ui`/`cart`），私有记忆按`(agent_id, topic_id)`隔离存储
2. **Keyword 路由**: Router通过关键词匹配用户指令到目标Agent（`SimpleRegistry.MatchAgent`）
3. **Workspace 精炼交换**: 子Agent不直接读取其他Agent的私有记忆，只通过`AgentOutput`交换精炼摘要
4. **Snapshot 独立**: 每个Agent有独立的快照（`snapshot:{agent_id}:{topic_id}`），切出/切入互不影响

---

## 2. Eino ChatModel 配置文件设计

每个Agent可独立配置模型参数。主Agent(Router)用轻量模型做决策，业务Agent用重模型做推理。

### 配置文件: `config/models.yaml`

```yaml
# 全局默认模型配置
default:
  provider: openai           # 模型供应商: openai / anthropic / qwen / doubao
  model: gpt-4o
  api_key: ${OPENAI_API_KEY} # 环境变量引用
  base_url: ""
  temperature: 0.7
  max_tokens: 4096
  timeout: 30s

# 主Agent (Router) — 轻量模型，快速决策
agents:
  router:
    provider: openai
    model: gpt-4o-mini       # 轻量，只做路由决策
    temperature: 0.3         # 低温度，确定性高
    max_tokens: 1024
    system_prompt: |
      你是多Agent系统的调度中枢。职责：
      1. 根据用户指令和Agent能力注册表，选择下一个激活的子Agent
      2. 验证子Agent返回结果的结构、一致性、完整性
      3. 处理工作区Event队列，维护话题状态
      4. 检测到Agent间冲突时触发升级

  # 业务Agent: UI模块
  ui_agent_homepage:
    provider: openai
    model: gpt-4o
    temperature: 0.5
    max_tokens: 4096
    system_prompt: |
      你是前端UI专家Agent，负责首页相关的界面问题诊断与修复。
      工作规则：
      - 只关注首页UI层代码（CSS/HTML/JS组件）
      - 发现其他模块（如购物车）引起的UI问题时，通过publish()向工作区提交CrossModify事件
      - 不直接修改其他模块的代码
    tools:
      - name: GitDiff
        enabled: true
      - name: ReadFile
        enabled: true
      - name: CSSInspect
        enabled: true

  # 业务Agent: 购物车模块
  cart_agent:
    provider: openai
    model: gpt-4o
    temperature: 0.5
    max_tokens: 4096
    system_prompt: |
      你是购物车模块专家Agent，负责购物车逻辑与数据管理。
      工作规则：
      - 只关注购物车模块代码
      - 修改全局样式/变量时，必须通过publish()通知相关UI Agent
      - 回滚操作需验证影响范围
    tools:
      - name: GitDiff
        enabled: true
      - name: ReadFile
        enabled: true
      - name: DBQuery
        enabled: false

  # 业务Agent: 订单模块 (示例)
  order_agent:
    provider: anthropic
    model: claude-sonnet-4-6
    api_key: ${ANTHROPIC_API_KEY}
    temperature: 0.6
    max_tokens: 8192
    system_prompt: |
      你是订单处理专家Agent...

# 记忆控制器嵌入模型 (用于摘要/检索/评分)
embedding:
  provider: openai
  model: text-embedding-3-small
  api_key: ${OPENAI_API_KEY}
  dimensions: 768

# 重要性评分轻量模型 (可选，也可规则实现)
scorer:
  provider: openai
  model: gpt-4o-mini
  temperature: 0.0
  max_tokens: 256
```

---

## 3. Eino ChatModel 接入代码

### 3.1 配置加载器

```go
// pkg/config/model_config.go
package config

import (
    "os"
    "time"

    "gopkg.in/yaml.v3"
)

// ModelConfig 模型配置
type ModelConfig struct {
    Default  DefaultConfig            `yaml:"default"`
    Agents   map[string]AgentModelConfig `yaml:"agents"`
    Embedding EmbeddingConfig           `yaml:"embedding"`
    Scorer    AgentModelConfig          `yaml:"scorer"`
}

// DefaultConfig 全局默认配置
type DefaultConfig struct {
    Provider    string `yaml:"provider"`
    Model       string `yaml:"model"`
    APIKey      string `yaml:"api_key"`
    BaseURL     string `yaml:"base_url"`
    Temperature float64 `yaml:"temperature"`
    MaxTokens   int    `yaml:"max_tokens"`
    Timeout     string `yaml:"timeout"`
}

// AgentModelConfig Agent专用模型配置
type AgentModelConfig struct {
    Provider     string            `yaml:"provider"`
    Model        string            `yaml:"model"`
    APIKey       string            `yaml:"api_key"`
    BaseURL      string            `yaml:"base_url"`
    Temperature  float64           `yaml:"temperature"`
    MaxTokens    int               `yaml:"max_tokens"`
    SystemPrompt string            `yaml:"system_prompt"`
    Tools        map[string]bool   `yaml:"tools"`
}

// EmbeddingConfig 嵌入模型配置
type EmbeddingConfig struct {
    Provider   string `yaml:"provider"`
    Model      string `yaml:"model"`
    APIKey     string `yaml:"api_key"`
    Dimensions int    `yaml:"dimensions"`
}

// LoadModelConfig 从文件加载配置
func LoadModelConfig(path string) (*ModelConfig, error) {
    data, err := os.ReadFile(path)
    if err != nil {
        return nil, err
    }
    var cfg ModelConfig
    if err := yaml.Unmarshal(data, &cfg); err != nil {
        return nil, err
    }
    // 解析环境变量 (如 ${OPENAI_API_KEY})
    cfg.resolveEnvVars()
    return &cfg, nil
}

func (c *ModelConfig) resolveEnvVars() {
    c.Default.APIKey = resolveEnv(c.Default.APIKey)
    for k, v := range c.Agents {
        v.APIKey = resolveEnv(v.APIKey)
        c.Agents[k] = v
    }
    c.Embedding.APIKey = resolveEnv(c.Embedding.APIKey)
    c.Scorer.APIKey = resolveEnv(c.Scorer.APIKey)
}

func resolveEnv(s string) string {
    if len(s) > 3 && s[0] == '$' && s[1] == '{' && s[len(s)-1] == '}' {
        return os.Getenv(s[2 : len(s)-1])
    }
    return s
}

// GetAgentConfig 获取指定Agent的模型配置 (继承default)
func (c *ModelConfig) GetAgentConfig(agentID string) AgentModelConfig {
    agentCfg, ok := c.Agents[agentID]
    if !ok {
        // 回退到default
        return AgentModelConfig{
            Provider:    c.Default.Provider,
            Model:       c.Default.Model,
            APIKey:      c.Default.APIKey,
            BaseURL:     c.Default.BaseURL,
            Temperature: c.Default.Temperature,
            MaxTokens:   c.Default.MaxTokens,
        }
    }

    // 继承default的未设置字段
    if agentCfg.Provider == "" {
        agentCfg.Provider = c.Default.Provider
    }
    if agentCfg.Model == "" {
        agentCfg.Model = c.Default.Model
    }
    if agentCfg.APIKey == "" {
        agentCfg.APIKey = c.Default.APIKey
    }
    if agentCfg.BaseURL == "" {
        agentCfg.BaseURL = c.Default.BaseURL
    }
    if agentCfg.Temperature == 0 && agentCfg.Provider == "" {
        agentCfg.Temperature = c.Default.Temperature
    }
    if agentCfg.MaxTokens == 0 {
        agentCfg.MaxTokens = c.Default.MaxTokens
    }
    return agentCfg
}
```

### 3.2 Eino ChatModel 工厂

```go
// internal/model/factory.go
package model

import (
    "context"
    "fmt"

    "github.com/cloudwego/eino/components/model"
    "github.com/cloudwego/eino/compose"
    "github.com/blockmemory/agent/backend/pkg/config"
)

// ChatModelFactory ChatModel 工厂
type ChatModelFactory struct {
    cfg       *config.ModelConfig
    models    map[string]model.ChatModel
}

// NewChatModelFactory 创建工厂
func NewChatModelFactory(cfg *config.ModelConfig) *ChatModelFactory {
    return &ChatModelFactory{
        cfg:    cfg,
        models: make(map[string]model.ChatModel),
    }
}

// GetModel 获取指定Agent的ChatModel
func (f *ChatModelFactory) GetModel(ctx context.Context, agentID string) (model.ChatModel, error) {
    if m, ok := f.models[agentID]; ok {
        return m, nil
    }

    agentCfg := f.cfg.GetAgentConfig(agentID)

    var m model.ChatModel
    var err error

    switch agentCfg.Provider {
    case "openai":
        m, err = f.createOpenAIModel(ctx, agentCfg)
    case "anthropic":
        m, err = f.createAnthropicModel(ctx, agentCfg)
    case "qwen":
        m, err = f.createQwenModel(ctx, agentCfg)
    case "doubao":
        m, err = f.createDoubaoModel(ctx, agentCfg)
    default:
        return nil, fmt.Errorf("unsupported provider: %s", agentCfg.Provider)
    }

    if err != nil {
        return nil, fmt.Errorf("create model for %s: %w", agentID, err)
    }

    f.models[agentID] = m
    return m, nil
}

// createOpenAIModel 创建OpenAI模型
func (f *ChatModelFactory) createOpenAIModel(ctx context.Context, cfg config.AgentModelConfig) (model.ChatModel, error) {
    // Eino OpenAI 适配器
    // import "github.com/cloudwego/eino-ext/components/model/openai"
    // return openai.NewChatModel(ctx, &openai.ChatModelConfig{
    //     APIKey:      cfg.APIKey,
    //     Model:       cfg.Model,
    //     BaseURL:     cfg.BaseURL,
    //     Temperature: &cfg.Temperature,
    //     MaxTokens:   cfg.MaxTokens,
    // })
    return nil, fmt.Errorf("openai model not implemented, need import eino-ext")
}

// createAnthropicModel 创建Anthropic模型
func (f *ChatModelFactory) createAnthropicModel(ctx context.Context, cfg config.AgentModelConfig) (model.ChatModel, error) {
    // Eino Anthropic 适配器
    // import "github.com/cloudwego/eino-ext/components/model/anthropic"
    return nil, fmt.Errorf("anthropic model not implemented, need import eino-ext")
}

// createQwenModel 创建通义千问模型
func (f *ChatModelFactory) createQwenModel(ctx context.Context, cfg config.AgentModelConfig) (model.ChatModel, error) {
    return nil, fmt.Errorf("qwen model not implemented, need import eino-ext")
}

// createDoubaoModel 创建豆包模型
func (f *ChatModelFactory) createDoubaoModel(ctx context.Context, cfg config.AgentModelConfig) (model.ChatModel, error) {
    return nil, fmt.Errorf("doubao model not implemented, need import eino-ext")
}

// GetEmbeddingModel 获取嵌入模型
func (f *ChatModelFactory) GetEmbeddingModel(ctx context.Context) (/* embedding.Embedder */ error) {
    // return openai.NewEmbedder(ctx, &openai.EmbeddingConfig{
    //     APIKey: f.cfg.Embedding.APIKey,
    //     Model:  f.cfg.Embedding.Model,
    // })
    return fmt.Errorf("embedding model not implemented")
}
```

### 3.3 AgentExecutorNode 改造 (接入真实ChatModel)

```go
// internal/graph/agent_node.go (改造后)
package graph

import (
    "context"
    "fmt"

    "github.com/cloudwego/eino/components/model"
    "github.com/cloudwego/eino/schema"
    "github.com/blockmemory/agent/backend/pkg/config"
    "github.com/blockmemory/agent/backend/pkg/types"
)

// ChatModelFactory 模型工厂接口
type ChatModelFactory interface {
    GetModel(ctx context.Context, agentID string) (model.ChatModel, error)
}

// AgentExecutorNode Agent 执行节点 (Eino 集成版)
type AgentExecutorNode struct {
    name         string
    agentID      string
    chatModel    model.ChatModel        // Eino ChatModel
    tools        []tool.BaseTool        // Eino Tool 列表
    assembler    ContextAssembler
    snapshotMgr  SnapshotManager
    workspace    WorkspaceWriter
    modelCfg     config.AgentModelConfig // 该Agent的模型配置
}

// NewAgentExecutorNode 创建 Agent 执行节点
func NewAgentExecutorNode(
    agentID string,
    modelFactory ChatModelFactory,
    assembler ContextAssembler,
    snapshotMgr SnapshotManager,
    workspace WorkspaceWriter,
    cfg config.AgentModelConfig,
) (*AgentExecutorNode, error) {
    ctx := context.Background()
    chatModel, err := modelFactory.GetModel(ctx, agentID)
    if err != nil {
        return nil, fmt.Errorf("get model for %s: %w", agentID, err)
    }

    return &AgentExecutorNode{
        name:        agentID,
        agentID:     agentID,
        chatModel:   chatModel,
        assembler:   assembler,
        snapshotMgr: snapshotMgr,
        workspace:   workspace,
        modelCfg:    cfg,
    }, nil
}

// Invoke 执行 Agent
func (n *AgentExecutorNode) Invoke(ctx context.Context, state *State) (*State, error) {
    // 1. 加载快照
    snapshot, err := n.snapshotMgr.Load(ctx, n.agentID, state.TopicID)
    if err != nil {
        return nil, fmt.Errorf("load snapshot: %w", err)
    }

    // 2. 构建上下文
    ctxPack, err := n.assembler.BuildContext(ctx, &BuildRequest{
        AgentID:   n.agentID,
        TopicID:   state.TopicID,
        TaskQuery: state.TopicGoal,
        Snapshot:  snapshot,
        DependsOn: nil,
    })
    if err != nil {
        return nil, fmt.Errorf("build context: %w", err)
    }

    // 3. 转换为 Eino schema.Message
    messages := convertToEinoMessages(ctxPack.Messages)

    // 4. 注入 System Prompt (从配置)
    if n.modelCfg.SystemPrompt != "" {
        messages = append([]*schema.Message{{
            Role:    schema.System,
            Content: n.modelCfg.SystemPrompt,
        }}, messages...)
    }

    // 5. 注入工具描述
    if len(n.tools) > 0 {
        messages = append(messages, &schema.Message{
            Role:    schema.User,
            Content: formatTools(n.tools),
        })
    }

    // 6. 调用 Eino ChatModel
    resp, err := n.chatModel.Generate(ctx, messages)
    if err != nil {
        return nil, fmt.Errorf("chat model generate: %w", err)
    }

    // 7. 处理 Tool Call (Eino 自动处理循环)
    // 如果 resp 包含 tool_calls，Eino 会自动执行工具并再次调用模型
    // 这里简化展示，实际使用 Eino 的 Tool Node 或 Chain 处理
    result := extractContent(resp)

    // 8. 保存输出到工作区
    output := &types.AgentOutput{
        AgentID:   n.agentID,
        Version:   getNextVersion(state, n.agentID),
        Summary:   result,
        Validated: false,
        Timestamp: time.Now(),
    }
    if err := n.workspace.SaveAgentOutput(ctx, state.TopicID, output); err != nil {
        return nil, fmt.Errorf("save output: %w", err)
    }

    // 9. 更新状态
    state.CurrentAgent = n.agentID
    state.SetAgentOutput(n.agentID, output)

    return state, nil
}

// convertToEinoMessages 转换为 Eino 消息格式
func convertToEinoMessages(msgs []*Message) []*schema.Message {
    var result []*schema.Message
    for _, m := range msgs {
        role := schema.User
        switch m.Role {
        case "system":
            role = schema.System
        case "assistant":
            role = schema.Assistant
        }
        result = append(result, &schema.Message{
            Role:    role,
            Content: m.Content,
        })
    }
    return result
}

// extractContent 提取模型响应内容
func extractContent(resp *schema.Message) string {
    if resp == nil {
        return ""
    }
    return resp.Content
}

// formatTools 格式化工具列表
func formatTools(tools []tool.BaseTool) string {
    var result string
    for _, t := range tools {
        result += fmt.Sprintf("- %s: %s\n", t.Name(), t.Description())
    }
    return result
}
```

### 3.4 main.go 初始化改造

```go
// main.go 关键改造部分
func main() {
    // ... 原有 flag 和存储初始化 ...

    // 加载模型配置
    modelCfg, err := config.LoadModelConfig("config/models.yaml")
    if err != nil {
        log.Fatalf("load model config: %v", err)
    }

    // 初始化模型工厂
    modelFactory := model.NewChatModelFactory(modelCfg)

    // 初始化记忆控制器
    // ... 原有代码 ...

    // Agent 节点工厂 — 每个Agent独立ChatModel
    agentFactory := func(agentID string) (graph.Node, error) {
        agentCfg := modelCfg.GetAgentConfig(agentID)
        // 创建 ContextAssembler
        assembler := memory.NewContextAssembler(redisStore, globalRetriever, pgStore)

        return graph.NewAgentExecutorNode(
            agentID,
            modelFactory,      // 每个Agent从工厂获取自己的ChatModel
            assembler,
            snapshotMgr,
            redisStore,
            agentCfg,          // 传递该Agent的模型配置
        )
    }

    // 构建 Graph — 为每个注册的Agent创建独立Node
    router := graph.NewRouterNode(registry, redisStore)
    validator := graph.NewValidatorNode()
    workspaceNode := graph.NewWorkspaceUpdaterNode(redisStore)
    escalation := graph.NewEscalationHandlerNode()
    sinker := graph.NewSinkerNode(redisStore)

    // 动态构建：为每个Agent注册独立Node
    builder := graph.NewGraphBuilder()
    builder.AddNode(router)
    builder.AddNode(validator)
    builder.AddNode(workspaceNode)
    builder.AddNode(escalation)
    builder.AddNode(sinker)

    for _, agentID := range registry.GetAllAgents() {
        agentNode, err := agentFactory(agentID)
        if err != nil {
            log.Fatalf("create agent %s: %v", agentID, err)
        }
        builder.AddNode(agentNode)
    }

    builder.SetStartNode("Router")
    // ... 添加边 ...

    compiledGraph, err := builder.Compile()
    // ...
}
```

---

## 4. Agent 结构总结

### 2层架构

| 层级 | 组件 | 数量 | 模型配置 | 职责 |
|------|------|------|----------|------|
| **Layer 1** | Router (主Agent) | 1/Topic | 轻量模型 (gpt-4o-mini) | 路由、编排、验证、状态管理 |
| **Layer 2** | Agent Executor | N/Topic | 按模块配置 (gpt-4o/claude) | 业务执行、工具调用、推理 |

### 关键区分点

1. **ModuleID 绑定**: 每个业务Agent绑定一个模块ID（ui/cart/order），私有记忆按模块隔离
2. **独立 ChatModel**: 每个Agent可有独立的模型配置（供应商/模型/Temperature/工具）
3. **独立 SystemPrompt**: 每个Agent在配置文件中有专属系统提示词，定义其角色和边界
4. **独立 ToolSet**: 每个Agent可配置不同的工具权限（如UI Agent有CSSInspect，Cart Agent有DBQuery）
5. **上下文隔离**: 通过 Snapshot + Assembler 四段式构建，确保每个Agent只看到自己的上下文

### 扩展方向

- 如需增加 **Layer 3 (工具Agent/子任务Agent)**，可在 AgentExecutor 内部嵌套 Eino Chain，Chain 内再细分子节点
- 如需 **协调Agent群** (多个Router协作)，可在 Router 层之上增加 Meta-Router 做跨话题调度
