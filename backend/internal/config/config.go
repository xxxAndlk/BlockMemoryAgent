package config

import (
	"fmt"     // 用于包装和格式化错误信息
	"os"      // 用于读取配置文件与获取环境变量
	"strings" // 用于字符串切分、裁剪等处理

	"gopkg.in/yaml.v3" // YAML 解析库，用于反序列化 config.yaml
)

// Config 是后端服务的顶层配置结构，对应 config/config.yaml 的根节点。
// 它聚合了 Postgres、PgVector、Redis、HTTP、Memory 五大子配置模块，
// 由 Load 函数从 YAML 文件读取并填充后返回。
type Config struct {
	Postgres PostgresConfig `yaml:"postgres"` // PostgreSQL 关系型存储配置（私有记忆、快照、知识库 CRUD）
	PgVector PgVectorConfig `yaml:"pgvector"` // pgvector 向量索引配置（语义检索）
	Redis    RedisConfig    `yaml:"redis"`    // Redis 热缓存配置（workspace、快照、Stream）
	HTTP     HTTPConfig     `yaml:"http"`     // HTTP 服务监听与超时配置
	Memory   MemoryConfig   `yaml:"memory"`   // 记忆管线运行参数（批写/刷新/快照间隔）
	Agent    AgentConfig    `yaml:"agent"`    // Agent 运行时动态参数（上下文窗口/工具轮数/重试等）
	Logging  LoggingConfig  `yaml:"logging"`  // 日志文件输出（按天分割，按入口分文件）
}

// LoggingConfig 日志文件输出配置。
// 不同入口（浏览器 HTTP 服务 / TUI）写入独立文件，便于按入口排查问题。
type LoggingConfig struct {
	Dir     string `yaml:"dir"`     // 日志目录，空则默认 ./logs；自动按入口 + 日期生成文件名
	Enabled bool   `yaml:"enabled"` // 是否启用文件日志；false 时仅输出到 stderr
}

// AgentConfig 集中所有 Agent 运行时动态可配置参数。
// 替代散落在 graph 包中的硬编码常量（maxSteps=200、skillSetSize=8、
// retry=3、LLM 软/硬超时 30s/90s 等），让运维可以通过 config.yaml 调整。
type AgentConfig struct {
	SkillSetSize      int `yaml:"skill_set_size"`       // 每个 DomainAgent 装配的 Skill 子集上限
	ToolCallMaxRounds int `yaml:"tool_call_max_rounds"` // Assistant 单任务 ReAct 工具调用循环最大轮数
	RetryCount        int `yaml:"retry_count"`          // Assistant 任务执行指数退避重试次数
	RetryBackoffMs    int `yaml:"retry_backoff_ms"`     // 重试初始退避时长（毫秒）
	LLMSoftTimeoutSec int `yaml:"llm_soft_timeout_sec"` // LLM 调用软超时（秒，建议取消）
	LLMHardTimeoutSec int `yaml:"llm_hard_timeout_sec"` // LLM 调用硬超时（秒，强制取消）
	ContextWindow     int `yaml:"context_window"`       // Agent 上下文窗口（token 数），用于 Watchdog / 装配预算
	// 抢占中断与队列注入（特性6）
	InterruptEnabled   bool `yaml:"interrupt_enabled"`    // 是否启用抢占中断
	QueueInjectEnabled bool `yaml:"queue_inject_enabled"` // 是否启用队列注入
	// 人机对话（特性5）
	HumanClarifyEnabled    bool `yaml:"human_clarify_enabled"`     // 是否启用人机对话
	HumanClarifyTimeoutSec int  `yaml:"human_clarify_timeout_sec"` // 等待用户回答超时（秒）
	// domainAgent 持久化（特性4）
	DomainArchiveTTLHours  int `yaml:"domain_archive_ttl_hours"`  // domainAgent 归档默认存活时长（小时）
	StallSteps             int `yaml:"stall_steps"`               //  Graph 死循环防护 无进展步数阈值：连续 N 步未产生新 Episode / 工具结果 / state 变化 → 判死循环
	MaxRepeatFingerprint   int `yaml:"max_repeat_fingerprint"`    // 状态指纹重复阈值：同一 state 指纹连续出现 N 次 → 判死循环
	SessionTimeoutMin      int `yaml:"session_timeout_min"`       // 单次 Graph Invoke 的 wall-clock 超时（分钟），防 LLM/工具卡死
	DomainArchiveMaxWeight int `yaml:"domain_archive_max_weight"` // domainAgent 归档权重上限
}

