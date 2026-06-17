# BlockMemoryAgent 开发结果文档（v3）

> 对应开发流程：`doc/DEVELOPMENT_LOG_v3.md`
> 对应设计文档：`doc/设计文档_v3.md`
> 提交时间：2026-06-17

---

## 1. 总体结果

按 v3 设计文档实施 7 个核心任务，**全部完成**：

| # | 任务 | 状态 | 关键产物 |
|---|---|---|---|
| 1 | Skill 库系统 | ✅ | `internal/skill/`、`config/skills.yaml`、单元测试 |
| 2 | 任务看板 | ✅ | `internal/board/`、单元测试 |
| 3 | 邮箱通知 | ✅ | `internal/mailbox/`、单元测试 |
| 4 | 人格 + Temperature | ✅ | `internal/soul/`、`config/soul.md`、单元测试 |
| 5 | Watchdog | ✅ | `internal/watchdog/`、接入 MetaAgent、单元测试 |
| 6 | AIOps 测试 | ✅ | `test/aiopsmock/`、`test/aiopstest/` 4 场景全部通过 |
| 7 | 文档 | ✅ | 本篇 + `DEVELOPMENT_LOG_v3.md` |

---

## 2. 架构演进

### 2.1 v3 后的整体视图

```
┌────────────────────────────────────────────────────────────┐
│                          用户                               │
└─────────────────────────────┬──────────────────────────────┘
                              │
                ┌─────────────▼──────────────┐
                │   server.SessionManager    │   HTTP / SSE / TUI
                └─────────────┬──────────────┘
                              │ Invoke
                ┌─────────────▼──────────────────────────────┐
                │        ThreeLayerGraph                     │
                │ ┌──────────────────────────────────────┐   │
                │ │  MetaAgentNode                       │   │
                │ │  - Watchdog Check (§4.4)             │   │
                │ │  - Mailbox Drain & Forward (§7.2)    │   │
                │ │  - TaskBoard Init (§7.1)             │   │
                │ └────────────────┬─────────────────────┘   │
                │   动态构造          │                          │
                │ ┌────────────────▼─────────────────────┐   │
                │ │  DomainAgentNode                     │   │
                │ │  - ensureSkillSet (§5)               │   │
                │ │  - dispatchAssistantsParallel        │   │
                │ └────────────────┬─────────────────────┘   │
                │ ┌────────────────▼─────────────────────┐   │
                │ │ SubDomainAgent / Assistant + Tools   │   │
                │ └──────────────────────────────────────┘   │
                └─────────────┬──────────────────────────────┘
                              │ 共享 Runtime
                ┌─────────────▼─────────────┐
                │  internal/runtime         │
                │  ├─ Boards    (board)     │
                │  ├─ Mailbox   (mailbox)   │
                │  ├─ Skills    (skill)     │
                │  ├─ Soul      (soul)      │
                │  └─ Watchdog  (watchdog)  │
                └───────────────────────────┘
```

### 2.2 Skill 装配流水

```
┌─────────────┐  Domain     ┌───────────────────┐ LLM   ┌──────────┐
│ skills.yaml │──filter────►│ candidates (≤ N)  │──pick►│ SkillSet │
└─────────────┘             └───────────────────┘       └────┬─────┘
                                                             │bind
                                                             ▼
                                              registry.GetForAgent(id)
                                                             │
                                                             ▼
                                       skill_brief 注入 system prompt
```

### 2.3 一次会话的端到端时序

```
user goal
   │
MetaAgent.handleInitial
   │ analyzeDomains (LLM 路由温度=0)
   │ TaskBoard.GetOrCreate + AddSubTask(每个领域)
   │ factory.CreateDomainAgent
   ▼
DomainAgent.Invoke
   │ ensureSkillSet  ── Pool.AssembleSet ── (LLM/规则) ── Bind
   │ analyzeTasks
   │ shouldSplitToSubDomains?
   │   ├─ 是 → handleSubDomainSplit → SubDomainAgent.Invoke (并行)
   │   └─ 否 → dispatchAssistantsParallel
   │             └─ executeAssistantTask
   │                  └─ executeWithTools(skill_brief, tools)
   │                       └─ ToolExecutor.Execute
   ▼
MetaAgent.switchToNextBlock → finalizeSession → ActionFinish
   │
   └─ 期间每一步：runWatchdog + processMailbox（v3 §4.4 / §7.2）
```

---

## 3. 模块成果详解

### 3.1 Skill 库（`internal/skill/`）

- `Pool`：线程安全 Skill 池，支持 YAML / 内置注入。
- `Pool.FilterByDomain`：按领域名 / 标签 / 通配符 `*` 预筛。
- `Pool.AssembleSet`：候选 ≤ N 时直接返回；否则用 LLM 选 N 个。
- `SelectOne`：将 SkillSet 与具体任务交给 LLM，返回唯一 SkillID 或 `""`（无合适者）。
- `Registry.Bind / GetForAgent / AddSkillToAgent`：按 agent 实例 ID 绑定。

