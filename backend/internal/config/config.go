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
	Data     DataConfig     `yaml:"data"`     // 数据生命周期（陈旧知识归档/日志与工具输出保留期，TODO #18-2 T29）
	Notify   NotifyConfig   `yaml:"notify"`   // 会话终态 Webhook 通知（TODO #18-5 T32）
	Skills   SkillsConfig   `yaml:"skills"`   // 经验技能库治理（上限/目录收敛/自动整理）
	Decision DecisionConfig `yaml:"decision"` // 决策层 provider（TODO #23）：默认 LLM 兜底，外部 HTTP 默认关
}

// DecisionConfig 决策层 provider 配置（TODO #23，config.yaml 顶级 decision: 节）。
// 立场：学范式不接死外部 API——provider 可插拔，默认 lightweight 通道；
// 外部 HTTP（TypeSafe 兼容）仅显式配置时启用，失败自动回退 LLM 兜底。
// 行为参数（影子开关/超时/per-点阈值）在 agent: 节，不在此处。
type DecisionConfig struct {
	// Provider 提供者选择：llm（默认）| http。llm 走 CallDecisionWithRetry。
	Provider string `yaml:"provider"`
	// HTTPURL 外部决策服务端点（provider=http 必填；空即视为未配置回退 llm）。
	HTTPURL string `yaml:"http_url"`
	// HTTPAPIKey 外部服务鉴权（Bearer）；空=不带。
	HTTPAPIKey string `yaml:"http_api_key"`
	// HTTPTimeoutSec 外部服务单次超时（秒，默认 8）。
	HTTPTimeoutSec int `yaml:"http_timeout_sec"`
}

// SkillsConfig 经验技能库（learned_skills）治理配置。
// 背景：SessionEvolver 每次会话结束最多新增 2 个技能且没有淘汰，技能库无限增长时
// meta 系统提示的【可用技能】块越拉越长、模型选择准确率下降。三档闸门：
//   - MaxCount 写入上限（B）：达到上限后新技能只允许合并进既有技能，不再新建；
//   - MetaCatalogTop 目录收敛（A）：meta 提示只列常用 top-N，其余经 list_skills 检索；
//   - ConsolidateThreshold 库存整理（C）：启用数达到阈值后每日跑一次合并/归档。
type SkillsConfig struct {
	// MaxCount enabled 经验技能上限；默认 50；负数/0=不限（回退默认）。
	MaxCount int `yaml:"max_count"`
	// MetaCatalogTop meta 系统提示列出的经验技能条数（按 use_count 降序）；默认 20。
	MetaCatalogTop int `yaml:"meta_catalog_top"`
	// ConsolidateThreshold 达到该启用数后每日自动整理（合并/归档）；默认 30；<=0 关闭。
	ConsolidateThreshold int `yaml:"consolidate_threshold"`
}

// NotifyConfig 会话终态 Webhook 通知配置（TODO #18-5 T32）。
type NotifyConfig struct {
	// WebhookURL 通知目标地址；空=关闭（默认）。会话到达终态时 best-effort
	// POST 一条 JSON（3s 超时，失败仅记日志）。
	WebhookURL string `yaml:"webhook_url"`
	// WebhookEvents 触发通知的会话终态列表，默认 [completed, error]
	//（会话状态枚举值见 pkg/enums：running/completed/error/awaiting_clarify/...）。
	WebhookEvents []string `yaml:"webhook_events"`
}

