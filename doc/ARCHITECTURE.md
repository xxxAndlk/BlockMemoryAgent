# 三层Agent架构设计

## 架构总览

```
┌─────────────────────────────────────────────────────────────────────┐
│                         Layer 1: MetaAgent                           │
│                    (主Agent / 会话调度器)                              │
│                                                                      │
│  职责:                                                               │
│  - 分析用户目标，识别需要的业务领域                                     │
│  - 为每个领域创建 DomainAgent（会话块Agent）                           │
│  - 监控各 DomainAgent 执行状态                                        │
│  - 处理跨领域协作请求                                                  │
│  - 定期生成会话总结                                                    │
│  - 所有领域完成后，汇总结果并结束会话                                    │
│                                                                      │
│  配置: 轻量模型 (gpt-4o-mini), 低Temperature, 确定性高                  │
└─────────────────────────────────────────────────────────────────────┘
                              │
              ┌───────────────┼───────────────┐
              ▼               ▼               ▼
┌─────────────────┐ ┌─────────────────┐ ┌─────────────────┐
│  DomainAgent    │ │  DomainAgent    │ │  DomainAgent    │
│  商城页面负责人  │ │  购物模块负责人  │ │  订单模块负责人  │
└────────┬────────┘ └────────┬────────┘ └────────┬────────┘
         │ Layer 2: 会话块Agent                      │
         │ (领域上下文管理 + 任务分发)                 │
         │                                          │
         │ 职责:                                     │
         │ - 管理该领域的上下文信息                     │
         │ - 分析任务，匹配或创建合适的助手              │
         │ - 调用助手处理子任务                         │
         │ - 汇总助手结果                               │
         │                                          │
         │ 生命周期: 会话级 (session)                   │
         │ 创建方式: 大模型API动态生成                   │
         └────────┬─────────────────────────────────┘
                  │
     ┌────────────┼────────────┐
     ▼            ▼            ▼
┌─────────┐ ┌─────────┐ ┌─────────┐
│Assistant│ │Assistant│ │Assistant│
│代码助手  │ │UI助手    │ │测试助手  │
└─────────┘ └─────────┘ └─────────┘
         Layer 3: 助手角色
         (专注一件具体事情)

         两类助手:
         - 固定助手: 配置文件预定义 (代码助手/UI助手/提示词审查助手等)
         - 临时助手: 大模型动态创建 (随任务结束消亡)
```

---

## 三层详解

### Layer 1: MetaAgent (主Agent)

**定位**: 每个会话一个实例，全局唯一调度中枢。

**核心能力**:
- **领域分析**: 根据用户目标，识别涉及的业务领域
- **会话块管理**: 创建/切换/销毁 DomainAgent 会话块
- **跨领域协调**: 处理 DomainAgent 间的协作请求（如A领域需要B领域数据）
- **会话总结**: 每N步自动生成会话信息总结
- **生命周期管理**: 清理过期角色实例

**调度流程**:
```
用户输入 → MetaAgent分析领域 → 创建DomainAgent(s)
  → 激活第一个DomainAgent → 等待DomainAgent完成
  → 切换下一个DomainAgent → ... → 全部完成 → 汇总输出
```

**代码**: `internal/graph/meta_agent.go`

---

### Layer 2: DomainAgent (会话块Agent)

**定位**: 每个业务领域一个实例，管理该领域的全部上下文。

**核心能力**:
- **领域上下文管理**: 维护该领域的私有记忆、工作状态、中间结果
- **任务拆解**: 将领域目标拆解为可执行的子任务列表
- **助手匹配**: 根据子任务匹配固定助手，无匹配则动态创建临时助手
- **助手调用**: 通过 CallStack 机制调用助手，支持嵌套调用
- **结果汇总**: 收集所有助手输出，生成领域级总结

**生命周期**:
- 创建: 由 MetaAgent 在会话开始时动态创建
- 存活: 整个会话期间
- 销毁: 会话结束时由 MetaAgent 清理

**示例角色**:
- 商城页面负责人（管理首页/商品页/购物车页等UI上下文）
- 购物模块负责人（管理购物车/结算/优惠等逻辑）
- 订单模块负责人（管理订单状态/物流/售后等）

**代码**: `internal/graph/domain_agent.go`

---

### Layer 3: Assistant (助手角色)

**定位**: 专注处理单一具体任务，可被任意 DomainAgent 或 MetaAgent 调用。

**两类助手**:

#### 固定助手 (配置文件定义)

| 角色ID | 名称 | 技能 | 生命周期 |
|--------|------|------|----------|
| `code_assistant` | 代码助手 | 代码编写、审查、重构、单元测试 | permanent |
| `ui_assistant` | UI助手 | UI修复、组件开发、响应式设计 | permanent |
| `prompt_reviewer` | 提示词审查助手 | Prompt审查、优化、安全检测 | permanent |
| `test_assistant` | 测试助手 | 单元测试、集成测试、边界测试 | permanent |
| `doc_assistant` | 文档助手 | 技术文档、API文档、代码注释 | permanent |

配置方式: `config/roles.yaml` 中 `fixed_roles` 列表

#### 临时助手 (大模型动态创建)

- **创建时机**: DomainAgent 遇到无固定助手能处理的任务时
- **创建方式**: 调用大模型API生成角色定义（system_prompt、skills、keywords）
- **生命周期**: task 级（单次任务后自动消亡）
- **示例**: "数据库迁移助手"、"性能分析助手"、"安全审计助手"

**调用机制**:
```
DomainAgent 分析任务 → 匹配固定助手?
  → 是: 创建固定助手实例 → 压入CallStack → 执行
  → 否: 调用大模型生成临时角色 → 创建临时助手实例 → 压入CallStack → 执行
```

