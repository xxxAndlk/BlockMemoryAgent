package store

import (
	"context"       // 上下文,贯穿所有数据库调用以支持超时与取消
	"database/sql"  // 标准库 SQL 抽象层,底层驱动为 postgres
	"encoding/json" // 包装 Topic/Snapshot 等 JSONB 列
	"fmt"           // 格式化错误信息
	"time"          // 时间戳与连接池生命周期管理

	"github.com/blockmemory/agent/backend/internal/embed" // 伪嵌入生成
	"github.com/blockmemory/agent/backend/pkg/enums"      // 枚举常量
	"github.com/blockmemory/agent/backend/pkg/types"      // 领域模型
)

// PostgresStore 是 PostgreSQL 存储层的复合入口。
//
// 职责: 按领域拆分为 Episode/Snapshot/Knowledge/Topic/AgentRegistry/Session 子存储，
// 自身保留连接池、向量维度、嵌入实现等全局能力，并通过薄包装方法保持向后兼容。
//
// 并发安全: 内部仅持有 *sql.DB 连接池,database/sql 自身线程安全,可在多 goroutine 间共享。
type PostgresStore struct {
	db                         *sql.DB             // 共享连接池,所有子存储通过该句柄执行 SQL
	dim                        int                 // 向量维度，由 SetEmbeddingDim 设置；默认 768，需与 schema 中 VECTOR(N) 一致
	embedder                   embed.Embedder      // 文本嵌入实现（P3-3）；nil 时回退 PseudoEmbed
	log                        Logger              // 结构化日志器，由 SetLogger 注入；nil 时回退标准库 log
	searchBlockMemoryMaxTokens int                 // 块记忆检索摘要 token 上限；由配置注入
	Episode                    *EpisodeStore       // 私有 Episode 存储
	Snapshot                   *SnapshotStore      // Agent 快照存储
	Knowledge                  *KnowledgeStore     // 全局知识/块记忆存储
	Topic                      *TopicStore         // 话题元数据与归档存储
	AgentRegistry              *AgentRegistryStore // Agent 注册表与决策日志存储
	Session                    *SessionStore       // 会话历史/事件存储
	LearnedSkills              *LearnedSkillStore  // 自进化技能库 + 进化日志（migration 007）
}

// NewPostgresStore 创建 PostgreSQL 存储实例。
// 参数:
//   - ctx: 用于 Ping 超时控制的上下文
//   - dsn: PostgreSQL 数据源字符串 (host/port/user/password/dbname/sslmode 等)
//
// 返回:
//   - *PostgresStore: 已通过 Ping 校验的存储实例
//   - error: 打开连接或 Ping 失败时返回包装错误
//
// 副作用: 初始化连接池参数 (20 最大连接 / 10 空闲 / 1 小时连接寿命)，并装配各子存储。
func NewPostgresStore(ctx context.Context, dsn string) (*PostgresStore, error) {
	// 仅解析 DSN 构建连接池,真正建连发生在 Ping
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	// 主动 Ping 一次,提前暴露网络/凭证类错误；使用超时 context 避免启动挂死
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	// 限制最大打开连接数,避免突发流量打满 Postgres
	db.SetMaxOpenConns(20)
	// 维持 10 个空闲连接,降低冷启动延迟
	db.SetMaxIdleConns(10)
	// 连接最长存活 1 小时,促进后端重新负载均衡
	db.SetConnMaxLifetime(time.Hour)

	// 初始化复合存储，默认 768 维；可通过 SetEmbeddingDim 覆盖
	s := &PostgresStore{db: db, dim: 768}
	// 装配各子存储，Knowledge 需要反向引用 PostgresStore 以复用 Embed 能力
	s.Episode = &EpisodeStore{db: db}
	s.Snapshot = &SnapshotStore{db: db}
	s.Knowledge = &KnowledgeStore{db: db, pg: s}
	s.Topic = &TopicStore{db: db}
	s.AgentRegistry = &AgentRegistryStore{db: db}
	s.Session = &SessionStore{db: db}
	s.LearnedSkills = &LearnedSkillStore{db: db, pg: s}
	return s, nil
}

