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
