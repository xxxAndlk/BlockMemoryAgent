package graph

import (
	"context"
	"fmt"
	"github.com/blockmemory/agent/backend/pkg/types"
	"log"
	"strings"
	"time"
)

func (n *MetaAgentNode) ensureBlockArchived(ctx context.Context, state *types.ThreeLayerState, block *types.SessionBlock) {
	if n.blockMemory == nil || block == nil {
		return
	}
	// 异步归档已成功 → 跳过，避免重复落库
	if block.IsArchived() {
		return
	}
	// 摘要优先取 state.Reason（含各任务结果）；为空退化为块目标
	summary := state.Reason
	if summary == "" {
		summary = block.Goal
	}
	// 同步兜底：短超时，失败仅日志不阻塞
	bgCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := n.blockMemory.SaveBlockMemory(bgCtx, block.SessionID, block.Domain, block.Goal, summary, nil); err != nil {
		log.Printf("[MetaAgent] block memory fallback archive failed: session=%s domain=%s err=%v", block.SessionID, block.Domain, err)
		return
	}
	block.MarkArchived()
	log.Printf("[MetaAgent] block memory fallback archive ok: session=%s domain=%s", block.SessionID, block.Domain)
}

// collectBlockResult 收集block结果到session summary。
//
// 职责（P0-1）：
//   - 把块返回的 AgentResult.SummaryForUser 拼成段落，追加到 SessionSummary（给用户看的总结）。
//   - 把 AgentResult.MemoryForMeta 与 Facts 以 MetaMemoryEntry 形式追加到 state.MetaMemory（调度记忆）。
//
// 参数：
//   - state：图全局状态（原地修改 SessionSummary / MetaMemory）
//   - block：当前会话块
//
// 副作用：在 SessionSummary 末尾追加段落（用换行分隔）。
func (n *MetaAgentNode) collectBlockResult(state *types.ThreeLayerState, block *types.SessionBlock) {
	var parts []string
	// 段首加领域名标签
	if block.Domain != "" {
		parts = append(parts, fmt.Sprintf("【%s】", block.Domain))
	}

	// P0-1：优先使用块级 AgentResult
	if block.Result != nil {
		if block.Result.SummaryForUser != "" {
			parts = append(parts, block.Result.SummaryForUser)
		}
		if block.Result.MemoryForMeta != "" {
			state.MetaMemory = append(state.MetaMemory, types.MetaMemoryEntry{
				Timestamp: time.Now(),
				Source:    block.ID,
				Content:   block.Result.MemoryForMeta,
				Tags:      []string{"summary"},
			})
		}
		for _, fact := range block.Result.Facts {
			if strings.TrimSpace(fact) == "" {
				continue
			}
			state.MetaMemory = append(state.MetaMemory, types.MetaMemoryEntry{
				Timestamp: time.Now(),
				Source:    block.ID,
				Content:   fact,
				Tags:      []string{"fact"},
			})
		}
	}

	// 兼容旧路径：拼接各任务结果
	for task, result := range block.TaskResults {
		// 跳过空结果
		if result != "" {
			parts = append(parts, fmt.Sprintf("%s: %s", task, result))
		}
	}
	// 有内容则追加到 SessionSummary
	if len(parts) > 0 {
		// 已有摘要则先加换行
		if state.SessionSummary != "" {
			state.SessionSummary += "\n"
		}
		// 追加本块结果段落
		state.SessionSummary += strings.Join(parts, "\n")
	}
}

// finalizeSession 会话结束，生成最终回答。
//
// 职责：
//   - SessionSummary 为空时调 updateSessionSummary 生成默认摘要
//   - 有 modelFactory 时调 LLM 把各助手结果润色为最终回答
//   - LLM 超时/失败则保留原始结果
//
// 参数：
//   - ctx：请求上下文
//   - state：图全局状态（原地修改 SessionSummary）
//
// 副作用：可能用 LLM 重写 SessionSummary。
func (n *MetaAgentNode) finalizeSession(ctx context.Context, state *types.ThreeLayerState) {
	// 无摘要则生成默认摘要
	if state.SessionSummary == "" {
		n.updateSessionSummary(state)
		return
	}
	// 有模型工厂且未触发 LLM 跳过：调 LLM 润色最终回答
	if n.modelFactory != nil && !n.llmTracker.ShouldSkipLLM() {
		resp, err, timedOut := n.callLLM(ctx, fmt.Sprintf(`你是BlockMemoryAgent，一个本地AI开发助手。请基于以下各助手的执行结果，生成一个清晰、完整的最终回答给用户。

各助手执行结果：
%s

请直接输出最终回答，不要加任何前缀或总结性语句。回答请控制在 2000 字以内，保留关键结论与必要细节。`, state.SessionSummary))
		// 成功：替换为润色后的回答
		if !timedOut && err == nil && resp != "" {
			state.SessionSummary = resp // 替换为润色后的回答
			return
		}
		// 超时：保留原始结果并打印警告
		if timedOut {
			log.Printf("[MetaAgent] LLM 调用超时(最终汇总阶段)，保留原始结果。%s\n", n.llmTracker.StatsString())
		}
	}
}

// updateSessionSummary 更新会话总结。
//
// 职责：用步数/已完成块/活跃块数/当前领域拼一个简短摘要，写入 SessionSummary。
//
// 参数：
//   - state：图全局状态（原地修改 SessionSummary）
//
// 用途：定期被 Invoke 调用（按 summaryInterval），以及 finalizeSession 的回退路径。
func (n *MetaAgentNode) updateSessionSummary(state *types.ThreeLayerState) {
	var parts []string
	// 拼接各维度信息
	parts = append(parts, fmt.Sprintf("会话[%s]已执行%d步", state.SessionID, n.stepCount)) // 会话ID+步数
	parts = append(parts, fmt.Sprintf("完成领域: %v", state.CompletedBlocks))            // 已完成块列表
	parts = append(parts, fmt.Sprintf("活跃领域: %d个", len(state.ActiveBlocks)))         // 活跃块数量
	if state.CurrentDomain != "" {
		// 有当前领域则追加
		parts = append(parts, fmt.Sprintf("当前领域: %s", state.CurrentDomain))
	}
	// 用分号连接写入 SessionSummary
	state.SessionSummary = strings.Join(parts, "; ")
}