// DataConfig 数据生命周期配置（TODO #18-2 T29），时间以"天"为单位。
// 语义约定：knowledge_archive_days 缺省 0=关（归档是数据保全动作，opt-in）；
// 两个保留期 0=按默认开启、负数=关（日志/工具输出是可再生产物，默认清理防磁盘涨满）。
type DataConfig struct {
	// KnowledgeArchiveDays 陈旧知识归档阈值：block_memory/external_kb 中 last_accessed
	// 早于 N 天的记录 archived=true（查询层已全局排除 archived，语义是"冷备不删"）。
	// 0=关闭（默认），正数=开启。
	KnowledgeArchiveDays int `yaml:"knowledge_archive_days"`
	// LogRetentionDays 日志文件保留天数（logs/<entry>/YYYY-MM-DD.log 按文件 mtime 清理）。
	// 0=默认 30 天；负数=关闭清理。
	LogRetentionDays int `yaml:"log_retention_days"`
	// ToolOutputsRetentionDays 工具输出全文落盘保留天数（<workDir>/.bma/tool_outputs）。
	// 0=默认 14 天；负数=关闭清理。
	ToolOutputsRetentionDays int `yaml:"tool_outputs_retention_days"`
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
	HistoryMaxMessages          int `yaml:"history_max_messages"`            // 单次 LLM 请求携带的最大历史消息数（默认 400，滑动窗口兜底防 API 上下文溢出；负数表示不裁剪）
	ToolOutputHistoryMaxRunes   int `yaml:"tool_output_history_max_runes"`   // 写入历史的单条工具输出最大字符数（默认 2000；负数表示不截断）
	// SummarizeEvery 退役字段（原步频压缩扳机：每 N 步强制压缩一次）。实测步数扳机与上下文
	// 实际大小脱钩，致 Agent 每 10 步强制失忆、反复重读文件；压缩已改为仅由 token 阈值
	// （ContextTokenBudget/TokenBudgetPerRole，默认 150K）驱动。保留解析不破坏旧配置加载，
	// 不再驱动任何逻辑（同 TokenBudgetPerGoal 退役先例）。
	SummarizeEvery              int `yaml:"summarize_every"`
	SummarizeKeepRecent         int `yaml:"summarize_keep_recent"`           // 压缩时保留最近 K 条原始消息（默认 15；<=0 视为 15。2026-08-21 由 10 上调：场景装配 Agent 压缩后丢工具结果细节被迫重读文件，多留 5 条原始消息换少一轮重侦察）
	SummarizeMaxBundles         int `yaml:"summarize_max_bundles"`           // 层级压缩包数量上限（默认 20）：每次压缩触发产生一个结构化压缩包，超限把最老的一半合并为 1 个更粗的包，循环往复保留远期上下文
	SummarizeTimeoutSec         int `yaml:"summarize_timeout_sec"`           // 事件摘要轻量模型调用超时（秒，默认 120）。旧硬编码 5s 对思考型模型必然超时，摘要全挂降级 raw join，上下文全量回注致 token 预算提前耗尽（实证 verify 子 Agent 300K 预算 7 分钟烧穿）
	// SalvageLLMTimeoutSec 失败打捞轻量调用超时（秒，默认 30；思考型模型场景建议 >=60）。
	// 旧硬编码 5s 对思考型模型（glm/deepseek 推理系）来不及出首 token，打捞 facts=0 全降级（TODO #33）。
	SalvageLLMTimeoutSec int `yaml:"salvage_llm_timeout_sec"`
	// EngineLLMTimeoutSec 引擎辅助 LLM（reflection 自检 judge / plan_execute 规划）单次调用
	// 超时（秒，默认 300）。该路径不吃 react_llm_timeout 的 CallLLM 包装，只吃 SDK 默认
	// 600s/请求；无独立超时时 provider 3 次重试 × judge 内部重试叠加可烧 ~70 分钟直到
	// sub_agent_timeout 强杀（2026-08-19 引擎 Agent 事故）。超时包住整次调用含 provider 重试。
	// 负数表示仅受子 Agent 墙钟控制。
	EngineLLMTimeoutSec int `yaml:"engine_llm_timeout_sec"`
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
	// DomainReconWallClockMin DomainAgent 派发无显式 wall_clock_min 时的默认侦察墙钟（分钟，
	// 默认 30；0=关闭回退全局 sub_agent_timeout_min）。2026-08-21 慢任务根因修复：实证领域
	// Agent 侦察失控（炮塔领域 1.5h 零交付——"契约反推"读消费点文件 15+ 轮慢思考从未派发/写入），
	// 中点邮件预警"停止侦察开始产出"。显式 wall_clock_min 的派发不受影响。
	DomainReconWallClockMin int `yaml:"domain_recon_wall_clock_min"`
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
	// ToolResultDumpRunes 工具结果统一收口阈值（TODO 第9项②，rune，默认 8000）：单条工具输出
	// 超该值全文落盘 <workDir>/.bma/tool_outputs/，历史只留头部摘录（ToolResultDigestRunes）+
	// 【全文已落盘】路径。落盘失败降级原样截断。负数=关闭。
	ToolResultDumpRunes int `yaml:"tool_result_dump_runes"`
	// ToolResultDigestRunes 收口后保留的头部摘录 rune 数（默认 2000）。负数=按默认。
	ToolResultDigestRunes int `yaml:"tool_result_digest_runes"`
	// StaleToolEvictRounds 陈旧只读工具结果驱逐轮数（TODO 第9项③，默认 20）：请求构建期把
	// N 轮前（assistant 轮序）的 ReadFile/SearchInFiles 结果替换为"已驱逐需重读"占位符；
	// 只影响请求视图，canonical history 与持久化不动；验证证据类工具不驱逐。负数=关闭。
	StaleToolEvictRounds int `yaml:"stale_tool_evict_rounds"`
	// AgentsMDMaxRunes AGENTS.md/CLAUDE.md 项目自述注入上限（TODO 第10项⑦，rune，默认 4000）：
	// workDir 根部 AGENTS.md 优先、其次 CLAUDE.md，注入【项目自述】段；缺失零开销。负数=关闭。
	AgentsMDMaxRunes int `yaml:"agents_md_max_runes"`
	// ToolParallelEnabled 轮内并行工具执行开关（TODO 第9项①，默认 true）：同轮多个
	// tool_calls 并发派发（无依赖调用并行，墙钟≈最慢一个），结果按原序串行回填。
	// false=关闭（退回逐个串行执行）。
	ToolParallelEnabled *bool `yaml:"tool_parallel_enabled"`
	// ToolParallelMaxConcurrency 并行工具并发上限（TODO 第9项①，默认 4）：信号量限流，
	// 防同轮大量 tool_calls 打满下游（LLM 派发/命令子进程）。<=0 按默认。
	ToolParallelMaxConcurrency int `yaml:"tool_parallel_max_concurrency"`
}

