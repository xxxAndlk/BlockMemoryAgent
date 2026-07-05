// Package memory block_vector.go 提供特性3（domainAgent 后向量检索）所需的
// 块记忆记录结构与 global_knowledge 表之间的转换。
//
// 伪嵌入实现抽到 internal/embed 包，避免 store ↔ memory 循环依赖。
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/blockmemory/agent/backend/internal/embed"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// FactScope 定义事实的作用范围。
type FactScope string

const (
	// FactScopeGlobal 全局共享事实：跨领域、跨任务都有效（如项目技术栈）。
	FactScopeGlobal FactScope = "global"
	// FactScopeDomain 领域级事实：仅在同一领域内有效（如该领域使用的框架）。
	FactScopeDomain FactScope = "domain"
	// FactScopeTask 任务级事实：仅在当前任务内有效（如本次修复的具体错误模式）。
	FactScopeTask FactScope = "task"
)

// Fact 块记忆键值化事实。
// 比纯文本摘要更精准，注入 prompt 时可按 scope 按需取用，减少 token 占用。
type Fact struct {
	Key   string    `json:"key"`
	Value string    `json:"value"`
	Scope FactScope `json:"scope"`
}

// BlockMemoryRecord 块记忆条目：domainAgent 完成一个 SessionBlock 后归档的
// 领域/目标/结果摘要 + 结构化 facts，写入 global_knowledge 表（KnowledgeType="block_memory"）。
type BlockMemoryRecord struct {
	SessionID string    // 所属会话
	Domain    string    // 领域名
	Goal      string    // 领域目标
	Summary   string    // 任务结果摘要
	Facts     []Fact    // 结构化关键事实
	CreatedAt time.Time // 归档时间
}

// ToKnowledgeRecord 把块记忆转为 global_knowledge 表记录。
// Content 拼装为可读文本，便于检索后直接注入 prompt；facts 以 JSON 数组存入 meta。
func (r *BlockMemoryRecord) ToKnowledgeRecord(dim int) *types.KnowledgeRecord {
	content := strings.Join([]string{
		"领域:", r.Domain,
		"目标:", r.Goal,
		"结果:", r.Summary,
	}, "\n")
	factsJSON, _ := json.Marshal(r.Facts)
	meta := map[string]any{
		"session_id": r.SessionID,
		"domain":     r.Domain,
		"goal":       r.Goal,
		"facts":      string(factsJSON),
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
	// Embed 将查询文本编码为向量（P3-3：由 store 层统一封装嵌入实现）。
	Embed(ctx context.Context, text string) ([]float32, error)
}

// SearchBlockMemory 按领域过滤检索块记忆，返回结构化记录。
// 使用 searcher.Embed 生成查询向量（P3-3：可接入真实 embedding 模型），通过 searcher 做数据库检索。
// 默认只取 scope=domain 与 scope=task 的事实；全局共享事实请用 GlobalRetriever 单独召回。
// 结果总摘要长度控制在 TokenBudget 20% 以内（约 800 token，按每字符 0.5 token 保守估算）。
func SearchBlockMemory(ctx context.Context, searcher BlockMemorySearcher, domain, goal string, topK int) ([]*BlockMemoryRecord, error) {
	if topK <= 0 {
		topK = 5
	}
	emb, err := searcher.Embed(ctx, goal)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
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
		// 当前领域任务默认只保留 domain / task 级事实
		bmr.Facts = filterFactsByScope(bmr.Facts, FactScopeDomain, FactScopeTask)
		// 粗略估算 token：中文约 1 token / 字，保守按 0.5 折算
		tokens := utf8.RuneCountInString(bmr.Summary)/2 + factsTokenEstimate(bmr.Facts)
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

// filterFactsByScope 保留指定 scope 的事实。
func filterFactsByScope(facts []Fact, scopes ...FactScope) []Fact {
	if len(facts) == 0 || len(scopes) == 0 {
		return facts
	}
	allowed := make(map[FactScope]bool, len(scopes))
	for _, s := range scopes {
		allowed[s] = true
	}
	var out []Fact
	for _, f := range facts {
		if allowed[f.Scope] {
			out = append(out, f)
		}
	}
	return out
}

// factsTokenEstimate 粗略估算 facts 的 token 数。
func factsTokenEstimate(facts []Fact) int {
	total := 0
	for _, f := range facts {
		total += utf8.RuneCountInString(f.Key) / 2
		total += utf8.RuneCountInString(f.Value) / 2
	}
	return total
}

// knowledgeToBlockMemory 把 KnowledgeRecord 转回 BlockMemoryRecord。
// 从 Content 字段中解析 Summary，从 Meta 中提取 session_id / domain / goal / facts。
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
	var facts []Fact
	if raw, ok := rec.Meta["facts"].(string); ok && raw != "" {
		if err := json.Unmarshal([]byte(raw), &facts); err != nil {
			log.Printf("[memory] unmarshal block memory facts failed: %v", err)
		}
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
		Facts:     facts,
		CreatedAt: rec.CreatedAt,
	}
}

// FormatBlockMemoryFacts 把 facts 格式化为 prompt 注入文本。
// 每行一条：- key: value (scope)
func FormatBlockMemoryFacts(facts []Fact) string {
	if len(facts) == 0 {
		return ""
	}
	var b strings.Builder
	for _, f := range facts {
		b.WriteString(fmt.Sprintf("- %s: %s (%s)\n", f.Key, f.Value, f.Scope))
	}
	return b.String()
}
