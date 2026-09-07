package tool

// 导入所需标准库：context 用于在上下文中传递 SessionID；fmt 用于格式化错误信息；
// os 用于获取当前工作目录；path/filepath 用于路径处理；regexp 用于命令黑名单正则匹配；
// strings 用于字符串前缀判断与空白处理。
import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// isWindows 标记当前是否运行在 Windows 平台，用于路径大小写归一化决策。
var isWindows = runtime.GOOS == "windows"

// SandboxConfig 定义了 Executor 使用的轻量级沙箱策略。
// 该结构体控制哪些命令被禁止执行，以及允许写入哪些路径。
type SandboxConfig struct {
	// BlockedCmds 是被屏蔽的命令关键字列表。
	// 执行器在运行命令前会检查命令字符串中是否包含这些关键字。
	BlockedCmds []string
	// AllowedPaths 是允许访问的额外绝对路径列表。
	// 除了工作目录之外，这些路径下的读写操作也会被允许。
	AllowedPaths []string
	// AllowWriteOutsideWorkDir 控制是否允许在工作目录之外写入。
	// 如果为 true，则跳过路径沙箱检查；命令黑名单仍然生效。
	AllowWriteOutsideWorkDir bool
}

// DefaultSandboxConfig 返回推荐的生产环境沙箱配置。
// 默认配置禁用了大量危险的系统命令，并限制写入只能在工作目录内进行。
func DefaultSandboxConfig() SandboxConfig {
	// 返回一个包含默认黑名单和空允许路径列表的沙箱配置。
	return SandboxConfig{
		// BlockedCmds 列出默认被屏蔽的高危命令。
		// 这些命令可能破坏文件系统、影响系统运行或绕过安全限制。
		BlockedCmds: []string{
			"rm", "rmdir", "rd", "del", "erase", // 删除类命令
			"mv", "move", "ren", "rename", // 移动/重命名类命令
			"cp", "copy", "xcopy", "robocopy", // 复制类命令
			"dd", "format", "mkfs", "fdisk", "diskpart", // 磁盘操作类命令
			"shutdown", "reboot", "poweroff", "halt", "init", // 系统电源类命令
			"sudo", "su", "doas", // 权限提升类命令
			"chmod", "chown", "chgrp", "setfacl", // 权限修改类命令
			"curl", "wget", "nc", "netcat", "telnet", "ssh", "scp", "ftp", // 网络传输/远程访问类命令
			"taskkill", "kill", "killall", "pkill", // 进程终止类命令
		},
		// AllowedPaths 默认不额外开放任何路径。
		AllowedPaths: nil,
		// AllowWriteOutsideWorkDir 默认不允许写到工作目录之外。
		AllowWriteOutsideWorkDir: false,
	}
}

// disabledSandboxConfig 返回一个沙箱配置：保留命令黑名单，但关闭路径检查。
// 该方法主要用于需要临时放宽路径限制的内部场景。
func disabledSandboxConfig() SandboxConfig {
	// 先复制一份默认配置，避免直接修改全局默认值。
	cfg := DefaultSandboxConfig()
	// 将 AllowWriteOutsideWorkDir 设为 true，禁用路径沙箱限制。
	cfg.AllowWriteOutsideWorkDir = true
	// 返回修改后的配置副本。
	return cfg
}

// SetSandboxConfig 为当前 Executor 设置沙箱策略。
// 如果传入 nil，则重置为一个空的 SandboxConfig，并补齐默认命令黑名单。
func (e *Executor) SetSandboxConfig(cfg *SandboxConfig) {
	// 如果调用方传入 nil，则创建一个新的空配置。
	// 这样后续对 cfg 的解引用不会触发空指针异常。
	if cfg == nil {
		cfg = &SandboxConfig{}
	}
	// 将传入（或新建）的配置值拷贝到 Executor 的沙箱字段中。
	e.sandbox = *cfg
	// 如果配置中没有指定任何被禁命令，则自动填充默认黑名单。
	// 这保证 Executor 始终至少具备基础的安全防护。
	if len(e.sandbox.BlockedCmds) == 0 {
		e.sandbox.BlockedCmds = DefaultSandboxConfig().BlockedCmds
	}
}

// SetRoleWritePathResolver 注入角色级写路径解析器。
// 解析器按 roleID 返回该角色的 Sandbox.AllowedWritePaths；返 nil/空切片表示该角色不限制。
// 由 bootstrap 注入（闭包查 roleRegistry.Get(roleID).Sandbox）。nil 解析器=全局不限制（当前行为）。
func (e *Executor) SetRoleWritePathResolver(fn func(roleID string) []string) {
	e.roleWritePaths = fn
}

