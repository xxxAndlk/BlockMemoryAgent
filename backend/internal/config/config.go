package config

import (
	"fmt"           // 用于包装和格式化错误信息
	"os"            // 用于读取配置文件与获取环境变量
	"path/filepath" // 用于定位同目录下的拆分配置文件
	"strings"       // 用于字符串切分、裁剪等处理

	"gopkg.in/yaml.v3" // YAML 解析库，用于反序列化 config.yaml
)

// Config 是后端服务的顶层配置结构，对应 config/config.yaml 的根节点。
// 它聚合了 Postgres、PgVector、Redis、HTTP、Memory、Agent、Logging 六个子配置模块，
// 注：文本嵌入模型配置（EmbedConfig）已迁移到 roles.yaml，由 pkg/config 解析。
// 由 Load 函数从 YAML 文件读取并填充后返回。
type Config struct {
	Postgres PostgresConfig `yaml:"postgres"` // PostgreSQL 关系型存储配置（私有记忆、快照、知识库 CRUD）
	PgVector PgVectorConfig `yaml:"pgvector"` // pgvector 向量索引配置（语义检索）
	Redis    RedisConfig    `yaml:"redis"`    // Redis 热缓存配置（workspace、快照、Stream）
	HTTP     HTTPConfig     `yaml:"http"`     // HTTP 服务监听与超时配置
	Memory   MemoryConfig   `yaml:"memory"`   // 记忆管线运行参数（批写/刷新/快照间隔）
	Agent    AgentConfig    `yaml:"agent"`    // Agent 运行时动态参数（上下文窗口/工具轮数/重试等）
	Logging  LoggingConfig  `yaml:"logging"`  // 日志文件输出（按天分割，按入口分文件）
	Plugins  PluginsConfig  `yaml:"plugins"`  // 插件预留开关（P3-5）
}

// PluginsConfig 插件预留配置（P3-5）。
// 当前仅作开关，后续接入 MCP / RAG / Computer Use 时展开字段。
type PluginsConfig struct {
	MCP         PluginToggle `yaml:"mcp"`          // MCP 工具接入
	RAG         PluginToggle `yaml:"rag"`          // 外部 RAG 知识源
	ComputerUse PluginToggle `yaml:"computer_use"` // Computer Use 浏览器/GUI 自动化
}

// PluginToggle 单个插件开关。
type PluginToggle struct {
	Enabled bool `yaml:"enabled"`
}

// LoggingConfig 日志文件输出配置。
// 不同入口（浏览器 HTTP 服务 / TUI）写入独立文件，便于按入口排查问题。
type LoggingConfig struct {
	Dir     string `yaml:"dir"`     // 日志目录，空则默认 ./logs；自动按入口 + 日期生成文件名
	Enabled bool   `yaml:"enabled"` // 是否启用文件日志；false 时仅输出到 stderr
}

// MemoryPolicyConfig 记忆管线策略配置。
// 控制上下文装配时的分段比例、私有记忆裁剪参数等，替代 memory 包中的硬编码魔法数。
type MemoryPolicyConfig struct {
	ContextWindow        int `yaml:"context_window"`        // 模型上下文窗口总 token 数
	SystemSegmentRatio   int `yaml:"system_segment_ratio"`  // System 段占比（百分之 N）
	TopicGlobalRatio     int `yaml:"topic_global_ratio"`    // TopicGlobal 段占比（百分之 N）
	SharedStateRatio     int `yaml:"shared_state_ratio"`    // SharedState 段占比（百分之 N）
	GlobalKBRatio        int `yaml:"global_kb_ratio"`       // GlobalKB 段占比（百分之 N）
	PrivateMemoryRatio   int `yaml:"private_memory_ratio"`  // PrivateMemory 段占比（百分之 N）
	TaskRatio            int `yaml:"task_ratio"`            // TaskQuery 段占比（百分之 N）
	ReserveRatio         int `yaml:"reserve_ratio"`         // Reserve 预留段占比（百分之 N）
	DefaultTokensPerItem int `yaml:"default_tokens_per_item"` // 私有记忆单条默认 token 数，用于按预算裁剪
}

// GraphPolicyConfig Graph 死循环防护与全局步数策略配置。
type GraphPolicyConfig struct {
	StallSteps           int `yaml:"stall_steps"`            //  Graph 死循环防护 无进展步数阈值：连续 N 步未产生新 Episode / 工具结果 / state 变化 → 判死循环
	MaxRepeatFingerprint int `yaml:"max_repeat_fingerprint"` // 状态指纹重复阈值：同一 state 指纹连续出现 N 次 → 判死循环
	SessionTimeoutMin    int `yaml:"session_timeout_min"`    // 单次 Graph Invoke 的 wall-clock 超时（分钟），防 LLM/工具卡死
	MaxSteps             int `yaml:"max_steps"`              //  Graph 绝对步数上限（安全网，超出即使无死循环也终止），0 表示不限制
}

