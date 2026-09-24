package store

import (
	"context"       // 上下文，贯穿所有数据库调用以支持超时与取消
	"database/sql"  // 标准库 SQL 抽象层
	"encoding/json" // 结构体与 JSONB/JSON 列之间的序列化
	"fmt"           // 格式化错误信息
	"strings"       // TrimSpace 空查询判定
	"time"          // 超时与 NULL 时间处理

	"github.com/blockmemory/agent/backend/pkg/enums" // 枚举常量
	"github.com/blockmemory/agent/backend/pkg/types" // 领域模型
)

// minBlockMemoryScore 是块记忆召回的最小余弦相似度阈值。
// 低于该值的命中视为弱相关（不同任务语义擦边），不注入上下文，防旧任务事实
// 诱导模型重复执行已完成改动。bge-m3 对"修改怪物血条 UI"vs"调整按钮样式"这类
// 跨任务擦边的相似度通常低于该值；同域复用（"怪物路径"vs"怪物路径宽度"）高于该值。
const minBlockMemoryScore = 0.4

// crossSessionBlockMemoryScore 是跨 session 块记忆补位召回的相似度阈值，
// 高于 session 内阈值：历史记忆没有当前会话上下文佐证，仅语义足够强才注入。
// 实测（bge-m3，塔防项目 goal vs 前日事实）：同项目相关事实 0.50-0.69，
// 弱相关跨任务事实 ~0.47 及以下，0.5 恰好分隔。
const crossSessionBlockMemoryScore = 0.5

// KnowledgeStore 是全局知识库/块记忆相关的 PostgreSQL 存储子层。
// 职责: global_knowledge 表的写入、按类型查询、向量相似搜索、归档与访问计数。
type KnowledgeStore struct {
	db  *sql.DB        // 共享连接池
	pg  *PostgresStore // 反向引用，用于复用 Embed / EmbeddingDim 能力
	log Logger         // 结构化日志器，由 PostgresStore.SetLogger 传播注入；nil 时回退标准库 log
}

// Save 写入一条全局知识记录 (含向量)。
// 参数:
//   - ctx: 请求上下文。
//   - rec: 知识记录,含 KnowledgeType/TopicID/Content/Embedding/Meta 等
//
// 返回: SQL 执行错误。
// 副作用: embedding 通过 pgVector 转为字符串文本,依赖 pgvector 扩展解析。
func (s *KnowledgeStore) Save(ctx context.Context, rec *types.KnowledgeRecord) error {
	// meta 即使为 nil 也写入 "{}",保证列非空，避免下游 JSONB 操作报错
	meta, err := json.Marshal(rec.Meta)
	if err != nil {
		return fmt.Errorf("marshal knowledge meta: %w", err)
	}
	// 执行 INSERT，向量以 pgVector 文本形式传入。
	// last_accessed 与 created_at 同值初始化：ArchiveStale（data.knowledge_archive_days）
	// 按 last_accessed 判定陈旧度，缺省 NULL 会让归档条件恒为 NULL 而永不命中
	//（2026-09-16 修复：1067 条块记忆全部 last_accessed IS NULL，归档开关形同虚设）。
	_, err = s.db.ExecContext(ctx, `
			INSERT INTO global_knowledge (knowledge_type, topic_id, content, embedding, meta, created_at, last_accessed)
			VALUES ($1, $2, $3, $4, $5, $6, $6)
		`, rec.KnowledgeType, rec.TopicID, rec.Content, pgVector(rec.Embedding), meta, rec.CreatedAt)
	return err
}

// FindSimilarBlockMemory 在同 task_domain 的未归档块记忆中按向量近邻找最相似一条
// （cosine 距离 <= maxDistance 才返回，无命中返回 nil）。
// 供写入侧近邻去重：同一结论跨会话反复沉淀时更新既有记录而非新增行。
// taskDomain 为空串时只匹配 meta 无 task_domain 标签的记录（coalesce 归一）。
func (s *KnowledgeStore) FindSimilarBlockMemory(ctx context.Context, embedding []float32, taskDomain string, maxDistance float64) (*types.KnowledgeRecord, error) {
	if len(embedding) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `
			SELECT id, knowledge_type, topic_id, content, meta, access_count, last_accessed, created_at, archived,
			       1 - (embedding <=> $1) AS score
			FROM global_knowledge
			WHERE archived = false AND knowledge_type = $2
			  AND coalesce(meta->>'task_domain','') = $3
			  AND embedding IS NOT NULL AND embedding <=> $1 <= $4
			ORDER BY embedding <=> $1
			LIMIT 1`,
		pgVector(embedding), enums.KnowledgeTypeBlockMemory, strings.TrimSpace(taskDomain), maxDistance)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	recs, err := s.scanKnowledgeRowsWithScore(ctx, rows)
	if err != nil || len(recs) == 0 {
		return nil, err
	}
	return recs[0], nil
}

