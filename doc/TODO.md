## 待完成 / 开放项

1. **ReAct 端到端集成测试**（已部分修复）
   - 背景：`test/coding/*` 与 `test/api/*` 曾基于旧 ThreeLayerGraph 状态机 mock。
   - 当前状态：`test/fixtures/llm.go` mockMessage.Content 改 `json.RawMessage` 兼容 blades contrib/openai 数组格式后,e2e 全部通过（commit `e1e0eef`）。`test/coding/{snake,css,bug_fix}` 与 `test/api/*` 跑通。
   - 开放动作：持续补充覆盖 Agent 树 cancel、事实提取回退路径等新端点的 e2e。

2. **TUI Agent 树运行时构建**（部分完成）
   - 当前状态：`internal/domain/orchestrator/tree.go` 权威树 struct 已落地（commit `2e97eff`）,PG 持久化已落地（commit `5a1134d`）。Dispatcher 派发时 Register/SetCancel/Finish,HTTP 暴露 `GET /api/sessions/{id}/tree` + `POST /api/sessions/{id}/agents/{aid}/cancel`。`ReactService.ListAgents` 仍返回单 MetaAgent,但 `Tree()` 方法返回权威树快照。TUI `deriveSubAgentNodes` 保留作 fallback。
   - 开放动作：TUI 改为读 `Tree()` 替代事件流派生（树已持久化,可切）；`verifyloop.ExecuteChild` 同步路径入树（phase 2）。

3. **记忆事件持久化**
   - 背景：`domain/memory.Pipeline` 当前使用 `InMemoryStore`，会话结束后事件丢失。
   - 开放动作：实现 `domain/memory.Store` 的 Postgres 适配器,把 agent 事件流写入新表 `agent_events`（旧 `session_events` 保留只读）。

4. **记忆 hot/cold 2 阶段分层**（部分完成,commit `ced2e07`）
   - 当前状态：`summarizeWindow` 从 `react_agent.go` 迁入 `domain/memory/pipeline.go` 的 `compressHistory`。Pipeline 加 `WithCompression(every, keepRecent)` + 按 agentID 步频计数器,Assemble 在步频命中时调 `compressHistory`。ReActAgent 主循环简化为仅 `windowMessages` 硬上限。`config.go` 的 `SummarizeEvery`/`SummarizeKeepRecent` 改由 Pipeline 消费。
   - 开放动作：MetaAgent 根任务仍无 recall 注入（仅子 Agent 有 `injectRecalledMemory`）。需要 BlockMemorySearcher 接口跨域注入到 Pipeline,scope 较大留独立 commit。

5. **配置清理**（已完成）
   - 当前状态：`config.go` 707 -> ~370 行,死字段已删（`GraphPolicyConfig`/`MemoryPolicyConfig`/`PluginsConfig` 等）。见 #10 与 cleanup commit。
   - 开放动作：无遗留。

6. **文档漂移清理**（部分完成,本次更新）
   - 当前状态：`doc/项目说明.md` / `README.md` / `doc/流程图.md` 本次已同步 Agent 树 + 事实提取两步改造。`doc/设计文档_v3.md` 仍保留旧 ThreeLayerGraph 描述。
   - 开放动作：`设计文档_v3.md` 顶部已有状态声明,适当时机重写或归档。

7. **Soul.Inject 死代码清理**
   - 背景：`soul.Loader` 加载 `soul.md` 正常,人格名经 API 暴露,但 `Inject(systemPrompt)` 方法从未被调用,人格内容未注入任何 ReAct prompt。
   - 开放动作：在 `react_agent.go systemPrompt()` 头部调 `soulLoader.Inject(base)` 注入人格前缀；或在 `service_react.go` 构造子 Agent 时注入；或确认 Soul 仅用于温度策略后删除 `Inject` 方法。

8. **话题隔离轻量版**（已完成,commit `2b68d51`）
   - 当前状态：`SwitchTopic` 重写为轻量话题隔离:`Tree.EndCurrentTopic` 取消 Running 节点 + 快照 + 清内存 + best-effort 删 PG(`DeleteNodesBySession`);旧树快照压缩为摘要写入 sharedKV `topic:{id}:summary`;生成本会话单调递增 topicID(`nextTopicID`),新话题从空树开始。无状态机,纯 KV 摘要 + 树切换。`Session`/`server.Session` 加 `ActiveTopicID` 字段透传前端。`reactInternalSession.activeTopicID` 字段。依赖 Agent 树持久化(commit `5a1134d`)已满足。
   - 开放动作：MetaAgent 新话题召回旧摘要依赖步骤 4 part C(MetaAgent 根 recall 注入),摘要已落 KV,part C 按 `topic:{id}:summary` key 读即可。

9. **动态角色注册中心**（已完成,commit `f27b166`）
   - 当前状态：`domain/role/registry.go` 加 RWMutex + dynamic map + `Register`/`Unregister`/`List` 运行时 API。`Get` 优先查 dynamic 层再回退 cfg;`CallableFixedRoles` 含动态角色;`CanCall` 现有逻辑已覆盖 `RoleTypeDynamic` 分支。新增 `role/tools.go`:`create_role`/`list_roles` 工具实现 Tool 接口,`Registry.RegisterTools` 注入 `tool.Registry`,Schema 暴露两个工具。MetaAgent Tools 白名单加 `create_role`/`list_roles`。Register 校验:ID 非空、不撞内置、Type 必须为 Dynamic、SystemPrompt 非空;ID 冲突拒绝。进程重启不保留（roles.yaml 才持久化）,与 Agent 树不持久化同 scope。

