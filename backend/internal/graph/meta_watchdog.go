package graph

import (
	"context"
	"fmt"
	"github.com/blockmemory/agent/backend/internal/watchdog"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
	"strings"
	"time"
)

// drainCommandQueue 处理用户指令队列（特性6）。
//
// 行为：
//   - IntentInterrupt：清空 ActiveBlocks / CallStack / CurrentBlockID 等上下文，
//     把新指令作为 DomainGoal，置 ActionContinue 让 Invoke 下一 tick 进入 handleInitial。
//     返回 true 表示已重置 state，调用方应立即返回。
//   - IntentEnqueue：把指令作为 user 消息追加到 state.Messages，继续当前任务。
//     返回 false。
//
// 没有队列或队列为空时返回 false。
func (n *MetaAgentNode) drainCommandQueue(ctx context.Context, state *types.ThreeLayerState) bool {
	// 若抢占中断与队列注入均未启用，直接跳过，避免不必要的队列 drain
	if n.rt == nil || n.rt.AgentCfg == nil || (!n.rt.AgentCfg.InterruptEnabled && !n.rt.AgentCfg.QueueInjectEnabled) {
		return false
	}
	items := n.rt.CmdQueue.Drain(state.SessionID)
	if len(items) == 0 {
		return false
	}
	for _, it := range items {
		switch it.Intent {
		case 1: // cmdqueue.IntentInterrupt
			if !n.rt.AgentCfg.InterruptEnabled {
				continue
			}
			n.emit(ctx, "intend", "抢占中断：暂停当前任务上下文，按新指令重新启动")
			// 阶段回落：保留 CompletedBlocks（已完成任务记忆不丢）；
			// 把 ActiveBlocks 移动到 PausedBlocks 暂存，未来可支持恢复；
			// 重置当前执行上下文，让下一 tick 进入 handleInitial 按新指令重新路由。
			if state.PausedBlocks == nil {
				state.PausedBlocks = make(map[string]*types.SessionBlock)
			}
			for id, block := range state.ActiveBlocks {
				state.PausedBlocks[id] = block
			}
			state.ActiveBlocks = make(map[string]*types.SessionBlock)
			state.CallStack = make([]*types.CallRequest, 0)
			state.CurrentBlockID = ""
			state.CurrentDomain = ""
			state.DomainGoal = it.Content // 新指令覆盖原 goal，下一 tick 由 handleInitial 重新拆分
			state.CurrentAssistantID = ""
			state.TargetRoleID = ""
			state.DirectExecute = false
			state.EnableSubdomain = false
			state.PendingClarify = nil // 同步丢弃旧的澄清请求，避免恢复后误挂起
			state.NextAction = enums.ActionContinue
			state.Reason = "interrupted by user"
			// 追加为最新用户消息：LLM 在新 handleInitial 中会读到这条消息作为输入
			state.Messages = append(state.Messages, types.ChatMessage{
				Role: enums.ChatRoleUser, Content: it.Content, Timestamp: time.Now(),
			})
			return true
		default: // IntentEnqueue
			// 仅追加消息，不重置状态；当前 tick 继续，下个 tick 起各 Agent 会读到新消息
			n.emit(ctx, "intend", "队列注入：追加用户指令到当前上下文")
			state.Messages = append(state.Messages, types.ChatMessage{
				Role: enums.ChatRoleUser, Content: it.Content, Timestamp: time.Now(),
			})
		}
	}
	// 多条 enqueue 都处理完不中断，调用方继续原 tick 流程
	return false
}

