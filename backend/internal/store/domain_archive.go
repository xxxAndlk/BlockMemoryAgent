// Package store domain_archive.go 实现特性4（domainAgent 持久化复用）的存储层。
//
// 复用 global_knowledge 表 + pgvector，KnowledgeType 固定为 "domain_archive"。
// Meta 列以 JSON 存放 skills/weight/expires_at/role_def_id/session_id 等结构化字段，
// 既保持单表简洁，又便于 SearchKnowledgeByType 复用向量检索能力。
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/blockmemory/agent/backend/internal/embed"
	"github.com/blockmemory/agent/backend/internal/graph"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// domainArchiveMeta 归档元数据，存入 global_knowledge.meta 列。
type domainArchiveMeta struct {
	RoleDefID    string   `json:"role_def_id"`
	Skills       []string `json:"skills"`
	Weight       int      `json:"weight"`
	ExpiresAt    int64    `json:"expires_at"`    // unix seconds
	CreatedAt    int64    `json:"created_at"`    // unix seconds
	Domain       string   `json:"domain"`
	Goal         string   `json:"goal"`
	SessionID    string   `json:"session_id"`
}

// SaveDomainArchive upsert 一条 domainAgent 归档（特性4）。
// 按 (knowledge_type='domain_archive', topic_id=session_id, content 前缀=domain) 近似唯一。
// 为简化实现，写入即新增；权重与过期通过 BumpDomainArchiveWeight 维护。
func (s *PostgresStore) SaveDomainArchive(ctx context.Context, rec *graph.DomainArchiveRecord) error {
	if rec == nil {
		return fmt.Errorf("nil record")
	}
	meta := domainArchiveMeta{
		RoleDefID: rec.RoleDefID,
		Skills:    rec.Skills,
		Weight:    rec.Weight,
		ExpiresAt: rec.ExpiresAt.Unix(),
		CreatedAt: rec.CreatedAt.Unix(),
		Domain:    rec.Domain,
		Goal:      rec.Goal,
		SessionID: rec.SessionID,
	}
	content := fmt.Sprintf("领域:%s\n目标:%s\n摘要:%s", rec.Domain, rec.Goal, rec.ContextSummary)
	emb := embed.PseudoEmbed(content, 768)
	krec := &types.KnowledgeRecord{
		KnowledgeType: "domain_archive",
		TopicID:       rec.SessionID,
		Content:       content,
		Embedding:     emb,
		Meta: map[string]any{
			"role_def_id": meta.RoleDefID,
			"skills":      meta.Skills,
			"weight":      meta.Weight,
			"expires_at":  meta.ExpiresAt,
			"created_at":  meta.CreatedAt,
			"domain":      meta.Domain,
			"goal":        meta.Goal,
			"session_id":  meta.SessionID,
		},
		CreatedAt: rec.CreatedAt,
	}
	return s.SaveKnowledge(ctx, krec)
}

// SearchDomainArchive 按领域/目标文本检索 topK 条相似归档，过滤已过期。
func (s *PostgresStore) SearchDomainArchive(ctx context.Context, domain, goal string, topK int) ([]*graph.DomainArchiveRecord, error) {
	if topK <= 0 {
		topK = 5
	}
	query := fmt.Sprintf("领域:%s\n目标:%s", domain, goal)
	emb := embed.PseudoEmbed(query, 768)
	recs, err := s.SearchKnowledgeByType(ctx, "domain_archive", emb, topK)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	var out []*graph.DomainArchiveRecord
	for _, r := range recs {
		expiresAt, _ := r.Meta["expires_at"].(float64)
		if int64(expiresAt) > 0 && int64(expiresAt) < now {
			continue // 已过期，跳过
		}
		out = append(out, knowledgeToArchive(r))
	}
	return out, nil
}

// BumpDomainArchiveWeight 命中复用时权重 +1 且延后过期时间。
// 通过 UPDATE global_knowledge SET meta=... WHERE id=$1 实现。
func (s *PostgresStore) BumpDomainArchiveWeight(ctx context.Context, archiveID string, ttl time.Duration) error {
	// 取现有 meta
	var metaRaw []byte
	row := s.db.QueryRowContext(ctx, `SELECT meta FROM global_knowledge WHERE id=$1`, archiveID)
	if err := row.Scan(&metaRaw); err != nil {
		return fmt.Errorf("load archive meta: %w", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		return fmt.Errorf("unmarshal meta: %w", err)
	}
	w, _ := meta["weight"].(float64)
	meta["weight"] = int(w) + 1
	meta["expires_at"] = time.Now().Add(ttl).Unix()
	newMeta, _ := json.Marshal(meta)
	_, err := s.db.ExecContext(ctx, `UPDATE global_knowledge SET meta=$1, last_accessed=NOW() WHERE id=$2`, newMeta, archiveID)
	return err
}

// CleanupExpiredDomainArchives 删除已过期的 domain_archive 记录。
// 通过 meta->>'expires_at' 与当前时间比较过滤。
func (s *PostgresStore) CleanupExpiredDomainArchives(ctx context.Context) (int, error) {
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM global_knowledge
		WHERE knowledge_type='domain_archive'
		  AND (meta->>'expires_at')::bigint < $1
	`, now)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// knowledgeToArchive 把 KnowledgeRecord 转为 DomainArchiveRecord。
func knowledgeToArchive(r *types.KnowledgeRecord) *graph.DomainArchiveRecord {
	if r == nil {
		return nil
	}
	getStr := func(k string) string {
		if v, ok := r.Meta[k].(string); ok {
			return v
		}
		return ""
	}
	getInt := func(k string) int {
		switch v := r.Meta[k].(type) {
		case float64:
			return int(v)
		case int:
			return v
		}
		return 0
	}
	getTime := func(k string) time.Time {
		if v, ok := r.Meta[k].(float64); ok {
			return time.Unix(int64(v), 0)
		}
		return time.Time{}
	}
	var skills []string
	if v, ok := r.Meta["skills"].([]any); ok {
		for _, s := range v {
			if sv, ok := s.(string); ok {
				skills = append(skills, sv)
			}
		}
	}
	return &graph.DomainArchiveRecord{
		ArchiveID:      fmt.Sprintf("%d", r.ID),
		SessionID:      getStr("session_id"),
		Domain:         getStr("domain"),
		Goal:           getStr("goal"),
		RoleDefID:      getStr("role_def_id"),
		Skills:         skills,
		ContextSummary: r.Content,
		Weight:         getInt("weight"),
		ExpiresAt:      getTime("expires_at"),
		CreatedAt:      r.CreatedAt,
	}
}
