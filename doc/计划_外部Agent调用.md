# 外部 Agent 调用功能计划书（CallExternalAgent）

> 状态：**待实施**（2026-10-21 计划书，经代码调研后撰写；调研结论均带 文件:行号 证据）
> 目标版本：v1（对话工具 + 管理页 + 编排复用）；flow 专用节点类型留作 v2。

## 1. 背景与目标

BMA 解决不了的問題（超出内置工具能力、需要别的模型/Agent 的长任务）目前只能自己硬扛或靠用户手动开第二个终端。目标：让 BMA 能调用本机已安装的其他 Agent CLI（claude code、codex、Kimi Code、opencode、Hermes、DeepSeek Harness 等），作为**工具**融入现有体系：

1. **常态对话工作**：主 Agent（meta/domain 顶层）与子 Agent 在任务超出能力时，经 `CallExternalAgent` 工具把任务派给外部 CLI 执行，输出回流会话上下文；
2. **编排工作流嵌入**：workflow 配方与 flow 节点图零引擎改动直接复用该工具；
3. **前端可视化**：聊天时间线调用卡片（已有通用渲染）、新增"外部 Agent"管理页、PATH 安装状态一眼可见。

## 2. 现状与调研结论（已验证）

### 2.1 内置工具机制（复用面）

- 工具契约 `Tool` 接口 `Name()/Aliases()/Execute(ctx,args)*Result`（`backend/internal/domain/tool/registry.go:46-54`）；注册表 `Registry`（`registry.go:100-183`），构造入口 `NewBuiltinRegistry(workDir, cfg, progress)`（`registry.go:187-263`），静态工具在 `registerDefaults()`（`registry.go:266-299`）。
- 对 LLM 暴露的 Schema：`Registry.Schema()`（`registry.go:1206-1667`），每个内置工具一个 `tools.NewFunc("Name", "描述", fn)` 块 + 入参结构体 json tag；动态工具走 `SchemaSource` 兜底（`registry.go:39-44`），`covered` map（`registry.go:1617-1626`）需同步加名。
- **只要走 `Registry.Dispatch` 并返回 `*Result`，以下全部自动生效**：tool_call/tool_result 事件（`registry.go:690/1034-1059`）→ `handleToolEvent` 落会话事件（`backend/internal/agent/service_react.go:3570-3649`）→ `session_events` 表持久化（`session_react.go:1133-1175`）→ history tool 消息 + 记忆流水线 MemoryEvent（`react_agent.go:951-971`）→ LiveEvent 前端实时流（`react_agent.go:889`）。长输出自动按 `tool_result_dump_runes` 落盘 `.bma/tool_outputs/`（`react_agent.go:1068-1110`）。
- 角色工具硬门 `role_tool_gate_enabled: true`（`config.yaml`）：白名单 = 角色静态 tools ∪ 可见插件工具（`backend/internal/bootstrap/bootstrap.go:348-417`），白名单外调用直接拒绝（`registry.go:692-708`）。角色 tools 列表在 `config/roles.yaml`。
- 审批链：工具实现 `Destructive()` 接口即接入（`destructive.go:25-27`），三级信任模式经 ctx TrustModeFunc（`registry.go:511-531`）；本机 `tool_approval_disabled: true` 全放行，命令黑名单不受影响。

### 2.2 子进程设施（复用面）

`RunCommand` 执行链（`backend/internal/domain/tool/builtin.go:1129-1283`）已解决同类问题，可直接复用：

| 设施 | 位置 | 用途 |
|---|---|---|
| `runCommandWithTreeKill` / `killProcessTree` | `builtin.go:1230-1283` | 超时树杀（Windows `taskkill /T /F`，Unix `kill -9 -PGID`） |
| `DecodeCommandOutput` | `result.go:155-167` | stdout/stderr 合并 + GBK 回退解码 |
| `workDirOf(ctx)` | `executor.go:173-180` | ctx 注入 > 构造目录 > cwd，取会话 workDir |
| `tool.WithWorkDir` / `WorkDirFromContext` | `sandbox.go:401-416` | workDir 的 ctx 传递 |
| `BMA_SESSION_TEMP_DIR` 环境变量 | `builtin.go:1188-1190` | 会话级临时目录约定 |
| 审批/黑名单 | `destructive.go:38-82`、`sandbox.go:38-59` | 危险命令模式 + 生产目录边界 + BlockedCmds |

### 2.3 编排引擎嵌入面（零改动依据）