// runWatchdog 评估当前活跃 Agent 的上下文规模并视情况触发动作。
//
// 职责：
//   - 取当前块绑定的 Agent 列表，从 llmTracker 累加真实 input token 总量
//   - 调 Watchdog.Check 评估等级（用真实 token 数估算上下文规模，而非拼 block 文本）
//   - Evict 级别：仅记录警告（不再强制结束会话）
//   - Compress 级别：推送"建议压缩"提示并尝试 Episode 压缩
//   - Warn 级别：静默
//
// 参数：
//   - ctx：请求上下文
//   - state：图全局状态
//
// 设计意图：原实现拼接 block.Domain+Goal+TaskResults 文本估算 token，
// 该文本不含 blades Agent 内部 ReAct 循环累积的工具结果，导致真实上下文爆炸时
// watchdog 永不触发。改用 llmTracker 中该 block 已记录的 input token 总和
// 作为上下文规模代理——虽然单轮 input_tokens 含历史回灌，累计值更能反映
// 上下文增长趋势。仍调用 Watchdog.Check 走原阈值分级，保持决策一致性。
func (n *MetaAgentNode) runWatchdog(ctx context.Context, state *types.ThreeLayerState) {
	// Runtime 或 Watchdog 缺失则跳过
	if n.rt == nil || n.rt.Watchdog == nil {
		return
	}
	// 取当前活跃块；无块则跳过
	block := state.ActiveBlocks[state.CurrentBlockID]
	if block == nil {
		return
	}
	// 从 llmTracker 取该 block 内所有 Agent 的累计 input token。
	// block.Agents 元素形如 "assistant_2_session-_2" / "DomainAgent[xxx]"，
	// 在 CallRecord.Caller 中以子串形式存在（Caller 形如 "助手[临时助手]" / "DomainAgent[xxx]"）。
	// 这里按 block.Agents 中每个 agent 名累加；同时把 block.Domain 也计入，
	// 因为 DomainAgent 自身的 LLM 调用也贡献到上下文。
	var totalInput int
	if n.llmTracker != nil {
		for _, agentID := range block.Agents {
			in, _ := n.llmTracker.TokenTotalsByAgent(agentID)
			totalInput += in
		}
		// DomainAgent 自身的调用 Caller 含 block.Domain
		in, _ := n.llmTracker.TokenTotalsByAgent(block.Domain)
		totalInput += in
	}
	// 构造上下文规模代理文本：用 token 数生成定长字符串供 Estimator 估算回 token。
	// Watchdog.Estimator 是 bytes/4+1，这里直接传 totalInput*4 字节让 Check 得回 totalInput，
	// 保持 Watchdog 阈值语义不变。totalInput=0 时传空串让 Estimator 返回 0。
	var ctxText string
	if totalInput > 0 {
		ctxText = strings.Repeat("a", totalInput*4)
	} else {
		// 兜底：无 tracker 数据时退回原拼接逻辑，避免 watchdog 完全失活
		var ctxBuf strings.Builder
		ctxBuf.WriteString(block.Domain)
		ctxBuf.WriteString("\n")
		ctxBuf.WriteString(block.Goal)
		ctxBuf.WriteString("\n")
		for k, v := range block.TaskResults {
			ctxBuf.WriteString(k)
			ctxBuf.WriteString(": ")
			ctxBuf.WriteString(v)
			ctxBuf.WriteString("\n")
		}
		ctxText = ctxBuf.String()
	}
	// 调 Watchdog 评估
	d := n.rt.Watchdog.Check(state.CurrentBlockID, ctxText)
	// 按等级处理
	switch d.Level {
	case watchdog.LevelEvict:
		// 不再强制升级结束会话；仅记录警告，让当前任务继续完成。
		n.emitDetail(ctx, "wait",
			fmt.Sprintf("Watchdog 触发 EVICT（上下文 %d tokens 超硬阈值），已转为警告，不中断会话: %s", d.Tokens, d.Reason), "")
	case watchdog.LevelCompress:
		// 接近软阈值：推送建议压缩提示，并尝试对当前块主 Agent 执行 Episode 压缩
		n.emit(ctx, "think",
			fmt.Sprintf("Watchdog 提示上下文接近软阈值 (%d tokens)，建议后续压缩: %s", d.Tokens, d.Reason))
		if n.compressor != nil && len(block.Agents) > 0 {
			for _, agentID := range block.Agents {
				stats, err := n.compressor.Compress(ctx, agentID, state.SessionID)
				if err == nil && stats != nil {
					n.emit(ctx, "think", fmt.Sprintf("已对 Agent %s 执行 Episode 压缩: raw=%d standard=%d saved=%dB",
						agentID, stats.RawKept, stats.Compressed, stats.BytesBefore-stats.BytesAfter))
				}
			}
		}
	case watchdog.LevelWarn:
		// 静默
	}
}
