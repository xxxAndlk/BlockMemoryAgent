package config

import (
	"fmt"           // 用于包装和格式化错误信息
	"os"            // 用于读取配置文件与获取环境变量
	"path/filepath" // 用于定位同目录下的拆分配置文件
	"strings"       // 用于字符串切分、裁剪等处理

	"gopkg.in/yaml.v3" // YAML 解析库，用于反序列化 config.yaml
)

// Config 是后端服务的顶层配置结构，对应 config/config.yaml 的根节点。
// 它聚合了 Postgres、PgVector、Redis、HTTP、Memory、Agent、Logging 五个子配置模块，
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
}

// LoggingConfig 日志文件输出配置。
// 不同入口（浏览器 HTTP 服务 / TUI）写入独立文件，便于按入口排查问题。
type LoggingConfig struct {
	Dir          string `yaml:"dir"`           // 日志目录，空则默认 ./logs；自动按入口 + 日期生成文件名
	Enabled      bool   `yaml:"enabled"`       // 是否启用文件日志；false 时仅输出到 stderr
	Format       string `yaml:"format"`        // 输出格式：console（默认）或 json
	Level        string `yaml:"level"`         // 日志级别：debug | info（默认）| warn | error
	Timezone     string `yaml:"timezone"`      // 时区：Local（默认）| UTC | 如 Asia/Shanghai
	StackEnabled *bool  `yaml:"stack_enabled"` // error 级别是否打印调用栈，默认 true
	NoColor      *bool  `yaml:"no_color"`      // 是否禁用 ANSI 颜色，文件日志建议 true，默认 true
}

// LLMRuntimeConfig LLM 调用与重试运行时参数配置。
type LLMRuntimeConfig struct {
	ToolCallMaxRounds int `yaml:"tool_call_max_rounds"` // Assistant 单任务 ReAct 工具调用循环最大轮数（含重复调用/空转检测提前退出）；负数表示不限制
	RetryCount        int `yaml:"retry_count"`          // Assistant 任务执行指数退避重试次数
	RetryBackoffMs    int `yaml:"retry_backoff_ms"`     // 重试初始退避时长（毫秒）

	ReactLLMTimeoutSec          int `yaml:"react_llm_timeout_sec"`           // ReAct 单次 LLM 调用超时（秒，默认 300；负数表示仅受会话取消控制）
	SubAgentTimeoutMin          int `yaml:"sub_agent_timeout_min"`           // 子 Agent 独立执行超时（分钟，默认 30；负数表示不限制）
	SubAgentHeartbeatTimeoutMin int `yaml:"sub_agent_heartbeat_timeout_min"` // 子 Agent 心跳超时（分钟，默认 5；<=0 关闭巡检，仅靠 sub_agent_timeout 兜底）
	HistoryMaxMessages          int `yaml:"history_max_messages"`            // 单次 LLM 请求携带的最大历史消息数（默认 40，滑动窗口防 token 爆炸；负数表示不裁剪）
	ToolOutputHistoryMaxRunes   int `yaml:"tool_output_history_max_runes"`   // 写入历史的单条工具输出最大字符数（默认 2000；负数表示不截断）
	SummarizeEvery              int `yaml:"summarize_every"`                 // 每 N 步触发一次历史压缩（默认 10；<=0 关闭压缩，仅用滑动窗口）
	SummarizeKeepRecent         int `yaml:"summarize_keep_recent"`           // 压缩时保留最近 K 条原始消息（默认 10；<=0 视为 10）
	SummarizeTimeoutSec         int `yaml:"summarize_timeout_sec"`           // 事件摘要轻量模型调用超时（秒，默认 120）。旧硬编码 5s 对思考型模型必然超时，摘要全挂降级 raw join，上下文全量回注致 token 预算提前耗尽（实证 verify 子 Agent 300K 预算 7 分钟烧穿）
	// TokenBudgetPerGoal 单次 RunWithHistory 累计 token 上限（input+output 之和，跨轮累加）。
	// 超限后主循环 break 返回部分完成（LimitReached），与 maxIter 轮数上限正交。
	// 默认 0 表示不限制；config.yaml 设 token_budget_per_goal: 100000 启用。
	// 被 TokenBudgetPerRole 覆盖:按角色设预算时此项对该角色无效。
	TokenBudgetPerGoal int `yaml:"token_budget_per_goal"`
	// TokenBudgetPerRole 按角色 ID 设单 Agent 累计 token 上限（resume 重置）。
	// DomainAgent 默认 120000;叶子助手默认 40000;meta 默认 200000(安全网,不为 0 因 config tool_call_max_rounds=-1 使 maxIter 无界,双无界会死循环)。
	// codegen 单次可吐 10K+ token,旧值 50K/20K 扛不住多文件生成。未列出的角色按上述默认。显式配置覆盖默认,如 token_budget_per_role: {domain: 120000}。
	TokenBudgetPerRole map[string]int `yaml:"token_budget_per_role"`
	// PausedDomainMaxResumes 同一 Paused DomainAgent 允许的最大续跑次数（默认 1；<=0 按默认）。
	// 续跑重置 fresh token 预算，不设上限则"触限-暂停-续跑"环路永不绑定（实证：验收领域研磨
	// 32 轮 30 分钟不收敛）。触顶后强制收口：部分产出返回父 Agent 并标 Done，由 MetaAgent 决定返工。
	PausedDomainMaxResumes int `yaml:"paused_domain_max_resumes"`
}