// isCommandBlocked 检查给定的命令字符串是否命中黑名单。
// 如果命中，返回匹配到的关键字以及 true；否则返回空字符串和 false。
func (e *Executor) isCommandBlocked(cmd string) (string, bool) {
	// 遍历所有被禁命令关键字。
	for _, pattern := range e.sandbox.BlockedCmds {
		// 跳过空字符串，避免构建出无意义的正则表达式。
		if pattern == "" {
			continue
		}
		// 构建正则表达式：在命令分隔符（空白、分号、管道、与或、重定向、括号）包围下匹配关键字。
		// (?i) 表示忽略大小写；regexp.QuoteMeta 对关键字中的特殊字符进行转义。
		re, err := regexp.Compile(`(?i)(^|[\s\;\|\&\(\)\<\>])` + regexp.QuoteMeta(pattern) + `([\s\;\|\&\(\)\<\>]|$)`)
		// 如果正则编译失败，跳过该模式，继续检查下一个。
		if err != nil {
			continue
		}
		// 如果命令字符串匹配该正则，说明包含被禁关键字。
		if re.MatchString(cmd) {
			// 返回命中的关键字和 true。
			return pattern, true
		}
	}
	// 全部模式均未命中，返回空字符串和 false。
	return "", false
}

// isPathAllowed 判断给定的绝对路径是否位于允许访问的区域内。
// 允许区域包括会话工作目录（ctx 注入值优先，见 workDirOf）以及 AllowedPaths 中列出的路径。
func (e *Executor) isPathAllowed(ctx context.Context, absPath string) bool {
	// 先将路径规范化，消除 .、.. 等相对符号，统一分隔符。
	absPath = filepath.Clean(absPath)
	// 规范化当前会话的工作目录，用于后续前缀比较。
	workDirClean := filepath.Clean(e.workDirOf(ctx))
	// 如果目标路径在工作目录下（或就是工作目录本身），则允许访问。
	if hasPathPrefix(absPath, workDirClean) {
		return true
	}
	// 遍历用户额外配置的允许路径列表。
	for _, p := range e.sandbox.AllowedPaths {
		// 跳过空字符串，避免误判。
		if p == "" {
			continue
		}
		// 规范化配置的允许路径。
		allowedClean := filepath.Clean(p)
		// Windows 下大小写不敏感，统一转小写比较；非 Windows 保持原样。
		if isWindows {
			if strings.EqualFold(absPath, allowedClean) || hasPathPrefix(absPath, allowedClean) {
				return true
			}
			continue
		}
		// 如果目标路径等于允许路径，或位于允许路径之下，则允许访问。
		if absPath == allowedClean || hasPathPrefix(absPath, allowedClean) {
			return true
		}
	}
	// 不在任何允许区域内，返回 false。
	return false
}

// hasPathPrefix 安全地判断 child 是否位于 parent 目录下。
// 该方法通过显式追加路径分隔符来避免类似 /foo 与 /foobar 的误判。
// Windows 文件系统大小写不敏感，需归一化后再比较；否则 d:\... 与 D:\... 会被误判为越界。
func hasPathPrefix(child, parent string) bool {
	// Windows 下统一转小写做比较；非 Windows 保持原样。
	if isWindows {
		child = strings.ToLower(child)
		parent = strings.ToLower(parent)
	}
	// 如果两个路径完全相同，也视为在范围内。
	if child == parent {
		return true
	}
	// 构造父目录前缀，例如 /foo/，以确保子路径必须是该目录下的真正子项。
	prefix := parent + string(filepath.Separator)
	// 使用 strings.HasPrefix 检查 child 是否以该前缀开头。
	return strings.HasPrefix(child, prefix)
}

// sanitizeWritePath 检查并返回一个安全的写入路径。
// 如果路径超出沙箱范围，则返回错误。
func (e *Executor) sanitizeWritePath(ctx context.Context, absPath string) error {
	// 如果配置允许写到工作目录之外，则直接放行，不再检查路径。
	if e.sandbox.AllowWriteOutsideWorkDir {
		return nil
	}
	// 如果路径在允许区域内，则允许写入。
	if e.isPathAllowed(ctx, absPath) {
		return nil
	}
	// 路径超出沙箱，返回包含路径信息的格式化错误。
	return fmt.Errorf("path escapes sandbox: %s (allowed base: %s)", absPath, filepath.Clean(e.workDirOf(ctx)))
}