// LLMRuntimeConfig LLM 调用与重试运行时参数配置。
type LLMRuntimeConfig struct {
	ToolCallMaxRounds int `yaml:"tool_call_max_rounds"` // Assistant 单任务 ReAct 工具调用循环最大轮数（含重复调用/空转检测提前退出）
	RetryCount        int `yaml:"retry_count"`          // Assistant 任务执行指数退避重试次数
	RetryBackoffMs    int `yaml:"retry_backoff_ms"`     // 重试初始退避时长（毫秒）
	LLMSoftTimeoutSec int `yaml:"llm_soft_timeout_sec"` // LLM 调用软超时（秒，建议取消）
	LLMHardTimeoutSec int `yaml:"llm_hard_timeout_sec"` // LLM 调用硬超时（秒，强制取消）
}

// SafetyConfig 工具沙箱与安全策略配置。
type SafetyConfig struct {
	ToolSandboxDisabled     bool     `yaml:"tool_sandbox_disabled"`      // true 时关闭写路径逃逸检测（保留命令黑名单）
	ToolSandboxAllowedPaths []string `yaml:"tool_sandbox_allowed_paths"` // 允许读写的额外绝对路径白名单
	ToolSandboxBlockedCmds  []string `yaml:"tool_sandbox_blocked_cmds"`  // 额外命令黑名单（追加到默认黑名单）
}

// FeatureTogglesConfig Agent 特性开关配置。
type FeatureTogglesConfig struct {
	InterruptEnabled     bool `yaml:"interrupt_enabled"`         // 是否启用抢占中断
	QueueInjectEnabled   bool `yaml:"queue_inject_enabled"`      // 是否启用队列注入
	HumanClarifyEnabled  bool `yaml:"human_clarify_enabled"`     // 是否启用人机对话
	HumanClarifyTimeoutSec int  `yaml:"human_clarify_timeout_sec"` // 等待用户回答超时（秒）
	DAGEnabled           bool `yaml:"dag_enabled"`               // 是否启动 DAG 调度器
	PlanEnabled          bool `yaml:"plan_enabled"`              // 是否为复杂任务启用 Plan 层（多任务时生成结构化计划）
	ReflectionEnabled    bool `yaml:"reflection_enabled"`        // 是否在助手执行后做 Self-Reflection（不达标重试一次）
	AssistantSelfTestEnabled bool `yaml:"assistant_self_test_enabled"` // 助手完成子任务后是否派遣测试助手验证
	DomainSelfTestEnabled    bool `yaml:"domain_self_test_enabled"`    // 领域 Agent 完成后是否派遣测试助手验证完整模块
}

