package memory

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/config"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// SnapshotStore 持久化快照存储接口：抽象 Postgres 等冷存后端。
// 实现方负责将 AgentSnapshot 持久化并在缺失时返回 nil（未找到）或错误。
type SnapshotStore interface {
	// SaveSnapshot 持久化一条 Agent 快照（无 TTL，长期保留）。
	SaveSnapshot(ctx context.Context, snapshot *types.AgentSnapshot) error
	// GetSnapshot 按 Agent + Topic 读取快照；未找到时返回 (nil, nil)。
	GetSnapshot(ctx context.Context, agentID, topicID string) (*types.AgentSnapshot, error)
}

// RedisSnapshotStore 热存快照存储接口：抽象 Redis 等带 TTL 的热存后端。
// 热存用于加速加载，TTL 由调用方显式指定。
type RedisSnapshotStore interface {
	// SaveSnapshot 写入快照并设置 TTL，超时后自动失效。
	SaveSnapshot(ctx context.Context, snapshot *types.AgentSnapshot, ttl time.Duration) error
	// GetSnapshot 按 Agent + Topic 读取热存快照；未命中时返回 (nil, nil)。
	GetSnapshot(ctx context.Context, agentID, topicID string) (*types.AgentSnapshot, error)
}

// SnapshotManager 快照管理器：协调 Redis 热存与 Postgres 冷存的两级快照读写。
// 读路径：Redis → Postgres → 空快照；写路径：同步写 Redis + 异步落 Postgres。
type SnapshotManager struct {
	redisStore RedisSnapshotStore  // 热存，优先读取与同步写入
	pgStore    SnapshotStore       // 冷存，异步持久化
	cfg        *config.AgentConfig // Agent 运行时配置（决定摘要条数、未决问题阈值等）；nil 时使用默认值
}

// NewSnapshotManager 创建快照管理器，注入热存与冷存后端。
// 参数：redis 热存后端；pg 冷存后端；cfg Agent 运行时配置（可为 nil，使用默认）。
// 返回：组装好的 *SnapshotManager。
// 副作用：无。
func NewSnapshotManager(redis RedisSnapshotStore, pg SnapshotStore, cfg *config.AgentConfig) *SnapshotManager {
	// 注入两级存储依赖，后续 Load / Save 复用。
	return &SnapshotManager{
		redisStore: redis,
		pgStore:    pg,
		cfg:        cfg,
	}
}

// summaryCount 返回快照应保留的最近摘要条数。
func (m *SnapshotManager) summaryCount() int {
	if m.cfg != nil && m.cfg.SnapshotSummaryCount > 0 {
		return m.cfg.SnapshotSummaryCount
	}
	return 20
}

// openIssueThreshold 返回未决问题的重要性阈值。
func (m *SnapshotManager) openIssueThreshold() float64 {
	if m.cfg != nil && m.cfg.SnapshotOpenIssueThreshold > 0 {
		return m.cfg.SnapshotOpenIssueThreshold
	}
	return 0.7
}

// Load 加载 Agent 快照，遵循两级回退策略。
// 顺序：Redis 热存 → Postgres 冷存 → 初始化空快照。
//
// 参数：
//   - ctx: 取消信号。
//   - agentID: Agent 实例 ID。
//   - topicID: 话题 ID，用于按话题隔离私有快照。
//
// 返回：命中的 *types.AgentSnapshot；热存与冷存均未命中时返回初始化的空快照。
// 副作用：可能触发 Redis / Postgres 读取。
// 并发安全：实例无共享可变状态；底层依赖由实现方保证。
func (m *SnapshotManager) Load(ctx context.Context, agentID, topicID string) (*types.AgentSnapshot, error) {
	// 1. 优先从 Redis 加载（热数据），命中即返回，避免冷存 IO。
	snap, err := m.redisStore.GetSnapshot(ctx, agentID, topicID)
	if err != nil {
		// 热存出错视为致命，向上抛出，避免掩盖问题。
		return nil, fmt.Errorf("redis get snapshot: %w", err)
	}
	if snap != nil {
		// 热存命中，直接返回。
		return snap, nil
	}

	// 2. Redis 未命中，从 PostgreSQL 加载（冷数据）。
	snap, err = m.pgStore.GetSnapshot(ctx, agentID, topicID)
	if err != nil {
		// 冷存出错同样向上抛出。
		return nil, fmt.Errorf("pg get snapshot: %w", err)
	}

	// 3. 都不存在，初始化空快照，保证调用方拿到可用对象。
	if snap == nil {
		snap = &types.AgentSnapshot{
			AgentID:      agentID,                       // 标记归属 Agent
			TopicID:      topicID,                       // 标记归属话题
			KeySummaries: make([]types.SummaryBlock, 0), // 关键摘要初始化为空切片（避免 nil）
			OpenIssues:   make([]types.Issue, 0),        // 未决问题初始化为空切片
			LocalVars:    make(map[string]any),          // 私有变量初始化为空 map
			UpdatedAt:    time.Now(),                    // 记录初始化时间
		}
	}

	// 返回最终快照（命中或新建）。
	return snap, nil
}