// enforceRoleWritePath 角色级写沙箱：当 ctx 携带 roleID 且解析器为该角色返回非空
// AllowedWritePaths 时，要求 absPath 必须落在其中一条路径（相对 workDir 解析）下。
// roleID 为空（未注入）、解析器为 nil、或角色无 Sandbox 配置（返空）时返回 nil，等价于不限制。
// 这是 Layer 4 的 dormant opt-in：未配置 allowed_write_paths 的角色完全跳过，现有行为不变。
func (e *Executor) enforceRoleWritePath(ctx context.Context, absPath string) error {
	if e.roleWritePaths == nil {
		return nil
	}
	roleID := RoleIDFromContext(ctx)
	if roleID == "" {
		return nil
	}
	paths := e.roleWritePaths(roleID)
	if len(paths) == 0 {
		return nil
	}
	absPath = filepath.Clean(absPath)
	for _, p := range paths {
		if p == "" {
			continue
		}
		// 相对路径以会话工作目录为基解析；绝对路径原样使用。
		resolved := p
		if !filepath.IsAbs(p) {
			resolved = filepath.Join(e.workDirOf(ctx), p)
		}
		if hasPathPrefix(absPath, filepath.Clean(resolved)) {
			return nil
		}
	}
	return fmt.Errorf("role %s not allowed to write: %s (allowed paths: %v)", roleID, absPath, paths)
}

// resolvePathWithSandbox 将相对路径解析为绝对路径，并执行读取访问检查。
// 如果路径超出沙箱范围，则返回错误。
func (e *Executor) resolvePathWithSandbox(ctx context.Context, path string) (string, error) {
	// 使用 Executor 的内部方法将路径解析为绝对路径。
	absPath := e.resolvePath(ctx, path)
	// 如果配置允许写到工作目录之外，则跳过沙箱检查，直接返回绝对路径。
	if e.sandbox.AllowWriteOutsideWorkDir {
		return absPath, nil
	}
	// 如果解析后的路径在允许区域内，则返回该路径。
	if e.isPathAllowed(ctx, absPath) {
		return absPath, nil
	}
	// 路径超出沙箱范围，返回空字符串和错误信息。
	return "", fmt.Errorf("path escapes sandbox: %s (allowed base: %s)", absPath, filepath.Clean(e.workDirOf(ctx)))
}

// ensureSandboxDefaults 确保 Executor 的沙箱字段已被初始化。
// 如果当前沙箱为空配置，则替换为默认配置。
func (e *Executor) ensureSandboxDefaults() {
	// 判断沙箱是否处于完全未初始化状态：没有黑名单、没有关闭路径限制、也没有额外允许路径。
	if len(e.sandbox.BlockedCmds) == 0 && !e.sandbox.AllowWriteOutsideWorkDir && len(e.sandbox.AllowedPaths) == 0 {
		// 将沙箱设置为默认配置，以提供基础保护。
		e.sandbox = DefaultSandboxConfig()
	}
}

// sessionTempDir 返回某个 session 对应的临时目录路径。
// 格式为：<会话工作目录>/.bma/tmp/<sessionID>。
func (e *Executor) sessionTempDir(ctx context.Context, sessionID string) string {
	// 如果 sessionID 为空，则无法构造有效临时目录，返回空字符串。
	if sessionID == "" {
		return ""
	}
	// 使用 filepath.Join 拼接工作目录、.bma、tmp 和 sessionID。
	return filepath.Join(e.workDirOf(ctx), ".bma", "tmp", sessionID)
}

// sessionIDKey 是用于在 context 中携带 session ID 的私有键类型。
// 使用私有结构体类型可以防止外部包意外覆盖或读取该值。
type sessionIDKey struct{}

// WithSessionID 返回一个携带 session ID 的新 context。
// 调用方可以通过返回的 context 在调用链中传递会话标识。
func WithSessionID(ctx context.Context, sessionID string) context.Context {
	// 将 sessionID 与私有键 sessionIDKey{} 关联后存入 context。
	return context.WithValue(ctx, sessionIDKey{}, sessionID)
}

// SessionIDFromContext 从 context 中读取 session ID。
// 如果 context 中没有设置或类型不匹配，则返回空字符串。
func SessionIDFromContext(ctx context.Context) string {
	// 使用私有键从 context 中取值，并断言为 string 类型。
	if v, ok := ctx.Value(sessionIDKey{}).(string); ok {
		// 类型断言成功，返回 session ID。
		return v
	}
	// 未找到或类型不匹配，返回空字符串。
	return ""
}

// stopCtxKey 用于在 runCtx 中携带会话级 stopCtx（TODO 第10项④ 中断传播基底）：
// 会话 Stop 时 cancel 的 context，dispatchOne/ResumePaused/runDomainTask 派生子 Agent
// ctx 以它为基底（缺省回退 Background，测试/旧路径兼容），stop 窗口期新派发与深层
// 孙代即刻随会话终止。值为 context.Context。
type stopCtxKey struct{}

