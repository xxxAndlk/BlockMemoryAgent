package memory

import (
	"context"
	"time"

	"github.com/blockmemory/agent/backend/pkg/types"
	"github.com/blockmemory/agent/backend/internal/server"
)

// CallbackHandler 记忆回调处理器
type CallbackHandler struct {
	writeProcessor *WriteProcessor
	snapshotMgr    *SnapshotManager
	broadcaster    *server.TUIBroadcaster
}

// NewCallbackHandler 创建回调处理器
func NewCallbackHandler(writeProcessor *WriteProcessor, snapshotMgr *SnapshotManager, broadcaster *server.TUIBroadcaster) *CallbackHandler {
	return &CallbackHandler{
		writeProcessor: writeProcessor,
		snapshotMgr:    snapshotMgr,
		broadcaster:    broadcaster,
	}
}

// OnStart Node 开始回调
func (h *CallbackHandler) OnStart(ctx context.Context, agentID, topicID string) {
	if h.broadcaster != nil {
		h.broadcaster.BroadcastAgentStatus(topicID, types.AgentStatusPayload{
			AgentID: agentID,
			State:   "ACTIVE",
		})
	}
}

// OnEnd Node 结束回调
func (h *CallbackHandler) OnEnd(ctx context.Context, agentID, topicID, action, rawContent string, stepCount int) {
	// 1. 异步写入 Episode
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		episode, err := h.writeProcessor.Process(ctx, agentID, topicID, action, rawContent)
		if err != nil {
			return
		}

		// 广播 Episode
		if h.broadcaster != nil {
			h.broadcaster.BroadcastEpisode(topicID, types.EpisodePayload{
				AgentID:    agentID,
				StepID:     episode.StepID,
				Importance: episode.Importance,
				Summary:    episode.ObservationSummary,
				Facts:      episode.Facts,
				Timestamp:  episode.Timestamp,
				ToolCalls:  episode.ToolCalls,
			})
		}
	}()

	// 2. 保存快照
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		// 获取最近 Episode
		episodes, _ := h.writeProcessor.store.GetEpisodes(ctx, agentID, topicID, 10)

		// 构建临时输出
		output := &types.AgentOutput{
			AgentID:   agentID,
			Version:   stepCount,
			Summary:   rawContent,
			Timestamp: time.Now(),
		}

		_ = h.snapshotMgr.SaveFromState(ctx, agentID, topicID, output, episodes)
	}()

	// 3. 广播状态
	if h.broadcaster != nil {
		h.broadcaster.BroadcastAgentStatus(topicID, types.AgentStatusPayload{
			AgentID:     agentID,
			State:       "WAITING",
			CurrentStep: stepCount,
			LastOutput:  rawContent,
		})
	}
}

// OnError 错误回调
func (h *CallbackHandler) OnError(ctx context.Context, agentID, topicID string, err error) {
	if h.broadcaster != nil {
		h.broadcaster.BroadcastAgentStatus(topicID, types.AgentStatusPayload{
			AgentID: agentID,
			State:   "ERROR",
		})
	}
}
