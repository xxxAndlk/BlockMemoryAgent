## 待完成 / 开放项

1. **ReAct 端到端集成测试**
   - 背景：`test/coding/*` 与 `test/api/*` 仍基于旧 ThreeLayerGraph 状态机 mock，需要重写为 ReAct 事件流 mock。
   - 开放动作：更新 `test/fixtures/mock_llm.go`，为 ReAct 循环提供序列化响应；重写 snake / css-refactor / bug-fix 三个端到端用例。

2. **TUI Agent 树运行时构建**
   - 背景：当前 `ReactService.ListAgents` 只返回单一 MetaAgent 节点，TUI 右侧 Agent 编排栏无法展示子 Agent 调用树。
   - 开放动作：从会话事件流中动态构建 Agent 调用树（主 Agent → call_sub_agent → 子 Agent），替代旧编译期层级。

3. **记忆事件持久化**
   - 背景：`domain/memory.Pipeline` 当前使用 `InMemoryStore`，会话结束后事件丢失。
   - 开放动作：实现 `domain/memory.Store` 的 Postgres 适配器，把 agent 事件流写入新表 `agent_events`（旧 `session_events` 保留只读）。

4. **Watchdog 事件流截断**
   - 背景：新架构无记忆压缩，长会话事件流可能膨胀。
   - 开放动作：在 `watchdog` 超限时触发 `domain/memory.Pipeline` 截断（保留最近 N 条 + 生成总结事件）。

5. **配置清理**
   - 背景：`config.AgentConfig` 仍保留 `GraphPolicyConfig`、`MemoryPolicyConfig`、snapshot/compress 等旧字段。
   - 开放动作：确认无引用后删除失效字段，或标记为 deprecated。

6. **文档漂移清理**
   - 背景：`doc/设计文档_v3.md` 仍有大量 ThreeLayerGraph 描述。
   - 开放动作：标记历史章节，或在适当时机重写。
  
7. **评估项**
   - 多Agent协作：类似人类之间互相交流询问是否正确。例如：代码Agent完成代码后测试Agent进行测试，不明确具体测试方向时需要先把方案列出，给开发处方案的代码Agent是否符合代码Agent的逻辑，不符合代码Agent纠正，符合测试Agent测试Agent进行测试。测试结果代码Agent代码Agent对比是否与需求符合，不一致则代码Agent重新修正。复合后代码Agent返回给上级的领域Agent或者主Agent，期间各Agent可以反复询问，纠正，需要Agent在被询问时开一个协程进行回复，需要携带关键记忆或者协程Agent常驻共享代码Agent记忆，可评估。测试流程最为重要，查看现在的测试是否严谨与多重确认。代码Agent完成开发，找到固定助手的测试助手，先进行自测试，返回结果成功后返回给上级Agent，上级Agent按更大范围模块进行统一测试，哪个部分不行打回。
   - Agent执行任务，先拆解任务，拆解为不会干涉的单元任务后，一个单元任务使用一个干净上下文的Agent编码助手执行，压缩Token成本。执行完成后暂时不销毁，一定时间不使用直接消耗，有使用则重置使用时间并加长使用时间。原因：我在使用claude时，经常会有不切换会话在同一个会话使用重复上下文一直执行任务。越到后面越会上下文污染严重导致模型幻觉，并且上下文上每一次输入的Token成本也会激增。可评估是否可以替换块记忆，块记忆过于抽象，可用性与传统感觉不明显，。或者把块记忆与每个单独感觉上下文的Agent进行集成融合等尝试。现在的领域子Agent排发就相当于这个方案的初始模式，每个领域干净上下文负责自己的事。

   **已完成（阶段 0-3）：**
   - **阶段 0 - 测试基线修复**：`test/api/memory_test.go` 重写为 ReAct 现状断言；`test/role_config_sanity_test.go` 去硬编码模型名；`test/coding/{snake,css,bug_fix}` 三个 e2e 用例接入 `RegisterSequence` 精细化断言（`helpers_test.go` 新增 `toolCall`/`writeFileCall`/`readFileCall`/`toolResultEchoed`/`toolResultSucceeded`）。
   - **阶段 1 - 块记忆闭环**：`dispatcher.saveBlockMemory` 子 Agent 成功完成时沉淀结果摘要；`blockMemorySaver` 适配 `PostgresStore.Embed + SaveKnowledge`；`config.block_memory_write_enabled`（默认 true）控制开关；`roleIDFromAgentID` 修复连字符角色 ID bug；`pipeline.WithMaxEventsPerAgent` + `InMemoryStore.WithMaxEventsPerAgent` 防事件流无限增长；`runSubAgent` defer `mailbox.Purge` 修子 Agent 收件箱泄漏。
   - **阶段 2 - 协作验证闭环**：`mailbox.Message` 增加 `ReplyTo`/`ThreadID` 字段 + `MsgReply` 类型 + `WaitForMessage` 阻塞等待；`send_message` 工具支持任意 Agent 向另一个 Agent 实例邮箱投递消息（请求/通知/回复）；`roles.yaml` `code_assistant <-> test_assistant` 通过 `parents` 白名单平级互调；`config.verification_max_rounds`（默认 5）+ `Dispatcher.WithVerificationMaxRounds` 防循环死锁；`agent.PendingChildrenChecker` 接口 + `Dispatcher.PendingChildren`/`WaitForAnyChild` + `ReActAgent` 终结保护分支（有未决子 Agent 时阻塞等待而非立即终结，防迟到 mailbox 消息丢失）。
   - **阶段 3 - 子 Agent 实例池**：`Dispatcher.WithReuse(enabled, idleTimeout)` + `config.sub_agent_reuse_enabled`（默认 false）+ `config.sub_agent_idle_timeout_sec`（默认 300）；`servePooled` 子 Agent 完成初始任务后转入服务态，阻塞等待 mailbox 询问，收到消息时 `RunWithHistory` 处理并把回复投递给 `ReplyTo`，闲置超时退出；`trimPooledHistory` 防长生命周期历史无限增长；`Dispatcher.Stop` 释放所有池化子 Agent，`bootstrap.Close` 注册为 cleanup 回调。
   - **待办**：`send_message` 工具当前对"目标 Agent 不存在/已销毁"无显式校验（消息落入邮箱后若目标已 Purge 则丢失），后续可加 `IsPooled`/`running` 检查并返回错误；`AssistantSelfTestEnabled`/`DomainSelfTestEnabled` 死配置字段仍未消费（阶段 2 未接线，可由 `servePooled` + `send_message` 组合替代，或单独实现"派发测试助手验证"编排器）；`sub_agent_reuse_enabled` 默认关闭，实测验证闭环稳定性后再考虑默认开启。

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