// SafetyConfig 工具沙箱与安全策略配置。
type SafetyConfig struct {
	ToolSandboxDisabled     bool     `yaml:"tool_sandbox_disabled"`      // true 时关闭写路径逃逸检测（保留命令黑名单）
	ToolSandboxAllowedPaths []string `yaml:"tool_sandbox_allowed_paths"` // 允许读写的额外绝对路径白名单
	ToolSandboxBlockedCmds  []string `yaml:"tool_sandbox_blocked_cmds"`  // 额外命令黑名单（追加到默认黑名单）
	// ProductionWorkDir 生产环境工作目录（绝对路径，默认空=未启用）。
	// 非空且当前工具工作目录等于/位于其下时，破坏性工具（WriteFile、写类/危险 RunCommand）
	// 经 approvalHook 推「需确认」事件，上层暂停会话等用户确认；空时仅在命中危险命令模式
	// （git push/rm -rf/drop table 等，见 tool/destructive.go）时要求确认。
	ProductionWorkDir string `yaml:"production_workdir"`
}

// FeatureTogglesConfig Agent 特性开关配置。
type FeatureTogglesConfig struct {
	DAGEnabled              bool  `yaml:"dag_enabled"`                // 是否启动 DAG 调度器
	RestoreSessions         *bool `yaml:"restore_sessions"`           // 启动时是否从 session_history 恢复最近会话到内存（默认 true；显式 false 关闭）
	BlockMemoryWriteEnabled *bool `yaml:"block_memory_write_enabled"` // 子 Agent 成功完成后是否将结果摘要沉淀到块记忆知识库（默认 true；显式 false 关闭）
	// MaxTotalDispatches 单 session 内所有角色派发总数上限（合计），超过拒绝派发。
	// 计数在用户发送新消息时重置。默认 30；<=0 时回退默认，负数表示不限制。
	MaxTotalDispatches int `yaml:"max_total_dispatches"`
	// SpecEnforcementEnabled 派发方调用 call_sub_agent 前是否强制先写 WriteSpec。
	// 默认 false（基础任务先跑通）；Pipeline 重构后由 PlanStage 替代，配置项整体移除。
	SpecEnforcementEnabled *bool `yaml:"spec_enforcement_enabled"`
}

