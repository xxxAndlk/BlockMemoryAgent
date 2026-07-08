package graph

import (
	"context"
	"fmt"
	"time"

	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/soul"
)

// defaultLLMSoftTimeout 与 defaultLLMHardTimeout 是三层节点共用的默认 LLM 超时。
// 仅当调用方未在 LLMCallOptions 中显式指定 SoftTimeout/HardTimeout 时使用。
const (
	defaultLLMSoftTimeout = 30 * time.Second
	defaultLLMHardTimeout = 90 * time.Second
)

// routingTemperature 是路由/总结类调用的固定温度（确定性输出）。
// 与原先 `soul.Temperature(soul.KindRouting, 0)` 等价。
var routingTemperature = soul.Temperature(soul.KindRouting, 0)

// LLMCallOptions 封装 BaseAgentNode.CallLLM 的调用选项。
//
// 设计意图：把 Meta/Domain/SubDomain 三层原本重复的 LLM 调用逻辑收敛到 CallLLM，
// 同时保留各节点在超时、soul 注入、模型选择上的差异。
type LLMCallOptions struct {
	// Caller 是调用方标识，用于事件展示与 llmTracker 记录。
	Caller string

	// Lightweight 为 true 时使用轻量模型（GetLightweightModel）。
	Lightweight bool

	// UseMetaModel 为 true 且 Lightweight 为 false 时，使用 MetaAgent 模型（GetMetaModel）。
	// 仅 MetaAgent 的非轻量调用置为 true；Domain/SubDomain 使用 domain 模型。
	UseMetaModel bool

	// InjectSoul 为 true 时，在 prompt 前注入 soul.md 人格内容。
	// 仅 MetaAgent 调用置为 true；Domain/SubDomain 不注入。
	InjectSoul bool

	// Temperature 非 nil 且模型支持 TemperatureAware 时，用该温度包装调用。
	// nil 表示不叠加温度包装（与 Domain/SubDomain callLLMAs 原行为一致）。
	Temperature *float64

	// SoftTimeout / HardTimeout 为本次调用指定的超时。
	// 零值时使用 defaultLLMSoftTimeout / defaultLLMHardTimeout。
	SoftTimeout time.Duration
	HardTimeout time.Duration

	// DisableAgentCfgTimeout 为 true 时，禁用 AgentCfg 对超时的覆盖。
	// 用于 DomainAgentNode.callLightweightAs 的硬编码 15s/25s 短超时路径。
	DisableAgentCfgTimeout bool

	// UseLightweightLabel 为 true 时，prompt / llm_response 事件的 message 中附加
	// "(轻量)" 标注，与原先 DomainAgentNode.callLightweightAs 的展示行为一致。
	UseLightweightLabel bool
}

// CallLLM 是三层节点统一的 LLM 调用入口。
//
// 职责：
//   - 按 Lightweight 取模型
//   - 按 InjectSoul 注入 soul.md 人格
//   - 推送 prompt 调试事件
//   - 解析超时（选项显式值 → AgentCfg 覆盖 → 默认值）
//   - 可选温度包装后调用 llmTracker.CallWithTimeout
//   - 推送 token_usage / llm_response 调试事件
//
// 返回：(响应文本, 错误, 是否超时)。
func (b *BaseAgentNode) CallLLM(ctx context.Context, prompt string, opts LLMCallOptions) (string, error, bool) {
	// 1. 取模型。
	llm, err := b.resolveLLM(ctx, opts)
	if err != nil {
		return "", err, false
	}

	// 2. 注入 soul（仅 MetaAgent 调用启用）。
	if opts.InjectSoul {
		if b.rt != nil && b.rt.Soul != nil {
			prompt = b.rt.Soul.Inject(prompt)
		}
	}

	// 3. 推送 prompt 调试事件。
	promptMsg := fmt.Sprintf("[%s] 发送 Prompt (%d tokens)", opts.Caller, model.EstimateTokens(prompt))
	if opts.UseLightweightLabel {
		promptMsg = fmt.Sprintf("[%s] 发送 Prompt (轻量,%d tokens)", opts.Caller, model.EstimateTokens(prompt))
	}
	b.emitDetail(ctx, "prompt", promptMsg, model.SummarizePrompt(prompt, 500))

	// 4. 解析超时。
	softTimeout, hardTimeout := b.resolveTimeouts(opts)

	// 5. 可选温度包装后调用 LLM。
	var resp string
	var callErr error
	var timedOut bool
	if opts.Temperature != nil {
		if t, ok := llm.(model.TemperatureAware); ok {
			wrapped := &temperatureWrappedLLM{base: llm, t: t, temperature: *opts.Temperature}
			resp, callErr, timedOut = b.llmTracker.CallWithTimeout(ctx, wrapped, prompt, opts.Caller,
				softTimeout, hardTimeout)
		} else {
			resp, callErr, timedOut = b.llmTracker.CallWithTimeout(ctx, llm, prompt, opts.Caller,
				softTimeout, hardTimeout)
		}
	} else {
		resp, callErr, timedOut = b.llmTracker.CallWithTimeout(ctx, llm, prompt, opts.Caller,
			softTimeout, hardTimeout)
	}

	// 6. 推送 token_usage / llm_response 调试事件。
	b.emitLLMEvents(ctx, opts.Caller, resp, opts.UseLightweightLabel)

	return resp, callErr, timedOut
}

