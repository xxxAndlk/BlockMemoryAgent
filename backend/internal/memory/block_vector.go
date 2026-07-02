// Package memory block_vector.go 提供特性3（domainAgent 后向量检索）所需的
// 块记忆记录结构与 global_knowledge 表之间的转换。
//
// 伪嵌入实现抽到 internal/embed 包，避免 store ↔ memory 循环依赖。
package memory

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/blockmemory/agent/backend/internal/embed"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// BlockMemoryRecord 块记忆条目：domainAgent 完成一个 SessionBlock 后归档的
// 领域/目标/结果摘要，写入 global_knowledge 表（KnowledgeType="block_memory"）。
type BlockMemoryRecord struct {
	SessionID string    // 所属会话
	Domain    string    // 领域名
	Goal      string    // 领域目标
	Summary   string    // 任务结果摘要
	CreatedAt time.Time // 归档时间
}

// ToKnowledgeRecord 把块记忆转为 global_knowledge 表记录。
// Content 拼装为可读文本，便于检索后直接注入 prompt。
func (r *BlockMemoryRecord) ToKnowledgeRecord(dim int) *types.KnowledgeRecord {
	content := strings.Join([]string{
		"领域:", r.Domain,
		"目标:", r.Goal,
		"结果:", r.Summary,
	}, "\n")
	meta := map[string]any{
		"session_id": r.SessionID,
		"domain":     r.Domain,
		"goal":       r.Goal,
	}
	return &types.KnowledgeRecord{
		KnowledgeType: enums.KnowledgeTypeBlockMemory,
		TopicID:       r.SessionID,
		Content:       content,
		Embedding:     embed.PseudoEmbed(content, dim),
		Meta:          meta,
		CreatedAt:     r.CreatedAt,
	}
}

// BlockMemorySearcher 提供按类型+domain 过滤的向量检索能力。
// 实现方通常为 *store.PostgresStore。
type BlockMemorySearcher interface {
	SearchKnowledgeByTypeAndDomain(ctx context.Context, knowledgeType enums.KnowledgeType, domain string, embedding []float32, topK int) ([]*types.KnowledgeRecord, error)
	EmbeddingDim() int
}

// SearchBlockMemory 按领域过滤检索块记忆，返回结构化记录。
// 使用 embed.PseudoEmbed 生成查询向量，通过 searcher 做数据库检索。
// 结果总摘要长度控制在 TokenBudget 20% 以内（约 800 token，按每字符 0.5 token 保守估算）。
func SearchBlockMemory(ctx context.Context, searcher BlockMemorySearcher, domain, goal string, topK int) ([]*BlockMemoryRecord, error) {
	if topK <= 0 {
		topK = 5
	}
	emb := embed.PseudoEmbed(goal, searcher.EmbeddingDim())
	recs, err := searcher.SearchKnowledgeByTypeAndDomain(ctx, enums.KnowledgeTypeBlockMemory, domain, emb, topK)
	if err != nil {
		return nil, err
	}
	var out []*BlockMemoryRecord
	totalTokens := 0
	maxTokens := 800 // TokenBudget 20% 保守上限
	for _, rec := range recs {
		if totalTokens >= maxTokens {
			break
		}
		bmr := knowledgeToBlockMemory(rec)
		if bmr == nil {
			continue
		}
		// 粗略估算 token：中文约 1 token / 字，保守按 0.5 折算
		tokens := utf8.RuneCountInString(bmr.Summary) / 2
		if tokens == 0 {
			tokens = 1
		}
		if totalTokens+tokens > maxTokens && totalTokens > 0 {
			// 截断最后一个摘要到剩余预算
			remaining := (maxTokens - totalTokens) * 2
			if remaining > 10 {
				runes := []rune(bmr.Summary)
				if len(runes) > remaining {
					bmr.Summary = string(runes[:remaining])
				}
				out = append(out, bmr)
				totalTokens = maxTokens
			}
			break
		}
		totalTokens += tokens
		out = append(out, bmr)
	}
	return out, nil
}

// knowledgeToBlockMemory 把 KnowledgeRecord 转回 BlockMemoryRecord。
// 从 Content 字段中解析 Summary，从 Meta 中提取 session_id / domain / goal。
func knowledgeToBlockMemory(rec *types.KnowledgeRecord) *BlockMemoryRecord {
	if rec == nil {
		return nil
	}
	var sessionID, domain, goal string
	if v, ok := rec.Meta["session_id"].(string); ok {
		sessionID = v
	}
	if v, ok := rec.Meta["domain"].(string); ok {
		domain = v
	}
	if v, ok := rec.Meta["goal"].(string); ok {
		goal = v
	}
	// 从 Content 中解析 Summary：Content 格式为 "领域:...\n目标:...\n结果:..."
	summary := rec.Content
	if idx := strings.Index(summary, "结果:"); idx >= 0 {
		summary = summary[idx+len("结果:"):]
		summary = strings.TrimSpace(summary)
	}
	return &BlockMemoryRecord{
		SessionID: sessionID,
		Domain:    domain,
		Goal:      goal,
		Summary:   summary,
		CreatedAt: rec.CreatedAt,
	}
}

