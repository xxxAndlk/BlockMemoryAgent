package store

import (
	"context"       // 上下文，贯穿所有数据库调用以支持超时与取消
	"database/sql"  // 标准库 SQL 抽象层
	"encoding/json" // 结构体与 JSONB/JSON 列之间的序列化
	"fmt"           // 格式化错误信息
	"log"           // 反序列化失败时记录坏数据
	"time"          // 超时与 NULL 时间处理

	"github.com/blockmemory/agent/backend/pkg/enums" // 枚举常量
	"github.com/blockmemory/agent/backend/pkg/types" // 领域模型
)

// KnowledgeStore 是全局知识库/块记忆相关的 PostgreSQL 存储子层。
// 职责: global_knowledge 表的写入、按类型查询、向量相似搜索、归档与访问计数。
type KnowledgeStore struct {
	db *sql.DB       // 共享连接池
	pg *PostgresStore // 反向引用，用于复用 Embed / EmbeddingDim 能力
}

// Save 写入一条全局知识记录 (含向量)。
// 参数:
//   - rec: 知识记录,含 KnowledgeType/TopicID/Content/Embedding/Meta 等
//
// 返回: SQL 执行错误。
// 副作用: embedding 通过 pgVector 转为字符串文本,依赖 pgvector 扩展解析。
func (s *KnowledgeStore) Save(ctx context.Context, rec *types.KnowledgeRecord) error {
	// meta 即使为 nil 也写入 "{}",保证列非空
	meta, err := json.Marshal(rec.Meta)
	if err != nil {
		return fmt.Errorf("marshal knowledge meta: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO global_knowledge (knowledge_type, topic_id, content, embedding, meta, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, rec.KnowledgeType, rec.TopicID, rec.Content, pgVector(rec.Embedding), meta, rec.CreatedAt)
	return err
}

// GetByType 按知识类型列出记录 (按最近访问时间倒序)。
// 参数:
//   - knowledgeType: 例如 "playbook"/"postmortem"
//   - limit: 返回上限,<=0 时默认 10
//
// 返回: 知识记录切片与 SQL 错误。
// 设计意图: 排除已归档记录,优先返回热数据。
func (s *KnowledgeStore) GetByType(ctx context.Context, knowledgeType string, limit int) ([]*types.KnowledgeRecord, error) {
	if limit <= 0 {
		// 兜底默认值
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, knowledge_type, topic_id, content, meta, access_count, last_accessed, created_at, archived
		FROM global_knowledge
		WHERE knowledge_type = $1 AND archived = false
		ORDER BY last_accessed DESC NULLS LAST
		LIMIT $2
	`, knowledgeType, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// 复用统一的行扫描逻辑
	return scanKnowledgeRows(rows)
}

// Search 向量相似搜索 (依赖 pgvector)。
// 参数:
//   - embedding: 查询向量
//   - topK: 返回前 K 条,<=0 时默认 5
//
// 返回: 按相似度 (L2 距离) 升序的知识记录切片。
// 注意: <=> 是 pgvector 的距离算子,值越小越相似。
func (s *KnowledgeStore) Search(ctx context.Context, embedding []float32, topK int) ([]*types.KnowledgeRecord, error) {
	if topK <= 0 {
		// 兜底默认值
		topK = 5
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, knowledge_type, topic_id, content, meta, access_count, last_accessed, created_at, archived
		FROM global_knowledge
		WHERE archived = false
		ORDER BY embedding <=> $1
		LIMIT $2
	`, pgVector(embedding), topK)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanKnowledgeRows(rows)
}

// SearchByTypeAndDomain 按 knowledge_type 与 meta->>'domain' 双重过滤的向量相似搜索。
// 职责：先按类型和领域精确过滤，再在过滤后的结果中按 pgvector 余弦距离排序取 topK，
// 避免不同领域块记忆之间的串扰。
//
// 参数：
//   - knowledgeType：必填过滤条件（如 KnowledgeTypeBlockMemory）
//   - domain：meta->>'domain' 精确匹配值
//   - embedding：查询向量
//   - topK：返回上限
func (s *KnowledgeStore) SearchByTypeAndDomain(ctx context.Context, knowledgeType enums.KnowledgeType, domain string, embedding []float32, topK int) ([]*types.KnowledgeRecord, error) {
	if topK <= 0 {
		topK = 5
	}
	// pgvector 检索可能因数据量大或索引失效而变慢，加独立超时防止阻塞主流程。
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, knowledge_type, topic_id, content, meta, access_count, last_accessed, created_at, archived
		FROM global_knowledge
		WHERE archived = false AND knowledge_type = $1 AND meta->>'domain' = $2
		ORDER BY embedding <=> $3
		LIMIT $4
	`, knowledgeType, domain, pgVector(embedding), topK)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanKnowledgeRows(rows)
}

// SearchBlockMemory 按 domain 过滤后再语义匹配检索块记忆。
// 先通过 meta->>'domain' 做精确过滤，再在过滤后的结果中按向量相似度排序，
// 避免不同领域块记忆之间的串扰。
func (s *KnowledgeStore) SearchBlockMemory(ctx context.Context, domain, goal string, topK int) ([]*types.KnowledgeRecord, error) {
	if topK <= 0 {
		topK = 5
	}
	query := fmt.Sprintf("领域:%s\n目标:%s", domain, goal)
	emb, err := s.pg.Embed(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	return s.SearchByTypeAndDomain(ctx, enums.KnowledgeTypeBlockMemory, domain, emb, topK)
}

// SearchByType 按 knowledge_type 过滤的向量相似搜索（特性3使用）。
// 职责：限定返回记录的 KnowledgeType，便于把 block_memory / playbook 等分类检索。
// 参数：
//   - knowledgeType：必填过滤条件
//   - embedding：查询向量
//   - topK：返回上限
func (s *KnowledgeStore) SearchByType(ctx context.Context, knowledgeType enums.KnowledgeType, embedding []float32, topK int) ([]*types.KnowledgeRecord, error) {
	if topK <= 0 {
		topK = 5
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, knowledge_type, topic_id, content, meta, access_count, last_accessed, created_at, archived
		FROM global_knowledge
		WHERE archived = false AND knowledge_type = $1
		ORDER BY embedding <=> $2
		LIMIT $3
	`, knowledgeType, pgVector(embedding), topK)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanKnowledgeRows(rows)
}

// Archive 归档指定 ID 的知识 (软删除)。
// 参数:
//   - id: 知识记录主键
//
// 返回: SQL 执行错误。
// 副作用: archived 置 true,后续查询自动排除该记录。
func (s *KnowledgeStore) Archive(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE global_knowledge SET archived = true WHERE id = $1
	`, id)
	return err
}

// IncrementAccessCount 自增访问计数并刷新最近访问时间。
// 参数:
//   - id: 知识记录主键
//
// 返回: SQL 执行错误。
// 设计意图: 配合 GetByType 的排序,实现简单的热度衰减。
func (s *KnowledgeStore) IncrementAccessCount(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE global_knowledge
		SET access_count = access_count + 1, last_accessed = NOW()
		WHERE id = $1
	`, id)
	return err
}