// AgentConfig 集中所有 Agent 运行时动态可配置参数。
// 通过嵌入若干聚焦的子配置，既保持 cfg.Agent.Xxx 的既有访问方式，又将相关字段按职责分组。
// 替代散落在 graph 包中的硬编码常量（maxSteps=200、skillSetSize=8、
// retry=3、LLM 软/硬超时 30s/90s 等），让运维可以通过 config.yaml 调整。
type AgentConfig struct {
	GraphPolicyConfig    `yaml:",inline"`
	LLMRuntimeConfig     `yaml:",inline"`
	SafetyConfig         `yaml:",inline"`
	FeatureTogglesConfig `yaml:",inline"`

	// 记忆管线策略保持为具名字段，以维持 agent.memory_policy 的 YAML 路径不变。
	MemoryPolicy MemoryPolicyConfig `yaml:"memory_policy"`

	// 其余不适合归入上述子配置的独立字段。
	SkillSetSize      int `yaml:"skill_set_size"`       // 每个 DomainAgent 装配的 Skill 子集上限
	ContextWindow     int `yaml:"context_window"`       // Agent 上下文窗口（token 数），用于 Watchdog / 装配预算
	SnapshotSummaryCount       int     `yaml:"snapshot_summary_count"`        // 快照保留的最近摘要条数（默认 20）
	SnapshotOpenIssueThreshold float64 `yaml:"snapshot_open_issue_threshold"` // 未决问题重要性阈值（默认 0.7）
	SummaryMaxRunes             int     `yaml:"summary_max_runes"`             // 摘要最大 rune 数（默认 400）
	FactMaxSentences            int     `yaml:"fact_max_sentences"`            // 事实提取最大句数（默认 5）
	CompressImportanceThreshold float64 `yaml:"compress_importance_threshold"` // 压缩保留 Raw 的重要性阈值（默认 0.7）
	CompressAgeHours            int     `yaml:"compress_age_hours"`            // 压缩保留 Raw 的最大年龄（小时，默认 24）
	ReadFileMaxChars            int `yaml:"read_file_max_chars"`            // ReadFile / SearchInFiles 输出截断字符数
	RunCommandMaxOutput         int `yaml:"run_command_max_output"`         // RunCommand 输出截断字符数
	RunCommandTimeoutSec        int `yaml:"run_command_timeout_sec"`        // RunCommand 最大允许超时（秒）
	ToolExecMaxBytes            int `yaml:"tool_exec_max_bytes"`            // Execute 入参 JSON 摘要截断字节数
	LLMPromptMaxChars           int `yaml:"llm_prompt_max_chars"`           // prompt / response 日志摘要截断字符数
	LLMTrackerSlowModeThreshold int `yaml:"llm_tracker_slow_mode_threshold"` // 连续多少次 LLM 失败进入 slow mode
	LoggerRetryCount            int `yaml:"logger_retry_count"`             // 日志异步写表重试次数
	ContextExplodeSoftLimit     int `yaml:"context_explode_soft_limit"`     // blades 上下文爆炸软阈值（input tokens）
	ContextExplodeHardLimit     int `yaml:"context_explode_hard_limit"`     // blades 上下文爆炸硬阈值（input tokens）
	SummaryTruncateChars        int `yaml:"summary_truncate_chars"`         // CommonCollectTaskSummaries 单条结果截断字符数
	SearchBlockMemoryMaxTokens  int `yaml:"search_block_memory_max_tokens"` // 块记忆检索摘要 token 上限
	SummaryBaseLimit            int `yaml:"summary_base_limit"`             // MetaAgent 最终总结基础字数
	SummaryExtendedLimit        int `yaml:"summary_extended_limit"`         // MetaAgent 最终总结每子 Agent 增加字数
	LoopDetectorWindowSize      int `yaml:"loop_detector_window_size"`      // 循环检测器窗口大小
	LoopDetectorMaxRepeat       int `yaml:"loop_detector_max_repeat"`       // 循环检测器重复阈值
	LoopDetectorMaxEmpty        int `yaml:"loop_detector_max_empty"`        // 循环检测器连续空转阈值
	DomainMemoryRecallMaxChars  int `yaml:"domain_memory_recall_max_chars"` // DomainAgent 记忆召回事件截断字符数
	DomainMemoryContextMaxChars int `yaml:"domain_memory_context_max_chars"` // DomainAgent 记忆上下文注入截断字符数
	DomainResultLogMaxChars     int `yaml:"domain_result_log_max_chars"`    // DomainAgent 结果日志截断字符数
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
	// 简单 Token 鉴权
	AuthEnabled bool   `yaml:"auth_enabled"` // 是否启用 Token 鉴权
	AuthToken   string `yaml:"auth_token"`   // API Token，建议通过 BMA_API_TOKEN 环境变量注入
}

// MemoryConfig 描述记忆管线的运行节奏参数，
// 控制写入批量化与快照周期，时间字段以"秒"为单位。
type MemoryConfig struct {
	WriteBatchSize     int `yaml:"write_batch_size"`     // 单批写入的最大记忆条数
	WriteFlushInterval int `yaml:"write_flush_interval"` // 定时刷新间隔（秒）
	SnapshotInterval   int `yaml:"snapshot_interval"`    // 快照持久化间隔（秒）
}

