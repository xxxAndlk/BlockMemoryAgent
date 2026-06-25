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
	cfg.applyDefaults()
	// 将 ${VAR:default} 形式的引用替换为真实环境变量值
	cfg.resolveEnvVars()
	return cfg, nil
}

// applyDefaults 为 Config 中所有零值字段填充合理的默认值。
// 职责：仅在字段为零值时写入默认，避免覆盖用户显式配置。
// 副作用：就地修改 Config。
func (c *Config) applyDefaults() {
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