**代码**: `internal/graph/assistant.go`

---

## 角色间调用关系

### 调用权限矩阵

| 调用者 \ 被调用者 | MetaAgent | DomainAgent | 固定助手 | 临时助手 |
|------------------|-----------|-------------|----------|----------|
| **MetaAgent**    | -         | 创建/调度   | -        | -        |
| **DomainAgent**  | 返回结果  | 跨域请求    | 调用     | 创建/调用 |
| **固定助手**     | -         | -           | -        | -        |
| **临时助手**     | -         | -           | -        | -        |

### 调用栈 (CallStack)

支持嵌套调用，用栈结构管理:

```
CallStack:
  [0] DomainAgent(商城页面) → Assistant(UI助手)     ← 当前执行
  [1] MetaAgent → DomainAgent(商城页面)              ← 等待返回
```

Assistant 完成后弹出栈，返回给调用者 DomainAgent。

**代码**: `pkg/types/role.go` (CallRequest/CallResponse)

---

## 角色注册表与工厂

### RoleRegistry (角色注册表)

管理所有角色定义和实例:

```
固定角色定义 (fixedDefs)
  └── 来自 config/roles.yaml

动态角色定义 (dynamicDefs)
  └── 来自大模型API动态生成

角色实例 (instances)
  └── 运行时创建的实例，带SessionID/Domain/状态
```

**代码**: `internal/graph/role_registry.go`

### RoleFactory (角色工厂)

动态创建临时角色:

```go
// 创建领域Agent
factory.CreateDomainAgent(ctx, sessionID, "商城页面", "修复穿模", "")

// 创建临时助手
factory.CreateAssistant(ctx, sessionID, "分析CSS冲突", domainAgentID, parentDefID)
```

**流程**:
1. 构建Prompt（包含领域/任务描述）
2. 调用大模型API生成角色定义JSON
3. 解析并注册到 RoleRegistry
4. 创建实例并返回

**代码**: `internal/graph/role_factory.go`

---

## 配置文件

### config/roles.yaml

```yaml
# Layer 1 配置
meta_agent:
  model_config:
    provider: openai
    model: gpt-4o-mini
    api_key: ${OPENAI_API_KEY}
    temperature: 0.3
  max_blocks: 10
  summary_interval: 5

# Layer 3 固定助手配置
fixed_roles:
  - id: code_assistant
    name: 代码助手
    type: fixed
    lifecycle: permanent
    skills: ["代码编写", "代码审查"]
    can_be_called: true

# 动态角色模板
dynamic_templates:
  - id: domain_template
    type: domain
    prompt_template: "你是{domain}领域的负责人..."
```

**完整示例**: `config/roles.yaml`

---

## 状态流转

### 角色状态机

```
Idle ──MetaAgent调度──→ Active ──调用助手──→ Calling
  ↑                      │                        │
  │                      │                        ▼
  └────等待返回──────────┘              Assistant执行
                                              │
                                              ▼
                              Assistant完成 ──→ Done ──→ 返回调用者
```

### 会话状态流转

```
[新建会话]
  │
  ▼
[MetaAgent分析领域] → 创建DomainAgent(s)
  │
  ▼
[DomainAgent执行任务] → 匹配/创建助手 → 调用助手
  │                       │
  │                       ▼
  │              [Assistant处理子任务]
  │                       │
  │                       ▼
  │              [返回结果给DomainAgent]
  │                       │
  │                       ▼
  │              [DomainAgent汇总]
  │                       │
  ▼                       │
[MetaAgent切换下一领域] ←─┘
  │
  ▼
[所有领域完成]
  │
  ▼
[MetaAgent汇总输出] → [会话结束]
```

---

## 代码结构

```
internal/graph/
├── meta_agent.go          # Layer 1: MetaAgent
├── domain_agent.go        # Layer 2: DomainAgent
├── assistant.go           # Layer 3: Assistant
├── role_registry.go       # 角色注册表
├── role_factory.go        # 角色工厂（动态创建）
├── three_layer_graph.go   # 三层图编排
├── state.go               # 状态定义
├── router.go              # 旧版路由（保留兼容）
└── ...

pkg/types/role.go          # 角色相关类型定义
pkg/config/role_config.go  # 角色配置加载
config/roles.yaml          # 角色配置文件
```

---

## 扩展方向

### 增加 Layer 2.5: SubDomainAgent

当 DomainAgent 管理的领域过于复杂时，可再细分:

```
DomainAgent(商城页面)
├── SubDomainAgent(首页头部)
├── SubDomainAgent(商品列表)
└── SubDomainAgent(底部导航)
```

### 增加 Meta-MetaAgent

多个会话需要协调时，增加超级调度层:

```
Meta-MetaAgent
├── Session A → MetaAgent
└── Session B → MetaAgent
```

### 助手自治化

让助手具备自我调用能力:

```
Assistant(代码助手) 遇到需要UI协助时
  → 主动发起 CallRequest 给 Assistant(UI助手)
  → 无需经过 DomainAgent 中转
```

---

## 运行演示

```bash
cd backend/cmd/demo
go run main.go
```

输出示例:
```
=== 角色配置加载完成 ===
固定角色数量: 5
  - [code_assistant] 代码助手 (类型: fixed, 生命周期: permanent)
  - [ui_assistant] UI助手 (类型: fixed, 生命周期: permanent)
  ...

=== 启动三层架构会话 ===
会话ID: demo-session-001
用户目标: 修复商城主页穿模问题

=== 会话执行完成 ===
最终动作: Finish

=== 会话中创建的角色实例 ===
  - [domain_xxx] 商城页面负责人 (类型: domain, 领域: 商城页面, 状态: done)
  - [assistant_xxx] 临时助手 (类型: dynamic, 状态: done)
```