// Load 从指定路径读取 YAML 配置文件并构造 *Config。
// 职责：读取文件 → 反序列化为 Config → 合并同目录下的 infrastructure.yaml / agent-policy.yaml
// （如果存在）→ 填充默认值 → 解析环境变量引用。
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

	// 先以通用 map 加载主配置，便于与拆分配置做深度合并
	raw := map[string]any{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// 若同目录存在 infrastructure.yaml / agent-policy.yaml，则按深度合并方式叠加上去。
	// 拆分文件中的同名字段覆盖主配置，新增字段追加；config.yaml 本身保留完整内容时仍可独立使用。
	dir := filepath.Dir(path)
	for _, name := range []string{"infrastructure.yaml", "agent-policy.yaml"} {
		extraPath := filepath.Join(dir, name)
		extraData, err := os.ReadFile(extraPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		extra := map[string]any{}
		if err := yaml.Unmarshal(extraData, &extra); err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		mergeMap(raw, extra)
	}

	// 将合并后的通用 map 反序列化为 Config 结构体
	merged, err := yaml.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("marshal merged config: %w", err)
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(merged, cfg); err != nil {
		return nil, fmt.Errorf("parse merged config: %w", err)
	}

	// 为零值字段填充默认值，保证运行参数始终可用
	if err := cfg.applyDefaults(); err != nil {
		return nil, err
	}
	// 将 ${VAR:default} 形式的引用替换为真实环境变量值
	cfg.resolveEnvVars()
	return cfg, nil
}

// mergeMap 将 src 深度合并到 dst 中。
// 对于同名的 map 键递归合并；否则 src 的值覆盖 dst 的值。
func mergeMap(dst, src map[string]any) {
	for k, sv := range src {
		dv, ok := dst[k]
		if !ok {
			dst[k] = sv
			continue
		}
		dm, dOk := dv.(map[string]any)
		sm, sOk := sv.(map[string]any)
		if dOk && sOk {
			mergeMap(dm, sm)
		} else {
			dst[k] = sv
		}
	}
}

// applyDefaults 为 Config 中所有零值字段填充合理的默认值。
// 职责：仅在字段为零值时写入默认，避免覆盖用户显式配置。
// 副作用：就地修改 Config；必要配置缺失时返回 error，让 Load 失败、程序拒绝启动。
func (c *Config) applyDefaults() error {
	c.applyPostgresDefaults()
	c.applyPgVectorDefaults()
	c.applyRedisDefaults()
	if err := c.applyHTTPDefaults(); err != nil {
		return err
	}
	c.applyMemoryDefaults()
	if err := c.applyAgentDefaults(); err != nil {
		return err
	}
	c.applyLoggingDefaults()
	return nil
}

func (c *Config) applyPostgresDefaults() {
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
}

func (c *Config) applyPgVectorDefaults() {
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
}

func (c *Config) applyRedisDefaults() {
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
}

func (c *Config) applyHTTPDefaults() error {
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
	if c.HTTP.AuthEnabled && c.HTTP.AuthToken == "" {
		return fmt.Errorf("HTTP auth_enabled=true 但 auth_token 为空：请设置 BMA_API_TOKEN 环境变量或在 config.yaml 中配置 auth_token")
	}
	return nil
}

func (c *Config) applyMemoryDefaults() {
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

func (c *Config) applyAgentDefaults() error {
	// —— Agent 运行时参数默认值 ——
	// 约定：所有数值字段以 0 表示"未配置"，按字段语义填充默认。
	// 因此配置侧无法把任一项显式设为 0；如需禁用某参数，请改用对应的
	// 布尔开关（如 InterruptEnabled）而非将其置 0。
	if err := c.applyGraphPolicyDefaults(); err != nil {
		return err
	}
	c.applyLLMRuntimeDefaults()
	c.applySafetyDefaults()
	c.applyFeatureTogglesDefaults()
	c.applyAgentStandaloneDefaults()
	c.applyMemoryPolicyDefaults()
	return nil
}

func (c *Config) applyGraphPolicyDefaults() error {
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
	return nil
}

func (c *Config) applyLLMRuntimeDefaults() {
	if c.Agent.ToolCallMaxRounds == 0 {
		c.Agent.ToolCallMaxRounds = 50
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
}

func (c *Config) applySafetyDefaults() {
	// SafetyConfig 当前所有字段（黑名单/白名单/开关）零值即表示关闭/空列表，无需默认值兜底。
}

func (c *Config) applyFeatureTogglesDefaults() {
	if c.Agent.HumanClarifyTimeoutSec == 0 {
		c.Agent.HumanClarifyTimeoutSec = 120
	}
}

func (c *Config) applyAgentStandaloneDefaults() {
	if c.Agent.SkillSetSize == 0 {
		c.Agent.SkillSetSize = 8
	}
	if c.Agent.ContextWindow == 0 {
		c.Agent.ContextWindow = 32000
	}
	if c.Agent.SnapshotSummaryCount == 0 {
		c.Agent.SnapshotSummaryCount = 20
	}
	if c.Agent.SnapshotOpenIssueThreshold == 0 {
		c.Agent.SnapshotOpenIssueThreshold = 0.7
	}
	if c.Agent.SummaryMaxRunes == 0 {
		c.Agent.SummaryMaxRunes = 400
	}
	if c.Agent.FactMaxSentences == 0 {
		c.Agent.FactMaxSentences = 5
	}
	if c.Agent.CompressImportanceThreshold == 0 {
		c.Agent.CompressImportanceThreshold = 0.7
	}
	if c.Agent.CompressAgeHours == 0 {
		c.Agent.CompressAgeHours = 24
	}
	if c.Agent.ReadFileMaxChars == 0 {
		c.Agent.ReadFileMaxChars = 4000
	}
	if c.Agent.RunCommandMaxOutput == 0 {
		c.Agent.RunCommandMaxOutput = 10000
	}
	if c.Agent.RunCommandTimeoutSec == 0 {
		c.Agent.RunCommandTimeoutSec = 60
	}
	if c.Agent.ToolExecMaxBytes == 0 {
		c.Agent.ToolExecMaxBytes = 300
	}
	if c.Agent.LLMPromptMaxChars == 0 {
		c.Agent.LLMPromptMaxChars = 500
	}
	if c.Agent.LLMTrackerSlowModeThreshold == 0 {
		c.Agent.LLMTrackerSlowModeThreshold = 3
	}
	if c.Agent.LoggerRetryCount == 0 {
		c.Agent.LoggerRetryCount = 3
	}
	if c.Agent.ContextExplodeSoftLimit == 0 {
		c.Agent.ContextExplodeSoftLimit = 50000
	}
	if c.Agent.ContextExplodeHardLimit == 0 {
		c.Agent.ContextExplodeHardLimit = 80000
	}
	if c.Agent.SummaryTruncateChars == 0 {
		c.Agent.SummaryTruncateChars = 100
	}
	if c.Agent.SearchBlockMemoryMaxTokens == 0 {
		c.Agent.SearchBlockMemoryMaxTokens = 800
	}
	if c.Agent.SummaryBaseLimit == 0 {
		c.Agent.SummaryBaseLimit = 500
	}
	if c.Agent.SummaryExtendedLimit == 0 {
		c.Agent.SummaryExtendedLimit = 200
	}
	if c.Agent.LoopDetectorWindowSize == 0 {
		c.Agent.LoopDetectorWindowSize = 20
	}
	if c.Agent.LoopDetectorMaxRepeat == 0 {
		c.Agent.LoopDetectorMaxRepeat = 1
	}
	if c.Agent.LoopDetectorMaxEmpty == 0 {
		c.Agent.LoopDetectorMaxEmpty = 4
	}
	if c.Agent.DomainMemoryRecallMaxChars == 0 {
		c.Agent.DomainMemoryRecallMaxChars = 300
	}
	if c.Agent.DomainMemoryContextMaxChars == 0 {
		c.Agent.DomainMemoryContextMaxChars = 500
	}
	if c.Agent.DomainResultLogMaxChars == 0 {
		c.Agent.DomainResultLogMaxChars = 200
	}
}

func (c *Config) applyMemoryPolicyDefaults() {
	// 记忆管线策略默认值（与 assembler.go 原硬编码比例一致：6/12/25/12/25/6/14）
	if c.Agent.MemoryPolicy.ContextWindow == 0 {
		c.Agent.MemoryPolicy.ContextWindow = c.Agent.ContextWindow
	}
	if c.Agent.MemoryPolicy.SystemSegmentRatio == 0 {
		c.Agent.MemoryPolicy.SystemSegmentRatio = 6
	}
	if c.Agent.MemoryPolicy.TopicGlobalRatio == 0 {
		c.Agent.MemoryPolicy.TopicGlobalRatio = 12
	}
	if c.Agent.MemoryPolicy.SharedStateRatio == 0 {
		c.Agent.MemoryPolicy.SharedStateRatio = 25
	}
	if c.Agent.MemoryPolicy.GlobalKBRatio == 0 {
		c.Agent.MemoryPolicy.GlobalKBRatio = 12
	}
	if c.Agent.MemoryPolicy.PrivateMemoryRatio == 0 {
		c.Agent.MemoryPolicy.PrivateMemoryRatio = 25
	}
	if c.Agent.MemoryPolicy.TaskRatio == 0 {
		c.Agent.MemoryPolicy.TaskRatio = 6
	}
	if c.Agent.MemoryPolicy.ReserveRatio == 0 {
		c.Agent.MemoryPolicy.ReserveRatio = 14
	}
	if c.Agent.MemoryPolicy.DefaultTokensPerItem == 0 {
		c.Agent.MemoryPolicy.DefaultTokensPerItem = 200
	}
}

func (c *Config) applyLoggingDefaults() {
	// —— 日志默认值：默认目录 ./logs ——
	if c.Logging.Dir == "" {
		c.Logging.Dir = "logs"
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
	// API Token 走环境变量，避免硬编码到 YAML
	c.HTTP.AuthToken = resolveEnvWithDefault(c.HTTP.AuthToken)
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
