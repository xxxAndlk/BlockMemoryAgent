package tool

// 导入所需标准库与项目内部包。
import (
	"log/slog" // 结构化日志，用于输出调试信息
	"os"       // 操作系统接口，用于获取当前工作目录等
	"time"     // 时间类型与常量，用于设置执行超时

	"github.com/blockmemory/agent/backend/internal/config" // 项目配置包，注入 Agent 阈值配置
)

// Callback 是工具执行完成后的回调函数类型。
// 每次工具执行结束后，调用方可以通过该回调接收执行结果。
type Callback func(result *Result)

// Executor 负责在轻量级沙箱中运行内置工具。
// 它管理执行目录、超时、沙箱策略、Guard 注册表以及回调。
type Executor struct {
	workDir  string              // 工作目录，工具执行时以此目录为上下文
	timeout  time.Duration       // 默认执行超时时间
	sandbox  SandboxConfig       // 沙箱配置，控制允许/禁止的行为
	guards   *GuardRegistry      // Guard 注册表，用于拦截危险的写操作或命令
	agentCfg *config.AgentConfig // Agent 阈值配置，控制资源使用上限
	callback Callback            // 执行完成后的回调函数
}

// NewExecutor 创建一个新的 Executor 实例。
// workDir 指定工作目录；如果为空，则尝试使用 fallbackWorkDir() 获取默认值。
func NewExecutor(workDir string) *Executor {
	// 如果调用方没有提供工作目录，则回退到默认目录
	if workDir == "" {
		workDir = fallbackWorkDir()
		// 如果回退目录仍然为空，则记录调试日志，后续由调用方或 readWorkDir 处理
		if workDir == "" {
			slog.Debug("tool_executor: fallback to empty workDir")
		}
	}
	// 初始化 Executor，设置工作目录、默认超时和默认沙箱配置
	e := &Executor{
		workDir: workDir,
		timeout: 30 * time.Second,
		sandbox: DefaultSandboxConfig(),
	}
	// 初始化默认的 Guard 注册表
	e.guards = e.defaultGuardRegistry()
	return e
}

// defaultGuardRegistry 构造 Executor 默认使用的 Guard 注册表。
// 它会注册多个写操作 Guard 和命令 Guard，用于在执行前后拦截危险行为。
func (e *Executor) defaultGuardRegistry() *GuardRegistry {
	// 创建一个新的 Guard 注册表
	g := NewGuardRegistry()

	// 注册写操作 Guard：保护关键路径不被随意写入
	g.RegisterWriteGuard(protectedPathGuard{})
	// 注册写操作 Guard：防止误写邮箱相关文件
	g.RegisterWriteGuard(mailboxFileGuard{})
	// 注册写操作 Guard：防止误写文件助手脚本
	g.RegisterWriteGuard(fileHelperScriptGuard{})
	// 注册写操作 Guard：防止误写邮箱 Go 程序文件
	g.RegisterWriteGuard(mailboxGoProgramGuard{})
	// 注册写操作 Guard：检查路径中是否包含空白字符，避免命令解析问题
	g.RegisterWriteGuard(pathWhitespaceGuard{})

	// 注册命令 Guard：拦截可能长期运行的服务端命令
	g.RegisterCommandGuard(longRunningServerGuard{})
	// 注册命令 Guard：根据沙箱规则动态判断命令是否被禁止
	g.RegisterCommandGuard(&funcCommandGuard{
		name: "sandbox-block",
		check: func(cmd string) (blocked bool, reason string) {
			// 调用 Executor 的沙箱规则检查该命令
			pattern, blocked := e.isCommandBlocked(cmd)
			// 如果未被拦截，返回空原因
			if !blocked {
				return false, ""
			}
			// 如果被拦截，返回 true 并附带匹配的沙箱规则信息
			return true, "blocked command matches sandbox rule: " + pattern
		},
	})
	return g
}

// ensureGuardDefaults 确保 guards 字段已被初始化。
// 如果当前 guards 为空，则使用默认 Guard 注册表填充。
func (e *Executor) ensureGuardDefaults() {
	// guards 为空时，重新初始化为默认值
	if e.guards == nil {
		e.guards = e.defaultGuardRegistry()
	}
}

// SetAgentConfig 注入 Agent 阈值配置。
// 调用方可以通过该配置控制文件读取字符数、命令输出字节数等限制。
func (e *Executor) SetAgentConfig(cfg *config.AgentConfig) {
	// 直接保存配置引用，后续 agentConfig() 会优先返回该配置
	e.agentCfg = cfg
}

// agentConfig 返回非空的 Agent 阈值配置。
// 如果调用方未通过 SetAgentConfig 注入配置，则返回硬编码的默认值。
func (e *Executor) agentConfig() *config.AgentConfig {
	// 优先使用外部注入的配置
	if e.agentCfg != nil {
		return e.agentCfg
	}
	// 未注入配置时，返回硬编码的默认阈值
	return &config.AgentConfig{
		ReadFileMaxChars:     4000,  // 读取文件时最大字符数限制
		RunCommandMaxOutput:  10000, // 执行命令时最大输出字节数限制
		RunCommandTimeoutSec: 60,    // 执行命令时默认超时秒数
		ToolExecMaxBytes:     300,   // 工具执行结果最大字节数限制
	}
}

// SetCallback 设置工具执行完成后的回调函数。
// 执行结果会通过该回调返回给调用方。
func (e *Executor) SetCallback(cb Callback) {
	// 保存回调函数引用
	e.callback = cb
}

// ensureDefaults 确保沙箱配置和 Guard 注册表都已初始化。
// 通常在执行前调用，以避免空指针或默认配置缺失。
func (e *Executor) ensureDefaults() {
	// 初始化沙箱默认值
	e.ensureSandboxDefaults()
	// 初始化 Guard 默认值
	e.ensureGuardDefaults()
}

// SetGuardRegistry 注入自定义的 Guard 注册表。
// 如果传入 nil，则恢复为 Executor 默认的 Guard 注册表。
func (e *Executor) SetGuardRegistry(g *GuardRegistry) {
	// 传入 nil 表示恢复默认注册表
	if g == nil {
		e.guards = e.defaultGuardRegistry()
		return
	}
	// 否则使用调用方提供的自定义注册表
	e.guards = g
}

// readWorkDir 返回配置的工作目录。
// 如果未配置工作目录，则回退到当前进程的 cwd。
func (e *Executor) readWorkDir() string {
	// 当工作目录为空时，尝试获取当前进程的工作目录作为回退
	if e.workDir == "" {
		if wd, err := os.Getwd(); err == nil {
			return wd
		}
	}
	// 返回已配置的工作目录，即使获取 cwd 失败也能返回原值
	return e.workDir
}

// WorkDir 公开 Executor 的工作目录，供 ReActAgent 在系统提示词中注入环境信息。
// 与 readWorkDir 同语义，但命名公开导出，便于跨包调用。
func (e *Executor) WorkDir() string {
	return e.readWorkDir()
}
