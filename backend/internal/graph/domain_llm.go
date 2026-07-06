package graph

import (
	"context"
	"fmt"
	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/soul"
	"time"
)

// callLLM 统一LLM调用入口（带自适应超时 + Prompt/Token 日志）。
// caller 固定为 "DomainAgent/任务拆解"。
func (n *DomainAgentNode) callLLM(ctx context.Context, prompt string) (string, error, bool) {
	return n.callLLMAs(ctx, "DomainAgent/任务拆解", prompt)
}

// callLightweightAs 以轻量模型身份执行 LLM 调用（用于 analyzeTasks 等简单文本任务）。
//
// 职责：
//   - 取轻量模型（比 domain_agent 模型更快、更便宜）
//   - 推送 prompt 调试事件
//   - 带短超时调用 LLM（15s 软 / 25s 硬）—— analyzeTasks 是简单文本拆分，
//     不需要 90s；长超时只让 graph loop 卡住（参见塔防 demo 事故：91s 超时 ×2）
//   - 推送 Token 消耗
func (n *DomainAgentNode) callLightweightAs(ctx context.Context, caller string, prompt string) (string, error, bool) {
	llm, err := n.modelFactory.GetLightweightModel(ctx)
	if err != nil {
		// 轻量模型不可用：回退到领域模型
		return n.callLLMAs(ctx, caller, prompt)
	}

	n.emitDetail(ctx, "prompt", fmt.Sprintf("[%s] 发送 Prompt (轻量,%d tokens)", caller, model.EstimateTokens(prompt)), model.SummarizePrompt(prompt, 500))

	// analyzeTasks 用短超时：15s 软 / 25s 硬。简单文本拆分 25s 足够，
	// 避免 kimi-for-coding 等 slow model 拖死 graph loop。
	softTimeout := 15 * time.Second
	hardTimeout := 25 * time.Second

	var resp string
	var callErr error
	var timedOut bool
	if t, ok := llm.(model.TemperatureAware); ok {
		desired := soul.Temperature(soul.KindRouting, 0)
		wrapped := &temperatureWrappedLLM{base: llm, t: t, temperature: desired}
		resp, callErr, timedOut = n.llmTracker.CallWithTimeout(ctx, wrapped, prompt, caller, softTimeout, hardTimeout)
	} else {
		resp, callErr, timedOut = n.llmTracker.CallWithTimeout(ctx, llm, prompt, caller, softTimeout, hardTimeout)
	}

	records := n.llmTracker.Records()
	if len(records) > 0 {
		last := records[len(records)-1]
		n.emitDetail(ctx, "token_usage",
			fmt.Sprintf("[%s] Token 消耗: in=%d out=%d dur=%v", caller, last.InputTokens, last.OutputTokens, last.Duration.Round(time.Millisecond)),
			"")
	}

	if resp != "" {
		n.emitDetail(ctx, "llm_response",
			fmt.Sprintf("[%s] LLM 响应 (轻量,%d 字符)", caller, len(resp)),
			model.SummarizePrompt(resp, 500))
	}

	return resp, callErr, timedOut
}

// callLLMAs 以指定调用者身份执行 LLM 调用。
//
// 职责：
//   - 取领域模型
//   - 推送 prompt 调试事件（含 token 估算与摘要）
//   - 带自适应超时调用 LLM（30s 软超时 / 90s 硬超时）
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
func (n *DomainAgentNode) callLLMAs(ctx context.Context, caller string, prompt string) (string, error, bool) {
	// 取领域模型；失败则直接返回错误
	llm, err := n.modelFactory.GetDomainModel(ctx)
	if err != nil {
		return "", err, false
	}

	// 推送 prompt 调试事件（含 token 估算与 500 字摘要）
	n.emitDetail(ctx, "prompt", fmt.Sprintf("[%s] 发送 Prompt (%d tokens)", caller, model.EstimateTokens(prompt)), model.SummarizePrompt(prompt, 500))

	// 带自适应超时调用 LLM：默认 30s 软超时 / 90s 硬超时，可被 AgentCfg 覆盖（特性2）
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
	resp, callErr, timedOut := n.llmTracker.CallWithTimeout(ctx, llm, prompt, caller,
		softTimeout,
		hardTimeout,
	)

	// 推送最近一次调用的 Token 消耗（in/out/耗时）
	records := n.llmTracker.Records()
	if len(records) > 0 {
		last := records[len(records)-1] // 取最新一条记录
		n.emitDetail(ctx, "token_usage",
			fmt.Sprintf("[%s] Token 消耗: in=%d out=%d dur=%v", caller, last.InputTokens, last.OutputTokens, last.Duration.Round(time.Millisecond)),
			"")
	}

	// 推送 LLM 响应摘要调试事件（500 字截断），便于排查任务拆解/子任务结果
	if resp != "" {
		n.emitDetail(ctx, "llm_response",
			fmt.Sprintf("[%s] LLM 响应 (%d 字符)", caller, len(resp)),
			model.SummarizePrompt(resp, 500))
	}

	return resp, callErr, timedOut
}
