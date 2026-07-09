package memory

import (
	"context"
	"log"
	"time"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// Broadcaster 是回调处理器对外部广播器的最小依赖接口，避免 memory 包依赖 server 包造成循环引用。
type Broadcaster interface {
	BroadcastAgentStatus(topicID string, payload types.AgentStatusPayload)
	BroadcastEpisode(topicID string, payload types.EpisodePayload)
}

// CallbackHandler 记忆回调处理器：在 Graph 节点生命周期事件上驱动记忆写入与状态广播。
// 按设计文档建议，记忆写入在节点结束时显式同步调用，避免回调遗漏与重试复杂度。
type CallbackHandler struct {
	writeProcessor *WriteProcessor  // Episode 写入流水线
	snapshotMgr    *SnapshotManager // 快照两级存储管理
	broadcaster    Broadcaster      // TUI / SSE 事件广播器，可为 nil
}

// NewCallbackHandler 创建回调处理器，注入核心依赖。
// 参数：writeProcessor 记忆写入器；snapshotMgr 快照管理器；broadcaster TUI 广播器（可空）。
// 返回：组装好的 *CallbackHandler。
// 副作用：无（不再启动后台 worker）。
func NewCallbackHandler(writeProcessor *WriteProcessor, snapshotMgr *SnapshotManager, broadcaster Broadcaster) *CallbackHandler {
	return &CallbackHandler{
		writeProcessor: writeProcessor,
		snapshotMgr:    snapshotMgr,
		broadcaster:    broadcaster,
	}
}

// OnStart Node 开始回调：广播 Agent 进入 ACTIVE 状态。
// 参数：ctx 取消信号；agentID Agent 实例 ID；topicID 话题 ID。
// 返回：无。
// 副作用：当 broadcaster 非空时，向该 Topic 推送 agent.status 事件。
func (h *CallbackHandler) OnStart(ctx context.Context, agentID, topicID string) {
	if h.broadcaster != nil {
		h.broadcaster.BroadcastAgentStatus(topicID, types.AgentStatusPayload{
			AgentID: agentID,
			State:   "ACTIVE",
		})
	}
}

// OnEnd Node 结束回调：显式同步写入 Episode 与快照。
//
// 按设计文档 §4.4 建议，记忆写入在 return 前最后一行显式调用，100% 执行，失败可追踪。
// 本实现不再使用内存队列、后台 worker、指数退避重试或死信表。
//
// 参数：
//   - ctx: 取消信号。
//   - agentID: Agent 实例 ID。
//   - topicID: 话题 ID。
//   - action: 本步动作摘要，写入 Episode.Action。
//   - rawContent: 原始输出文本，写入 Episode 并作为快照摘要。
//   - stepCount: 当前步骤序号，作为快照版本号与幂等键。
//
// 返回：无。
// 副作用：同步触发 Episode 写入、快照保存与 TUI 状态广播；写入失败仅记录日志，不阻塞主路径。
func (h *CallbackHandler) OnEnd(ctx context.Context, agentID, topicID, action, rawContent string, stepCount int) {
	// 1. 同步写入 Episode
	if h.writeProcessor != nil {
		ep, err := h.writeProcessor.ProcessWithStepCount(ctx, agentID, topicID, action, rawContent, stepCount)
		if err != nil {
			log.Printf("[memory_write] episode_write_failed session=%s agent=%s step=%d action=%s err=%v", topicID, agentID, stepCount, action, err)
		} else if h.broadcaster != nil {
			h.broadcaster.BroadcastEpisode(topicID, types.EpisodePayload{
				AgentID:    agentID,
				StepID:     ep.StepID,
				Importance: ep.Importance,
				Summary:    ep.ObservationSummary,
				Facts:      ep.Facts,
				Timestamp:  ep.Timestamp,
				ToolCalls:  ep.ToolCalls,
			})
		}
	}

	// 2. 同步保存快照
	if h.snapshotMgr != nil && h.writeProcessor != nil {
		episodes, err := h.writeProcessor.store.GetEpisodes(ctx, agentID, topicID, 10)
		if err != nil {
			log.Printf("[memory_write] snapshot_get_episodes_failed session=%s agent=%s step=%d err=%v", topicID, agentID, stepCount, err)
		} else {
			output := &types.AgentOutput{
				AgentID:   agentID,
				Version:   stepCount,
				Summary:   rawContent,
				Timestamp: time.Now(),
			}
			if err := h.snapshotMgr.SaveFromState(ctx, agentID, topicID, output, episodes); err != nil {
				log.Printf("[memory_write] snapshot_save_failed session=%s agent=%s step=%d err=%v", topicID, agentID, stepCount, err)
			}
		}
	}

	// 3. 广播 Agent 进入 WAITING 状态
	if h.broadcaster != nil {
		h.broadcaster.BroadcastAgentStatus(topicID, types.AgentStatusPayload{
			AgentID:     agentID,
			State:       "WAITING",
			CurrentStep: stepCount,
			LastOutput:  rawContent,
		})
	}
}

// OnError 错误回调：广播 Agent 进入 ERROR 状态，便于 TUI 显著提示。
func (h *CallbackHandler) OnError(ctx context.Context, agentID, topicID string, err error) {
	if h.broadcaster != nil {
		h.broadcaster.BroadcastAgentStatus(topicID, types.AgentStatusPayload{
			AgentID: agentID,
			State:   "ERROR",
		})
	}
}

// Close 关闭回调处理器。
// 当前实现无需释放资源，保留接口以兼容现有调用方。
func (h *CallbackHandler) Close() error {
	return nil
}