// AgentConfig 集中所有 Agent 运行时动态可配置参数。
// 通过嵌入若干聚焦的子配置，既保持 cfg.Agent.Xxx 的既有访问方式，又将相关字段按职责分组。
type AgentConfig struct {
	LLMRuntimeConfig     `yaml:",inline"`
	SafetyConfig         `yaml:",inline"`
	FeatureTogglesConfig `yaml:",inline"`

	// 其余不适合归入上述子配置的独立字段。
	ContextWindow              int `yaml:"context_window"`                 // Agent 上下文窗口（token 数），用于 Watchdog / 装配预算
	ReadFileMaxChars           int `yaml:"read_file_max_chars"`            // ReadFile / SearchInFiles 输出截断字符数
	RunCommandMaxOutput        int `yaml:"run_command_max_output"`         // RunCommand 输出截断字符数
	RunCommandTimeoutSec       int `yaml:"run_command_timeout_sec"`        // RunCommand 最大允许超时（秒）
	ToolExecMaxBytes           int `yaml:"tool_exec_max_bytes"`            // Execute 入参 JSON 摘要截断字节数
	SearchBlockMemoryMaxTokens int `yaml:"search_block_memory_max_tokens"` // 块记忆检索摘要 token 上限
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
// 职责：读取文件 -> 反序列化为 Config -> 合并同目录下的 infrastructure.yaml / agent-policy.yaml
// （如果存在）-> 填充默认值 -> 解析环境变量引用。
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
	// -- Postgres 默认值：本地开发 DSN 与中等规模连接池 --
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
	// -- pgvector 默认值：768 维、ivfflat 索引、cosine 距离 --
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
	// -- Redis 默认值：本地地址、小型连接池、毫秒级超时、7 天快照 TTL --
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
	// -- HTTP 默认值：10010 端口、30 秒超时 --
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
	// -- 记忆管线默认值：100 条/批、5 秒刷新、300 秒快照 --
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
	// -- Agent 运行时参数默认值 --
	// 约定：所有数值字段以 0 表示"未配置"，按字段语义填充默认。
	// 因此配置侧无法把任一项显式设为 0；如需禁用某参数，请改用对应的
	// 布尔开关（如 DAGEnabled）而非将其置 0。
	c.applyLLMRuntimeDefaults()
	c.applySafetyDefaults()
	c.applyFeatureTogglesDefaults()
	c.applyAgentStandaloneDefaults()
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
	if c.Agent.ReactLLMTimeoutSec == 0 {
		c.Agent.ReactLLMTimeoutSec = 300
	}
	if c.Agent.SubAgentTimeoutMin == 0 {
		c.Agent.SubAgentTimeoutMin = 30
	}
	if c.Agent.SubAgentHeartbeatTimeoutMin == 0 {
		c.Agent.SubAgentHeartbeatTimeoutMin = 5
	}
	if c.Agent.HistoryMaxMessages == 0 {
		c.Agent.HistoryMaxMessages = 40
	}
	if c.Agent.ToolOutputHistoryMaxRunes == 0 {
		c.Agent.ToolOutputHistoryMaxRunes = 2000
	}
	if c.Agent.SummarizeEvery == 0 {
		c.Agent.SummarizeEvery = 10
	}
	if c.Agent.SummarizeKeepRecent == 0 {
		c.Agent.SummarizeKeepRecent = 10
	}
	if c.Agent.SummarizeTimeoutSec == 0 {
		c.Agent.SummarizeTimeoutSec = 120
	}
}

func (c *Config) applySafetyDefaults() {
	// SafetyConfig 当前所有字段（黑名单/白名单/开关）零值即表示关闭/空列表，无需默认值兜底。
}

func (c *Config) applyFeatureTogglesDefaults() {
	// 块记忆写入默认开启，与 RestoreSessions 同样采用 *bool 以区分"未配置"与"显式 false"。
	if c.Agent.BlockMemoryWriteEnabled == nil {
		t := true
		c.Agent.BlockMemoryWriteEnabled = &t
	}
	// 全局派发总数默认 30：按"13 文件级编排任务约需 25-30 次派发"的实证校准；
	// 旧的按角色对 5 次限额会在多文件任务中途卡死派发。
	if c.Agent.MaxTotalDispatches == 0 {
		c.Agent.MaxTotalDispatches = 30
	}
	// Spec 强制默认关闭：基础任务先跑通。Pipeline 重构后由 PlanStage 替代。
	// *bool 区分"未配置"（默认 false）与"显式 true"（开启强制）。
	if c.Agent.SpecEnforcementEnabled == nil {
		f := false
		c.Agent.SpecEnforcementEnabled = &f
	}
}

func (c *Config) applyAgentStandaloneDefaults() {
	if c.Agent.ContextWindow == 0 {
		c.Agent.ContextWindow = 32000
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
	if c.Agent.SearchBlockMemoryMaxTokens == 0 {
		c.Agent.SearchBlockMemoryMaxTokens = 800
	}
}

func (c *Config) applyLoggingDefaults() {
	// -- 日志默认值：默认目录 ./logs --
	if c.Logging.Dir == "" {
		c.Logging.Dir = "logs"
	}
	if c.Logging.Format == "" {
		c.Logging.Format = "console"
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Logging.Timezone == "" {
		c.Logging.Timezone = "Local"
	}
	if c.Logging.StackEnabled == nil {
		t := true
		c.Logging.StackEnabled = &t
	}
	if c.Logging.NoColor == nil {
		t := true
		c.Logging.NoColor = &t
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
