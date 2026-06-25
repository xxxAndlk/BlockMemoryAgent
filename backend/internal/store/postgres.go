package store

import (
	"context"       // 上下文,贯穿所有数据库调用以支持超时与取消
	"database/sql"   // 标准库 SQL 抽象层,底层驱动为 postgres
	"encoding/json"  // 结构体与 JSONB/JSON 列之间的序列化
	"fmt"            // 格式化错误信息与 pgvector 字符串
	"strings"        // 拼接 pgvector 的逗号分隔向量分量
	"time"           // 时间戳与连接池生命周期管理

	"github.com/blockmemory/agent/backend/pkg/types" // 领域模型 (Episode / Snapshot / KnowledgeRecord 等)
	_ "github.com/lib/pq"                            // 注册 postgres 驱动,无需直接引用
)

// PostgresStore 是 PostgreSQL 存储层。
// 职责: 私有记忆/快照/知识库/topic/agent 注册/决策日志/session_history 的 CRUD,
// 并依赖 pgvector 扩展完成向量相似检索。
// 并发安全: 内部仅持有 *sql.DB 连接池,database/sql 自身线程安全,可在多 goroutine 间共享。
type PostgresStore struct {
	db *sql.DB // 共享连接池,所有方法通过该句柄执行 SQL
}

// NewPostgresStore 创建 PostgreSQL 存储实例。
// 参数:
//   - dsn: PostgreSQL 数据源字符串 (host/port/user/password/dbname/sslmode 等)
// 返回:
//   - *PostgresStore: 已通过 Ping 校验的存储实例
//   - error: 打开连接或 Ping 失败时返回包装错误
// 副作用: 初始化连接池参数 (20 最大连接 / 10 空闲 / 1 小时连接寿命)。
func NewPostgresStore(dsn string) (*PostgresStore, error) {
	// 仅解析 DSN 构建连接池,真正建连发生在 Ping
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	// 主动 Ping 一次,提前暴露网络/凭证类错误
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	// 限制最大打开连接数,避免突发流量打满 Postgres
	db.SetMaxOpenConns(20)
	// 维持 10 个空闲连接,降低冷启动延迟
	db.SetMaxIdleConns(10)
	// 连接最长存活 1 小时,促进后端重新负载均衡
	db.SetConnMaxLifetime(time.Hour)
	return &PostgresStore{db: db}, nil
}

// Close 关闭底层连接池并释放数据库资源。
// 调用后该 store 不可再用;幂等调用安全。
// 返回: 连接池关闭错误。
func (s *PostgresStore) Close() error {
	return s.db.Close()
}

// DB 暴露原始 *sql.DB 连接,用于迁移脚本或外部工具。
// 设计意图: 让 main.go 在启动时执行 schema 迁移而无需暴露内部字段。
func (s *PostgresStore) DB() *sql.DB {
	return s.db
}

// SaveEpisode 保存单条 Episode 到 agent_private_memory 表。
// 参数:
//   - agentID: 所属 Agent ID
//   - topicID: 话题 ID
//   - ep:      待持久化的 Episode (含重要性、时间戳、内容)
// 返回: SQL 执行错误。
// 副作用: 写入一行新记录;compression_level 字段使用数据库默认值 0 (Raw 层级)。
func (s *PostgresStore) SaveEpisode(ctx context.Context, agentID, topicID string, ep *types.Episode) error {
	// 将 Episode 整体序列化为 JSON,存入 JSONB 列 episode
	data, err := json.Marshal(ep)
	if err != nil {
		return fmt.Errorf("marshal episode: %w", err)
	}
	// 插入行,importance_score 单独冗余以便后续按重要性排序
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO agent_private_memory (agent_id, topic_id, episode, importance_score, created_at)
		VALUES ($1, $2, $3, $4, $5)
	`, agentID, topicID, data, ep.Importance, ep.Timestamp)
	return err
}

// GetEpisodes 获取 Agent 在指定话题下的全部 Episode (按创建时间倒序)。
// 参数:
//   - agentID, topicID: 检索范围
//   - limit: 最大返回条数;<=0 时默认 100
// 返回: Episode 切片 (可能为空) 与 SQL 错误。
// 注意: 反序列化失败的行被静默跳过,保证部分坏数据不阻断整体读取。
func (s *PostgresStore) GetEpisodes(ctx context.Context, agentID, topicID string, limit int) ([]*types.Episode, error) {
	if limit <= 0 {
		// 兜底默认值,避免下游不传 limit 时返回过多数据
		limit = 100
	}
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

// GetEpisodesByLevel 按压缩层级过滤 Episode。
// 参数:
//   - agentID, topicID: 检索范围
//   - level: Raw/Standard/Compact/Marker 之一
//   - limit: 返回上限,<=0 时默认 100
// 返回: 按重要性降序、再按创建时间倒序的 Episode 切片。
// 设计意图: 压缩管道各阶段拉取对应层级的记忆做组装。
func (s *PostgresStore) GetEpisodesByLevel(ctx context.Context, agentID, topicID string, level types.CompressionLevel, limit int) ([]*types.Episode, error) {
	if limit <= 0 {
		// 兜底默认值
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT episode FROM agent_private_memory
		WHERE agent_id = $1 AND topic_id = $2 AND compression_level = $3
		ORDER BY importance_score DESC, created_at DESC
		LIMIT $4
	`, agentID, topicID, int(level), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var episodes []*types.Episode
	for rows.Next() {
		var raw []byte
		// 读取 JSONB 原始字节
		if err := rows.Scan(&raw); err != nil {
			continue
		}
		var ep types.Episode
		// 反序列化失败跳过当前行
		if err := json.Unmarshal(raw, &ep); err != nil {
			continue
		}
		episodes = append(episodes, &ep)
	}
	return episodes, rows.Err()
}

