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
	// SalvageLLMTimeoutSec 失败打捞轻量调用超时（秒，默认 30；思考型模型场景建议 >=60）。
	// 旧硬编码 5s 对思考型模型（glm/deepseek 推理系）来不及出首 token，打捞 facts=0 全降级（TODO #33）。
	SalvageLLMTimeoutSec int `yaml:"salvage_llm_timeout_sec"`
	// TokenBudgetPerGoal 退役字段（原累计跨轮 token 预算，已替换为按角色上下文阈值）。
	// 保留不破坏旧配置加载，但不再驱动任何闸门。见 TokenBudgetPerRole。
	TokenBudgetPerGoal int `yaml:"token_budget_per_goal"`
	// TokenBudgetPerRole 按 roleID 设上下文 token 阈值：Assemble 压缩后估算 messages
	// token >= 阈值即 LimitReached 暂停（近 N 单独就超、压不下去）。语义=上下文阈值非累计跨轮。
	// 未列出角色默认 150000（bootstrap 注入）。显式配置覆盖，如 token_budget_per_role: {meta: 150000}。
	TokenBudgetPerRole map[string]int `yaml:"token_budget_per_role"`
	// ContextTokenBudget 上下文 token 阈值默认值（未在 TokenBudgetPerRole 列出的角色用此值）。
	// 替换原累计 token 预算：Assemble 压缩后估算 messages token >= 阈值即 LimitReached 暂停，
	// 每轮独立估算（各 Agent 独立上下文统计）。默认 150000；<=0 不限制。
	ContextTokenBudget int `yaml:"context_token_budget"`
	// PausedDomainMaxResumes 同一 Paused DomainAgent 允许的最大续跑次数（默认 1；<=0 按默认）。
	// 续跑重置 fresh token 预算，不设上限则"触限-暂停-续跑"环路永不绑定（实证：验收领域研磨
	// 32 轮 30 分钟不收敛）。触顶后强制收口：部分产出返回父 Agent 并标 Done，由 MetaAgent 决定返工。
	PausedDomainMaxResumes int `yaml:"paused_domain_max_resumes"`
	// PromptEnhance 用户输入自动提示词补全开关（TODO #36 Phase 0 规则版，默认 true）。
	// 指针三态：nil=默认开启（applyDefaults 兜底）；显式 true/false 尊重显式值。
	// 开启时 sendMessage 对命中续跑/控制/诊断意图的输入附加【系统补全】段
	// （意图标签 + 最近失败/未完成任务绑定），只增不改用户原文；关闭=原样直通。
	PromptEnhance *bool `yaml:"prompt_enhance"`
	// PromptEnhanceLLM L2 轻量模型仲裁开关（TODO #39，默认 true）：灰区输入
	// （L1 未命中但句中有弱词信号）调轻量模型四分类（none/resume/control/diagnose）；
	// 超时/失败/低置信一律按普通任务直通（宁漏判不误判）。关闭=降级纯规则（Phase 0 行为）。
	PromptEnhanceLLM *bool `yaml:"prompt_enhance_llm"`
	// PromptEnhanceLLMTimeoutSec L2 仲裁单次超时秒（默认 10；<=0 按默认）。
	// 轻量模型为推理系首 token 慢，宁可短超时降级也不阻塞用户输入（最坏延迟=本值）。
	PromptEnhanceLLMTimeoutSec int `yaml:"prompt_enhance_llm_timeout_sec"`
	// PromptEnhanceMaxInputRunes L0 输入形态闸门长度上限（rune，默认 30；<=0 按默认）。
	// 续跑/控制/诊断本质是短指令（实证 ≤15 字），超过视为任务描述直接直通不分类。
	PromptEnhanceMaxInputRunes int `yaml:"prompt_enhance_max_input_runes"`
	// StopDestroyCountdownSec 软停止销毁倒计时（秒，TODO #37，默认 300；负数=关闭=永久暂停）。
	// Stop 后到期未续跑则硬销毁全部节点（cascadeCancelTree + 会话 error）；续跑触发即取消。
	StopDestroyCountdownSec int `yaml:"stop_destroy_countdown_sec"`
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
	// 默认 true（塔防实证 WriteSpec 有用，config.yaml 显式配置覆盖）；Pipeline 重构后由 PlanStage 替代。
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
	// DispatchRetryCount 叶子助手 kind=error 失败的自动重派次数（TODO #23，最小一档）。
	// 默认 1：失败自动重跑一次（同任务同前缀）；domain/timeout/killed/loop_guard 不自动重试。
	// 与 LLM 调用层重试（retry_count）正交：那层重试模型调用本身，这层重跑整个子 Agent。
	DispatchRetryCount int `yaml:"dispatch_retry_count"`
	// SessionMaxWallClockMin 会话全局墙钟上限（分钟）：从会话创建起超时未终止则级联取消
	// 全部节点 + 会话置 error"超全局时限"（TODO #25-4 硬止损）。默认 0=关闭（保持现状）。
	SessionMaxWallClockMin int `yaml:"session_max_wall_clock_min"`
	// DomainHeartbeatTimeoutMin DomainAgent 心跳超时（分钟）（TODO #25-3 防误杀版）。
	// 默认 0 = 2× sub_agent_heartbeat_timeout_min：domain 等子/等回信靠后代活动冒泡保活，
	// 后代全静默后超该阈值判假死。
	DomainHeartbeatTimeoutMin int `yaml:"domain_heartbeat_timeout_min"`
	// AskUserTimeoutSec ask_user 工具提问默认超时（秒）（TODO #24 人在回路）。
	// 默认 0=不限；>0 时超时未答复工具返回"用户未答复，自行决策"。单次调用可经
	// ask_user(timeout_sec=N) 覆盖。
	AskUserTimeoutSec int `yaml:"ask_user_timeout_sec"`
	// ReflectionMaxRounds 派发 mode=reflection 时自检不达标重试轮数上限（TODO #29）。
	// 默认 2；<=0 引擎内部按默认。每轮自检成本 ≈ +1 次辅助 LLM 调用。
	ReflectionMaxRounds int `yaml:"reflection_max_rounds"`
	// PlanExecuteMaxSteps 派发 mode=plan_execute 时最大执行步数（TODO #29）。
	// 默认 8；<=0 引擎内部按默认。超限后强制收口终答。
	PlanExecuteMaxSteps int `yaml:"plan_execute_max_steps"`
	// JudgeRole 校验 judge 的角色 ID（TODO #43 交叉模型）：engineLLMForJudge 优先取该角色的
	// provider（与被审角色不同模型，防同模型自评放水/幻觉 pass）。默认 "prompt_reviewer"
	// （deepseek-v4-flash）；取不到时 dispatcher 回退同角色 provider（测试/角色未注册场景）。
	JudgeRole string `yaml:"judge_role"`
	// ExploreBudget 单个叶子 Agent 任务内探索类工具（ReadFile/ListDir/SearchInFiles 与只读型 RunCommand）
	// 调用次数上限。超过后这些工具返回错误，逼迫 Agent 开始 WriteFile。
	// 默认 20：单页 200 行 × 20 次 ≈ 4000 行，覆盖单领域职责文件全集。
	ExploreBudget int `yaml:"explore_budget"`
	// ExploreBudgetDomain DomainAgent（协调者）写入未开始前的探索预算。
	// domain 职责是拆任务/派发/整合/验证，亲自探索是全舰队最贵路径，预算收紧倒逼下放叶子。
	// 默认 8：查共享契约 + 1-2 次 SearchInFiles 定位 + 验收期精读失败点。
	ExploreBudgetDomain int `yaml:"explore_budget_domain"`
	// ExploreBudgetPostWrite 写入已开始后的探索预算升档上限。
	// 修复期"读报错位置->改->复验"循环需要精读，禁读逼盲改回归震荡；写后放开精读。
	// 默认 40：仍设上限防"逐文件通读"式发散，由连读循环守卫与 token 预算兜底。
	ExploreBudgetPostWrite int `yaml:"explore_budget_post_write"`
	// TaskMaxRunes 派发 task 文本软上限（rune 计数，TODO #35 放开预算）。
	// 超软限未达硬限放行附压缩警告（软着陆），超硬限拒绝。默认 3000（原 2000 实证过紧，
	// 强模型+大上下文时代转贴代价低，放开容纳完整自包含描述）。
	TaskMaxRunes int `yaml:"task_max_runes"`
	// TaskMaxRunesHard 派发 task 文本硬上限。默认 4000（原 2600）：仍拦截全量规格转贴。
	TaskMaxRunesHard int `yaml:"task_max_runes_hard"`
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
	if c.Agent.ContextTokenBudget == 0 {
		c.Agent.ContextTokenBudget = 150000
	}
	if c.Agent.SalvageLLMTimeoutSec == 0 {
		c.Agent.SalvageLLMTimeoutSec = 30
	}
	if c.Agent.ExploreBudget == 0 {
		c.Agent.ExploreBudget = 20
	}
	if c.Agent.ExploreBudgetDomain == 0 {
		c.Agent.ExploreBudgetDomain = 8
	}
	if c.Agent.ExploreBudgetPostWrite == 0 {
		c.Agent.ExploreBudgetPostWrite = 40
	}
	if c.Agent.TaskMaxRunes == 0 {
		c.Agent.TaskMaxRunes = 3000
	}
	if c.Agent.TaskMaxRunesHard == 0 {
		c.Agent.TaskMaxRunesHard = 4000
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
	// Spec 强制默认开启：塔防实证 WriteSpec 有用；*bool 区分"未配置"（默认 true）与"显式 false"。
	if c.Agent.SpecEnforcementEnabled == nil {
		t := true
		c.Agent.SpecEnforcementEnabled = &t
	}
	// 输入补全默认开启（TODO #36）：*bool 区分"未配置"（默认 true）与"显式 false"。
	if c.Agent.PromptEnhance == nil {
		t := true
		c.Agent.PromptEnhance = &t
	}
	// L2 仲裁默认开启（TODO #39）：灰区输入调轻量模型四分类，失败降级纯规则。
	if c.Agent.PromptEnhanceLLM == nil {
		t := true
		c.Agent.PromptEnhanceLLM = &t
	}
	// L2 单次超时默认 10s；L0 闸门长度默认 30 rune。
	if c.Agent.PromptEnhanceLLMTimeoutSec == 0 {
		c.Agent.PromptEnhanceLLMTimeoutSec = 10
	}
	if c.Agent.PromptEnhanceMaxInputRunes == 0 {
		c.Agent.PromptEnhanceMaxInputRunes = 30
	}
	// 软停止销毁倒计时默认 300s（TODO #37）。
	if c.Agent.StopDestroyCountdownSec == 0 {
		c.Agent.StopDestroyCountdownSec = 300
	}
}

