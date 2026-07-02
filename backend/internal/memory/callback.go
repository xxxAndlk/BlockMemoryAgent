package memory

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// Broadcaster 是回调处理器对外部广播器的最小依赖接口，避免 memory 包依赖 server 包造成循环引用。
type Broadcaster interface {
	BroadcastAgentStatus(topicID string, payload types.AgentStatusPayload)
	BroadcastEpisode(topicID string, payload types.EpisodePayload)
}

// DeadLetterStore 死信存储接口，用于持久化写入失败记录。
type DeadLetterStore interface {
	SaveMemoryWriteFailure(ctx context.Context, agentID, topicID string, stepCount int, action, rawContent, errStr string, retryCount int) error
	QueryUnresolvedMemoryWriteFailures(ctx context.Context, limit int) ([]*types.MemoryWriteFailure, error)
	ResolveMemoryWriteFailure(ctx context.Context, id int) error
}

// writeQueueItem 内存队列项。
type writeQueueItem struct {
	agentID    string
	topicID    string
	action     string
	rawContent string
	stepCount  int
}

// CallbackHandler 记忆回调处理器：在 Graph 节点生命周期事件上驱动记忆写入与状态广播。
// 持有写入处理器、快照管理器、TUI 广播器与死信存储四个依赖。
// P0-2 修复：OnEnd 不再直接起 goroutine，而是推送到内存队列由后台 worker 消费。
type CallbackHandler struct {
	writeProcessor  *WriteProcessor     // Episode 写入流水线
	snapshotMgr     *SnapshotManager    // 快照两级存储管理
	broadcaster     Broadcaster         // TUI / SSE 事件广播器，可为 nil
	deadLetterStore DeadLetterStore     // 死信存储，写入失败时归档
	writeQueue      chan writeQueueItem // 内存队列
	queueDone       chan struct{}       // worker 退出信号
	retryAttempts   int                 // 重试次数（测试可覆盖）
	retryDelay      time.Duration       // 重试基础退避（测试可覆盖）
	writeTimeout    time.Duration       // 单次写入超时（测试可覆盖）
}

// NewCallbackHandler 创建回调处理器，注入核心依赖并启动后台 worker。
// 参数：writeProcessor 记忆写入器；snapshotMgr 快照管理器；broadcaster TUI 广播器（可空）；deadLetterStore 死信存储（可空）。
// 返回：组装好的 *CallbackHandler。
// 副作用：启动后台 writeWorker goroutine。
func NewCallbackHandler(writeProcessor *WriteProcessor, snapshotMgr *SnapshotManager, broadcaster Broadcaster, deadLetterStore DeadLetterStore) *CallbackHandler {
	h := &CallbackHandler{
		writeProcessor:  writeProcessor,
		snapshotMgr:     snapshotMgr,
		broadcaster:     broadcaster,
		deadLetterStore: deadLetterStore,
		writeQueue:      make(chan writeQueueItem, 100), // 缓冲队列，满时降级同步写
		queueDone:       make(chan struct{}),
		retryAttempts:   3,
		retryDelay:      time.Second,
		writeTimeout:    5 * time.Second,
	}
	go h.writeWorker()
	return h
}

// OnStart Node 开始回调：广播 Agent 进入 ACTIVE 状态。
// 参数：ctx 取消信号（保留以便扩展）；agentID Agent 实例 ID；topicID 话题 ID。
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

