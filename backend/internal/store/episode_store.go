package store

import (
	"context"       // 上下文，贯穿所有数据库调用以支持超时与取消
	"database/sql"  // 标准库 SQL 抽象层
	"encoding/json" // 结构体与 JSONB/JSON 列之间的序列化
	"errors"        // 错误包装与判断
	"fmt"           // 格式化错误信息

	"github.com/blockmemory/agent/backend/pkg/enums" // 枚举常量
	"github.com/blockmemory/agent/backend/pkg/types" // 领域模型
	"github.com/lib/pq"                              // postgres 驱动与错误码
)

// EpisodeStore 是 Episode（私有记忆）相关的 PostgreSQL 存储子层。
// 职责: agent_private_memory 表的 CRUD，包括压缩层级统计与重复冲突判断。
// 注意: dormant——无运行时代码路径调用（ReAct 重构后私有记忆流已废弃），
// 仅经 PostgresStore 薄包装暴露给 server/api.go 的 snapshot 兼容接口。
type EpisodeStore struct {
	db *sql.DB // 共享连接池
}

// Save 保存单条 Episode 到 agent_private_memory 表。
// 参数:
//   - ctx:     请求上下文。
//   - agentID: 所属 Agent ID
//   - topicID: 话题 ID
//   - ep:      待持久化的 Episode (含重要性、时间戳、内容)
//
// 返回: SQL 执行错误。
// 副作用: 写入一行新记录;compression_level 根据 FullObservation 是否为空自动判定 Raw/Standard。
func (s *EpisodeStore) Save(ctx context.Context, agentID, topicID string, ep *types.Episode) error {
	// 将 Episode 整体序列化为 JSON,存入 JSONB 列 episode
	data, err := json.Marshal(ep)
	if err != nil {
		return fmt.Errorf("marshal episode: %w", err)
	}
	// 根据内容推断压缩层级
	level := compressionLevelOf(ep)
	// 插入行,importance_score 单独冗余以便后续按重要性排序；step_count 写 0 表示未使用幂等键
	_, err = s.db.ExecContext(ctx, `
			INSERT INTO agent_private_memory (agent_id, topic_id, episode, compression_level, importance_score, step_count, created_at)
			VALUES ($1, $2, $3, $4, $5, 0, $6)
		`, agentID, topicID, data, level, ep.Importance, ep.Timestamp)
	return err
}

// SaveWithStepCount 保存单条 Episode 到 agent_private_memory 表，使用 step_count 作为幂等键。
// 若相同 (agent_id, topic_id, step_count) 已存在，则忽略冲突（ON CONFLICT DO NOTHING）。
// 参数:
//   - ctx:      请求上下文。
//   - agentID:  所属 Agent ID。
//   - topicID:  话题 ID。
//   - stepCount: 幂等步数，作为唯一键一部分。
//   - ep:       待持久化的 Episode。
//
// 返回: SQL 执行或序列化错误。
func (s *EpisodeStore) SaveWithStepCount(ctx context.Context, agentID, topicID string, stepCount int, ep *types.Episode) error {
	// 序列化 Episode 为 JSON
	data, err := json.Marshal(ep)
	if err != nil {
		return fmt.Errorf("marshal episode: %w", err)
	}
	// 推断压缩层级
	level := compressionLevelOf(ep)
	// 插入时使用 ON CONFLICT DO NOTHING 实现幂等写入
	_, err = s.db.ExecContext(ctx, `
			INSERT INTO agent_private_memory (agent_id, topic_id, episode, compression_level, importance_score, step_count, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (agent_id, topic_id, step_count) DO NOTHING
		`, agentID, topicID, data, level, ep.Importance, stepCount, ep.Timestamp)
	return err
}

// compressionLevelOf 根据 Episode 内容推断压缩层级。
// FullObservation 为空表示已压缩到 Standard，否则为 Raw。
// 参数:
//   - ep: Episode 指针。
//
// 返回: 0 (Raw) 或 1 (Standard)。
func compressionLevelOf(ep *types.Episode) int {
	if ep.FullObservation == "" {
		// 完整观察为空，说明是摘要层级
		return int(enums.LevelStandard)
	}
	// 完整观察非空，说明是原始层级
	return int(enums.LevelRaw)
}