// PostgresConfig 描述 PostgreSQL 连接与连接池参数。
// 时间字段以"秒"为单位，由 applyDefaults 在零值时填充默认。
type PostgresConfig struct {
	DSN             string `yaml:"dsn"`               // 数据库连接串，支持 ${VAR:default} 环境变量插值
	MaxOpenConns    int    `yaml:"max_open_conns"`    // 最大打开连接数
	MaxIdleConns    int    `yaml:"max_idle_conns"`    // 最大空闲连接数
	ConnMaxLifetime int    `yaml:"conn_max_lifetime"` // 连接最大存活时间（秒）
}

// PgVectorConfig 描述 pgvector 扩展的向量索引参数，
// 仅当 Enabled 为 true 时由 store 层启用语义检索能力。
type PgVectorConfig struct {
	Enabled        bool   `yaml:"enabled"`         // 是否启用 pgvector 语义检索
	Dimensions     int    `yaml:"dimensions"`      // 向量维度，需与嵌入模型一致（默认 768）
	IndexType      string `yaml:"index_type"`      // 索引类型：ivfflat | hnsw
	DistanceMetric string `yaml:"distance_metric"` // 距离度量：cosine | l2 | inner_product
}

// RedisConfig 描述 Redis 连接池与超时参数。
// 超时字段以"毫秒"为单位，SnapshotTTLDays 以"天"为单位。
type RedisConfig struct {
	Addr            string `yaml:"addr"`              // Redis 地址，支持 ${VAR:default} 插值
	Password        string `yaml:"password"`          // 认证密码，支持 ${VAR:default} 插值
	DB              int    `yaml:"db"`                // 选择的 Redis 数据库编号
	PoolSize        int    `yaml:"pool_size"`         // 连接池大小
	MinIdleConns    int    `yaml:"min_idle_conns"`    // 最小空闲连接数
	DialTimeout     int    `yaml:"dial_timeout"`      // 拨号超时（毫秒）
	ReadTimeout     int    `yaml:"read_timeout"`      // 读超时（毫秒）
	WriteTimeout    int    `yaml:"write_timeout"`     // 写超时（毫秒）
	SnapshotTTLDays int    `yaml:"snapshot_ttl_days"` // 快照在 Redis 中的存活天数
}

// HTTPConfig 描述 HTTP 服务监听与超时参数，时间字段以"秒"为单位。
type HTTPConfig struct {
	Addr         string `yaml:"addr"`          // 监听地址（如 :10010），支持 ${VAR:default} 插值
	ReadTimeout  int    `yaml:"read_timeout"`  // 读超时（秒）
	WriteTimeout int    `yaml:"write_timeout"` // 写超时（秒）
}

// MemoryConfig 描述记忆管线的运行节奏参数，
// 控制写入批量化与快照周期，时间字段以"秒"为单位。
type MemoryConfig struct {
	WriteBatchSize     int `yaml:"write_batch_size"`     // 单批写入的最大记忆条数
	WriteFlushInterval int `yaml:"write_flush_interval"` // 定时刷新间隔（秒）
	SnapshotInterval   int `yaml:"snapshot_interval"`    // 快照持久化间隔（秒）
}

// Load 从指定路径读取 YAML 配置文件并构造 *Config。
// 职责：读取文件 → 反序列化为 Config → 填充默认值 → 解析环境变量引用。
// 参数：path 为 config.yaml 的文件路径。
// 返回：填充完成的 *Config；任一阶段失败均返回包装后的 error。
// 副作用：仅读取文件系统与进程环境变量，不修改它们。
func Load(path string) (*Config, error) {
	// 读取整个 YAML 文件内容到内存
	data, err := os.ReadFile(path)
	if err != nil {
		// 读取失败时包装错误，便于上层定位
		return nil, fmt.Errorf("read config: %w", err)
	}

	// 创建空 Config 实例，准备接收 YAML 反序列化结果
	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		// YAML 语法错误时包装并返回
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// 为零值字段填充默认值，保证运行参数始终可用
	if err := cfg.applyDefaults(); err != nil {
		return nil, err
	}
	// 将 ${VAR:default} 形式的引用替换为真实环境变量值
	cfg.resolveEnvVars()
	return cfg, nil
}