测试用例：`pool_test.go` 覆盖 5 项关键路径（领域过滤、LLM 选择、SelectOne、Registry 绑定与扩展）。

### 3.2 任务看板（`internal/board/`）

- `TaskBoard.AddSubTask` 自动去重。
- `TaskBoard.MarkDone/MarkFailed/MarkBlocked` 自动重算整体状态：全部到达终态且包含 FAILED → `FAILED`；全部 DONE → `DONE`；其余 → `IN_PROGRESS`。
- `TaskBoard.Brief(maxTasks)` 输出 ≤ ~200 token 的紧凑视图，可作为子 Agent 上下文注入（v3 §4.4）。
- `Manager.GetOrCreate`：会话级单例。

### 3.3 邮箱（`internal/mailbox/`）

- 直发：`Mailbox.Send` → `Drain(target)` 按 Priority 降序、CreatedAt 升序。
- 广播：`To == "*" / ""` → `DrainBroadcast`，主 Agent 决议后用 `Forward(msgID, target)` 转交。
- `Purge(agentID)`：Agent 销毁时清理收件箱。

### 3.4 人格 + 温度（`internal/soul/`）

- `Loader.Load / Reload`：热加载 `config/soul.md`，文件不存在不致命。
- `Loader.Inject(prompt)`：把人格拼到 system prompt 之前。
- `Temperature(kind, base)`：6 种 `TaskKind`，对应 routing=0、summarize=0、code=0.15、analysis=0.3、creative=0.8、generic=base。
- `InferKind(text)`：启发式推断任务类别（用于未显式声明 kind 的 LLM 调用）。

### 3.5 Watchdog（`internal/watchdog/`）

- `Estimator(text)`：4 字符/token 粗估，相对值稳定。
- `Watchdog.Check(agentID, contextText)`：返回 `Decision{Level, Reason, Suggested}`。
- 阈值：`SoftLimit=2400` 触发 COMPRESS；`HardLimit=3000` 触发 EVICT。
- MetaAgent 在每轮 Invoke 中调用 `runWatchdog`，EVICT 时注入 `EventEscalation`。

### 3.6 Runtime（`internal/runtime/`）

把上述 5 个组件聚合为单一对象，`main.go` 一次构造，`graph.ThreeLayerGraphBuilder.SetRuntime(rt)` 注入；图在动态构造 MetaAgentNode / DomainAgentNode 时自动 `SetRuntime`。

---

## 4. 测试结果

### 4.1 单元测试

```
ok  internal/board    0.094s
ok  internal/mailbox  0.235s
ok  internal/skill    0.239s
ok  internal/soul     0.280s
ok  internal/watchdog 0.212s
```

| 测试名 | 验证点 |
|---|---|
| `TestPool_FilterByDomain` | 领域名筛选正确 |
| `TestPool_AssembleSet_NoLLMReturnsAllOrTrim` | 无 LLM 时按 cost 截断 |
| `TestPool_AssembleSet_WithLLM` | LLM 返回 ID 列表后被尊重 |
| `TestSelectOne` | "NONE" 与无效 ID 处理 |
| `TestRegistry_BindAndAdd` | 绑定与扩展技能 |
| `TestTaskBoard_LifeCycle` | 增/分配/完成/失败/状态机 |
| `TestManager` | 会话级单例 |
| `TestMailbox_DirectSendAndDrain` | 优先级排序 |
| `TestMailbox_BroadcastAndForward` | 广播桶 + Forward |
| `TestMailbox_Purge` | 清理收件箱 |
| `TestEstimator` / `TestCheck_Levels` | Watchdog 三段阈值 |
| `TestLoaderInjectAndReload` | soul 热加载 |
| `TestTemperature` / `TestInferKind` | 温度与启发式 |

### 4.2 图集成测试

```
ok  internal/graph    0.314s
```

`TestThreeLayerGraph_RuntimeWired` 验证：

- 启动一次 Mock Session 后 `Runtime.Boards.Get(sessionID)` 非空（看板被 MetaAgent 创建）
- `Runtime.Watchdog.History()` 至少 1 条决策（每轮都跑了）

### 4.3 AIOps 4 场景集成

```
=== RUN   TestScenarioA_AlertStormAndRootCause   --- PASS (0.07s)
=== RUN   TestScenarioB_PlaybookWithApproval     --- PASS (0.01s)
=== RUN   TestScenarioC_ChaosDrillRollback       --- PASS (0.01s)
=== RUN   TestScenarioD_PostmortemAndKnowledge   --- PASS (0.01s)
PASS
ok  test/aiopstest  0.203s
```