// SetEmbeddingDim 设置向量维度（H6 修复：原块记忆硬编码 768，
// 不随 config.PgVector.Dimensions 走，导致维度不匹配时 pgvector 查询报错）。
// 必须在 SaveKnowledge / SearchKnowledge 之前调用，且需与 schema 中 VECTOR(N) 一致。
// 参数:
//   - dim: 目标维度，仅正数生效。
func (s *PostgresStore) SetEmbeddingDim(dim int) {
	if dim > 0 {
		s.dim = dim
	}
}

// EmbeddingDim 返回当前向量维度。
// 返回值: 正数维度；未设置或设置无效时返回 768 默认值。
func (s *PostgresStore) EmbeddingDim() int {
	if s.dim > 0 {
		return s.dim
	}
	return 768
}

// SetEmbedder 注入文本嵌入实现（P3-3）。
// 注入后 SearchKnowledge / SearchBlockMemory 等将向量化委托给该实现；
// nil 时仍使用兼容旧行为的 embed.PseudoEmbed。
// 参数:
//   - e: 嵌入器接口实现。
func (s *PostgresStore) SetEmbedder(e embed.Embedder) {
	s.embedder = e
}

// SetLogger 注入结构化日志器，用于反序列化失败等错误日志的 [ERRO] 输出，
// 并传播到持有日志能力的子存储（Session/Knowledge）。
// 未调用时各存储回退标准库 log，保持旧行为。
// 参数:
//   - l: 日志器实现，通常为 *logger.Logger（经窄接口 Logger 注入，避免循环导入）。
func (s *PostgresStore) SetLogger(l Logger) {
	s.log = l
	if s.Session != nil {
		s.Session.log = l
	}
	if s.Knowledge != nil {
		s.Knowledge.log = l
	}
}

// Embed 实现 memory.BlockMemorySearcher 接口，将查询文本编码为向量。
// 优先使用注入的 embedder，否则回退 embed.PseudoEmbed。
// 参数:
//   - ctx: 请求上下文。
//   - text: 待编码文本。
//
// 返回: 浮点向量与错误。
func (s *PostgresStore) Embed(ctx context.Context, text string) ([]float32, error) {
	if s.embedder != nil {
		// 注入外部嵌入服务时使用外部实现
		return s.embedder.Embed(ctx, text)
	}
	// 无外部实现时回退到本地伪嵌入，保证离线可用
	return embed.PseudoEmbed(text, s.EmbeddingDim()), nil
}

// SetSearchBlockMemoryMaxTokens 设置块记忆检索摘要的 token 上限。
// 参数:
//   - n: 最大 token 数。
func (s *PostgresStore) SetSearchBlockMemoryMaxTokens(n int) {
	s.searchBlockMemoryMaxTokens = n
}

// SearchBlockMemoryMaxTokens 实现 memory.BlockMemorySearcher 接口。
// 返回值: 当前配置的 token 上限。
func (s *PostgresStore) SearchBlockMemoryMaxTokens() int {
	return s.searchBlockMemoryMaxTokens
}

// Close 关闭底层连接池并释放数据库资源。
// 调用后该 store 不可再用;幂等调用安全。
// 返回: 连接池关闭错误。
func (s *PostgresStore) Close() error {
	return s.db.Close()
}

// DB 暴露原始 *sql.DB 连接,用于迁移脚本或外部工具。
// 设计意图: 让 main.go 在启动时执行 schema 迁移而无需暴露内部字段。
// 返回: 底层 *sql.DB。
func (s *PostgresStore) DB() *sql.DB {
	return s.db
}

// ---- 向后兼容的薄包装方法 ----
// 以下方法全部委托给对应子存储，保持现有调用方无需修改。

// SaveEpisode 保存单条 Episode；委托给 EpisodeStore.Save。
func (s *PostgresStore) SaveEpisode(ctx context.Context, agentID, topicID string, ep *types.Episode) error {
	return s.Episode.Save(ctx, agentID, topicID, ep)
}

// SaveEpisodeWithStepCount 保存单条 Episode（带 step_count 幂等键）；委托给 EpisodeStore.SaveWithStepCount。
func (s *PostgresStore) SaveEpisodeWithStepCount(ctx context.Context, agentID, topicID string, stepCount int, ep *types.Episode) error {
	return s.Episode.SaveWithStepCount(ctx, agentID, topicID, stepCount, ep)
}

// IsDuplicateError 判断是否为唯一约束冲突；委托给 EpisodeStore.IsDuplicateError。
func (s *PostgresStore) IsDuplicateError(err error) bool {
	return s.Episode.IsDuplicateError(err)
}