func (c *Config) applyAgentStandaloneDefaults() {
	if c.Agent.ContextWindow == 0 {
		c.Agent.ContextWindow = 32000
	}
	if c.Agent.ReadFileMaxChars == 0 {
		// 16000 ≈ 200 行代码（含行号前缀与 UTF-8 中文注释字节）的整页容量；
		// 旧值 4000 会把 200 行页截停到 60-110 行，抵消 defaultReadFileLimit=200。
		c.Agent.ReadFileMaxChars = 16000
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
	// 叶子助手 kind=error 失败自动重派一次（TODO #23）；<=0 关闭。
	if c.Agent.DispatchRetryCount == 0 {
		c.Agent.DispatchRetryCount = 1
	}
	// domain 心跳默认 0 = 2× 叶子（bootstrap 侧兜底），此处只保证非负。
	if c.Agent.DomainHeartbeatTimeoutMin < 0 {
		c.Agent.DomainHeartbeatTimeoutMin = 0
	}
	// 派发执行模式引擎参数（TODO #29）：reflection 自检轮数 / plan_execute 最大步数。
	if c.Agent.ReflectionMaxRounds == 0 {
		c.Agent.ReflectionMaxRounds = 2
	}
	if c.Agent.PlanExecuteMaxSteps == 0 {
		c.Agent.PlanExecuteMaxSteps = 8
	}
	// 校验 judge 角色（TODO #43 交叉模型）：默认 prompt_reviewer（与被审角色不同模型）。
	if c.Agent.JudgeRole == "" {
		c.Agent.JudgeRole = "prompt_reviewer"
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