| 场景 | 步骤 | 验证 |
|---|---|---|
| A 告警风暴+根因 | 8 条告警 → 列表 → 触发 RCA → 轮询 → mysql-primary | 全 OK |
| B Playbook+审批 | 列表 → execute(require_approval=true) → 审批 step2 → 查最终 | 状态由 pending_approval → completed |
| C 容灾演练回滚 | execute(chaos-drill-order) → 验证步骤 verify_failover failed → 回滚分支步骤齐全 | rollback_traffic / recover_db 都被记录 |
| D 复盘+知识 | 创建复盘 → 知识库搜索 → kb-001 命中 | OK |

---

## 5. 兼容性

- `go.mod` 的 `go 1.25.4 → 1.25.0`，与本机 `go version go1.25.0 windows/amd64` 一致；CI 中如使用 1.25.4 仍可正常构建。
- 所有改动保持向后兼容：原 `executeWithTools(...)` 调用点在 `domain_agent.go` / `subdomain_agent.go` 已同步增加 `skillBrief` 参数，外部调用者无变化。
- 在没有 `OPENAI_API_KEY` 时 `mockClient` 仍工作；`role_factory.go` 在 `modelFactory==nil` 时回退模板，所以单元测试不依赖外部 LLM。

---

## 6. 运行 / 验证指引

### 6.1 拉起依赖

```bash
cd docker && docker compose up -d && cd ..
cp .env.example .env  # 填入 OPENAI_API_KEY
```

### 6.2 启动主进程

```bash
GOTOOLCHAIN=local go run ./backend \
  -config config/config.yaml \
  -roles  config/roles.yaml \
  -soul   config/soul.md \
  -skills config/skills.yaml
# → http://localhost:10010
```

### 6.3 直接跑测试

```bash
GOTOOLCHAIN=local go test ./backend/... -count=1
```

### 6.4 离线模式

不配置 `OPENAI_API_KEY` 时所有 LLM 调用走 `mockClient`；规则回退保证流程仍能跑通：
- 领域分析 → `analyzeDomainsByRules`
- 任务拆解 → `analyzeTasksByRules`
- Skill 装配 → `Pool.FilterByDomain` 直接返回（候选 ≤ N 即不调用 LLM）

---

## 7. 已知限制

1. **会话块向量检索未启用**：v3 §4.2 描述的语义相似 Top-K 注入需要 Embedding API；当前仍按重要性排序粗选。
2. **温度调节范围有限**：目前仅 MetaAgent 路由调用使用 `KindRouting`；DomainAgent / Assistant 的代码 / 创意路径暂未按 `InferKind` 自动调温。
3. **Mailbox 内存实现**：进程重启后丢失；后续替换为 `RedisStore.PushEvent`。
4. **Watchdog 估算精度**：粗估 4 char ≈ 1 token，对纯中文偏低 30%；阈值留出余量足以覆盖。

---

## 8. 文件索引

```
e:/MyProject/BlockMemoryAgent/
├── config/
│   ├── soul.md         (新增) 默认人格
│   └── skills.yaml     (新增) 默认 Skill 池
├── doc/
│   ├── DEVELOPMENT_LOG_v3.md   (新增) 开发流程
│   └── RESULT_v3.md            (新增) 本篇
├── internal/
│   ├── skill/          (新增) Skill 池
│   ├── board/          (新增) 任务看板
│   ├── mailbox/        (新增) 邮箱
│   ├── soul/           (新增) 人格 + 温度
│   ├── watchdog/       (新增) 看门狗
│   └── runtime/        (新增) Runtime 聚合
├── pkg/types/skill.go  (新增) Skill 类型
└── test/
    ├── aiopsmock/      (新增) Mock Server
    └── aiopstest/      (新增) 4 场景集成测试
```

---

## 9. 提交清单（建议 commit message）

```
feat(v3): Skill 库、任务看板、邮箱、人格、Watchdog 与 AIOps 测试

- internal/skill: 结构化 Skill + Pool + Registry，支持 YAML 与 LLM 决策
- internal/board: 任务看板，含原子状态机与紧凑 Brief 注入
- internal/mailbox: Agent 异步邮箱，支持广播桶 + 转交
- internal/soul: 人格热加载 + Temperature 动态调节
- internal/watchdog: 上下文长度看门狗，超阈值自动升级
- internal/runtime: 五合一 Runtime
- graph.MetaAgent: 接入 Runtime，每轮调度跑 Watchdog & Mailbox
- graph.DomainAgent: ensureSkillSet 装配领域 Skill 子集
- graph.role_factory: 容忍 modelFactory==nil
- graph.tool_executor: 增加 HTTPGet / HTTPPost
- test/aiopsmock + aiopstest: 4 个 v3 §10 测试场景全部 PASS
- doc: DEVELOPMENT_LOG_v3 与 RESULT_v3
```