// scanKnowledgeRows 扫描知识库查询结果集,统一处理 NULL 字段与 JSONB 反序列化。
// 参数:
//   - rows: 已执行的 *sql.Rows,列顺序固定为
//     id, knowledge_type, topic_id, content, meta, access_count, last_accessed, created_at, archived
//
// 返回: 知识记录切片;扫描/反序列化失败的单行被跳过。
func scanKnowledgeRows(rows *sql.Rows) ([]*types.KnowledgeRecord, error) {
	var results []*types.KnowledgeRecord
	for rows.Next() {
		var r types.KnowledgeRecord
		var metaRaw []byte
		var lastAccessed sql.NullTime
		err := rows.Scan(&r.ID, &r.KnowledgeType, &r.TopicID, &r.Content, &metaRaw,
			&r.AccessCount, &lastAccessed, &r.CreatedAt, &r.Archived)
		if err != nil {
			// 单行扫描失败跳过,不影响其他行
			continue
		}
		// 仅在 meta 非空时反序列化
		if len(metaRaw) > 0 {
			if err := json.Unmarshal(metaRaw, &r.Meta); err != nil {
				log.Printf("[store] unmarshal global_knowledge.meta failed: id=%d err=%v", r.ID, err)
			}
		}
		// last_accessed 可能为 NULL,有效时填充指针
		if lastAccessed.Valid {
			r.LastAccessed = &lastAccessed.Time
		}
		results = append(results, &r)
	}
	return results, rows.Err()
}