// OnEnd Node 结束回调：推送到内存队列，后台 worker 消费写入；队列满时降级同步写（阻塞主路径，保数据）。
//
// 参数：
//   - ctx: 取消信号（保留以便扩展）。
//   - agentID: Agent 实例 ID。
//   - topicID: 话题 ID。
//   - action: 本步动作摘要，写入 Episode.Action。
//   - rawContent: 原始输出文本，写入 Episode 并作为快照摘要。
//   - stepCount: 当前步骤序号，作为快照版本号与幂等键。
//
// 返回：无。
// 副作用：触发 Episode 写入、快照保存与 TUI 状态广播。
// 并发安全：队列操作由内部 chan 保证；底层依赖需保证并发安全。
func (h *CallbackHandler) OnEnd(ctx context.Context, agentID, topicID, action, rawContent string, stepCount int) {
	item := writeQueueItem{
		agentID:    agentID,
		topicID:    topicID,
		action:     action,
		rawContent: rawContent,
		stepCount:  stepCount,
	}

	// 推送到队列；队列满时降级同步写（阻塞主路径，保数据）
	select {
	case h.writeQueue <- item:
		// 成功入队
	default:
		log.Printf("[memory_write] queue_full session=%s agent=%s step=%d action=%s, falling_back to sync", topicID, agentID, stepCount, action)
		h.syncWrite(item)
	}

	// 广播状态：将 Agent 切换为 WAITING，附带步骤序号与最近输出。
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

// writeWorker 后台 worker goroutine，消费队列并执行写入。
// 启动后持续监听 writeQueue，直到 chan 被关闭。
func (h *CallbackHandler) writeWorker() {
	for item := range h.writeQueue {
		h.processItem(item)
	}
	close(h.queueDone)
}

// processItem 处理单个队列项：先写 Episode，再保存快照。
// 每个操作独立失败重试，互不影响。
func (h *CallbackHandler) processItem(item writeQueueItem) {
	// 1. Episode 写入（重试 3 次，指数退避）
	h.retryEpisodeWrite(item)

	// 2. 快照保存（重试 3 次，指数退避）
	h.retrySnapshotSave(item)
}

// retryEpisodeWrite 写入 Episode，失败重试 3 次（指数退避：1s, 2s, 4s）。
func (h *CallbackHandler) retryEpisodeWrite(item writeQueueItem) {
	var lastErr error
	for attempt := 0; attempt < h.retryAttempts; attempt++ {
		if attempt > 0 {
			// 指数退避：1s, 2s, 4s
			time.Sleep(time.Duration(1<<uint(attempt-1)) * h.retryDelay)
		}

		ctx, cancel := context.WithTimeout(context.Background(), h.writeTimeout)
		episode, err := h.writeProcessor.ProcessWithStepCount(ctx, item.agentID, item.topicID, item.action, item.rawContent, item.stepCount)
		cancel()

		if err == nil {
			// 写入成功，广播 Episode 事件
			if h.broadcaster != nil {
				h.broadcaster.BroadcastEpisode(item.topicID, types.EpisodePayload{
					AgentID:    item.agentID,
					StepID:     episode.StepID,
					Importance: episode.Importance,
					Summary:    episode.ObservationSummary,
					Facts:      episode.Facts,
					Timestamp:  episode.Timestamp,
					ToolCalls:  episode.ToolCalls,
				})
			}
			return
		}

		lastErr = err
		log.Printf("[memory_write] retry session=%s agent=%s step=%d action=%s attempt=%d err=%v", item.topicID, item.agentID, item.stepCount, item.action, attempt+1, err)
	}

	// 3 次都失败，入死信表
	h.saveDeadLetter(item, "episode_write", lastErr, h.retryAttempts)
	log.Printf("[memory_write] dead_letter session=%s agent=%s step=%d action=%s err=%v", item.topicID, item.agentID, item.stepCount, item.action, lastErr)
}

// retrySnapshotSave 保存快照，失败重试 3 次（指数退避：1s, 2s, 4s）。
func (h *CallbackHandler) retrySnapshotSave(item writeQueueItem) {
	var lastErr error
	for attempt := 0; attempt < h.retryAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(1<<uint(attempt-1)) * h.retryDelay)
		}

		// 获取最近 10 条 Episode
		ctx, cancel := context.WithTimeout(context.Background(), h.writeTimeout)
		episodes, _ := h.writeProcessor.store.GetEpisodes(ctx, item.agentID, item.topicID, 10)
		cancel()

		output := &types.AgentOutput{
			AgentID:   item.agentID,
			Version:   item.stepCount,
			Summary:   item.rawContent,
			Timestamp: time.Now(),
		}

		// 构建快照并直接写入 Postgres（绕过 Redis 同步写与内部 goroutine，便于精确重试）
		snapshot := h.snapshotMgr.BuildSnapshotFromState(item.agentID, item.topicID, output, episodes)

		ctx, cancel = context.WithTimeout(context.Background(), h.writeTimeout)
		err := h.snapshotMgr.SaveSnapshotToPostgres(ctx, snapshot)
		cancel()

		if err == nil {
			return
		}

		lastErr = err
		log.Printf("[memory_write] snapshot_retry session=%s agent=%s step=%d attempt=%d err=%v", item.topicID, item.agentID, item.stepCount, attempt+1, err)
	}

	// 3 次都失败，入死信
	h.saveDeadLetter(item, "snapshot_save", lastErr, h.retryAttempts)
	log.Printf("[memory_write] snapshot_dead_letter session=%s agent=%s step=%d err=%v", item.topicID, item.agentID, item.stepCount, lastErr)
}

// syncWrite 同步写（队列满时的降级路径，阻塞主路径保数据）。
func (h *CallbackHandler) syncWrite(item writeQueueItem) {
	h.retryEpisodeWrite(item)
	h.retrySnapshotSave(item)
}

// saveDeadLetter 将失败记录写入死信表。
func (h *CallbackHandler) saveDeadLetter(item writeQueueItem, action string, err error, retryCount int) {
	if h.deadLetterStore == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.deadLetterStore.SaveMemoryWriteFailure(ctx, item.agentID, item.topicID, item.stepCount, action, item.rawContent, err.Error(), retryCount); err != nil {
		log.Printf("[memory_write] dead_letter_fail session=%s agent=%s step=%d action=%s err=%v", item.topicID, item.agentID, item.stepCount, action, err)
	}
}

// ReplayDeadLetters 在启动时扫描死信表并尝试重新写入。
// 每条死信会复用原有 stepCount，保证幂等；写入成功后标记为已解析。
func (h *CallbackHandler) ReplayDeadLetters(ctx context.Context) error {
	if h.deadLetterStore == nil {
		return nil
	}
	failures, err := h.deadLetterStore.QueryUnresolvedMemoryWriteFailures(ctx, 1000)
	if err != nil {
		return fmt.Errorf("query dead letters: %w", err)
	}
	if len(failures) == 0 {
		return nil
	}
	log.Printf("[memory_write] replay_start count=%d", len(failures))
	var replayed int
	for _, f := range failures {
		item := writeQueueItem{
			agentID:    f.AgentID,
			topicID:    f.TopicID,
			action:     f.Action,
			rawContent: f.RawContent,
			stepCount:  f.StepCount,
		}
		h.processItem(item)
		if err := h.deadLetterStore.ResolveMemoryWriteFailure(ctx, f.ID); err != nil {
			log.Printf("[memory_write] resolve_failed id=%d err=%v", f.ID, err)
			continue
		}
		replayed++
	}
	log.Printf("[memory_write] replay_done count=%d replayed=%d", len(failures), replayed)
	return nil
}

// Close 优雅关闭回调处理器：关闭写入队列并等待后台 worker 退出。
func (h *CallbackHandler) Close() error {
	close(h.writeQueue)
	select {
	case <-h.queueDone:
		return nil
	case <-time.After(10 * time.Second):
		return fmt.Errorf("callback handler worker shutdown timeout")
	}
}