- **workflow**：unit 无类型概念，执行固定走 `recipe.Tool → tool.Registry.Dispatch`（`backend/internal/workflow/executor.go:224`，args 由 `SynthArgs` 合成，`prompt.go:28`）。配方 `tool:` 填 `CallExternalAgent` 即嵌入，程序校验/评委/升级通道照常。
- **flow**：tool 节点 `execNode` case `NodeTypeTool`（`backend/internal/flow/engine.go:528-536`）→ `evalTool`（`nodes.go:213-223`）→ `Registry.Dispatch`；产出固定 `{result, path}` 两字段（`nodes.go:222`），外部 Agent 长输出靠落盘路径回填 result，够用。节点超时默认 120s（`config.yaml flow.node_timeout_ms`），长任务需调大。

### 2.4 插件机制为什么不贴合

现有插件 kind（`backend/internal/plugins/types.go:33-43`）：`mcp` 面向长驻 MCP server（握手/ListTools/重连监督，`mcpbridge/bridge.go:429-453`）；`service`/`process` 无工具注入；`bundle` 是 Claude/Codex 目录包展开。外部 Agent CLI 是"**一次调用 = 一次短命子进程**"（非 MCP 协议），包成 MCP 是削足适履。独立 builtin 工具是正解：注册表/审批/白名单/事件流全部继承。

### 2.5 前端现状

- 聊天时间线：`ToolCallCard.vue` 对工具名完全通用（折叠态标题 = 工具名原文 + 状态 tag），新工具零改动即可显示；`turns.ts:552-578` 的 `toolHeadline` 有按名特判的先例（可加 case 做"→ agent: 任务摘要"式折叠摘要）。
- 管理页骨架：`web/src/views/plugins/index.vue`（el-card 网格 + kind 徽标 + el-switch 热启停 + 元信息区）直接可复制。
- 后端 API 模式：简单资源 = handler 挂 `server.APIHandler` + 路由平铺在 `backend/internal/bootstrap/router.go:86-157`（样板 `server/plugins.go`、`server/tester_config.go`）。
- flow 编辑器节点类型是硬编码枚举（`FlowEditor.vue:306-313` 等 7 处），加真节点类型成本高，v1 不做。

## 3. 方案对比与选定

| 方案 | 说明 | 优点 | 缺点 | 结论 |
|---|---|---|---|---|
| A. RunCommand 直接跑 | 模型自己 `claude -p "..."` | 零开发 | 模型要记各家 CLI 语法；无超时/PATH 预检/注册表； prompt 注入风险（任务文本直接拼命令） | 否 |
| B. 包成 MCP 插件 | 外部 CLI 套 MCP server 壳 | 热插拔、角色可见性天花板 | 一次一进程的形态不匹配长驻 MCP 协议，实现扭曲 | 否 |
| C. **独立 builtin 工具 + 配置注册表**（选定） | `CallExternalAgent(agent, task)` 单工具 + `external_agents.yaml` 注册表 | 子进程设施/审批/事件流/前端全继承；注册表通用可扩；args 模板防注入拼接 | 需新增配置与工具文件 | **是** |

注入安全说明：方案 A 里模型生成的任务文本要拼进 shell 命令行；方案 C 中任务文本经 `{{task}}` 占位符渲染为 **argv 元素**（`exec.CommandContext` 数组形式，不经 shell 解析），命令注入面收敛为"外部 CLI 自身如何解释该参数"（各家 headless 模式均按文本处理）。

## 4. 详细设计

### 4.1 注册表配置 `config/external_agents.yaml`（新增）

```yaml
# 外部 Agent CLI 注册表。enabled=false 的条目不出现在工具描述里。
# args 模板：{{task}} 占位符替换为任务文本（以独立 argv 元素传入，不经 shell）；其余原样传递。
# timeout_sec 默认 1800（单调用墙钟上限，到期树杀）。
agents:
  claude_code:
    enabled: true
    command: claude
    args: ["-p", "{{task}}", "--output-format", "json"]
    timeout_sec: 1800
    description: "Anthropic Claude Code，复杂重构/长任务"
  codex:
    enabled: true
    command: codex
    args: ["exec", "{{task}}"]
    timeout_sec: 1800
    description: "OpenAI Codex CLI"
  kimi_code:
    enabled: true
    command: kimi
    args: ["-p", "{{task}}"]
    timeout_sec: 1800
    description: "Kimi Code CLI"
  opencode:
    enabled: true
    command: opencode
    args: ["run", "{{task}}"]
    timeout_sec: 1800
    description: "OpenCode CLI"
  hermes:
    enabled: false            # 无公认 headless CLI 约定，占位模板：填对 command/args 后启用
    command: hermes
    args: ["{{task}}"]
    timeout_sec: 1800
    description: ""
  deepseek_harness:
    enabled: false            # 同上，占位模板
    command: deepseek
    args: ["{{task}}"]
    timeout_sec: 1800
    description: ""
```