// UpdateContentEmbedding 就近刷新既有知识记录的内容与向量（去重合并写路径），
// 同时刷新 last_accessed（视作一次"使用"）。不改 meta/created_at。
func (s *KnowledgeStore) UpdateContentEmbedding(ctx context.Context, id int64, content string, embedding []float32) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `
			UPDATE global_knowledge SET content = $2, embedding = $3, last_accessed = NOW() WHERE id = $1
		`, id, content, pgVector(embedding))
	return err
}

// ListIndexEntries 返回沉淀索引槽的一行式条目（TODO #20③+#22③）：
// 全类型未归档记录按 (last_accessed DESC NULLS LAST, created_at DESC) 取 limit 条，
// 供会话启动渲染【沉淀索引】。含 untrusted 围栏的行由调用方过滤（provenance 门）。
func (s *KnowledgeStore) ListIndexEntries(ctx context.Context, limit int) ([]*types.KnowledgeRecord, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `
			SELECT id, knowledge_type, topic_id, content, meta, access_count, last_accessed, created_at, archived
			FROM global_knowledge
			WHERE archived = false
			ORDER BY last_accessed DESC NULLS LAST, created_at DESC
			LIMIT $1
		`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanKnowledgeRows(ctx, rows)
}

// SearchBlockMemoryByGoal 按 sessionID 过滤后做语义匹配检索块记忆。
// sessionID 非空时仅召回该 session 写入的记录，避免跨 session 污染
// （实证：旧 session 的"重写全部 JS"任务文本被召回，污染新 session 任务上下文）。
// sessionID 为空时退化为全局检索（向后兼容旧调用与测试）。
// 参数:
//   - ctx:       请求上下文。
//   - sessionID: 会话 ID；空串表示不过滤。
//   - goal:      目标文本，用于生成查询向量。
//   - topK:      返回上限。
//
// 返回: 知识记录切片与错误。
func (s *KnowledgeStore) SearchBlockMemoryByGoal(ctx context.Context, sessionID, goal string, topK int) ([]*types.KnowledgeRecord, error) {
	if topK <= 0 {
		topK = 5
	}
	// 调用 PostgresStore.Embed 将查询文本转为向量（支持外部 embedder 回退）
	emb, err := s.pg.Embed(ctx, goal)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	// sessionID 非空：按 session_id 精确过滤后做向量检索。
	if sessionID != "" {
		return s.SearchByTypeAndSession(ctx, enums.KnowledgeTypeBlockMemory, sessionID, emb, topK)
	}
	// 仅按 knowledge_type 过滤做向量检索，不做 domain/session 过滤
	return s.SearchByType(ctx, enums.KnowledgeTypeBlockMemory, emb, topK)
}

// SearchBlockMemoryCrossSession 跨 session 语义补位检索块记忆。
// 供 session 内召回不足 topK 时补位：块记忆是全局沉淀，任何会话只要相关度
// 足够高即可召回（纯语义过滤，不做项目隔离）；仅排除当前 session 已召回记录
// （session_id 不等），与 session 内召回结果天然不重叠。
// 阈值 crossSessionBlockMemoryScore 高于 session 内阈值，仅强相关命中补位。
// 参数:
//   - ctx:              请求上下文。
//   - excludeSessionID: 排除的当前 session ID。
//   - goal:             目标文本，用于生成查询向量。
//   - topK:             返回上限。
func (s *KnowledgeStore) SearchBlockMemoryCrossSession(ctx context.Context, excludeSessionID, goal string, topK int) ([]*types.KnowledgeRecord, error) {
	if topK <= 0 {
		return nil, nil
	}
	emb, err := s.pg.Embed(ctx, goal)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `
			SELECT id, knowledge_type, topic_id, content, meta, access_count, last_accessed, created_at, archived,
			       1 - (embedding <=> $3) AS score
			FROM global_knowledge
			WHERE archived = false AND knowledge_type = $1
			  AND (coalesce(meta->>'session_id','') = '' OR meta->>'session_id' <> $2)
			  AND embedding <=> $3 <= $4
			ORDER BY embedding <=> $3
			LIMIT $5`,
		enums.KnowledgeTypeBlockMemory, excludeSessionID, pgVector(emb), 1-crossSessionBlockMemoryScore, topK)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanKnowledgeRowsWithScore(ctx, rows)
}