// SafetyConfig 工具沙箱与安全策略配置。
type SafetyConfig struct {
	ToolSandboxDisabled     bool     `yaml:"tool_sandbox_disabled"`      // true 时关闭写路径逃逸检测（保留命令黑名单）
	// ToolApprovalDisabled 全信任模式（默认 false）：true 时关闭全部破坏性操作审批，
	// needsApproval 直接短路——WriteFile/EditFile 生产目录写入、RunCommand 危险命令
	// 模式、插件 Destructive 工具（computer_use 全工具自标 destructive）一律不再推
	// 「需确认」，直接执行。动机（2026-08-30 事故）：computer_use 插件全工具自标
	// destructive，视频诊断会话 30+ 次【需确认】拦截 list/read/wait 等无害只读调用，
	// 审批风暴吃掉整晚墙钟。仅关闭确认链，命令黑名单（hard block）与写路径沙箱不受影响；
	// 信任模型等于把裁决权从"每次人工确认"移交给提示词纪律 + 沙箱，单人开发机可开。
	ToolApprovalDisabled bool `yaml:"tool_approval_disabled"`
	ToolSandboxAllowedPaths []string `yaml:"tool_sandbox_allowed_paths"` // 允许读写的额外绝对路径白名单
	ToolSandboxBlockedCmds  []string `yaml:"tool_sandbox_blocked_cmds"`  // 额外命令黑名单（追加到默认黑名单）
	// RoleToolGateEnabled 角色工具白名单硬门（默认 false=关闭，现状行为）。
	// 角色 tools 白名单原本只是 Schema 可见性软过滤（LLM 看不到白名单外工具，但
	// Registry.Dispatch 不校验，幻觉/提示注入出白名单外工具名仍会执行）；开启后
	// Dispatch 执行前按 ctx 角色硬校验，白名单外调用直接拒绝（Agent 可见错误，不中止循环）。
	// 注意：verifyloop ExecuteChild 等程序化派发路径同样被拦——启用前先确认各角色
	// 白名单完整覆盖其实际所需（含 roles.yaml 对 meta/domain 的覆盖列表）。
	RoleToolGateEnabled bool `yaml:"role_tool_gate_enabled"`
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
	// BlockMemoryFactsMax 单次子 Agent 输出提取的关键事实条数上限（默认 5，1-8 区间）。
	// 事实逐条向量化落库，条数多则召回粒度细但噪声大；1-5 条为设计定档（每条 <=80 字）。
	BlockMemoryFactsMax int `yaml:"block_memory_facts_max"`
	// MemoryIndexMaxLines 记忆索引槽行数配额（TODO #20③，对标 CC MEMORY.md 200 行）。
	// 会话启动注入一行式沉淀索引；超限带重写指令不静默截断。<=0 按默认 200。
	MemoryIndexMaxLines int `yaml:"memory_index_max_lines"`
	// MemoryIndexMaxRunes 记忆索引槽 runes 配额（对标 CC 25KB）。<=0 按默认 25000。
	MemoryIndexMaxRunes int `yaml:"memory_index_max_runes"`
	// MaxTotalDispatches 单 session 内所有角色派发总数上限（合计），超过拒绝派发。
	// 计数在用户发送新消息时重置。默认 30；<=0 时回退默认，负数表示不限制。
	MaxTotalDispatches int `yaml:"max_total_dispatches"`
	// SpecEnforcementEnabled 派发方调用 call_sub_agent 前是否强制先写 WriteSpec。
	// 默认 true（塔防实证 WriteSpec 有用，config.yaml 显式配置覆盖）；Pipeline 重构后由 PlanStage 替代。
	SpecEnforcementEnabled *bool `yaml:"spec_enforcement_enabled"`
	// PlanConfirmationEnabled 计划确认机制开关：下级中大型任务动手前 submit_plan 给上级
	// （顶层给用户）确认，批准后才执行。默认 true；显式 false 关闭（submit_plan 直通不阻塞）。
	PlanConfirmationEnabled *bool `yaml:"plan_confirmation_enabled"`
	// PlanConfirmTimeoutSec 单次计划审批等待超时（秒）。默认 600；超时 fail-open
	// （按计划继续），防上级忙线/失联时下级永久挂起。
	PlanConfirmTimeoutSec int `yaml:"plan_confirm_timeout_sec"`
	// PlanMaxRevisions 每 Agent 计划被驳回重提上限。默认 0=不限制（循环直到批准，
	// 对齐"未经批准不执行"）；>0 时达上限不放行、转 send_message(escalate) 升级仲裁。
	PlanMaxRevisions int `yaml:"plan_max_revisions"`
	// BatchDigestEnabled call_sub_agents 波聚合整合纪要开关：domain 项 ≥2 的同波派发
	// 完成回传汇成一条【整合纪要】单条送达父邮箱（防 N 子完成 N 次打扰父）。
	// 默认 true；显式 false 退回逐条回传（逃生舱）。
	BatchDigestEnabled *bool `yaml:"batch_digest_enabled"`

	// DecisionShadowEnabled 决策层影子总开关（TODO #23，默认 true）：true=全部决策点
	// 影子运行（只记录不生效）；false=仅 enforce 点运行（省轻量调用成本）。
	DecisionShadowEnabled *bool `yaml:"decision_shadow_enabled"`
	// DecisionTimeoutSec 决策层单次 provider 往返超时（秒，默认 5）。超时 fail-open
	// 回退现状行为——决策层故障不阻塞链路。
	DecisionTimeoutSec int `yaml:"decision_timeout_sec"`
	// DecisionPoints per-决策点策略（晋级开关）：mode=shadow|enforce、min_confidence
	// 按后果分级、score_floor 仅 uptake_score 筛选下限。影子对拍达标才允许单点切
	// enforce（逐点独立晋级，不做全量开关）；未列的点按影子+0.6 兜底。
	DecisionPoints map[string]DecisionPointConfig `yaml:"decision_points"`
}