诚实声明：claude/codex/kimi/opencode 的 headless 参数是已知公开形态；Hermes 与 DeepSeek Harness 没有公认约定，先以 enabled:false 占位，用户在配置/管理页里填实际命令后启用。**注册表完全通用——任何能非交互执行的 CLI 都能注册，不限于这六家。**

### 4.2 配置解析 `backend/internal/config/external_agents.go`（新增）

照 `plugins.go`（`backend/internal/config/plugins.go:42-136`）三件套模式：

- `ExternalAgentsConfig{Agents map[string]ExternalAgentEntry}`；`ExternalAgentEntry{Enabled bool / Command string / Args []string / TimeoutSec int / Description string}`；
- `LoadExternalAgentsConfig(path)`：文件缺失返回空（零行为变化）；`${VAR:default}` env 插值（复用现有 resolveEnvVars 模式）；
- 归一化：空 command 的条目丢弃并记日志；timeout_sec<=0 回落 1800；无 `{{task}}` 占位符时任务文本追加为最后一个 argv。

### 4.3 工具实现 `backend/internal/domain/tool/external_agent.go`（新增）

**执行时序**（每次工具调用）：

```
LLM tool_call: CallExternalAgent{agent, task, timeout_sec?}
  └─ Registry.Dispatch（事件/白名单硬门/审批链自动）
      └─ externalAgentTool.Execute(ctx, args)
          ├─ 查注册表：未注册 / 条目 disabled → Result 错误"未注册的外部 Agent"
          ├─ exec.LookPath(command)：不在 PATH → Result 错误"未安装：command（请安装或检查 external_agents.yaml）"
          ├─ 渲染 args：{{task}} → task 文本（独立 argv 元素）
          ├─ exec.CommandContext(timeoutCtx, command, args...)
          │     cmd.Dir = e.workDirOf(ctx)        ← 外部 Agent 在会话工作目录干活
          │     env 注入 BMA_SESSION_TEMP_DIR
          ├─ 执行 + 树杀（复用 killProcessTree，到期整树终止）
          ├─ DecodeCommandOutput 合并解码；exit code 非 0 → Result 错误（输出保留进错误详情）
          └─ 返回 *Result（Output / Path）
              └─ 自动链：tool_result 事件 → session_events → history → 超长落盘 .bma/tool_outputs/
```

**工具契约**：

- 注册名 `CallExternalAgent`（PascalCase 与 `RunCommand`/`CallSubAgent` 一致），别名 `call_external_agent`；
- 入参：`agent`（注册表键，必填）、`task`（任务文本，必填，超软限附截断警告——同 `task_max_runes` 口径）、`timeout_sec`（可选，覆盖条目默认，封顶 7200）；
- `Destructive()` 返回 true：外部 CLI 副作用不可预期，接审批链（本机全放行，但生产目录边界/黑名单仍生效；换到多人环境审批自动恢复）；
- Schema 描述**动态内嵌当前启用清单**（`Schema()` 每次调用时从 runner 现取，热重载后下轮 LLM 调用即见新清单——已开跑会话当轮 schema 已冻结，可接受）。

### 4.4 注册与装配

- `registry.go`：`registerDefaults()` 注册 `externalAgentTool`；`Schema()` 加 `tools.NewFunc("CallExternalAgent", ...)` 块；`covered` map 加名；新增 `SetExternalAgentDefs(defs)` setter 供热更新。
- `bootstrap.go`：cfg 同目录推导 `external_agents.yaml`（缺文件不报错）→ `LoadExternalAgentsConfig` → `toolRegistry.SetExternalAgentDefs(...)`。
- `config/roles.yaml`：meta 与 domain 的 `tools:` 白名单加 `CallExternalAgent`（硬门开启，不加必被拒）。

### 4.5 管理 API（`backend/internal/server/external_agents.go` + 路由）

照 `server/plugins.go` handler 风格（`APIHandler` + `SetXxx` 注入）：

- `GET /api/external-agents` → 注册表清单（键/命令/args/超时/enabled/描述）+ 每项 `exec.LookPath` 状态（`installed` / `not_found`）——用户一眼看出哪台机器没装；
- `POST /api/external-agents/reload` → 重读 yaml + `SetExternalAgentDefs` 热更新（schema 描述随之刷新，无需重启）。

### 4.6 前端设计

