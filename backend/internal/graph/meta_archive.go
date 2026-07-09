package graph

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/textutil"
	"github.com/blockmemory/agent/backend/pkg/types"
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
	// 清洗非法 UTF-8，避免写入 Postgres 时报 22021 编码错误
	domain, goal, sum, _ := sanitizeBlockMemoryInputs(block.Domain, block.Goal, summary, nil)
	if err := n.blockMemory.SaveBlockMemory(bgCtx, block.SessionID, domain, goal, sum, nil); err != nil {
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

	// R8: 把会话级 MetaMemory 同步到所有活跃块，让并行/后续块看到最新决策
	syncMetaMemoryToActiveBlocks(state)
}

// finalizeSession 会话结束，生成最终回答。
//
// 职责：
//   - SessionSummary 为空时调 updateSessionSummary 生成默认摘要
//   - 有 modelFactory 时调 LLM 把各助手结果润色为最终回答
//   - 最终回答按子Agent数量限长：默认500字，每调用一个子Agent增加200字
//   - LLM 超时/失败则对原始结果兜底截断
//
// 参数：
//   - ctx：请求上下文
//   - state：图全局状态（原地修改 SessionSummary）
//
// 副作用：可能用 LLM 重写 SessionSummary，并截断到动态字数上限。
func (n *MetaAgentNode) finalizeSession(ctx context.Context, state *types.ThreeLayerState) {
	limit := summaryLimit(state)

	// 无摘要则生成默认摘要
	if state.SessionSummary == "" {
		n.updateSessionSummary(state)
	} else if n.modelFactory != nil && !n.llmTracker.ShouldSkipLLM() {
		// 有模型工厂且未触发 LLM 跳过：调 LLM 润色最终回答
		resp, err, timedOut := n.CallLLM(ctx, fmt.Sprintf(`你是BlockMemoryAgent，一个本地AI开发助手。请基于以下各助手的执行结果，生成一个清晰、完整的最终回答给用户。

各助手执行结果：
%s

请直接输出最终回答，不要加任何前缀或总结性语句。回答请控制在 %d 字以内，保留关键结论与必要细节。`, state.SessionSummary, limit), LLMCallOptions{
			Caller:       "MetaAgent",
			InjectSoul:   true,
			UseMetaModel: true,
			Temperature:  &routingTemperature,
			SoftTimeout:  30 * time.Second,
			HardTimeout:  90 * time.Second,
		})
		// 成功：替换为润色后的回答
		if !timedOut && err == nil && resp != "" {
			state.SessionSummary = resp
		} else if timedOut {
			// 超时：保留原始结果并打印警告
			log.Printf("[MetaAgent] LLM 调用超时(最终汇总阶段)，保留原始结果。%s\n", n.llmTracker.StatsString())
		}
	}

	// 统一截断到动态字数上限（按 rune 计数，避免中文被字节截断）
	state.SessionSummary = truncateStringByRunes(state.SessionSummary, limit)
}

const (
	baseSummaryLimit = 500 // 默认总结字数上限
	perAgentLimit    = 200 // 每调用一个子Agent增加的字数
)

// subAgentCount 统计当前会话中已调用的非 MetaAgent 角色实例数。
func subAgentCount(state *types.ThreeLayerState) int {
	if state == nil {
		return 0
	}
	count := 0
	for _, inst := range state.RoleInstances {
		if inst != nil && inst.Type != enums.RoleTypeMeta {
			count++
		}
	}
	return count
}

// summaryLimit 根据子Agent数量计算 MetaAgent 总结的字数上限。
// 默认 500 字，每调用一个子Agent增加 200 字。
func summaryLimit(state *types.ThreeLayerState) int {
	return baseSummaryLimit + perAgentLimit*subAgentCount(state)
}

// truncateStringByRunes 按 rune（字符）截断字符串并加省略号。
// 统一委托给 textutil.TruncateRunes。
func truncateStringByRunes(s string, n int) string {
	return textutil.TruncateRunes(s, n, "...")
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
