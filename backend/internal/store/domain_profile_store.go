package store

// domain_profile_store.go 领域档案存储（TODO #17 领域注册表 T23）+ 按源删除原语（#16 T21）。
//
// 零新表：领域档案复用 global_knowledge（knowledge_type=domain_profile），
// 以 meta->>'domain' 为唯一键 upsert（schema 已有 meta->>'domain' btree 索引）。
// meta 字段约定：{domain, display_name, aliases[], files[], subproject, created_at, last_used, source}。
// embedding = 指纹文本（display_name + 职责摘要）的语义向量，供跨场景语义寻档。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/pkg/enums" // 知识类型枚举
	"github.com/blockmemory/agent/backend/pkg/types" // 领域模型
)

// DomainProfileUpsert 一次领域档案写入（TODO #17 T23/T25/T26 共用）。
// Domain 是唯一键；其余字段按「并集 / 非空覆盖」语义合并进现有档案：
//   - Files / Aliases：与现有值取并集（幂等去重，T25 文件清单自动维护走这条）；
//   - DisplayName / Subproject：非空才覆盖（空串沿用现值）；
//   - Summary：档案正文（content，种子段的历史摘要源）；非空才覆盖；
//   - Fingerprint：语义指纹文本（展示名 + 职责摘要），非空时重算 embedding；
//     增量路径（仅补文件清单）留空，避免无谓 embed 调用；
//   - Source：写入来源标记（"block_memory" / "project_md" / ...）。
type DomainProfileUpsert struct {
	Domain      string
	DisplayName string
	Aliases     []string
	Files       []string
	Subproject  string
	Summary     string
	Fingerprint string
	Source      string
}

// UpsertDomainProfile 按 meta->>'domain' 精确查后 INSERT/UPDATE（TODO #17 T23）。
// nil-DB 静默返回（测试桩 / 未接库的降级形态）；调用方一律 best-effort（失败仅日志）。
func (s *KnowledgeStore) UpsertDomainProfile(ctx context.Context, up DomainProfileUpsert) error {
	if s == nil || s.db == nil {
		return nil
	}
	domain := strings.TrimSpace(up.Domain)
	if domain == "" {
		return fmt.Errorf("upsert domain profile: domain required")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	now := time.Now()

	// 同 domain 取最新一条（防历史重复行干扰），仅取 id + meta。
	var (
		id      int64
		metaRaw []byte
	)
	err := s.db.QueryRowContext(ctx, `
			SELECT id, meta FROM global_knowledge
			WHERE knowledge_type = $1 AND archived = false AND meta->>'domain' = $2
			ORDER BY created_at DESC
			LIMIT 1
		`, enums.KnowledgeTypeDomainProfile, domain).Scan(&id, &metaRaw)

	switch {
	case err == sql.ErrNoRows:
		// 新档案：embedding 用指纹文本；embed 失败降级为空向量（列可空，不阻塞建档）。
		var emb []float32
		if fp := strings.TrimSpace(up.Fingerprint); fp != "" {
			if e, ferr := s.pg.Embed(ctx, fp); ferr == nil {
				emb = e
			} else {
				logError(s.log, ctx, "[store] domain profile embed failed (store empty vector)", ferr)
			}
		}
		meta := map[string]any{
			"domain":       domain,
			"display_name": firstNonEmptyStr(up.DisplayName, domain),
			"aliases":      dedupStrings(up.Aliases),
			"files":        dedupStrings(up.Files),
			"subproject":   up.Subproject,
			"created_at":   now.UTC().Format(time.RFC3339),
			"last_used":    now.UTC().Format(time.RFC3339),
			"source":       up.Source,
		}
		metaJSON, merr := json.Marshal(meta)
		if merr != nil {
			return fmt.Errorf("marshal domain profile meta: %w", merr)
		}
		content := firstNonEmptyStr(up.Fingerprint, up.Summary, up.DisplayName, domain)
		_, ierr := s.db.ExecContext(ctx, `
				INSERT INTO global_knowledge (knowledge_type, content, embedding, meta, created_at, last_accessed)
				VALUES ($1, $2, $3, $4, $5, $5)
			`, enums.KnowledgeTypeDomainProfile, content, pgVector(emb), metaJSON, now)
		return ierr
	case err != nil:
		return err
	}

	// 更新分支：并集合并 files/aliases，非空覆盖 display_name/subproject，刷新 last_used。
	meta := map[string]any{}
	if len(metaRaw) > 0 {
		if uerr := json.Unmarshal(metaRaw, &meta); uerr != nil {
			logError(s.log, ctx, fmt.Sprintf("[store] unmarshal domain profile meta failed: id=%d", id), uerr)
			meta = map[string]any{}
		}
	}
	meta["domain"] = domain
	if up.DisplayName != "" {
		meta["display_name"] = up.DisplayName
	}
	meta["files"] = unionStrings(stringSliceFromAny(meta["files"]), up.Files)
	meta["aliases"] = unionStrings(stringSliceFromAny(meta["aliases"]), up.Aliases)
	if up.Subproject != "" {
		meta["subproject"] = up.Subproject
	}
	meta["last_used"] = now.UTC().Format(time.RFC3339)
	if up.Source != "" {
		meta["source"] = up.Source
	}
	metaJSON, merr := json.Marshal(meta)
	if merr != nil {
		return fmt.Errorf("marshal domain profile meta: %w", merr)
	}
	// content 覆盖（Summary 非空才接管）与 embedding 重算（指纹非空才做）按需拼装；
	// embed 失败保留旧向量（不阻塞档案更新）。
	sets := []string{"meta = $1", "last_accessed = $2"}
	args := []any{metaJSON, now}
	if s2 := strings.TrimSpace(up.Summary); s2 != "" {
		sets = append(sets, fmt.Sprintf("content = $%d", len(args)+1))
		args = append(args, s2)
	}
	if fp := strings.TrimSpace(up.Fingerprint); fp != "" {
		if e, ferr := s.pg.Embed(ctx, fp); ferr == nil {
			sets = append(sets, fmt.Sprintf("embedding = $%d", len(args)+1))
			args = append(args, pgVector(e))
		}
	}
	args = append(args, id)
	_, uerr := s.db.ExecContext(ctx,
		"UPDATE global_knowledge SET "+strings.Join(sets, ", ")+" WHERE id = $"+strconv.Itoa(len(args)), args...)
	return uerr
}

// GetDomainProfile 按 domain 精确取领域档案（TODO #17 T24 派发匹配用）。
// 未建档返回 (nil, nil)；nil-DB 静默返回 nil。
func (s *KnowledgeStore) GetDomainProfile(ctx context.Context, domain string) (*types.KnowledgeRecord, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `
			SELECT id, knowledge_type, topic_id, content, meta, access_count, last_accessed, created_at, archived
			FROM global_knowledge
			WHERE knowledge_type = $1 AND archived = false AND meta->>'domain' = $2
			ORDER BY created_at DESC
			LIMIT 1
		`, enums.KnowledgeTypeDomainProfile, domain)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	recs, err := s.scanKnowledgeRows(ctx, rows)
	if err != nil || len(recs) == 0 {
		return nil, err
	}
	return recs[0], nil
}

