# LLM 调用层基线盘点

> Task 1.2 Step 0 输出：三层（Meta/Domain/SubDomain）现有 `callLLMAs` / `callLightweightAs`
> 的超时、soul 注入、模型选择、emitDetail 行为。作为统一 `BaseAgentNode.CallLLM` 的默认值与行为保真依据。

## 1. 超时常量

| 节点 | 方法 | 默认 soft | 默认 hard | AgentCfg 覆盖 | 说明 |
|---|---|---|---|---|---|
| MetaAgentNode | `callLLMAs` | 30s | 90s | 是 | `n.rt.AgentCfg.LLMSoftTimeoutSec` / `LLMHardTimeoutSec` |
| MetaAgentNode | `callLightweightAs` | 30s | 90s | 是 | 与 `callLLMAs` 相同 |
| DomainAgentNode | `callLLMAs` | 30s | 90s | 是 | `n.rt.AgentCfg.LLMSoftTimeoutSec` / `LLMHardTimeoutSec` |
| DomainAgentNode | `callLightweightAs` | **15s** | **25s** | **否** | 硬编码；轻量模型不可用时回退 `callLLMAs`（30s/90s，可覆盖） |
| SubDomainAgentNode | `callLLMAs` | 30s | 90s | 是 | `n.rt.AgentCfg.LLMSoftTimeoutSec` / `LLMHardTimeoutSec` |
| SubDomainAgentNode | `callLightweightAs` | — | — | — | 不存在 |

## 2. Soul 注入

| 节点 | 方法 | 是否注入 soul | 条件 |
|---|---|---|---|
| MetaAgentNode | `callLLMAs` / `callLightweightAs` | 是 | `n.rt != nil && n.rt.Soul != nil`，调用 `n.rt.Soul.Inject(prompt)` |
| DomainAgentNode | `callLLMAs` / `callLightweightAs` | 否 | — |
| SubDomainAgentNode | `callLLMAs` | 否 | — |

## 3. 模型选择

| 节点 | 方法 | 模型 |
|---|---|---|
| MetaAgentNode | `callLLMAs` | `n.modelFactory.GetMetaModel(ctx)` |
| MetaAgentNode | `callLightweightAs` | `n.modelFactory.GetLightweightModel(ctx)` |
| DomainAgentNode | `callLLMAs` | `n.modelFactory.GetDomainModel(ctx)` |
| DomainAgentNode | `callLightweightAs` | `n.modelFactory.GetLightweightModel(ctx)`，失败回退 `GetDomainModel`（通过 `callLLMAs`） |
| SubDomainAgentNode | `callLLMAs` | `n.modelFactory.GetDomainModel(ctx)` |

## 4. 温度控制

- MetaAgentNode `callLLMAs` / `callLightweightAs`：若模型实现 `model.TemperatureAware`，用 `temperatureWrappedLLM` 包裹，温度取 `soul.Temperature(soul.KindRouting, 0)`（=0）。
- DomainAgentNode `callLLMAs`：无温度包装，直接调用。
- DomainAgentNode `callLightweightAs`：与 Meta 相同，使用 0 温度包装。
- SubDomainAgentNode `callLLMAs`：无温度包装。

## 5. emitDetail 事件顺序与 kind

所有方法统一按以下顺序推送 `ProgressEvent`：

1. `prompt`：调用前，message 形如 `[<caller>] 发送 Prompt (%d tokens)`，detail 为 500 字摘要。
2. `token_usage`：调用后，message 形如 `[<caller>] Token 消耗: in=%d out=%d dur=%v`，detail 为空。
3. `llm_response`：调用后且 `resp != ""`，message 形如 `[<caller>] LLM 响应 (%d 字符)`，detail 为 500 字摘要。

Domain `callLightweightAs` 的 message 会额外标注 `(轻量)`，例如：
- prompt: `[<caller>] 发送 Prompt (轻量,%d tokens)`
- llm_response: `[<caller>] LLM 响应 (轻量,%d 字符)`

## 6. llmTracker 调用顺序

- 先 `llmTracker.CallWithTimeout(ctx, llm, prompt, caller, softTimeout, hardTimeout)`
- 再 `llmTracker.Records()` 取最后一条记录，用于 `token_usage` 事件。

## 7. 风险点（RISK）

- RISK_7/8：`domain_llm.go` `callLightweightAs` 的 15s/25s 与 Meta 的 30s/90s 不同，统一时不可用 Meta 默认值覆盖。
- RISK：Domain 轻量模型不可用时回退到 `callLLMAs`，此时超时变回 30s/90s 且允许 AgentCfg 覆盖。
- RISK：Meta/Domain 的 `callLightweightAs`  soul 注入行为不同（Meta 注入，Domain 不注入），统一层需由 `InjectSoul` 选项控制。