// Query 黑板模式scope 确定性检索：按 session_id + parent_id + task_domain
// 精确过滤块记忆，替纯语义召回的"按兄弟真实 scope 取切片"。在 SearchBlockMemoryByGoal
// 语义召回之上叠加 scope 过滤--兄弟产出按 scope 共享，每个 Agent 只取自己 scope 的切片，
// 避免 mailbox 全量广播的上下文互染。
//
// query 非空：叠加向量语义排序（cosine 阈值过滤 + ORDER BY 距离），同语义召回但多 scope 过滤；
// query 空串：跳过 cosine（纯 scope 过滤 + ORDER BY created_at DESC），省 embedding，供每轮摄取等
// 无需语义重排的场景。excludeSubAgentID 非空时排除该 Agent 自身写入的记录（每轮摄取不回显自己结论）。
func (s *KnowledgeStore) Query(ctx context.Context, sessionID, parentID, taskDomain, query string, topK int, excludeSubAgentID string) ([]*types.KnowledgeRecord, error) {
	if topK <= 0 {
		topK = 5
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	query = strings.TrimSpace(query)
	// 语义分支：embedding + cosine 阈值过滤 + 距离排序。
	if query != "" {
		emb, err := s.pg.Embed(ctx, query)
		if err != nil {
			return nil, fmt.Errorf("embed query: %w", err)
		}
		rows, err := s.db.QueryContext(ctx, `
				SELECT id, knowledge_type, topic_id, content, meta, access_count, last_accessed, created_at, archived,
				       1 - (embedding <=> $3) AS score
				FROM global_knowledge
				WHERE archived = false AND knowledge_type = $1
				  AND meta->>'session_id' = $2
				  AND meta->>'parent_id' = $4 AND meta->>'task_domain' = $5
				  AND ($6 = '' OR meta->>'sub_agent_id' <> $6)
				  AND embedding <=> $3 <= $7
				ORDER BY embedding <=> $3
				LIMIT $8
			`, enums.KnowledgeTypeBlockMemory, sessionID, pgVector(emb), parentID, taskDomain, excludeSubAgentID, 1-minBlockMemoryScore, topK)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		return s.scanKnowledgeRowsWithScore(ctx, rows)
	}
	// 纯 scope 分支：无 cosine，省 embedding，按新近排序。
	rows, err := s.db.QueryContext(ctx, `
			SELECT id, knowledge_type, topic_id, content, meta, access_count, last_accessed, created_at, archived
			FROM global_knowledge
			WHERE archived = false AND knowledge_type = $1
			  AND meta->>'session_id' = $2
			  AND meta->>'parent_id' = $3 AND meta->>'task_domain' = $4
			  AND ($5 = '' OR meta->>'sub_agent_id' <> $5)
			ORDER BY created_at DESC
			LIMIT $6
		`, enums.KnowledgeTypeBlockMemory, sessionID, parentID, taskDomain, excludeSubAgentID, topK)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanKnowledgeRows(ctx, rows)
}

// SearchByTypeAndSession 按 knowledge_type + session_id 过滤的向量相似搜索。
// 供 block-memory 召回侧按 session 隔离使用，避免跨 session 污染。
// 同时过滤弱相关命中（余弦相似度 < minBlockMemoryScore 不返回）：旧任务事实与
// 新任务语义擦边时（如"怪物贴图"vs"UI 样式调整"）注入会诱导模型重复执行已完成
// 改动，是上下文污染的主要来源（实证：塔防怪物 UI 被重复修改事故调查结论）。
func (s *KnowledgeStore) SearchByTypeAndSession(ctx context.Context, knowledgeType enums.KnowledgeType, sessionID string, embedding []float32, topK int) ([]*types.KnowledgeRecord, error) {
	if topK <= 0 {
		topK = 5
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// SELECT 附带相似度分数（1 - cosine distance）；WHERE 按分数阈值过滤。
	rows, err := s.db.QueryContext(ctx, `
			SELECT id, knowledge_type, topic_id, content, meta, access_count, last_accessed, created_at, archived,
			       1 - (embedding <=> $3) AS score
			FROM global_knowledge
			WHERE archived = false AND knowledge_type = $1 AND meta->>'session_id' = $2
			  AND embedding <=> $3 <= $4
			ORDER BY embedding <=> $3
			LIMIT $5
		`, knowledgeType, sessionID, pgVector(embedding), 1-minBlockMemoryScore, topK)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanKnowledgeRowsWithScore(ctx, rows)
}

// SearchByType 按 knowledge_type 过滤的向量相似搜索（特性3使用）。
// 职责：限定返回记录的 KnowledgeType，便于把 block_memory / playbook 等分类检索。
// 参数：
//   - ctx：请求上下文。
//   - knowledgeType：必填过滤条件
//   - embedding：查询向量
//   - topK：返回上限
//
// 返回: 知识记录切片与错误。
func (s *KnowledgeStore) SearchByType(ctx context.Context, knowledgeType enums.KnowledgeType, embedding []float32, topK int) ([]*types.KnowledgeRecord, error) {
	if topK <= 0 {
		topK = 5
	}
	// 按类型过滤后，使用 L2 距离排序
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
	// 确保结果集关闭
	defer rows.Close()
	return s.scanKnowledgeRows(ctx, rows)
}

// Archive 归档指定 ID 的知识 (软删除)。
// 参数:
//   - ctx: 请求上下文。
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

// ArchiveStale 陈旧知识批量归档（TODO #18-2 T29 数据生命周期）：
// block_memory / external_kb 中 last_accessed 早于 olderThanDays 天的记录 archived=true。
// 语义是"冷备不删"——查询层全局排除 archived，归档后不再参与召回，数据仍在库里。
// 只动这两类：domain_profile / skill 等结构化知识有独立生命周期，不随访问冷热归档。
//
// 参数:
//   - ctx: 请求上下文。
//   - olderThanDays: 阈值天数；<=0 时直接返回 0（清理开关，config 缺省 0=关）。
//
// 返回: 归档行数与 SQL 错误；nil 库 / nil 接收者 no-op（测试与未接线场景）。
func (s *KnowledgeStore) ArchiveStale(ctx context.Context, olderThanDays int) (int64, error) {
	if s == nil || s.db == nil || olderThanDays <= 0 {
		return 0, nil
	}
	res, err := s.db.ExecContext(ctx, `
			UPDATE global_knowledge SET archived = true
			WHERE archived = false
			  AND knowledge_type IN ('block_memory', 'external_kb')
			  AND last_accessed < NOW() - make_interval(days => $1)
		`, olderThanDays)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ListAll 导出用全量清单（TODO #18-2 T29 GET /api/export/memory）：
// 未归档记录按 id 升序，上限 10000 条防一次性拉爆内存。
func (s *KnowledgeStore) ListAll(ctx context.Context) ([]*types.KnowledgeRecord, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
			SELECT id, knowledge_type, topic_id, content, meta, access_count, last_accessed, created_at, archived
			FROM global_knowledge
			WHERE archived = false
			ORDER BY id ASC
			LIMIT 10000
		`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanKnowledgeRows(ctx, rows)
}

// BumpReuse 递增知识记录的 Meta.reuse_count（JSONB 就地更新，缺省 0），
// 并同步刷新 last_accessed（召回命中 = 一次使用，归档判定据此延缓）。
// 供块记忆召回侧价值反馈闭环使用：召回命中后标记复用次数，下次排序按 reuse_count 降序。
// 返回 SQL 执行错误，调用方 best-effort 处理（失败仅记日志，不阻塞派发）。
func (s *KnowledgeStore) BumpReuse(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `
			UPDATE global_knowledge
			SET meta = jsonb_set(meta, '{reuse_count}',
				to_jsonb((COALESCE((meta->>'reuse_count')::int, 0) + 1)::int)),
			    last_accessed = NOW()
			WHERE id = $1
		`, id)
	return err
}

// scanKnowledgeRows 扫描知识库查询结果集,统一处理 NULL 字段与 JSONB 反序列化。
// 参数:
//   - ctx: 请求上下文，用于记录反序列化失败日志。
//   - rows: 已执行的 *sql.Rows,列顺序固定为：
//     id(ID)、knowledge_type(知识类型)、topic_id(话题 ID)、content(内容)、meta(元数据)、
//     access_count(访问计数)、last_accessed(最后访问时间)、created_at(创建时间)、archived(是否归档)
//
// 返回: 知识记录切片;扫描/反序列化失败的单行被跳过。
func (s *KnowledgeStore) scanKnowledgeRows(ctx context.Context, rows *sql.Rows) ([]*types.KnowledgeRecord, error) {
	var results []*types.KnowledgeRecord
	for rows.Next() {
		var r types.KnowledgeRecord
		var metaRaw []byte
		var lastAccessed sql.NullTime
		// 按固定列顺序扫描，last_accessed 用 NullTime 处理 NULL
		err := rows.Scan(&r.ID, &r.KnowledgeType, &r.TopicID, &r.Content, &metaRaw,
			&r.AccessCount, &lastAccessed, &r.CreatedAt, &r.Archived)
		if err != nil {
			// 单行扫描失败跳过,不影响其他行
			continue
		}
		// 仅在 meta 非空时反序列化
		if len(metaRaw) > 0 {
			if err := json.Unmarshal(metaRaw, &r.Meta); err != nil {
				logError(s.log, ctx, fmt.Sprintf("[store] unmarshal global_knowledge.meta failed: id=%d", r.ID), err)
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

// scanKnowledgeRowsWithScore 同 scanKnowledgeRows，额外扫描第 10 列相似度分数。
// 列顺序：id, knowledge_type, topic_id, content, meta, access_count,
// last_accessed, created_at, archived, score。
func (s *KnowledgeStore) scanKnowledgeRowsWithScore(ctx context.Context, rows *sql.Rows) ([]*types.KnowledgeRecord, error) {
	var results []*types.KnowledgeRecord
	for rows.Next() {
		var r types.KnowledgeRecord
		var metaRaw []byte
		var lastAccessed sql.NullTime
		err := rows.Scan(&r.ID, &r.KnowledgeType, &r.TopicID, &r.Content, &metaRaw,
			&r.AccessCount, &lastAccessed, &r.CreatedAt, &r.Archived, &r.Score)
		if err != nil {
			continue
		}
		if len(metaRaw) > 0 {
			if err := json.Unmarshal(metaRaw, &r.Meta); err != nil {
				logError(s.log, ctx, fmt.Sprintf("[store] unmarshal global_knowledge.meta failed: id=%d", r.ID), err)
			}
		}
		if lastAccessed.Valid {
			r.LastAccessed = &lastAccessed.Time
		}
		results = append(results, &r)
	}
	return results, rows.Err()
}

// SearchKeywords 按 knowledge_type 过滤的全文关键词检索（外部知识库混合检索）。
// 使用生成列 content_tsv（to_tsvector('simple', content)，见 schema.go）做 tsvector 匹配，
// 按 ts_rank 相关度排序取 topK。中文分词需部署 zhparser/pg_jieba 后把 'simple' 换 'zhparser'
//（retriever 文档备注）；'simple' 对英文/代码/数字词元已可用。
func (s *KnowledgeStore) SearchKeywords(ctx context.Context, knowledgeType enums.KnowledgeType, query string, topK int) ([]*types.KnowledgeRecord, error) {
	if topK <= 0 {
		topK = 5
	}
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
			SELECT id, knowledge_type, topic_id, content, meta, access_count, last_accessed, created_at, archived
			FROM global_knowledge
			WHERE archived = false AND knowledge_type = $1
			  AND content_tsv @@ plainto_tsquery('simple', $2)
			ORDER BY ts_rank(content_tsv, plainto_tsquery('simple', $2)) DESC
			LIMIT $3
		`, knowledgeType, query, topK)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.scanKnowledgeRows(ctx, rows)
}