// DecisionPointConfig 单决策点行为参数（agent.decision_points.<point>）。
type DecisionPointConfig struct {
	// Mode shadow（默认，只记录不生效）| enforce（置信达标时答案生效）。
	Mode string `yaml:"mode"`
	// MinConfidence 强制生效置信门槛（高后果动作配高门槛）；<=0 按默认 0.6。
	MinConfidence float64 `yaml:"min_confidence"`
	// ScoreFloor Score 原语筛选下限（uptake_score 过滤低相关候选用）；<=0 按默认 0.5。
	ScoreFloor float64 `yaml:"score_floor"`
}

// AgentConfig 集中所有 Agent 运行时动态可配置参数。
// 通过嵌入若干聚焦的子配置，既保持 cfg.Agent.Xxx 的既有访问方式，又将相关字段按职责分组。
type AgentConfig struct {
	LLMRuntimeConfig     `yaml:",inline"`
	SafetyConfig         `yaml:",inline"`
	FeatureTogglesConfig `yaml:",inline"`

	// 其余不适合归入上述子配置的独立字段。
	// DefaultWorkDir 新会话未选工作目录时的默认目录：相对路径按安装目录（BMA_HOME）解析，
	// 绝对路径原样；空=保持旧行为（回落进程 cwd，即服务从哪儿启动就用哪儿）。
	// bootstrap 启动时 MkdirAll，见 resolveDefaultWorkDir。
	DefaultWorkDir             string `yaml:"default_workdir"`
	ContextWindow              int    `yaml:"context_window"`                 // Agent 上下文窗口（token 数），用于 Watchdog / 装配预算
	ReadFileMaxChars           int    `yaml:"read_file_max_chars"`            // ReadFile / SearchInFiles 输出截断字符数
	RunCommandMaxOutput        int    `yaml:"run_command_max_output"`         // RunCommand 输出截断字符数
	RunCommandTimeoutSec       int    `yaml:"run_command_timeout_sec"`        // RunCommand 最大允许超时（秒）
	ToolExecMaxBytes           int    `yaml:"tool_exec_max_bytes"`            // Execute 入参 JSON 摘要截断字节数
	SearchBlockMemoryMaxTokens int    `yaml:"search_block_memory_max_tokens"` // 块记忆检索摘要 token 上限
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
	// TaskMaxRunes 派发 task 文本软上限（rune 计数，TODO #35 放开预算）。
	// 超软限未达硬限放行附压缩警告（软着陆），超硬限拒绝。默认 3000（原 2000 实证过紧，
	// 强模型+大上下文时代转贴代价低，放开容纳完整自包含描述）。
	TaskMaxRunes int `yaml:"task_max_runes"`
	// TaskMaxRunesHard 派发 task 文本硬上限。默认 4000（原 2600）：仍拦截全量规格转贴。
	TaskMaxRunesHard int `yaml:"task_max_runes_hard"`
	// DomainHotResidentEnabled DomainAgent 热驻留总开关（默认 false=旧行为：完成即销毁）。
	// 开启后：domain 任务完成/用户停止转入 Idle 热驻（goroutine 挂起等复用）；复用经
	// call_sub_agent(reuse_agent_id=X)；idle 加权倒计时在用户下一条消息后武装。
	DomainHotResidentEnabled bool `yaml:"domain_hot_resident_enabled"`
	// DomainIdleBaseTTLMin Idle 基础寿命（分钟，默认 30；<=0 按默认）。
	// 用户下一条消息武装倒计时后的存活时长基数。
	DomainIdleBaseTTLMin int `yaml:"domain_idle_base_ttl_min"`
	// DomainIdleExtendOnReuseMin 每次成功复用延长量（分钟，默认 30；<=0 按默认）。
	// 复用时 reuse_count+1 且 TTL 重置为 min(base+reuse*extend, max)。
	DomainIdleExtendOnReuseMin int `yaml:"domain_idle_extend_on_reuse_min"`
	// DomainIdleMaxTTLMin 加权倒计时上限（分钟，默认 240；<=0 按默认）。
	DomainIdleMaxTTLMin int `yaml:"domain_idle_max_ttl_min"`
	// DomainIdleMaxPerSession 单 session 热驻 domain 上限（默认 4；<=0 按默认）。
	// 超限时新 domain 进 Idle 前 LRU 淘毁最旧 idle。
	DomainIdleMaxPerSession int `yaml:"domain_idle_max_per_session"`
	// DomainIdleTaskQueueLen 忙碌（运行中/挂起）domain 新任务缓冲上限（默认 4；<=0 按默认）。
	// 超限拒绝派发并提示父 Agent 稍后重派。
	DomainIdleTaskQueueLen int `yaml:"domain_idle_task_queue_len"`
	// DomainReuseRosterInject 是否向 MetaAgent 上下文注入【空闲领域Agent】清单（默认 true）。
	// 注入后 MetaAgent 自主判定强相关复用 vs 弱相关新建。
	DomainReuseRosterInject *bool `yaml:"domain_reuse_roster_inject"`

	// ImageMaxEdge 工具图片长边降采样上限（TODO 第9项④，像素，默认 1024）：png/jpeg 超限
	// 等比缩小（不放大小图），原图落盘 <workDir>/.bma/images/；gif/webp 与缩放失败直通。
	// 截图类 MCP 工具（computer_use 等）的视觉 token 随边长平方膨胀，1024 足够读 UI 布局。
	// 负数=关闭。
	ImageMaxEdge int `yaml:"image_max_edge"`

	// TrustMode 默认信任模式（TODO 第10项⑥，对标 Codex 三级信任）：会话创建时取此值为初始档，
	// 会话内可经 API/TUI 随时切换（下一工具调用生效）。取值：
	//   - suggest：全部变更类动作（写文件/命令/插件破坏性工具）逐条推「需确认」，读类直通；
	//   - auto-edit：文件编辑直通，命令与插件破坏性工具审批；
	//   - full-auto：全自主，不再推「需确认」（现状默认；等价 tool_approval_disabled 语义，
	//     但后者是全局硬开关，本项是会话级可切档）。
	// 非法值回落 full-auto。ctx 未携带模式时 Registry 仍按生产边界 + 危险命令规则兜底。
	TrustMode string `yaml:"trust_mode"`

	// DefaultGear 默认执行档位（TODO #14 三档全手动，2026-09-16）：会话创建时取此值为
	// 初始档，会话内可经 API 随时切换（下一轮生效，随 session_history.meta_memory 持久化跨重启）。
	// 取值：
	//   - fast：快速档（文档助手 doc_assistant 顶层直达）；
	//   - daily：日常档（默认）——DomainAgent 顶层直接执行，可自行下拆叶子；
	//   - cluster：集群档（Meta 全装编排，现行为）。
	// 非法值回落 daily。历史值 auto（规则自动选档）已退役，持久化数据读侧映射为 daily。
	DefaultGear string `yaml:"default_gear"`

	// WorktreeEnabled worktree 隔离派发开关（TODO 第9项⑤/#10项⑤，默认 true）：允许
	// call_sub_agent 携带 worktree=true 派发到 git worktree 副本——子 Agent 全部文件
	// 写入落在副本（主目录零写入），成功收尾产出全量 patch，meta 经 merge_worktree
	// 合并门 review/merge/reject。false 时 worktree 参数按 validation_rejected 拒绝。
	// 主目录非 git 仓库时派发侧也会拒绝（不静默降级——隔离名存实亡不如明确报错）。
	WorktreeEnabled *bool `yaml:"worktree_enabled"`

	// Video 用户消息视频附件（Alt+V 粘贴视频）抽帧参数；0 值字段回落默认。
	Video VideoConfig `yaml:"video"`
}