// GetEpisodes 获取 Episode 列表；委托给 EpisodeStore.GetEpisodes。
func (s *PostgresStore) GetEpisodes(ctx context.Context, agentID, topicID string, limit int) ([]*types.Episode, error) {
	return s.Episode.GetEpisodes(ctx, agentID, topicID, limit)
}

// CountEpisodes 统计 Episode 总数；委托给 EpisodeStore.CountEpisodes。
func (s *PostgresStore) CountEpisodes(ctx context.Context, agentID, topicID string) (int, error) {
	return s.Episode.CountEpisodes(ctx, agentID, topicID)
}

// CountEpisodesByLevel 按压缩层级统计 Episode 数量；委托给 EpisodeStore.CountEpisodesByLevel。
func (s *PostgresStore) CountEpisodesByLevel(ctx context.Context, agentID, topicID string) (map[int]int, error) {
	return s.Episode.CountEpisodesByLevel(ctx, agentID, topicID)
}

// SaveSnapshot 保存 Agent 快照；委托给 SnapshotStore.Save。
func (s *PostgresStore) SaveSnapshot(ctx context.Context, snapshot *types.AgentSnapshot) error {
	return s.Snapshot.Save(ctx, snapshot)
}

// GetSnapshot 读取 Agent 快照；委托给 SnapshotStore.Get。
func (s *PostgresStore) GetSnapshot(ctx context.Context, agentID, topicID string) (*types.AgentSnapshot, error) {
	return s.Snapshot.Get(ctx, agentID, topicID)
}

// SaveKnowledge 写入全局知识记录；委托给 KnowledgeStore.Save。
func (s *PostgresStore) SaveKnowledge(ctx context.Context, rec *types.KnowledgeRecord) error {
	return s.Knowledge.Save(ctx, rec)
}

// GetKnowledgeByType 按类型查询知识记录；委托给 KnowledgeStore.GetByType。
func (s *PostgresStore) GetKnowledgeByType(ctx context.Context, knowledgeType string, limit int) ([]*types.KnowledgeRecord, error) {
	return s.Knowledge.GetByType(ctx, knowledgeType, limit)
}

// SearchKnowledge 向量相似搜索；委托给 KnowledgeStore.Search。
func (s *PostgresStore) SearchKnowledge(ctx context.Context, embedding []float32, topK int) ([]*types.KnowledgeRecord, error) {
	return s.Knowledge.Search(ctx, embedding, topK)
}

// SearchKnowledgeByTypeAndDomain 按类型+domain 过滤的向量搜索；委托给 KnowledgeStore.SearchByTypeAndDomain。
func (s *PostgresStore) SearchKnowledgeByTypeAndDomain(ctx context.Context, knowledgeType enums.KnowledgeType, domain string, embedding []float32, topK int) ([]*types.KnowledgeRecord, error) {
	return s.Knowledge.SearchByTypeAndDomain(ctx, knowledgeType, domain, embedding, topK)
}

// SearchBlockMemory 按 domain 过滤检索块记忆；委托给 KnowledgeStore.SearchBlockMemory。
func (s *PostgresStore) SearchBlockMemory(ctx context.Context, domain, goal string, topK int) ([]*types.KnowledgeRecord, error) {
	return s.Knowledge.SearchBlockMemory(ctx, domain, goal, topK)
}

// SearchBlockMemoryByGoal 按 sessionID 过滤后做语义检索块记忆；
// 委托给 KnowledgeStore.SearchBlockMemoryByGoal。
func (s *PostgresStore) SearchBlockMemoryByGoal(ctx context.Context, sessionID, goal string, topK int) ([]*types.KnowledgeRecord, error) {
	return s.Knowledge.SearchBlockMemoryByGoal(ctx, sessionID, goal, topK)
}

// Query 黑板模式（TODO #42）scope 确定性检索块记忆；委托给 KnowledgeStore.Query。
// 实现 domain/subagent.BlackboardSearcher 接口（鸭子类型，store 不依赖 domain）。
func (s *PostgresStore) Query(ctx context.Context, sessionID, parentID, taskDomain, query string, topK int, excludeSubAgentID string) ([]*types.KnowledgeRecord, error) {
	return s.Knowledge.Query(ctx, sessionID, parentID, taskDomain, query, topK, excludeSubAgentID)
}