// Save 保存 Agent 快照：同步写 Postgres 持久化，再同步写 Redis 热存。
// 主路径以 Postgres 为准；Redis 失败只记录日志，不阻塞主路径。
//
// 参数：
//   - ctx: 取消信号。
//   - snapshot: 待保存的快照指针，函数会就地更新 UpdatedAt。
//
// 返回：Postgres 写入失败时返回错误；Redis 写入失败仅记录日志。
// 副作用：更新 snapshot.UpdatedAt；触发 Postgres 与 Redis 同步写。
func (m *SnapshotManager) Save(ctx context.Context, snapshot *types.AgentSnapshot) error {
	// 刷新快照更新时间，标记本次写入。
	snapshot.UpdatedAt = time.Now()

	// 1. 同步保存到 PostgreSQL（持久化），主路径必须成功。
	if err := m.pgStore.SaveSnapshot(ctx, snapshot); err != nil {
		return fmt.Errorf("pg save snapshot: %w", err)
	}

	// 2. 同步保存到 Redis（TTL 7 天），失败仅记录日志。
	if err := m.redisStore.SaveSnapshot(ctx, snapshot, 7*24*time.Hour); err != nil {
		log.Printf("[snapshot] redis save failed: %v", err)
	}

	return nil
}

// SaveFromState 从 GraphState 派生并保存快照。
// 提取最近 K 步 Episode 的摘要块与未决问题，组装为 AgentSnapshot 后调用 Save。
//
// 参数：
//   - ctx: 取消信号。
//   - agentID: Agent 实例 ID。
//   - topicID: 话题 ID。
//   - output: Agent 本次公开输出，用于记录已发布版本号。
//   - episodes: 本话题下的 Episode 列表，从中提取摘要与未决问题。
//
// 返回：Save 过程中发生的错误。
// 副作用：同 Save（更新 UpdatedAt、写入 Redis、异步写 Postgres）。
func (m *SnapshotManager) SaveFromState(ctx context.Context, agentID, topicID string, output *types.AgentOutput, episodes []*types.Episode) error {
	k := m.summaryCount()
	threshold := m.openIssueThreshold()

	// 提取最近 K 步关键摘要：从末尾向前取最多 k 条。
	summaries := make([]types.SummaryBlock, 0, k)
	selected := make(map[string]bool, k)
	// 倒序遍历，确保最新步骤排在前面；达到 k 条即停止。
	for i := len(episodes) - 1; i >= 0 && len(summaries) < k; i-- {
		ep := episodes[i]
		selected[ep.StepID] = true
		// 将 Episode 摘要打包为可引用的 SummaryBlock。
		summaries = append(summaries, types.SummaryBlock{
			StepID:    ep.StepID,             // 关联原始步骤 ID
			Content:   ep.ObservationSummary, // 使用观察摘要作为快照内容
			Timestamp: ep.Timestamp,          // 保留原始时间戳
		})
	}

	// 混合策略：在最近的 k 条之外，补充高重要性但未入选的旧 Episode，
	// 避免"只看最近"导致关键历史决策/错误被遗漏。最多再补充 k 条。
	if len(summaries) < k*2 {
		for _, ep := range episodes {
			if selected[ep.StepID] {
				continue
			}
			if ep.Importance >= threshold {
				selected[ep.StepID] = true
				summaries = append(summaries, types.SummaryBlock{
					StepID:    ep.StepID,
					Content:   ep.ObservationSummary,
					Timestamp: ep.Timestamp,
				})
				if len(summaries) >= k*2 {
					break
				}
			}
		}
	}

	// 提取未解决问题：重要性高、无反思，且 Episode 本身提示未完成的才视为待解决。
	var openIssues []types.Issue
	for _, ep := range episodes {
		// 阈值过滤 + 必须有未完成/失败迹象，避免把正常高价值输出误判为未决问题。
		if ep.Importance > threshold && ep.Reflection == "" && episodeLooksUnresolved(ep) {
			openIssues = append(openIssues, types.Issue{
				ID:          ep.StepID,             // 问题 ID 复用 StepID
				Description: ep.ObservationSummary, // 问题描述使用观察摘要
				CreatedAt:   ep.Timestamp,          // 记录问题发现时间
			})
		}
	}

	// 组装最终快照对象。
	snapshot := &types.AgentSnapshot{
		AgentID:      agentID,              // 归属 Agent
		TopicID:      topicID,              // 归属话题
		LastStepID:   "",                   // 占位，后续按 episodes 末尾填充
		KeySummaries: summaries,            // 关键摘要块
		OpenIssues:   openIssues,           // 未决问题
		LocalVars:    make(map[string]any), // 私有变量初始化为空 map
		PublishedVer: output.Version,       // 记录已发布版本号
		UpdatedAt:    time.Now(),           // 记录快照生成时间
	}

	// 若存在 Episode，将最后一步 StepID 写入 LastStepID，便于增量更新。
	if len(episodes) > 0 {
		snapshot.LastStepID = episodes[len(episodes)-1].StepID
	}

	// 委托 Save 执行两级存储写入。
	return m.Save(ctx, snapshot)
}

// episodeLooksUnresolved 判断 Episode 是否提示任务未完成或失败。
// 用于减少 OpenIssues 误报：高重要性但已成功完成的 Episode 不应列为未决问题。
func episodeLooksUnresolved(ep *types.Episode) bool {
	// 1. 文本层面包含失败/错误/未完成的明确信号
	lower := strings.ToLower(ep.ObservationSummary + " " + ep.Action)
	failureSignals := []string{"error", "fail", "exception", "timeout", "panic", "失败", "错误", "超时", "异常"}
	for _, s := range failureSignals {
		if strings.Contains(lower, s) {
			return true
		}
	}
	// 2. 工具调用中存在明显失败输出（简单启发式：output 含 error/fail）
	for _, tc := range ep.ToolCalls {
		if strings.Contains(strings.ToLower(tc.Output), "error") || strings.Contains(strings.ToLower(tc.Output), "失败") {
			return true
		}
	}
	// 3. 内容极短且没有工具调用，可能表示任务尚未展开
	if len(ep.ToolCalls) == 0 && len(ep.ObservationSummary) < 20 {
		return true
	}
	return false
}