// IsDuplicateError 判断错误是否为 PostgreSQL 唯一约束冲突（23505）。
// 支持被 fmt.Errorf("...: %w") 包装过的错误。
// 参数:
//   - err: 待判断错误，可能为 nil 或被包装。
//
// 返回: true 表示确认为唯一约束冲突。
func (s *EpisodeStore) IsDuplicateError(err error) bool {
	var pgErr *pq.Error
	if errors.As(err, &pgErr) {
		// 23505 是 PostgreSQL 唯一性违反错误码
		return pgErr.Code == "23505"
	}
	return false
}

// GetEpisodes 获取 Agent 在指定话题下的全部 Episode (按创建时间倒序)。
// 参数:
//   - ctx:               请求上下文。
//   - agentID, topicID:  检索范围
//   - limit:             最大返回条数;<=0 时默认 100
//
// 返回: Episode 切片 (可能为空) 与 SQL 错误。
// 注意: 反序列化失败的行被静默跳过,保证部分坏数据不阻断整体读取。
func (s *EpisodeStore) GetEpisodes(ctx context.Context, agentID, topicID string, limit int) ([]*types.Episode, error) {
	if limit <= 0 {
		// 兜底默认值,避免下游不传 limit 时返回过多数据
		limit = 100
	}
	// 查询指定 agent + topic 的 episode JSONB，按创建时间倒序
	rows, err := s.db.QueryContext(ctx, `
			SELECT episode FROM agent_private_memory
			WHERE agent_id = $1 AND topic_id = $2
			ORDER BY created_at DESC
			LIMIT $3
		`, agentID, topicID, limit)
	if err != nil {
		return nil, err
	}
	// 确保结果集关闭,避免连接泄漏
	defer rows.Close()

	var episodes []*types.Episode
	for rows.Next() {
		var raw []byte
		// 仅取 JSONB 列原始字节
		if err := rows.Scan(&raw); err != nil {
			continue
		}
		var ep types.Episode
		// 反序列化失败跳过当前行,继续处理后续
		if err := json.Unmarshal(raw, &ep); err != nil {
			continue
		}
		episodes = append(episodes, &ep)
	}
	// rows.Err() 捕获迭代期间发生的错误
	return episodes, rows.Err()
}

// CountEpisodes 统计 Agent 在某话题下的 Episode 总数。
// 参数:
//   - ctx:              请求上下文。
//   - agentID, topicID: 检索范围。
//
// 返回: 行数与查询错误;用于触发压缩阈值判断。
func (s *EpisodeStore) CountEpisodes(ctx context.Context, agentID, topicID string) (int, error) {
	var count int
	// 执行 COUNT(*) 聚合查询
	err := s.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM agent_private_memory
			WHERE agent_id = $1 AND topic_id = $2
		`, agentID, topicID).Scan(&count)
	return count, err
}

// CountEpisodesByLevel 统计 Agent 在某话题下各 compression_level 的数量。
// 参数:
//   - ctx:              请求上下文。
//   - agentID, topicID: 检索范围。
//
// 返回: map[compression_level]count，用于记忆层评测（P3-1）。
func (s *EpisodeStore) CountEpisodesByLevel(ctx context.Context, agentID, topicID string) (map[int]int, error) {
	// 按 compression_level 分组计数
	rows, err := s.db.QueryContext(ctx, `
			SELECT compression_level, COUNT(*) FROM agent_private_memory
			WHERE agent_id = $1 AND topic_id = $2
			GROUP BY compression_level
		`, agentID, topicID)
	if err != nil {
		return nil, err
	}
	// 确保结果集关闭
	defer rows.Close()

	result := make(map[int]int)
	for rows.Next() {
		var level, count int
		if err := rows.Scan(&level, &count); err != nil {
			return nil, err
		}
		result[level] = count
	}
	return result, rows.Err()
}