// applyDefaults 为 Config 中所有零值字段填充合理的默认值。
// 职责：仅在字段为零值时写入默认，避免覆盖用户显式配置。
// 副作用：就地修改 Config；必要配置缺失时返回 error，让 Load 失败、程序拒绝启动。
func (c *Config) applyDefaults() error {
	// —— Postgres 默认值：本地开发 DSN 与中等规模连接池 ——
	if c.Postgres.DSN == "" {
		c.Postgres.DSN = "postgres://user:pass@localhost:5432/blockmemory?sslmode=disable"
	}
	if c.Postgres.MaxOpenConns == 0 {
		c.Postgres.MaxOpenConns = 25
	}
	if c.Postgres.MaxIdleConns == 0 {
		c.Postgres.MaxIdleConns = 5
	}
	if c.Postgres.ConnMaxLifetime == 0 {
		c.Postgres.ConnMaxLifetime = 300
	}

	// —— pgvector 默认值：768 维、ivfflat 索引、cosine 距离 ——
	if c.PgVector.Dimensions == 0 {
		c.PgVector.Dimensions = 768
	}
	if c.PgVector.IndexType == "" {
		c.PgVector.IndexType = "ivfflat"
	}
	if c.PgVector.DistanceMetric == "" {
		c.PgVector.DistanceMetric = "cosine"
	}

	// —— Redis 默认值：本地地址、小型连接池、毫秒级超时、7 天快照 TTL ——
	if c.Redis.Addr == "" {
		c.Redis.Addr = "localhost:6379"
	}
	if c.Redis.PoolSize == 0 {
		c.Redis.PoolSize = 10
	}
	if c.Redis.MinIdleConns == 0 {
		c.Redis.MinIdleConns = 3
	}
	if c.Redis.DialTimeout == 0 {
		c.Redis.DialTimeout = 5000
	}
	if c.Redis.ReadTimeout == 0 {
		c.Redis.ReadTimeout = 3000
	}
	if c.Redis.WriteTimeout == 0 {
		c.Redis.WriteTimeout = 3000
	}
	if c.Redis.SnapshotTTLDays == 0 {
		c.Redis.SnapshotTTLDays = 7
	}

	// —— HTTP 默认值：10010 端口、30 秒超时 ——
	if c.HTTP.Addr == "" {
		c.HTTP.Addr = ":10010"
	}
	if c.HTTP.ReadTimeout == 0 {
		c.HTTP.ReadTimeout = 30
	}
	if c.HTTP.WriteTimeout == 0 {
		c.HTTP.WriteTimeout = 30
	}

	// —— 记忆管线默认值：100 条/批、5 秒刷新、300 秒快照 ——
	if c.Memory.WriteBatchSize == 0 {
		c.Memory.WriteBatchSize = 100
	}
	if c.Memory.WriteFlushInterval == 0 {
		c.Memory.WriteFlushInterval = 5
	}
	if c.Memory.SnapshotInterval == 0 {
		c.Memory.SnapshotInterval = 300
	}

	// —— Agent 运行时参数默认值 ——
	// 约定：所有数值字段以 0 表示"未配置"，按字段语义填充默认。
	// 因此配置侧无法把任一项显式设为 0；如需禁用某参数，请改用对应的
	// 布尔开关（如 InterruptEnabled）而非将其置 0。
	// 例外：Graph 死循环防护三项 (StallSteps/MaxRepeatFingerprint/SessionTimeoutMin)
	// 不做默认值兜底，config.yaml 必须显式提供，缺失即报错 — 配置问题不应让程序运行起来。
	if c.Agent.StallSteps == 0 {
		return fmt.Errorf("config agent.stall_steps 未配置：Graph 死循环防护必需，请在 config.yaml 显式设置")
	}
	if c.Agent.MaxRepeatFingerprint == 0 {
		return fmt.Errorf("config agent.max_repeat_fingerprint 未配置：Graph 死循环防护必需，请在 config.yaml 显式设置")
	}
	if c.Agent.SessionTimeoutMin == 0 {
		return fmt.Errorf("config agent.session_timeout_min 未配置：Graph wall-clock 超时必需，请在 config.yaml 显式设置")
	}
	if c.Agent.SkillSetSize == 0 {
		c.Agent.SkillSetSize = 8
	}
	if c.Agent.ToolCallMaxRounds == 0 {
		c.Agent.ToolCallMaxRounds = 12
	}
	if c.Agent.RetryCount == 0 {
		c.Agent.RetryCount = 3
	}
	if c.Agent.RetryBackoffMs == 0 {
		c.Agent.RetryBackoffMs = 100
	}
	if c.Agent.LLMSoftTimeoutSec == 0 {
		c.Agent.LLMSoftTimeoutSec = 30
	}
	if c.Agent.LLMHardTimeoutSec == 0 {
		c.Agent.LLMHardTimeoutSec = 90
	}
	if c.Agent.ContextWindow == 0 {
		c.Agent.ContextWindow = 32000
	}
	if c.Agent.HumanClarifyTimeoutSec == 0 {
		c.Agent.HumanClarifyTimeoutSec = 120
	}
	if c.Agent.DomainArchiveTTLHours == 0 {
		c.Agent.DomainArchiveTTLHours = 168 // 7 天
	}
	if c.Agent.DomainArchiveMaxWeight == 0 {
		c.Agent.DomainArchiveMaxWeight = 100
	}

	// —— 日志默认值：默认目录 ./logs ——
	if c.Logging.Dir == "" {
		c.Logging.Dir = "logs"
	}
	return nil
}

