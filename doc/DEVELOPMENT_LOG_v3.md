# BlockMemoryAgent 开发流程文档（v3）

> 对应设计文档：`doc/设计文档_v3.md`
> 编制日期：2026-06-17
> 实施分支：`main`

---

## 1. 开发背景

`doc/设计文档_v3.md` 在原 v1/v2 之上重新提炼了 BlockMemoryAgent 的核心目标：

- 解决长上下文遗忘 → **会话块隔离 + 智能压缩**
- 解决 Skill 选择困难 → **按领域装配 + LLM 决策**
- 多 Agent 协同不污染上下文 → **任务看板 + 邮箱通知**

本轮（v3）开发任务在已有三层 Graph 基础上，把 v3 §4-7 章节缺失的能力补齐：

| v3 章节 | 能力 | 历史进度 |
|---|---|---|
| §4.4 | Watchdog 上下文长度监控 | 接口已写但未在 graph 主路径启用 |
| §5   | Skill 库管理（结构化 Skill + 领域子集 + LLM 选择） | 缺失 |
| §6.2 | 人格配置 soul.md | 缺失 |
| §6.3 | Temperature 动态调节 | 缺失 |
| §7.1 | 任务看板（Task Board） | 缺失 |
| §7.2 | 邮箱通知（Mailbox） | 缺失 |
| §10  | AIOps 测试用例 | doc 在但实现缺失 |

---

## 2. 总体方案

### 2.1 模块切分

新增 6 个高内聚的 internal 子包，避免污染已有 `graph/` 与 `memory/`：

```
internal/
├── skill/          §5  Skill 池 / 领域装配 / LLM 选择
├── board/          §7.1 任务看板
├── mailbox/        §7.2 异步邮箱
├── soul/           §6.2/§6.3 人格 + 温度调节
├── watchdog/       §4.4 上下文长度看门狗
└── runtime/        把上述 5 个组件聚合为单一 Runtime，避免在
                    MetaAgent / DomainAgent / SubDomainAgent 上挂
                    一堆独立指针字段。
```

`Runtime` 在 `main.go` 进程入口处一次性构造，通过 `ThreeLayerGraphBuilder.SetRuntime` 注入到图，再由图在动态构造 DomainAgent 时透传给节点。

### 2.2 Skill 体系流水线

```
config/skills.yaml ─► skill.Pool
                        │ FilterByDomain(domain)
                        ▼
                   候选 Skill 子集
                        │ LLM 决策 (Pool.AssembleSet)
                        ▼
                  types.SkillSet (≤ 8)
                        │ Registry.Bind(agentInst)
                        ▼
   DomainAgent.executeAssistantTask
       │ 取出 SkillSet.PromptList()
       ▼
   llm_tools.executeWithTools(skillBrief = ...)
       │ system prompt 中只暴露这几个 Skill
       ▼
       LLM ──tool_call──► ToolExecutor.Execute(toolRef)
```

### 2.3 任务看板与邮箱协同

- MetaAgent 在 `handleInitial` 中创建 `TaskBoard`，把 LLM 推断出的领域名作为顶层子任务。
- DomainAgent / SubDomainAgent 完成任务时通过看板 `MarkDone / MarkFailed` 更新整体状态。
- Agent 之间发送的事件统一进入 `Mailbox`，主 Agent 的 `processMailbox` 在每轮调度循环把广播桶里的事件转交给具体领域，并自动转换为 `EventEscalation` 投回 SessionBlock。

### 2.4 Watchdog 接入

`MetaAgentNode.Invoke` 在每一轮循环开始时执行 `runWatchdog`：

1. 把当前活跃 SessionBlock 的 `Domain + Goal + TaskResults` 拼成上下文代理；
2. 调用 `Watchdog.Check` 拿到 `Decision`；
3. 当 `Decision.Level == LevelEvict` 时，向 SessionBlock 注入一条 `EventEscalation`，由 `EscalationHandler` 接管。

阈值默认：`Soft=2400`、`Hard=3000`，可通过 `watchdog.New(Config{...})` 自定义。

### 2.5 人格与温度

- 人格文件 `config/soul.md`，由 `soul.Loader` 在启动时加载，`Inject(systemPrompt)` 把人格作为前置段落拼接到原 system prompt 之前。
- `soul.Temperature(kind, base)` 把 6 种任务类别映射到推荐温度（路由 0、总结 0、代码 0.15、分析 0.3、创意 0.8）。
- `model.GenerateWithTemperature` 对实现 `TemperatureAware` 接口的 EinoClient 进行 per-call 温度覆盖；`mockClient` 自动忽略覆盖。

---

## 3. 关键改动一览

### 3.1 新增源文件