// CountEpisodes 统计 Agent 在某话题下的 Episode 总数。
// 返回: 行数与查询错误;用于触发压缩阈值判断。
func (s *PostgresStore) CountEpisodes(ctx context.Context, agentID, topicID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM agent_private_memory
		WHERE agent_id = $1 AND topic_id = $2
	`, agentID, topicID).Scan(&count)
	return count, err
}

// CountEpisodesByLevel 按压缩层级分组计数。
// 返回: map[level]count,用于观察当前压缩管道进度。
func (s *PostgresStore) CountEpisodesByLevel(ctx context.Context, agentID, topicID string) (map[types.CompressionLevel]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT compression_level, COUNT(*) FROM agent_private_memory
		WHERE agent_id = $1 AND topic_id = $2
		GROUP BY compression_level
	`, agentID, topicID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[types.CompressionLevel]int)
	for rows.Next() {
		var level int
		var count int
		if err := rows.Scan(&level, &count); err != nil {
			// 单行扫描失败跳过
			continue
		}
		// 将 int 转回枚举类型作为 map key
		result[types.CompressionLevel(level)] = count
	}
	return result, rows.Err()
}

// BatchUpdateEpisodes 在单个事务中批量更新 Episode。
// 参数:
//   - agentID, topicID: 更新范围
//   - episodes: 需更新的 Episode (按 step_id 匹配)
// 返回: 事务提交错误。
// 设计意图: 压缩阶段一次性写回多条压缩结果,避免多次往返。
// 副作用: 任意一行序列化失败均跳过;事务提交时整体生效。
func (s *PostgresStore) BatchUpdateEpisodes(ctx context.Context, agentID, topicID string, episodes []*types.Episode) error {
	// 开启事务,保证批量更新的原子性
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	// 失败时回滚;Commit 后 Rollback 为 no-op
	defer tx.Rollback()

	for _, ep := range episodes {
		data, err := json.Marshal(ep)
		if err != nil {
			// 序列化失败跳过本条,继续处理其他
			continue
		}
		// 通过 episode->>'step_id' 从 JSONB 中提取字段做匹配
		_, err = tx.ExecContext(ctx, `
			UPDATE agent_private_memory
			SET episode = $1, importance_score = $2, updated_at = $3
			WHERE agent_id = $4 AND topic_id = $5 AND episode->>'step_id' = $6
		`, data, ep.Importance, time.Now(), agentID, topicID, ep.StepID)
		if err != nil {
			// 单条更新失败也跳过,保证事务整体推进
			continue
		}
	}
	return tx.Commit()
}

