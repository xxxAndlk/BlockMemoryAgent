package server

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/memory"
	"github.com/blockmemory/agent/backend/internal/store"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// MemoryService 封装与 Agent 私有记忆相关的查询与评分逻辑，
// 使 APIHandler 不再直接写 SQL 或调用 scoring 函数。
type MemoryService struct {
	pg *store.PostgresStore
}

// NewMemoryService 从 Postgres 存储构造记忆服务。
func NewMemoryService(pg *store.PostgresStore) *MemoryService {
	return &MemoryService{pg: pg}
}

// Search 按 domain / agent_id / topic_id / query 检索记忆。
// 提供 domain 时走块记忆向量检索；否则回退到 Episode 关键词评分。
func (s *MemoryService) Search(ctx context.Context, agentID, topicID, domain, query string, limit int) ([]map[string]any, error) {
	if s.pg == nil {
		return nil, nil
	}

	var results []map[string]any
	if domain != "" {
		recs, err := memory.SearchBlockMemory(ctx, s.pg, domain, query, limit)
		if err != nil {
			return nil, err
		}
		for _, rec := range recs {
			results = append(results, map[string]any{
				"step_id":    "",
				"summary":    rec.Summary,
				"action":     "block_memory",
				"importance": 0.0,
				"score":      0.0,
				"time":       rec.CreatedAt,
				"domain":     rec.Domain,
				"goal":       rec.Goal,
			})
		}
		return results, nil
	}

	eps, err := s.pg.GetEpisodes(ctx, agentID, topicID, 200)
	if err != nil {
		return nil, err
	}
	scored := scoreEpisodesByKeywords(eps, query)
	if len(scored) > limit {
		scored = scored[:limit]
	}
	for _, s := range scored {
		results = append(results, map[string]any{
			"step_id":    s.Episode.StepID,
			"summary":    s.Episode.ObservationSummary,
			"action":     s.Episode.Action,
			"importance": s.Episode.Importance,
			"score":      s.Score,
			"time":       s.Episode.Timestamp,
		})
	}
	return results, nil
}

// Levels 返回指定 agent/topic 的压缩层级分布（raw / standard）。
func (s *MemoryService) Levels(ctx context.Context, agentID, topicID string) (map[string]int, int, error) {
	levels := map[string]int{"raw": 0, "standard": 0}
	total := 0
	if s.pg == nil {
		return levels, total, nil
	}

	for lvl, key := range map[int]string{0: "raw", 1: "standard"} {
		var cnt int
		err := s.pg.DB().QueryRowContext(ctx, `
			SELECT COUNT(*) FROM agent_private_memory
			WHERE agent_id = $1 AND topic_id = $2 AND compression_level = $3
		`, agentID, topicID, lvl).Scan(&cnt)
		if err != nil {
			return nil, 0, err
		}
		levels[key] = cnt
		total += cnt
	}
	return levels, total, nil
}

// Eval 汇总所有 (agent_id, topic_id) 下的 Raw/Standard 分布，用于 P3-1 记忆层评测。
func (s *MemoryService) Eval(ctx context.Context) (map[string]any, error) {
	if s.pg == nil {
		return nil, fmt.Errorf("postgres store not available")
	}

	rows, err := s.pg.DB().QueryContext(ctx, `
		SELECT agent_id, topic_id, compression_level, COUNT(*)
		FROM agent_private_memory
		GROUP BY agent_id, topic_id, compression_level
		ORDER BY topic_id, agent_id, compression_level
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type pairStat struct {
		AgentID string         `json:"agent_id"`
		TopicID string         `json:"topic_id"`
		Levels  map[string]int `json:"levels"`
		Total   int            `json:"total"`
	}

	pairs := make(map[string]*pairStat)
	grandTotal := 0
	grandRaw := 0
	grandStandard := 0

	for rows.Next() {
		var agentID, topicID string
		var level, count int
		if err := rows.Scan(&agentID, &topicID, &level, &count); err != nil {
			return nil, err
		}
		key := topicID + "/" + agentID
		p, ok := pairs[key]
		if !ok {
			p = &pairStat{AgentID: agentID, TopicID: topicID, Levels: map[string]int{"raw": 0, "standard": 0}}
			pairs[key] = p
		}
		switch level {
		case 0:
			p.Levels["raw"] = count
			grandRaw += count
		case 1:
			p.Levels["standard"] = count
			grandStandard += count
		default:
			continue
		}
		p.Total += count
		grandTotal += count
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var pairList []*pairStat
	for _, p := range pairs {
		pairList = append(pairList, p)
	}
	sort.Slice(pairList, func(i, j int) bool {
		if pairList[i].TopicID != pairList[j].TopicID {
			return pairList[i].TopicID < pairList[j].TopicID
		}
		return pairList[i].AgentID < pairList[j].AgentID
	})

	return map[string]any{
		"total_episodes": grandTotal,
		"raw":            grandRaw,
		"standard":       grandStandard,
		"pairs":          pairList,
		"note":           "Run test/coding/ and test/api/ integration tests, then call this endpoint to evaluate Raw/Standard distribution.",
	}, nil
}

// scoredEpisode 包装 Episode 及其关键词评分。
type scoredEpisode struct {
	Episode *types.Episode
	Score   float64
}

// scoreEpisodesByKeywords 简化关键词评分（无 embedder 时兜底）。
// 评分 = 关键词重叠 * 0.05 + 时间衰减 * 0.3 + 重要性 * 0.3（上限 1.0）。
func scoreEpisodesByKeywords(eps []*types.Episode, query string) []*scoredEpisode {
	var out []*scoredEpisode
	queryRunes := []rune(strings.ToLower(query))
	for _, ep := range eps {
		score := 0.0
		summaryLower := strings.ToLower(ep.ObservationSummary)
		for _, r := range queryRunes {
			if strings.ContainsRune(summaryLower, r) {
				score += 0.05
			}
		}
		score += math.Exp(-0.01*time.Since(ep.Timestamp).Hours()) * 0.3
		score += ep.Importance * 0.3
		if score > 1.0 {
			score = 1.0
		}
		out = append(out, &scoredEpisode{Episode: ep, Score: score})
	}
	for i := 0; i < len(out)-1; i++ {
		for j := 0; j < len(out)-1-i; j++ {
			if out[j].Score < out[j+1].Score {
				out[j], out[j+1] = out[j+1], out[j]
			}
		}
	}
	return out
}
