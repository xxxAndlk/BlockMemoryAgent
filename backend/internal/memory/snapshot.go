package memory

import (
	"context"
	"fmt"
	"time"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// SnapshotStore 快照存储接口
type SnapshotStore interface {
	SaveSnapshot(ctx context.Context, snapshot *types.AgentSnapshot) error
	GetSnapshot(ctx context.Context, agentID, topicID string) (*types.AgentSnapshot, error)
}

// RedisSnapshotStore Redis 快照存储
type RedisSnapshotStore interface {
	SaveSnapshot(ctx context.Context, snapshot *types.AgentSnapshot, ttl time.Duration) error
	GetSnapshot(ctx context.Context, agentID, topicID string) (*types.AgentSnapshot, error)
}

// SnapshotManager 快照管理器
type SnapshotManager struct {
	redisStore RedisSnapshotStore
	pgStore    SnapshotStore
}

// NewSnapshotManager 创建快照管理器
func NewSnapshotManager(redis RedisSnapshotStore, pg SnapshotStore) *SnapshotManager {
	return &SnapshotManager{
		redisStore: redis,
		pgStore:    pg,
	}
}

// Load 加载 Agent 快照
func (m *SnapshotManager) Load(ctx context.Context, agentID, topicID string) (*types.AgentSnapshot, error) {
	// 1. 优先从 Redis 加载 (热数据)
	snap, err := m.redisStore.GetSnapshot(ctx, agentID, topicID)
	if err != nil {
		return nil, fmt.Errorf("redis get snapshot: %w", err)
	}
	if snap != nil {
		return snap, nil
	}

	// 2. Redis 未命中，从 PostgreSQL 加载
	snap, err = m.pgStore.GetSnapshot(ctx, agentID, topicID)
	if err != nil {
		return nil, fmt.Errorf("pg get snapshot: %w", err)
	}

	// 3. 都不存在，初始化空快照
	if snap == nil {
		snap = &types.AgentSnapshot{
			AgentID:      agentID,
			TopicID:      topicID,
			KeySummaries: make([]types.SummaryBlock, 0),
			OpenIssues:   make([]types.Issue, 0),
			LocalVars:    make(map[string]any),
			UpdatedAt:    time.Now(),
		}
	}

	return snap, nil
}

// Save 保存 Agent 快照
func (m *SnapshotManager) Save(ctx context.Context, snapshot *types.AgentSnapshot) error {
	snapshot.UpdatedAt = time.Now()

	// 1. 保存到 Redis (TTL 7 天)
	if err := m.redisStore.SaveSnapshot(ctx, snapshot, 7*24*time.Hour); err != nil {
		return fmt.Errorf("redis save snapshot: %w", err)
	}

	// 2. 异步保存到 PostgreSQL (持久化)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.pgStore.SaveSnapshot(ctx, snapshot)
	}()

	return nil
}

// SaveFromState 从 GraphState 保存快照
func (m *SnapshotManager) SaveFromState(ctx context.Context, agentID, topicID string, output *types.AgentOutput, episodes []*types.Episode) error {
	// 提取最近 K 步关键摘要
	const k = 5
	summaries := make([]types.SummaryBlock, 0, k)
	for i := len(episodes) - 1; i >= 0 && len(summaries) < k; i-- {
		ep := episodes[i]
		summaries = append(summaries, types.SummaryBlock{
			StepID:    ep.StepID,
			Content:   ep.ObservationSummary,
			Timestamp: ep.Timestamp,
		})
	}

	// 提取未解决问题
	var openIssues []types.Issue
	for _, ep := range episodes {
		if ep.Importance > 0.7 && ep.Reflection == "" {
			openIssues = append(openIssues, types.Issue{
				ID:          ep.StepID,
				Description: ep.ObservationSummary,
				CreatedAt:   ep.Timestamp,
			})
		}
	}

	snapshot := &types.AgentSnapshot{
		AgentID:      agentID,
		TopicID:      topicID,
		LastStepID:   "",
		KeySummaries: summaries,
		OpenIssues:   openIssues,
		LocalVars:    make(map[string]any),
		PublishedVer: output.Version,
		UpdatedAt:    time.Now(),
	}

	if len(episodes) > 0 {
		snapshot.LastStepID = episodes[len(episodes)-1].StepID
	}

	return m.Save(ctx, snapshot)
}