| 文件 | 作用 |
|---|---|
| `pkg/types/skill.go` | `Skill` / `SkillSet` 类型与 `PromptList()` |
| `internal/skill/pool.go` | `Pool` / `AssembleSet` / `SelectOne` |
| `internal/skill/registry.go` | `Registry` + 内置 `BuiltinPool` + YAML 加载 |
| `internal/board/board.go` | `TaskBoard` 与 `Manager` |
| `internal/mailbox/mailbox.go` | `Mailbox` / 优先级排序 / 广播桶 |
| `internal/soul/soul.go` | `Loader` / `TaskKind` / `Temperature` |
| `internal/watchdog/watchdog.go` | `Watchdog` / `Decision` / 阈值配置 |
| `internal/runtime/runtime.go` | 五合一 Runtime |
| `config/soul.md` | 默认人格 |
| `config/skills.yaml` | 默认 Skill 池 |
| `test/aiopsmock/server.go` | AIOps Mock Server |
| `test/aiopstest/scenario_test.go` | 4 个核心场景集成测试 |
| `internal/{skill,board,mailbox,watchdog,soul}/*_test.go` | 单元测试 |
| `internal/graph/runtime_wiring_test.go` | 三层图与 Runtime 集成验证 |

### 3.2 关键修改

| 文件 | 修改要点 |
|---|---|
| `internal/graph/meta_agent.go` | 接入 `runtime.Runtime`；`Invoke` 增加 `runWatchdog` / `processMailbox` / 看板初始化；`callLLM` 接入人格 + 路由温度 |
| `internal/graph/domain_agent.go` | `ensureSkillSet` 在初次执行时为本 DomainAgent 装配 Skill；`executeAssistantTask` 把 SkillSet 写入 system prompt |
| `internal/graph/subdomain_agent.go` | 接入新的 `executeAssistantWithTools` 签名 |
| `internal/graph/llm_tools.go` | `executeWithTools` / `executeAssistantWithTools` 增加 `skillBrief` 参数 |
| `internal/graph/three_layer_graph.go` | Builder 增加 `SetRuntime`；动态构造 DomainAgentNode 时注入 Runtime |
| `internal/graph/role_factory.go` | 容忍 `modelFactory==nil`，避免离线测试 panic |
| `internal/graph/tool_executor.go` | 增加 `HTTPGet` / `HTTPPost` 工具，支持 AIOps Mock Server 调用 |
| `internal/model/eino_client.go` | 增加 `GenerateWithOptions(temperature)` |
| `internal/model/factory.go` | 暴露 `TemperatureAware` 接口与 `GenerateWithTemperature` 工具函数 |
| `main.go` | 解析 `-soul`、`-skills` flag，构造 Runtime 并注入图 |
| `go.mod` | `go 1.25.4 → 1.25.0`（与本机工具链一致） |

### 3.3 文档/配置

- `config/soul.md`：默认人格（沟通风格、工作原则、安全准则）
- `config/skills.yaml`：默认 Skill 池（文件 / 代码 / HTTP 三类）

---

## 4. 实施顺序与提交记录

实施严格按 v3 章节顺序，每个模块完成后立即跑 `go build ./...` 和该模块的单元测试，以避免一次性大改导致的回归。

```
1. Skill 库 ─────────► 单元测试通过
2. Task Board ──────► 单元测试通过
3. Mailbox ─────────► 单元测试通过
4. soul + Temp ─────► 单元测试通过
5. Watchdog ────────► 单元测试通过
6. Runtime + 图接入 ► graph wiring 测试通过
7. AIOps Mock+测试 ► 4 场景全绿
8. 文档              （本篇）
```

---

## 5. 测试结果摘要

`GOTOOLCHAIN=local go test ./... -count=1` 输出（关键行）：

```
ok  	internal/board          0.094s
ok  	internal/graph          0.314s
ok  	internal/mailbox        0.235s
ok  	internal/skill          0.239s
ok  	internal/soul           0.280s
ok  	internal/watchdog       0.212s
ok  	test/aiopstest          0.203s
```

详细解读见 `doc/RESULT_v3.md`。

---

## 6. 与设计文档的偏差说明

| 设计点 | 实际实现 | 原因 |
|---|---|---|
| §4.2 会话块向量检索（pgvector） | 暂未启用，记忆模块仍按 `Episode + 重要性排序` 工作 | 嵌入模型尚未对接，本轮聚焦 v3 强调的"上下文纯净度"路径 |
| §6.3 Temperature 动态调节 | 仅 MetaAgent 在路由调用时启用 0 温度；DomainAgent / Assistant 仍按角色配置温度 | 避免一次性改动太多 LLM 调用点；后续按 `InferKind(task)` 渐进推广 |
| §7.2 Mailbox 持久化 | 当前为进程内内存实现；未落 Redis Stream | 与现有 `RedisStore.PushEvent` 解耦；接口稳定后再做存储替换 |

---

## 7. 后续工作

1. 把 `soul.InferKind` 推广到 DomainAgent / Assistant 的 LLM 调用路径
2. Mailbox 持久化到 `RedisStore.PushEvent`
3. SkillSet 持久化（断会话后能恢复装配状态）
4. 真实接入 LLM 后跑端到端的 AIOps 4 场景，对比 mock-only 的 baseline
5. 把 v3 §4.2 的会话块向量检索补齐（依赖 embedding API）

