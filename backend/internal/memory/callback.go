package memory

import (
	"context"
	"time"

	"github.com/blockmemory/agent/backend/internal/server"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// CallbackHandler 记忆回调处理器：在 Graph 节点生命周期事件上驱动记忆写入与状态广播。
// 持有写入处理器、快照管理器与 TUI 广播器三个依赖，串联"执行→落库→广播"链路。
type CallbackHandler struct {
	writeProcessor *WriteProcessor          // Episode 写入流水线
	snapshotMgr    *SnapshotManager         // 快照两级存储管理
	broadcaster    *server.TUIBroadcaster   // TUI / SSE 事件广播器，可为 nil
}

// NewCallbackHandler 创建回调处理器，注入三个核心依赖。
// 参数：writeProcessor 记忆写入器；snapshotMgr 快照管理器；broadcaster TUI 广播器（可空）。
// 返回：组装好的 *CallbackHandler。
// 副作用：无。
func NewCallbackHandler(writeProcessor *WriteProcessor, snapshotMgr *SnapshotManager, broadcaster *server.TUIBroadcaster) *CallbackHandler {
	// 注入依赖，后续 OnStart / OnEnd / OnError 复用。
	return &CallbackHandler{
		writeProcessor: writeProcessor,
		snapshotMgr:    snapshotMgr,
		broadcaster:    broadcaster,
	}
}

// OnStart Node 开始回调：广播 Agent 进入 ACTIVE 状态。
// 参数：ctx 取消信号（保留以便扩展）；agentID Agent 实例 ID；topicID 话题 ID。
// 返回：无。
// 副作用：当 broadcaster 非空时，向该 Topic 推送 agent.status 事件。
// 并发安全：broadcaster 自身需保证并发安全。
func (h *CallbackHandler) OnStart(ctx context.Context, agentID, topicID string) {
	// 仅在广播器存在时推送状态，避免 nil 解引用。
	if h.broadcaster != nil {
		// 广播 Agent 进入活跃状态，供 TUI 实时显示。
		h.broadcaster.BroadcastAgentStatus(topicID, types.AgentStatusPayload{
			AgentID: agentID,
			State:   "ACTIVE",
		})
	}
}

// OnEnd Node 结束回调：异步写 Episode、保存快照并广播状态变更。
// 三个动作相互独立，分别起 goroutine 并行执行，避免阻塞 Graph 主路径。
//
// 参数：
//   - ctx: 取消信号（保留以便扩展）。
//   - agentID: Agent 实例 ID。
//   - topicID: 话题 ID。
//   - action: 本步动作摘要，写入 Episode.Action。
//   - rawContent: 原始输出文本，写入 Episode 并作为快照摘要。
//   - stepCount: 当前步骤序号，作为快照版本号。
//
// 返回：无。
// 副作用：触发 Episode 写入、快照保存与多次 TUI 广播。
// 并发安全：内部 goroutine 使用独立 context，互不阻塞；底层依赖需保证并发安全。
func (h *CallbackHandler) OnEnd(ctx context.Context, agentID, topicID, action, rawContent string, stepCount int) {
	// 1. 异步写入 Episode：将本步输出交给写入流水线处理。
	go func() {
		// 独立 context：避免父 ctx 取消导致写入中断。
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		// 调用写入处理器生成并持久化 Episode。
		episode, err := h.writeProcessor.Process(ctx, agentID, topicID, action, rawContent)
		if err != nil {
			// 写入失败直接返回，不广播 Episode 事件。
			return
		}

		// 广播 Episode 新增事件，供 TUI 显示记忆增量。
		if h.broadcaster != nil {
			h.broadcaster.BroadcastEpisode(topicID, types.EpisodePayload{
				AgentID:    agentID,                  // 归属 Agent
				StepID:     episode.StepID,           // 步骤 ID
				Importance: episode.Importance,       // 重要性评分
				Summary:    episode.ObservationSummary, // 摘要文本
				Facts:      episode.Facts,            // 事实三元组
				Timestamp:  episode.Timestamp,        // 步骤时间
				ToolCalls:  episode.ToolCalls,        // 工具调用明细
			})
		}
	}()

	// 2. 保存快照：基于最近 Episode 生成快照并写入两级存储。
	go func() {
		// 独立 context 与超时，避免与 Episode 写入竞争资源。
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		// 获取最近 10 条 Episode 作为快照派生输入。
		episodes, _ := h.writeProcessor.store.GetEpisodes(ctx, agentID, topicID, 10)

		// 构建临时输出对象，承载版本号与摘要供快照使用。
		output := &types.AgentOutput{
			AgentID:   agentID,    // 归属 Agent
			Version:   stepCount,  // 步骤序号作为版本号
			Summary:   rawContent, // 原始输出作为摘要
			Timestamp: time.Now(), // 发布时间
		}

		// 委托快照管理器派生并保存；错误被忽略，避免影响主路径。
		_ = h.snapshotMgr.SaveFromState(ctx, agentID, topicID, output, episodes)
	}()

	// 3. 广播状态：将 Agent 切换为 WAITING，附带步骤序号与最近输出。
	if h.broadcaster != nil {
		h.broadcaster.BroadcastAgentStatus(topicID, types.AgentStatusPayload{
			AgentID:     agentID,     // 归属 Agent
			State:       "WAITING",   // 进入等待态，等待下一轮调度
			CurrentStep: stepCount,   // 当前步骤序号
			LastOutput:  rawContent,  // 最近输出摘要
		})
	}
}

// OnError 错误回调：广播 Agent 进入 ERROR 状态，便于 TUI 显著提示。
// 参数：ctx 取消信号（保留）；agentID Agent ID；topicID 话题 ID；err 错误对象（当前未使用，保留以便扩展）。
// 返回：无。
// 副作用：当 broadcaster 非空时推送 agent.status ERROR 事件。
func (h *CallbackHandler) OnError(ctx context.Context, agentID, topicID string, err error) {
	// 仅在广播器存在时推送错误状态。
	if h.broadcaster != nil {
		h.broadcaster.BroadcastAgentStatus(topicID, types.AgentStatusPayload{
			AgentID: agentID,
			State:   "ERROR",
		})
	}
}