10. **verifyloop 折叠进 ReAct**（未做,依赖 Agent 树终结语义）
    - 背景：当前双控制流（ReAct loop + verifyloop 状态机）不统一,`OnSubAgentDone` 钩子自动触发编排器。
    - 开放动作：删 `OnSubAgentDone` 钩子；加 `verify_and_fix` 工具（白名单限 MetaAgent/DomainAgent）；工具内部仍跑 `Verifier`/`Fixer`/`Reporter` 三接口,但作为工具调用而非独立编排器。MetaAgent 显式决定何时验证。

11. **硬 Token 预算（每用户目标上限）**【已规划落地,待编码】
    - 背景：`maxIter=50` 仅防死循环,无累计 token 上限。
    - 开放动作（约 15 行代码）：`config.go AgentConfig` 加 `TokenBudgetPerGoal int`；`config.yaml` 加 `token_budget_per_goal: 100000`；`react_agent.go` `RunWithHistory` 每轮累加 `resp.Message.TokenUsage.TotalTokens`,超限 break 返回部分完成。与 `maxIter=50` 正交。

12. **共享记忆一致性**【Layer 1-3 已落地 2026-07-28】
    - 当前状态：详见 `doc/计划_共享记忆一致性.md`。Layer 1-3 已完成（`writeSharedMemoryInput` 加 `Files []string` + `WriteFile` hook 失效 KV + `Dispatcher.buildSharedPrefix` mtime 校验）。KVMemory 已合并为单一 `SharedMemoryStore`,三路 inject 合并为 `buildSharedPrefix`。
    - 开放动作（P1/P2,未做）：Layer 4 角色写目录分区（`roles.yaml` sandbox.allowed_write_paths）；Layer 5 `mailbox.Message` 增 `FilesModified []string`。

13. **评估项**
    - 多Agent协作：类似人类之间互相交流询问是否正确。例如：代码Agent完成代码后测试Agent进行测试,不明确具体测试方向时需要先把方案列出,给开发处方案的代码Agent是否符合代码Agent的逻辑,不符合代码Agent纠正,符合测试Agent测试Agent进行测试。测试结果代码Agent代码Agent对比是否与需求符合,不一致则代码Agent重新修正。复合后代码Agent返回给上级的领域Agent或者主Agent,期间各Agent可以反复询问,纠正,需要Agent在被询问时开一个协程进行回复,需要携带关键记忆或者协程Agent常驻共享代码Agent记忆,可评估。测试流程最为重要,查看现在的测试是否严谨与多重确认。代码Agent完成开发,找到固定助手的测试助手,先进行自测试,返回结果成功后返回给上级Agent,上级Agent按更大范围模块进行统一测试,哪个部分不行打回。
    - Agent执行任务,先拆解任务,拆解为不会干涉的单元任务后,一个单元任务使用一个干净上下文的Agent编码助手执行,压缩Token成本。执行完成后暂时不销毁,一定时间不使用直接消耗,有使用则重置使用时间并加长使用时间。原因：我在使用claude时,经常会有不切换会话在同一个会话使用重复上下文一直执行任务。越到后面越会上下文污染严重导致模型幻觉,并且上下文上每一次输入的Token成本也会激增。可评估是否可以替换块记忆,块记忆过于抽象,可用性与传统感觉不明显,。或者把块记忆与每个单独感觉上下文的Agent进行集成融合等尝试。现在的领域子Agent排发就相当于这个方案的初始模式,每个领域干净上下文负责自己的事。

14. **已删除的投机性泛化代码**（见 #10 归档）
    - assembly 包 / verifiers.go 扩展占位 / computeruse 包 / 实例池 / KV 三套抽象 / 强制门默认 / 配置死字段 已删。详见 git 历史。

## 已完成（已归档到 git 历史）

- ReAct 主循环骨架（`internal/agent/react_agent.go`）
- 工具域（`internal/domain/tool/`）注册表 + 11 个内置工具
- 角色域（`internal/domain/role/`）加载与权限
- 记忆域（`internal/domain/memory/`）两段事件流
- 子 Agent 域（`internal/domain/subagent/`）异步分发 + Mailbox 集成
- `bootstrap.Build` 切换到 `agent.NewReactService`
- 删除 `internal/graph/` 全部代码
- 删除 `internal/memory/` 中 compress/search/assembler/snapshot/callback/block_vector/workspace_adapter
- 删除旧 `agent.Service` / `agent.sessionStore` 实现
- HTTP/TUI 入口适配新 `agent.Agent` 接口
- 包依赖规则文档更新
- **Agent 树显式化**（commit `2e97eff`，2026-07-30）：`internal/domain/orchestrator/tree.go` 权威树 struct,Dispatcher 派发时 Register/SetCancel/Finish,HTTP `/api/sessions/{id}/tree` + `/api/sessions/{id}/agents/{aid}/cancel`。agent.Agent 接口加 Tree/CancelAgent。
- **块记忆 LLM 事实提取**（commit `c57f22b`，2026-07-30）：`saveBlockMemory` 先调轻量模型提取 1-5 条关键事实,每条单独落 KnowledgeRecord。新增 `subagent.FactExtractor` 接口 + `ParseFactsJSON` 解析器,bootstrap `llmFactExtractor` 复用 `ModelFactory.CallLightweightWithRetry`。提取失败回退原始保存。