// SaveSnapshot 保存 Agent 快照 (UPSERT)。
// 参数:
//   - snapshot: 含 AgentID/TopicID 与完整上下文状态
// 返回: SQL 执行错误。
// 设计意图: 每个 (agent, topic) 仅保留一份最新快照,供热加载使用。
// 副作用: ON CONFLICT 命中主键则更新 snapshot 与 updated_at。
func (s *PostgresStore) SaveSnapshot(ctx context.Context, snapshot *types.AgentSnapshot) error {
	data, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("marshal snapshot: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO agent_snapshots (agent_id, topic_id, snapshot, updated_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (agent_id, topic_id)
		DO UPDATE SET snapshot = $3, updated_at = $4
	`, snapshot.AgentID, snapshot.TopicID, data, time.Now())
	return err
}

// GetSnapshot 读取 Agent 快照。
// 返回:
//   - *types.AgentSnapshot: 命中时返回;不存在时返回 (nil, nil)
//   - error: 其他 SQL/反序列化错误
// 设计意图: 配合 Redis 缓存做热加载,未命中时回源 PG。
func (s *PostgresStore) GetSnapshot(ctx context.Context, agentID, topicID string) (*types.AgentSnapshot, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT snapshot FROM agent_snapshots
		WHERE agent_id = $1 AND topic_id = $2
	`, agentID, topicID).Scan(&raw)
	// 行不存在视为正常情况,返回 (nil, nil)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var snap types.AgentSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, fmt.Errorf("unmarshal snapshot: %w", err)
	}
	return &snap, nil
}

// SaveKnowledge 写入一条全局知识记录 (含向量)。
// 参数:
//   - rec: 知识记录,含 KnowledgeType/TopicID/Content/Embedding/Meta 等
// 返回: SQL 执行错误。
// 副作用: embedding 通过 pgVector 转为字符串文本,依赖 pgvector 扩展解析。
func (s *PostgresStore) SaveKnowledge(ctx context.Context, rec *types.KnowledgeRecord) error {
	// meta 即使为 nil 也写入 "{}",保证列非空
	meta, _ := json.Marshal(rec.Meta)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO global_knowledge (knowledge_type, topic_id, content, embedding, meta, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, rec.KnowledgeType, rec.TopicID, rec.Content, pgVector(rec.Embedding), meta, rec.CreatedAt)
	return err
}

// GetKnowledgeByType 按知识类型列出记录 (按最近访问时间倒序)。
// 参数:
//   - knowledgeType: 例如 "playbook"/"postmortem"
//   - limit: 返回上限,<=0 时默认 10
// 返回: 知识记录切片与 SQL 错误。
// 设计意图: 排除已归档记录,优先返回热数据。
func (s *PostgresStore) GetKnowledgeByType(ctx context.Context, knowledgeType string, limit int) ([]*types.KnowledgeRecord, error) {
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

// SearchKnowledge 向量相似搜索 (依赖 pgvector)。
// 参数:
//   - embedding: 查询向量
//   - topK: 返回前 K 条,<=0 时默认 5
// 返回: 按相似度 (L2 距离) 升序的知识记录切片。
// 注意: <=> 是 pgvector 的距离算子,值越小越相似。
func (s *PostgresStore) SearchKnowledge(ctx context.Context, embedding []float32, topK int) ([]*types.KnowledgeRecord, error) {
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

// ArchiveKnowledge 归档指定 ID 的知识 (软删除)。
// 参数:
//   - id: 知识记录主键
// 返回: SQL 执行错误。
// 副作用: archived 置 true,后续查询自动排除该记录。
func (s *PostgresStore) ArchiveKnowledge(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE global_knowledge SET archived = true WHERE id = $1
	`, id)
	return err
}

// IncrementAccessCount 自增访问计数并刷新最近访问时间。
// 参数:
//   - id: 知识记录主键
// 返回: SQL 执行错误。
// 设计意图: 配合 GetKnowledgeByType 的排序,实现简单的热度衰减。
func (s *PostgresStore) IncrementAccessCount(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE global_knowledge
		SET access_count = access_count + 1, last_accessed = NOW()
		WHERE id = $1
	`, id)
	return err
}

// CreateTopic 创建话题元数据。
// 参数:
//   - topic: 含 ID/Goal/Status/CreatedAt/ExpiresAt
// 返回: SQL 执行错误。
// 副作用: constraints 字段写入空 JSON 对象占位。
func (s *PostgresStore) CreateTopic(ctx context.Context, topic *types.TopicMeta) error {
	// 暂以空 map 占位,后续通过 SetTopicConstraints 维护
	constraints, _ := json.Marshal(map[string]string{})
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO topics (id, goal, status, constraints, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, topic.ID, topic.Goal, topic.Status, constraints, topic.CreatedAt, topic.ExpiresAt)
	return err
}

// GetTopic 读取话题元数据。
// 参数:
//   - topicID: 话题 ID
// 返回: 命中返回 *TopicMeta;不存在返回 (nil, nil)。
// 副作用: expires_at 可能为 NULL,通过 sql.NullTime 安全读取。
func (s *PostgresStore) GetTopic(ctx context.Context, topicID string) (*types.TopicMeta, error) {
	var t types.TopicMeta
	var constraintsRaw []byte
	var expiresAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT id, goal, status, constraints, created_at, expires_at
		FROM topics WHERE id = $1
	`, topicID).Scan(&t.ID, &t.Goal, &t.Status, &constraintsRaw, &t.CreatedAt, &expiresAt)
	// 行不存在视为正常情况
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// 仅在有效时填充 ExpiresAt 指针
	if expiresAt.Valid {
		t.ExpiresAt = &expiresAt.Time
	}
	return &t, nil
}

// SaveTopicArchive 持久化话题归档摘要 (UPSERT)。
// 参数:
//   - topicID:   话题 ID
//   - summary:   归档摘要文本
//   - outputs:   产出 JSON (RawMessage)
//   - decisions: 决策 JSON (RawMessage)
//   - embedding: 摘要向量,用于跨话题检索
// 返回: SQL 执行错误。
// 副作用: 同一 topic_id 重复归档会覆盖原记录。
func (s *PostgresStore) SaveTopicArchive(ctx context.Context, topicID, summary string, outputs, decisions json.RawMessage, embedding []float32) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO topic_archives (topic_id, summary, outputs, decisions, embedding, archived_at)
		VALUES ($1, $2, $3, $4, $5, NOW())
		ON CONFLICT (topic_id) DO UPDATE
		SET summary = $2, outputs = $3, decisions = $4, embedding = $5, archived_at = NOW()
	`, topicID, summary, outputs, decisions, pgVector(embedding))
	return err
}