// resolveEnvVars 解析配置中 ${VAR} 与 ${VAR:"default"} 形式的环境变量引用。
// 职责：对 DSN、Redis 地址/密码、HTTP 地址等敏感或环境相关字段做插值，
// 使同一份 YAML 可在不同部署环境（本地/容器/CI）间复用。
// 副作用：就地替换 Config 中对应字段的字符串值。
func (c *Config) resolveEnvVars() {
	// 数据库连接串通常含密码，优先走环境变量
	c.Postgres.DSN = resolveEnvWithDefault(c.Postgres.DSN)
	// Redis 地址与密码同理，避免硬编码到 YAML
	c.Redis.Addr = resolveEnvWithDefault(c.Redis.Addr)
	c.Redis.Password = resolveEnvWithDefault(c.Redis.Password)
	// HTTP 监听端口可能由容器平台注入
	c.HTTP.Addr = resolveEnvWithDefault(c.HTTP.Addr)
}

// resolveEnvWithDefault 将形如 ${VAR} 或 ${VAR:"default"} 的字符串解析为实际值。
// 规则：
//   - 若字符串不是 ${...} 形式，原样返回。
//   - 若环境变量 VAR 已设置且非空，返回其值。
//   - 否则返回默认值（去除两侧双引号）；无默认值时返回空串。
//
// 设计意图：支持 ${VAR:default} 与 ${VAR:"default"} 两种写法，默认值可为空。
func resolveEnvWithDefault(s string) string {
	// 快速形态校验：长度 > 3 且以 ${ 开头、以 } 结尾
	if len(s) > 3 && s[0] == '$' && s[1] == '{' && s[len(s)-1] == '}' {
		// 取出大括号内部内容，形如 VAR 或 VAR:"default"
		inner := s[2 : len(s)-1]
		// 检查是否有默认值 ${VAR:"default"}
		if varName, defaultPart, ok := strings.Cut(inner, ":"); ok {
			// 去除默认值两侧的双引号，兼容 "default" 写法
			defaultVal := strings.Trim(defaultPart, "\"")
			// 环境变量非空则优先使用，否则回落到默认值
			if v := os.Getenv(varName); v != "" {
				return v
			}
			return defaultVal
		}
		// 无默认值分支：直接返回环境变量值（可能为空串）
		return os.Getenv(inner)
	}
	// 非环境变量引用格式，原样返回
	return s
}