a) **聊天时间线（零改动可用 + 微改增强）**：ToolCallCard 通用渲染自动生效。微改：`turns.ts` `toolHeadline` 加 `CallExternalAgent` case → 折叠摘要 `→ claude_code：任务前 80 字`。（注意：turns.ts 有未提交的用户改动，只做增量 case。）
b) **管理页 `web/src/views/external-agents/index.vue`（新增）**：照 plugins 页骨架——el-card 网格：名称 + 描述 + enabled el-switch + 命令行全文（command + args 拼接预览）+ timeout + PATH 状态徽标（已安装 green / 未安装 red）+ 顶部"重新加载"按钮；零配置态引导文案（提示编辑 `config/external_agents.yaml` 后重载）。API 封装 `web/src/api/external_agents.ts`（list/reload）。路由注册 `router/index.ts` children + 侧栏 meta（title/icon）。
c) **flow 编辑器**：v1 不动。tool 节点配置抽屉手填 `tool=CallExternalAgent`、`args={"agent":"claude_code","task":"..."}` 即可；v2 再做专用节点类型（后端 4 文件 + 前端 7 枚举点，独立任务）。

### 4.7 编排用法示例（实施后写进变更.md）

workflow 配方片段：

```yaml
id: external-refactor
tool: CallExternalAgent
deliverable: 重构后的 src/ 与验证输出
stages:
  - id: refactor
    instruction: |
      调用 claude_code 执行本阶段任务：{{item.task}}
      （args: {agent: claude_code, task: "{{item.task}}", timeout_sec: 1800}）
```

flow：tool 节点 `tool=CallExternalAgent`、`args.agent`/`args.task` 支持 `{{node.key}}` 变量引用上游输出；长任务先把 `flow.node_timeout_ms` 从 120000 调大。

### 4.8 提示词 Version

工具 schema 进系统提示 → 按 CLAUDE.md 规则 bump `backend/pkg/prompts` 的 Version。

## 5. 实施步骤

| # | 内容 | 产出 |
|---|---|---|
| 1 | 配置解析 `config/external_agents.go` + 单测（load/缺文件/env 插值/归一化） | 后端 |
| 2 | `config/external_agents.yaml` 六条内置模板 | 配置 |
| 3 | 工具实现 `domain/tool/external_agent.go` + 单测（os.Args[0] helper 进程假 CLI：跑通/非零退出/超时树杀/未注册/未安装/占位符渲染） | 后端 |
| 4 | `registry.go` 注册 + Schema 块 + covered + setter；`bootstrap.go` 接线；`roles.yaml` 白名单 | 后端 |
| 5 | `server/external_agents.go` + `router.go` + `apiHandlerOf` 装配 | 后端 |
| 6 | 前端 `api/external_agents.ts` + `views/external-agents/index.vue` + 路由 | 前端 |
| 7 | `turns.ts` toolHeadline 微改（增量 case） | 前端 |
| 8 | prompts Version bump | 后端 |
| 9 | 验证：`go build/vet/test`（config/tool/server 三包）；`web` 构建；冒烟：注册 echo 假 agent → 会话调用 → 事件流/落盘核对 | — |
| 10 | `doc/变更.md` 登记（含编排用法示例） | 文档 |

## 6. 验收标准

- 会话内模型可自主选择并调用已注册外部 Agent，结果（含超长输出落盘路径）正常回流历史与前端卡片；
- `enabled: false` 条目不出现在 schema 描述、调用时报明确错误；未安装 CLI 报"未安装"而非裸 exec 错误；
- 工具超时整树终止，无孤儿进程（Windows/Unix 双端）；
- 管理页可见全部条目 + PATH 状态，改 yaml 后 reload 即时生效；
- workflow 配方与 flow tool 节点各跑通一个最小用例；
- `go build ./... && go vet ./...` 全绿，相关包 `-count=1` 测试全绿，web 构建通过。

## 7. 风险与遗留

- **外部 Agent 不可控**：CLI 在会话 workDir 全权限跑（与 RunCommand 同级）；审批链在但本机默认全放行——以 enabled:false 默认关掉未确认 CLI 收敛。
- **超时语义**：工具执行是 ReAct 循环内同步阻塞，不受 `react_llm_timeout`（只管 LLM HTTP 请求）限制；flow 节点 120s 是独立上限，文档须提示调大。
- **上下文膨胀**：外部 Agent 长输出靠既有 `tool_result_dump_runes` 落盘收口，不新增风险。
- **schema 冻结**：已开跑会话当轮 schema 不随热重载刷新，下一轮生效，可接受。
- **v2 候选**：flow 专用节点类型（结构化参数表单/独立超时语义）、会话级并发闸、调用结果沉淀为项目经验（SessionEvolver 提取）。