// RegisterAgent 注册或更新 Agent 元信息 (UPSERT)。
// 参数:
//   - id, name, description, moduleID: 基础标识
//   - keywords, dependencies, capabilities: 三个标签切片,分别序列化为 JSONB
// 返回: SQL 执行错误。
// 设计意图: 让 MetaAgent 在动态创建 Agent 时持久化注册信息。
func (s *PostgresStore) RegisterAgent(ctx context.Context, id, name, description, moduleID string, keywords, dependencies, capabilities []string) error {
	// 三个切片分别 JSON 序列化,空切片写成 "[]"
	kw, _ := json.Marshal(keywords)
	deps, _ := json.Marshal(dependencies)
	caps, _ := json.Marshal(capabilities)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO agent_registry (id, name, description, module_id, keywords, dependencies, capabilities, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
		ON CONFLICT (id) DO UPDATE
		SET name = $2, description = $3, module_id = $4, keywords = $5, dependencies = $6, capabilities = $7
	`, id, name, description, moduleID, kw, deps, caps)
	return err
}

// GetAgentRegistry 列出全部已注册 Agent。
// 返回: 每行以 map[string]any 形式返回,keywords 等仍为 JSON 字节。
// 设计意图: 给路由/调度器读取注册表,字段保持原始 JSONB 字节由调用方按需解析。
func (s *PostgresStore) GetAgentRegistry(ctx context.Context) ([]map[string]any, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, description, module_id, keywords, dependencies, capabilities
		FROM agent_registry
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []map[string]any
	for rows.Next() {
		var id, name, description, moduleID string
		var keywords, dependencies, capabilities []byte
		if err := rows.Scan(&id, &name, &description, &moduleID, &keywords, &dependencies, &capabilities); err != nil {
			// 单行扫描失败跳过,继续累积其他行
			continue
		}
		results = append(results, map[string]any{
			"id":           id,
			"name":         name,
			"description":  description,
			"module_id":    moduleID,
			"keywords":     keywords,
			"dependencies": dependencies,
			"capabilities": capabilities,
		})
	}
	return results, rows.Err()
}

// SaveDecisionLog 记录一条决策日志。
// 参数:
//   - topicID, agentID, decision: 决策主体与文本
//   - context: 附加上下文,序列化为 JSONB
// 返回: SQL 执行错误。
// 副作用: created_at 由数据库 NOW() 生成。
func (s *PostgresStore) SaveDecisionLog(ctx context.Context, topicID, agentID, decision string, context map[string]any) error {
	ctxData, _ := json.Marshal(context)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO decision_logs (topic_id, agent_id, decision, context, created_at)
		VALUES ($1, $2, $3, $4, NOW())
	`, topicID, agentID, decision, ctxData)
	return err
}

