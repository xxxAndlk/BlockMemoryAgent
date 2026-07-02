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
			n.emit(ctx, "intend", "抢占中断：清空当前上下文，按新指令重新启动")
			// 清空图状态，保留 SessionID 与 Messages 中的历史对话
			// 注意：必须重置 ActiveBlocks/CallStack/CurrentBlockID 三件套，
			//       否则下一 tick 仍可能跳进旧 DomainAgent 的子任务路径
			state.ActiveBlocks = make(map[string]*types.SessionBlock)
			state.CompletedBlocks = nil
			state.CallStack = make([]*types.CallRequest, 0)
			state.CurrentBlockID = ""
			state.CurrentDomain = ""
			state.DomainGoal = it.Content // 新指令覆盖原 goal，下一 tick 由 handleInitial 重新拆分
			state.CurrentAssistantID = ""
			state.TargetRoleID = ""
			state.DirectExecute = false
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
//   - 取当前块的目标 + 任务结果作为粗略上下文规模代理
//   - 调 Watchdog.Check 评估等级
//   - Evict 级别：仅记录警告（不再强制结束会话）
//   - Compress 级别：推送"建议压缩"提示
//   - Warn 级别：静默
//
// 参数：
//   - ctx：请求上下文
//   - state：图全局状态
//
// 设计意图：原实现 Evict 级别会注入 EventEscalation，导致会话被强制结束。
// 实际场景下（如读大文件）一次工具输出就可能超硬阈值，强制结束会让
// 写文件等关键动作来不及执行。现在 Evict 仅记录警告事件，不中断会话；
// 真正的上下文压缩留待后续按 LevelCompress 实现。
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
	// 拼接粗略上下文：领域 + 目标 + 各任务结果
	var ctxBuf strings.Builder
	ctxBuf.WriteString(block.Domain) // 写入领域名
	ctxBuf.WriteString("\n")         // 换行
	ctxBuf.WriteString(block.Goal)   // 写入领域目标
	ctxBuf.WriteString("\n")         // 换行
	for k, v := range block.TaskResults {
		ctxBuf.WriteString(k)    // 写入任务名
		ctxBuf.WriteString(": ") // 分隔符
		ctxBuf.WriteString(v)    // 写入任务结果
		ctxBuf.WriteString("\n") // 换行
	}
	// 调 Watchdog 评估
	d := n.rt.Watchdog.Check(state.CurrentBlockID, ctxBuf.String())
	// 按等级处理
	switch d.Level {
	case watchdog.LevelEvict:
		// 不再强制升级结束会话；仅记录警告，让当前任务继续完成。
		n.emitDetail(ctx, "wait",
			fmt.Sprintf("Watchdog 触发 EVICT（上下文 %d tokens 超硬阈值），已降级为警告，不中断会话: %s", d.Tokens, d.Reason), "")
	case watchdog.LevelCompress:
		// 接近软阈值：推送建议压缩提示，并尝试对当前块主 Agent 执行 Episode 压缩
		n.emit(ctx, "think",
			fmt.Sprintf("Watchdog 提示上下文接近软阈值 (%d tokens)，建议后续压缩: %s", d.Tokens, d.Reason))
		if n.compressor != nil {
			if block != nil && len(block.Agents) > 0 {
				for _, agentID := range block.Agents {
					if err := n.compressor.Compress(ctx, agentID, state.SessionID); err == nil {
						n.emit(ctx, "think", fmt.Sprintf("已对 Agent %s 执行 Episode 压缩", agentID))
					}
				}
			}
		}
	case watchdog.LevelWarn:
		// 静默
	}
}