// VideoConfig 视频附件服务端处理参数（抽帧走图片链路兼容全部 provider；
// native 模式下小视频直传 video_url 供 Ark doubao/GLM 等视频理解模型原生消费）。
type VideoConfig struct {
	// Mode 视频消费模式：native（默认，含未配置——≤ native_max_mb 的视频整个直传，
	// openai-chat 兼容端点映射为 video_url——Ark doubao-seed/GLM 视频理解格式，
	// 已对照火山文档 82379/1895586 确认；模型获得完整时间维度信息）|
	// frames（显式退出：全部抽关键帧走图片链路，anthropic 等无视频 API 的
	// provider 组合必须显式配置 frames，否则 video/* 内容会被其白名单静默丢弃）。
	Mode string `yaml:"mode"`
	// NativeMaxMB native 模式下单视频直传上限（MB，base64 后约 ×1.37 进请求体）。
	// 默认 32；超过的仍回落抽帧。端点硬约束（火山文档）：base64 传入视频
	// ≤ 50MB 且请求体 ≤ 64MB → 本值理论上限约 46，默认 32 留余量。
	NativeMaxMB int64 `yaml:"native_max_mb"`
	// FrameCount 每个视频抽取的关键帧数。默认 6。
	FrameCount int `yaml:"frame_count"`
	// FrameMaxPixels 帧长边像素上限（不放大小图）。默认 1024。
	FrameMaxPixels int `yaml:"frame_max_pixels"`
	// MaxVideoMB 单视频大小上限（MB）。默认 200。
	MaxVideoMB int64 `yaml:"max_video_mb"`
	// ExtractTimeoutSec 抽帧整体超时（秒）。默认 25，必须小于 HTTP
	// write_timeout（30s），否则 handler 先被写超时掐断。
	ExtractTimeoutSec int `yaml:"extract_timeout_sec"`
	// FFmpegBin ffmpeg 可执行文件名/路径。默认 "ffmpeg"（PATH 探测，缺失降级仅元数据）。
	FFmpegBin string `yaml:"ffmpeg_bin"`
	// FFprobeBin ffprobe 可执行文件名/路径。默认 "ffprobe"。
	FFprobeBin string `yaml:"ffprobe_bin"`
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
	c.applyNotifyDefaults()
	c.applySkillsDefaults()
	return nil
}