// SearchKnowledgeByType 按 knowledge_type 过滤的向量搜索；委托给 KnowledgeStore.SearchByType。
func (s *PostgresStore) SearchKnowledgeByType(ctx context.Context, knowledgeType enums.KnowledgeType, embedding []float32, topK int) ([]*types.KnowledgeRecord, error) {
	return s.Knowledge.SearchByType(ctx, knowledgeType, embedding, topK)
}

// ArchiveKnowledge 归档指定知识记录；委托给 KnowledgeStore.Archive。
func (s *PostgresStore) ArchiveKnowledge(ctx context.Context, id int64) error {
	return s.Knowledge.Archive(ctx, id)
}

// IncrementAccessCount 自增访问计数；委托给 KnowledgeStore.IncrementAccessCount。
func (s *PostgresStore) IncrementAccessCount(ctx context.Context, id int64) error {
	return s.Knowledge.IncrementAccessCount(ctx, id)
}

// BumpReuse 递增知识记录 Meta.reuse_count；委托给 KnowledgeStore.BumpReuse。
func (s *PostgresStore) BumpReuse(ctx context.Context, id int64) error {
	return s.Knowledge.BumpReuse(ctx, id)
}

// CreateTopic 创建话题；委托给 TopicStore.Create。
func (s *PostgresStore) CreateTopic(ctx context.Context, topic *types.TopicMeta) error {
	return s.Topic.Create(ctx, topic)
}

// GetTopic 读取话题元数据；委托给 TopicStore.Get。
func (s *PostgresStore) GetTopic(ctx context.Context, topicID string) (*types.TopicMeta, error) {
	return s.Topic.Get(ctx, topicID)
}

// SaveTopicArchive 持久化话题归档摘要；委托给 TopicStore.SaveArchive。
func (s *PostgresStore) SaveTopicArchive(ctx context.Context, topicID, summary string, outputs, decisions json.RawMessage, embedding []float32) error {
	return s.Topic.SaveArchive(ctx, topicID, summary, outputs, decisions, embedding)
}

// RegisterAgent 注册或更新 Agent 元信息；委托给 AgentRegistryStore.Register。
func (s *PostgresStore) RegisterAgent(ctx context.Context, id, name, description, moduleID string, keywords, dependencies, capabilities []string) error {
	return s.AgentRegistry.Register(ctx, id, name, description, moduleID, keywords, dependencies, capabilities)
}

// GetAgentRegistry 列出全部已注册 Agent；委托给 AgentRegistryStore.GetAll。
func (s *PostgresStore) GetAgentRegistry(ctx context.Context) ([]map[string]any, error) {
	return s.AgentRegistry.GetAll(ctx)
}

// SaveDecisionLog 记录决策日志；委托给 AgentRegistryStore.SaveDecisionLog。
func (s *PostgresStore) SaveDecisionLog(ctx context.Context, topicID, agentID, decision string, context map[string]any) error {
	return s.AgentRegistry.SaveDecisionLog(ctx, topicID, agentID, decision, context)
}

// SaveSessionHistory 持久化会话历史；委托给 SessionStore.SaveHistory。
func (s *PostgresStore) SaveSessionHistory(ctx context.Context, rec *SessionHistoryRecord) error {
	return s.Session.SaveHistory(ctx, rec)
}

// SaveSessionEvents 批量持久化会话事件；委托给 SessionStore.SaveEvents。
func (s *PostgresStore) SaveSessionEvents(ctx context.Context, sessionID string, events []SessionEventRecord) error {
	return s.Session.SaveEvents(ctx, sessionID, events)
}

// GetSessionEvents 读取会话事件；委托给 SessionStore.GetEvents。
func (s *PostgresStore) GetSessionEvents(ctx context.Context, sessionID string) ([]SessionEventRecord, error) {
	return s.Session.GetEvents(ctx, sessionID)
}

// RecentSessionHistories 返回最近会话历史；委托给 SessionStore.RecentHistories。
func (s *PostgresStore) RecentSessionHistories(ctx context.Context, limit int) ([]*SessionHistoryRecord, error) {
	return s.Session.RecentHistories(ctx, limit)
}

// GetSessionHistoryByID 按 ID 查询单条会话历史；委托给 SessionStore.GetHistoryByID。
func (s *PostgresStore) GetSessionHistoryByID(ctx context.Context, id string) (*SessionHistoryRecord, error) {
	return s.Session.GetHistoryByID(ctx, id)
}