// resolveLLM 根据 opts.Lightweight / UseMetaModel 选择并返回模型客户端。
func (b *BaseAgentNode) resolveLLM(ctx context.Context, opts LLMCallOptions) (model.LLMClient, error) {
	if b.modelFactory == nil {
		return nil, fmt.Errorf("modelFactory is nil")
	}
	if opts.Lightweight {
		return b.modelFactory.GetLightweightModel(ctx)
	}
	if opts.UseMetaModel {
		return b.modelFactory.GetMetaModel(ctx)
	}
	// SubDomain 与 Domain 的非轻量调用都使用 domain 模型。
	return b.modelFactory.GetDomainModel(ctx)
}

// resolveTimeouts 解析本次调用的软/硬超时。
//
// 优先级：
//  1. opts.SoftTimeout / opts.HardTimeout 非零时使用显式值
//  2. 未禁用 AgentCfg 覆盖时，读取 b.rt.AgentCfg.LLMSoftTimeoutSec / LLMHardTimeoutSec
//  3. 使用 defaultLLMSoftTimeout / defaultLLMHardTimeout
func (b *BaseAgentNode) resolveTimeouts(opts LLMCallOptions) (time.Duration, time.Duration) {
	softTimeout := opts.SoftTimeout
	if softTimeout == 0 {
		softTimeout = defaultLLMSoftTimeout
	}
	hardTimeout := opts.HardTimeout
	if hardTimeout == 0 {
		hardTimeout = defaultLLMHardTimeout
	}

	if !opts.DisableAgentCfgTimeout && b.rt != nil && b.rt.AgentCfg != nil {
		if b.rt.AgentCfg.LLMSoftTimeoutSec > 0 {
			softTimeout = time.Duration(b.rt.AgentCfg.LLMSoftTimeoutSec) * time.Second
		}
		if b.rt.AgentCfg.LLMHardTimeoutSec > 0 {
			hardTimeout = time.Duration(b.rt.AgentCfg.LLMHardTimeoutSec) * time.Second
		}
	}

	return softTimeout, hardTimeout
}

// emitLLMEvents 推送 token_usage 与 llm_response 调试事件。
func (b *BaseAgentNode) emitLLMEvents(ctx context.Context, caller, resp string, lightweightLabel bool) {
	records := b.llmTracker.Records()
	if len(records) > 0 {
		last := records[len(records)-1]
		b.emitDetail(ctx, "token_usage",
			fmt.Sprintf("[%s] Token 消耗: in=%d out=%d dur=%v", caller, last.InputTokens, last.OutputTokens, last.Duration.Round(time.Millisecond)),
			"")
	}

	if resp != "" {
		respMsg := fmt.Sprintf("[%s] LLM 响应 (%d 字符)", caller, len(resp))
		if lightweightLabel {
			respMsg = fmt.Sprintf("[%s] LLM 响应 (轻量,%d 字符)", caller, len(resp))
		}
		b.emitDetail(ctx, "llm_response", respMsg, model.SummarizePrompt(resp, 500))
	}
}

// temperatureWrappedLLM 在 LLMClient 外层叠加 per-call temperature。
// 设计意图：让不支持运行时改温度的 ChatModel 也能按场景设置不同温度，
// 而不修改 modelFactory 缓存的实例。
type temperatureWrappedLLM struct {
	base        model.LLMClient
	t           model.TemperatureAware
	temperature float64
}

// Generate 实现 model.LLMClient 接口，转发到带温度选项的生成方法。
func (w *temperatureWrappedLLM) Generate(ctx context.Context, prompt string) (string, error) {
	return w.t.GenerateWithOptions(ctx, prompt, w.temperature)
}