// ListDomainProfiles 返回全部未归档领域档案（TODO #17 T24 派发匹配用）。
// 档案量级小（数十条），全量读出后由派发侧做名字/别名/路径匹配；nil-DB 返回空。
func (s *KnowledgeStore) ListDomainProfiles(ctx context.Context) ([]*types.KnowledgeRecord, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `
			SELECT id, knowledge_type, topic_id, content, meta, access_count, last_accessed, created_at, archived
			FROM global_knowledge
			WHERE knowledge_type = $1 AND archived = false
			ORDER BY meta->>'last_used' DESC
		`, enums.KnowledgeTypeDomainProfile)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanKnowledgeRows(ctx, rows)
}

// QueryBlockMemoryByDomain 按 meta->>'task_domain' 取未归档块记忆（TODO #17 T24 记忆成链用），
// 按创建时间倒序 LIMIT n。供档案命中后把该领域的既有块记忆摘要拼进冷复活种子。
func (s *KnowledgeStore) QueryBlockMemoryByDomain(ctx context.Context, taskDomain string, limit int) ([]*types.KnowledgeRecord, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	taskDomain = strings.TrimSpace(taskDomain)
	if taskDomain == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 5
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `
			SELECT id, knowledge_type, topic_id, content, meta, access_count, last_accessed, created_at, archived
			FROM global_knowledge
			WHERE archived = false AND knowledge_type = $1 AND meta->>'task_domain' = $2
			ORDER BY created_at DESC
			LIMIT $3
		`, enums.KnowledgeTypeBlockMemory, taskDomain, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanKnowledgeRows(ctx, rows)
}

// DeleteBySource 按来源删除知识记录（TODO #16 T21 按源删除原语）。
// 供外部摄入（ingest_file 类能力）重灌前清旧块：DELETE WHERE meta->>'source' = $1；
// knowledgeType 非空时限定类型（推荐——source 值跨类型可能撞名），空串跨类型删。
// 返回受影响行数；nil-DB 视为 (0, nil)。
func (s *KnowledgeStore) DeleteBySource(ctx context.Context, source string, knowledgeType enums.KnowledgeType) (int64, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	if strings.TrimSpace(source) == "" {
		return 0, fmt.Errorf("delete by source: source required")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	q := `DELETE FROM global_knowledge WHERE meta->>'source' = $1`
	args := []any{source}
	if knowledgeType != "" {
		q += ` AND knowledge_type = $2`
		args = append(args, knowledgeType)
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---- 小工具：字符串切片并集 / 去重 / any 还原 ----

// firstNonEmptyStr 返回第一个非空串；全空返回末参兜底。
func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// dedupStrings 原顺序去重（跳过空串）。
func dedupStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// unionStrings a ∪ b（原顺序 a 优先，幂等去重）。
func unionStrings(a, b []string) []string {
	return dedupStrings(append(append([]string{}, a...), b...))
}

// stringSliceFromAny 把 JSONB 反序列化出的 any（[]any 或 []string）还原为 []string。
func stringSliceFromAny(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}