// SessionHistoryRecord 跨会话历史摘要。
// 设计意图: 长期沉淀每次会话的 goal/summary/工具调用结果,供后续会话检索复用。
type SessionHistoryRecord struct {
	SessionID   string           `json:"session_id"`   // 会话唯一标识
	Goal        string           `json:"goal"`         // 会话目标
	Summary     string           `json:"summary"`      // 会话总结
	ToolResults []map[string]any `json:"tool_results"` // 工具调用结果数组
	CreatedAt   time.Time        `json:"created_at"`   // 创建时间
}

// SaveSessionHistory 持久化一次会话的 goal/summary/工具调用结果。
// 参数:
//   - rec: 会话历史记录;ToolResults 为 nil 时补为空数组,保证 JSONB 非 null
// 返回: SQL 执行错误。
// 副作用: ON CONFLICT DO NOTHING 保证同 session_id 重复写入幂等。
func (s *PostgresStore) SaveSessionHistory(ctx context.Context, rec *SessionHistoryRecord) error {
	if rec.ToolResults == nil {
		// 避免写入 null,统一为空数组便于下游读取
		rec.ToolResults = []map[string]any{}
	}
	data, err := json.Marshal(rec.ToolResults)
	if err != nil {
		return fmt.Errorf("marshal tool_results: %w", err)
	}
	// COALESCE 保证 created_at 为零值时回退到 NOW()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO session_history (session_id, goal, summary, tool_results, created_at)
		VALUES ($1, $2, $3, $4, COALESCE($5, NOW()))
		ON CONFLICT DO NOTHING
	`, rec.SessionID, rec.Goal, rec.Summary, data, rec.CreatedAt)
	return err
}

// RecentSessionHistories 返回最近 limit 条会话历史 (按时间倒序)。
// 参数:
//   - limit: 返回上限,<=0 时默认 10
// 返回: 会话历史切片与 SQL 错误。
// 设计意图: 给新会话提供"最近发生过什么"的上下文。
func (s *PostgresStore) RecentSessionHistories(ctx context.Context, limit int) ([]*SessionHistoryRecord, error) {
	if limit <= 0 {
		// 兜底默认值
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT session_id, goal, summary, tool_results, created_at
		FROM session_history
		ORDER BY created_at DESC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*SessionHistoryRecord
	for rows.Next() {
		var r SessionHistoryRecord
		var raw []byte
		if err := rows.Scan(&r.SessionID, &r.Goal, &r.Summary, &raw, &r.CreatedAt); err != nil {
			// 单行扫描失败跳过
			continue
		}
		// 仅在非空时反序列化 tool_results
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &r.ToolResults)
		}
		out = append(out, &r)
	}
	return out, nil
}

// pgVector 将 float32 切片转为 pgvector 字符串格式 [1,2,3]。
// 设计意图: database/sql 不直接支持 pgvector 类型,需以文本字面量形式传入 SQL。
// 返回: 形如 "[0.123000,0.456000]" 的字符串;空切片返回 "[]"。
func pgVector(v []float32) string {
	if len(v) == 0 {
		// 空向量返回 "[]",避免插入 NULL
		return "[]"
	}
	parts := make([]string, len(v))
	for i, f := range v {
		// 每个分量按 %f 格式化,保留小数位
		parts[i] = fmt.Sprintf("%f", f)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// EnsureSessionHistorySchema 自动创建 session_history 表 (幂等)。
// 启动时调用,避免用户忘记跑 migrations/002_session_history.sql 导致
// SaveSessionHistory 静默失败。
// 参数:
//   - db: 任意 *sql.DB 连接
// 返回: 建表/建索引错误。
// 副作用: 建表 + 建索引 (IF NOT EXISTS),可重复执行。
func EnsureSessionHistorySchema(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS session_history (
    id           BIGSERIAL PRIMARY KEY,
    session_id   VARCHAR(64) NOT NULL,
    goal         TEXT NOT NULL,
    summary      TEXT NOT NULL,
    tool_results JSONB DEFAULT '[]',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_session_history_created_at
    ON session_history (created_at DESC);
`)
	return err
}

// scanKnowledgeRows 扫描知识库查询结果集,统一处理 NULL 字段与 JSONB 反序列化。
// 参数:
//   - rows: 已执行的 *sql.Rows,列顺序固定为
//     id, knowledge_type, topic_id, content, meta, access_count, last_accessed, created_at, archived
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
			_ = json.Unmarshal(metaRaw, &r.Meta)
		}
		// last_accessed 可能为 NULL,有效时填充指针
		if lastAccessed.Valid {
			r.LastAccessed = &lastAccessed.Time
		}
		results = append(results, &r)
	}
	return results, rows.Err()
}
