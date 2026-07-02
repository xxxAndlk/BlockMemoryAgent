package graph

import (
	"context"
	"fmt"
	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/soul"
	"time"
)

// callLLM 统一的LLM调用入口。
// 职责：以 "MetaAgent" 身份调用 callLLMAs。
// 带 自适应超时 + 人格注入 + 温度调节 + Prompt/Token 日志。
func (n *MetaAgentNode) callLLM(ctx context.Context, prompt string) (string, error, bool) {
	return n.callLLMAs(ctx, "MetaAgent", prompt)
}

// callLLMAs 以指定调用者身份执行 LLM 调用。
//
// 职责：
//   - 取 Meta 模型
//   - 注入人格（soul.md）
//   - 推送 prompt 调试事件
//   - 用 0 温度（路由/总结场景）+ 自适应超时调用 LLM
//   - 推送最近一次调用的 Token 消耗
//
// 参数：
//   - ctx：请求上下文
//   - caller：调用者标识，用于事件展示
//   - prompt：发送给 LLM 的完整 prompt
//
// 返回：(响应文本, 错误, 是否超时)。
//
// 副作用：通过 emitDetail 推送 prompt 与 token_usage 事件。
func (n *MetaAgentNode) callLLMAs(ctx context.Context, caller string, prompt string) (string, error, bool) {
	// 取 Meta 模型；失败则直接返回错误
	llm, err := n.modelFactory.GetMetaModel(ctx)
	if err != nil {
		return "", err, false
	}

	// 注入人格（soul.md 内容拼到 prompt 前部）
	if n.rt != nil && n.rt.Soul != nil {
		prompt = n.rt.Soul.Inject(prompt)
	}

	// 发送 prompt 调试事件（含 token 估算与 500 字摘要）
	n.emitDetail(ctx, "prompt", fmt.Sprintf("[%s] 发送 Prompt (%d tokens)", caller, model.EstimateTokens(prompt)), model.SummarizePrompt(prompt, 500))

	// MetaAgent 主要做"路由 / 总结"决策，使用 0 温度
	var resp string   // LLM 响应文本
	var callErr error // 调用错误
	var timedOut bool // 是否超时
	// 解析 LLM 软/硬超时：默认 30s/90s，可被 AgentCfg 覆盖（特性2）
	softTimeout := 30 * time.Second
	hardTimeout := 90 * time.Second
	if n.rt != nil && n.rt.AgentCfg != nil {
		if n.rt.AgentCfg.LLMSoftTimeoutSec > 0 {
			softTimeout = time.Duration(n.rt.AgentCfg.LLMSoftTimeoutSec) * time.Second
		}
		if n.rt.AgentCfg.LLMHardTimeoutSec > 0 {
			hardTimeout = time.Duration(n.rt.AgentCfg.LLMHardTimeoutSec) * time.Second
		}
	}
	// 若 LLM 支持 TemperatureAware，用温度包装器叠加 0 温度
	if t, ok := llm.(model.TemperatureAware); ok {
		desired := soul.Temperature(soul.KindRouting, 0)                         // 路由场景温度
		wrapped := &temperatureWrappedLLM{base: llm, t: t, temperature: desired} // 包装器
		resp, callErr, timedOut = n.llmTracker.CallWithTimeout(ctx, wrapped, prompt, caller,
			softTimeout, hardTimeout)
	} else {
		// 不支持温度控制：直接调用
		resp, callErr, timedOut = n.llmTracker.CallWithTimeout(ctx, llm, prompt, caller,
			softTimeout, hardTimeout)
	}

	// 发送 token_usage 调试事件（从 tracker 最新记录读取）
	records := n.llmTracker.Records()
	if len(records) > 0 {
		last := records[len(records)-1] // 取最新一条记录
		n.emitDetail(ctx, "token_usage",
			fmt.Sprintf("[%s] Token 消耗: in=%d out=%d dur=%v", caller, last.InputTokens, last.OutputTokens, last.Duration.Round(time.Millisecond)),
			"")
	}

	// 发送 LLM 响应摘要调试事件（500 字截断），便于排查决策依据
	if resp != "" {
		n.emitDetail(ctx, "llm_response",
			fmt.Sprintf("[%s] LLM 响应 (%d 字符)", caller, len(resp)),
			model.SummarizePrompt(resp, 500))
	}

	return resp, callErr, timedOut
}

// temperatureWrappedLLM 在 LLMClient 外层叠加 per-call temperature。
// 设计意图：让不支持运行时改温度的 ChatModel 也能按场景（路由/创作）
// 设置不同温度，而不修改 modelFactory 缓存的实例。
type temperatureWrappedLLM struct {
	base        model.LLMClient        // 被包装的底层客户端
	t           model.TemperatureAware // 温度感知接口
	temperature float64                // 本次调用使用的温度
}

// Generate 实现 model.LLMClient 接口，转发到带温度选项的生成方法。
func (w *temperatureWrappedLLM) Generate(ctx context.Context, prompt string) (string, error) {
	return w.t.GenerateWithOptions(ctx, prompt, w.temperature)
}