// applySkillsDefaults 技能库治理默认值（语义见 SkillsConfig）：50 / 20 / 30。
func (c *Config) applySkillsDefaults() {
	if c.Skills.MaxCount <= 0 {
		c.Skills.MaxCount = 50
	}
	if c.Skills.MetaCatalogTop <= 0 {
		c.Skills.MetaCatalogTop = 20
	}
	if c.Skills.ConsolidateThreshold == 0 {
		c.Skills.ConsolidateThreshold = 30
	}
}

// applyNotifyDefaults 填充通知配置默认值：事件列表为空时默认 [completed, error]。
func (c *Config) applyNotifyDefaults() {
	if len(c.Notify.WebhookEvents) == 0 {
		c.Notify.WebhookEvents = []string{"completed", "error"}
	}
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
	// -- HTTP 默认值：本机回环 10010 端口、30 秒超时 --
	// 默认只绑 127.0.0.1（TODO #18 T27）：API 有会话执行/文件写/审批等能力，默认
	// ":10010" 会监听全部网卡把服务暴露到局域网。单机装机场景（install.ps1 + 本机
	// 浏览器）够用；确需局域网/容器访问时显式配置 addr（如 ":10010" 或 "0.0.0.0:10010"）。
	// 注意：docker 容器内跑服务端必须显式设 addr（回环地址容器外不可达）。
	if c.HTTP.Addr == "" {
		c.HTTP.Addr = "127.0.0.1:10010"
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
		// 默认 400：窗口只做兜底防爆（防 API 上下文溢出），真正的约束是 150K token 阈值
		// （压缩先于窗口动手）；步频压缩退役后 40/48 这类小窗口会抢在阈值前裁剪，
		// 成为新的隐性失忆点。
		c.Agent.HistoryMaxMessages = 400
	}
	if c.Agent.ToolOutputHistoryMaxRunes == 0 {
		c.Agent.ToolOutputHistoryMaxRunes = 2000
	}
	if c.Agent.SummarizeKeepRecent == 0 {
		c.Agent.SummarizeKeepRecent = 15
	}
	if c.Agent.SummarizeMaxBundles == 0 {
		c.Agent.SummarizeMaxBundles = 20
	}
	if c.Agent.DomainReconWallClockMin == 0 {
		c.Agent.DomainReconWallClockMin = 30
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
	if c.Agent.EngineLLMTimeoutSec == 0 {
		c.Agent.EngineLLMTimeoutSec = 300
	}
	if c.Agent.TaskMaxRunes == 0 {
		c.Agent.TaskMaxRunes = 3000
	}
	if c.Agent.TaskMaxRunesHard == 0 {
		c.Agent.TaskMaxRunesHard = 4000
	}
	// 上下文收口三件套（TODO 第9项②③）+ 项目自述注入（第10项⑦）：0=未配置走默认，负数=显式关闭。
	if c.Agent.ToolResultDumpRunes == 0 {
		c.Agent.ToolResultDumpRunes = 8000
	}
	if c.Agent.ToolResultDigestRunes == 0 {
		c.Agent.ToolResultDigestRunes = 2000
	}
	if c.Agent.StaleToolEvictRounds == 0 {
		c.Agent.StaleToolEvictRounds = 20
	}
	if c.Agent.AgentsMDMaxRunes == 0 {
		c.Agent.AgentsMDMaxRunes = 4000
	}
	// 轮内并行工具执行（TODO 第9项①）：未配置默认开启；并发上限 <=0 走默认（react 侧再兜底）。
	if c.Agent.ToolParallelEnabled == nil {
		t := true
		c.Agent.ToolParallelEnabled = &t
	}
	if c.Agent.ToolParallelMaxConcurrency == 0 {
		c.Agent.ToolParallelMaxConcurrency = 4
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
	if c.Agent.BlockMemoryFactsMax <= 0 {
		c.Agent.BlockMemoryFactsMax = 5
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
	// 计划确认默认开启：对照 Claude Code 差距分析（2026-08-29 塔防日志 2 次返工约 130 分钟），
	// 大任务先出计划确认可显著降低返工；*bool 区分"未配置"（默认 true）与"显式 false"。
	if c.Agent.PlanConfirmationEnabled == nil {
		t := true
		c.Agent.PlanConfirmationEnabled = &t
	}
	if c.Agent.PlanConfirmTimeoutSec <= 0 {
		c.Agent.PlanConfirmTimeoutSec = 600
	}
	// 波聚合整合纪要默认开启（集群档提速：N 子完成汇 1 条，省父 Agent N-1 轮空转）。
	if c.Agent.BatchDigestEnabled == nil {
		t := true
		c.Agent.BatchDigestEnabled = &t
	}
	// 决策层（TODO #23）：影子默认开（影子先行是本层立身原则）；超时默认 5s
	// （provider 故障/超时 fail-open 回退现状行为，不阻塞链路）。
	if c.Agent.DecisionShadowEnabled == nil {
		t := true
		c.Agent.DecisionShadowEnabled = &t
	}
	if c.Agent.DecisionTimeoutSec == 0 {
		c.Agent.DecisionTimeoutSec = 5
	}
	// decision_points 未配置时每点按影子+0.6 门槛兜底（decision.DefaultPolicy 同款）；
	// 影子对拍达标（单点 ≥200 样本或满 7 天、准确率≥阈值）才允许单点手动切 enforce。
	if c.Agent.DecisionPoints == nil {
		c.Agent.DecisionPoints = map[string]DecisionPointConfig{}
	}
	// decision provider 默认 llm 兜底；http 仅显式配置启用。
	if c.Decision.Provider == "" {
		c.Decision.Provider = "llm"
	}
	if c.Decision.HTTPTimeoutSec == 0 {
		c.Decision.HTTPTimeoutSec = 8
	}
	// PlanMaxRevisions 默认 0=不限制：子 Agent 计划必须循环修订直到上级批准，
	// 不做"达上限放行"；>0 仅作防失控兜底（达上限转升级仲裁，仍不放行）。
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
	// Domain 热驻留参数默认值（总开关默认 false=旧行为，故零值兜底仅在开启时有意义）。
	if c.Agent.DomainIdleBaseTTLMin == 0 {
		c.Agent.DomainIdleBaseTTLMin = 30
	}
	if c.Agent.DomainIdleBaseTTLMin < 0 {
		c.Agent.DomainIdleBaseTTLMin = 30
	}
	if c.Agent.DomainIdleExtendOnReuseMin <= 0 {
		c.Agent.DomainIdleExtendOnReuseMin = 30
	}
	if c.Agent.DomainIdleMaxTTLMin <= 0 {
		c.Agent.DomainIdleMaxTTLMin = 240
	}
	if c.Agent.DomainIdleMaxPerSession <= 0 {
		c.Agent.DomainIdleMaxPerSession = 4
	}
	if c.Agent.DomainIdleTaskQueueLen <= 0 {
		c.Agent.DomainIdleTaskQueueLen = 4
	}
	// 复用清单注入默认开启：*bool 区分"未配置"（默认 true）与"显式 false"。
	if c.Agent.DomainReuseRosterInject == nil {
		t := true
		c.Agent.DomainReuseRosterInject = &t
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
	// 工具图片长边降采样上限（TODO 第9项④）：默认 1024，负数=显式关闭。
	if c.Agent.ImageMaxEdge == 0 {
		c.Agent.ImageMaxEdge = 1024
	}
	// 默认信任模式（TODO 第10项⑥）：空/非法值回落 full-auto（现状语义）。
	switch c.Agent.TrustMode {
	case "suggest", "auto-edit", "full-auto":
	default:
		c.Agent.TrustMode = "full-auto"
	}
	// 默认执行档位（TODO #14 三档全手动）：空/非法值回落 daily（日常档）；
	// 历史值 auto（已退役）同守读侧映射语义，显式配置 auto 时按 daily 处理。
	switch c.Agent.DefaultGear {
	case "fast", "daily", "cluster":
	case "auto":
		c.Agent.DefaultGear = "daily"
	default:
		c.Agent.DefaultGear = "daily"
	}
	// worktree 隔离派发开关（TODO 第9项⑤）：nil（未配置）默认开启，显式 false 关闭。
	if c.Agent.WorktreeEnabled == nil {
		t := true
		c.Agent.WorktreeEnabled = &t
	}
	// 视频附件抽帧参数（0 值回落 agent.DefaultVideoOptions 内部默认）：
	// 二进制名留空由 agent 层 exec.LookPath 探测；帧数/像素/超时此处不重复设默认，
	// 仅校验超时上限（须 < http.write_timeout，超限拒绝启动防止写超时掐断响应）。
	if c.Agent.Video.ExtractTimeoutSec < 0 {
		c.Agent.Video.ExtractTimeoutSec = 0
	}
	if c.Agent.Video.MaxVideoMB < 0 {
		c.Agent.Video.MaxVideoMB = 0
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
