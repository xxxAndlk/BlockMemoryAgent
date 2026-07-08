package graph

import (
	"context"
	"time"

	"github.com/blockmemory/agent/backend/internal/soul"
)

// domainLightweightSoftTimeout 与 domainLightweightHardTimeout 是 DomainAgentNode
// callLightweightAs 的硬编码短超时。该值不能套用 MetaAgent 的 30s/90s，
// 否则 analyzeTasks 的快速失败路径会丢失（参见塔防 demo 事故）。
const (
	domainLightweightSoftTimeout = 15 * time.Second
	domainLightweightHardTimeout = 25 * time.Second
)

// callLightweightAs 以轻量模型身份执行 LLM 调用（用于 analyzeTasks 等简单文本任务）。
//
// 职责：
//   - 取轻量模型；不可用则回退到领域模型（保留原超时行为）
//   - 带短超时调用 LLM（15s 软 / 25s 硬），无 AgentCfg 覆盖
//   - 通过 BaseAgentNode.CallLLM 统一推送 prompt / token_usage / llm_response 事件
//
// 注意：本方法仅保留节点特定的“轻量模型不可用时回退领域模型”语义，
// 实际 LLM 调用逻辑已全部收敛到 CallLLM。
func (n *DomainAgentNode) callLightweightAs(ctx context.Context, caller string, prompt string) (string, error, bool) {
	// 轻量模型不可用：回退到领域模型，使用默认 30s/90s 超时并允许 AgentCfg 覆盖。
	if _, err := n.modelFactory.GetLightweightModel(ctx); err != nil {
		return n.CallLLM(ctx, prompt, LLMCallOptions{Caller: caller})
	}

	// 轻量模型可用：使用硬编码短超时，禁用 AgentCfg 覆盖。
	temp := soul.Temperature(soul.KindRouting, 0)
	return n.CallLLM(ctx, prompt, LLMCallOptions{
		Caller:                 caller,
		Lightweight:            true,
		SoftTimeout:            domainLightweightSoftTimeout,
		HardTimeout:            domainLightweightHardTimeout,
		DisableAgentCfgTimeout: true,
		Temperature:            &temp,
		UseLightweightLabel:    true,
	})
}