// WithStopContext 把会话级 stopCtx 注入 runCtx，供子派发侧取基底。
func WithStopContext(ctx context.Context, stopCtx context.Context) context.Context {
	if stopCtx == nil {
		return ctx
	}
	return context.WithValue(ctx, stopCtxKey{}, stopCtx)
}

// StopContextFrom 取出会话级 stopCtx；未注入返回 nil（派发侧回退 context.Background()）。
func StopContextFrom(ctx context.Context) context.Context {
	if v, ok := ctx.Value(stopCtxKey{}).(context.Context); ok {
		return v
	}
	return nil
}

// 信任模式常量（TODO 第10⑥ 三级信任，对标 Codex）：
//   - suggest：全部变更类动作（写文件/命令/动态破坏性工具）逐条审批，读类直通；
//   - auto-edit：文件编辑直通，命令与动态破坏性工具审批；
//   - full-auto：全自主，不再推「需确认」（现状默认）。
const (
	TrustModeSuggest  = "suggest"
	TrustModeAutoEdit = "auto-edit"
	TrustModeFullAuto = "full-auto"
)

// ValidTrustMode 校验信任模式枚举值。
func ValidTrustMode(mode string) bool {
	switch mode {
	case TrustModeSuggest, TrustModeAutoEdit, TrustModeFullAuto:
		return true
	}
	return false
}

// trustModeFnKey 携带信任模式读取器（返回当前会话模式），供 needsApproval 每次
// 工具调用实时读取——会话中途切换模式下一工具调用即生效（TODO 第10⑥）。
type trustModeFnKey struct{}

// WithTrustModeFunc 注入信任模式读取器。fn 返回空串表示未设置（回退全局配置语义）。
func WithTrustModeFunc(ctx context.Context, fn func() string) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, trustModeFnKey{}, fn)
}

// TrustModeFuncFrom 取出信任模式读取器；未注入返回 nil。
func TrustModeFuncFrom(ctx context.Context) func() string {
	if fn, ok := ctx.Value(trustModeFnKey{}).(func() string); ok {
		return fn
	}
	return nil
}

// TrustModeOf 便捷读取当前信任模式：先取读取器，无读取器或返回空串时回退默认 full-auto。
func TrustModeOf(ctx context.Context) string {
	if fn := TrustModeFuncFrom(ctx); fn != nil {
		if m := fn(); m != "" {
			return m
		}
	}
	return TrustModeFullAuto
}

// roleIDKey 用于在 context 中携带当前 Agent 的角色 ID，供角色级写沙箱校验读取。
type roleIDKey struct{}

// WithRoleID 返回一个携带角色 ID 的新 context。
// 由 ReActAgent.Run 注入（a.role.ID），经 Dispatch 流到 WriteFile 的 enforceRoleWritePath。
func WithRoleID(ctx context.Context, roleID string) context.Context {
	return context.WithValue(ctx, roleIDKey{}, roleID)
}

// RoleIDFromContext 从 ctx 中取出角色 ID；未设置时返回空串（如顶层 MetaAgent 未注入则跳过角色级校验）。
func RoleIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(roleIDKey{}).(string); ok {
		return v
	}
	return ""
}

// workDirKey 是会话级工作目录的 context 键（每会话独立工作目录，S2）。
type workDirKey struct{}

// WithWorkDir 把会话工作目录注入 ctx；空 dir 原样返回（回落 Executor 默认目录）。
func WithWorkDir(ctx context.Context, dir string) context.Context {
	if dir == "" {
		return ctx
	}
	return context.WithValue(ctx, workDirKey{}, dir)
}

// WorkDirFromContext 取出会话工作目录，未注入返回空串。
func WorkDirFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(workDirKey{}).(string); ok {
		return v
	}
	return ""
}

// resolvePath 将路径解析为相对于会话工作目录（ctx 注入值优先，见 workDirOf）的绝对路径。
// 如果传入的 path 已经是绝对路径，则原样返回。
func (e *Executor) resolvePath(ctx context.Context, path string) string {
	// 如果 path 已经是绝对路径，则无需拼接，直接返回。
	if filepath.IsAbs(path) {
		return path
	}
	// 否则将相对路径与工作目录拼接，得到绝对路径。
	return filepath.Join(e.workDirOf(ctx), path)
}

// fallbackWorkDir 在 Executor 的 workDir 为空时返回进程当前工作目录。
// 如果获取失败，则返回空字符串。
func fallbackWorkDir() string {
	// 获取当前进程的工作目录。
	if wd, err := os.Getwd(); err == nil {
		// 获取成功，返回该目录。
		return wd
	}
	// 获取失败，返回空字符串作为兜底。
	return ""
}
