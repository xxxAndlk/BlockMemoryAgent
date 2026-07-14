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
	"strings"
)

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
// 允许区域包括 Executor 的工作目录以及 AllowedPaths 中列出的路径。
func (e *Executor) isPathAllowed(absPath string) bool {
	// 先将路径规范化，消除 .、.. 等相对符号，统一分隔符。
	absPath = filepath.Clean(absPath)
	// 规范化 Executor 的工作目录，用于后续前缀比较。
	workDirClean := filepath.Clean(e.workDir)
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
func hasPathPrefix(child, parent string) bool {
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
func (e *Executor) sanitizeWritePath(absPath string) error {
	// 如果配置允许写到工作目录之外，则直接放行，不再检查路径。
	if e.sandbox.AllowWriteOutsideWorkDir {
		return nil
	}
	// 如果路径在允许区域内，则允许写入。
	if e.isPathAllowed(absPath) {
		return nil
	}
	// 路径超出沙箱，返回包含路径信息的格式化错误。
	return fmt.Errorf("path escapes sandbox: %s (allowed base: %s)", absPath, filepath.Clean(e.workDir))
}

// resolvePathWithSandbox 将相对路径解析为绝对路径，并执行读取访问检查。
// 如果路径超出沙箱范围，则返回错误。
func (e *Executor) resolvePathWithSandbox(path string) (string, error) {
	// 使用 Executor 的内部方法将路径解析为绝对路径。
	absPath := e.resolvePath(path)
	// 如果配置允许写到工作目录之外，则跳过沙箱检查，直接返回绝对路径。
	if e.sandbox.AllowWriteOutsideWorkDir {
		return absPath, nil
	}
	// 如果解析后的路径在允许区域内，则返回该路径。
	if e.isPathAllowed(absPath) {
		return absPath, nil
	}
	// 路径超出沙箱范围，返回空字符串和错误信息。
	return "", fmt.Errorf("path escapes sandbox: %s (allowed base: %s)", absPath, filepath.Clean(e.workDir))
}

// isPathWithinTempDir 判断 absPath 是否位于指定 sessionID 对应的临时目录内。
// 该方法用于判断文件是否属于某个会话的临时工作空间。
func (e *Executor) isPathWithinTempDir(absPath, sessionID string) bool {
	// 如果 sessionID 为空，则不存在对应的临时目录，直接返回 false。
	if sessionID == "" {
		return false
	}
	// 获取该 sessionID 对应的临时目录路径。
	tempDir := e.sessionTempDir(sessionID)
	// 规范化目标路径和临时目录后，使用 hasPathPrefix 判断归属关系。
	return hasPathPrefix(filepath.Clean(absPath), filepath.Clean(tempDir))
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
// 格式为：<workDir>/.bma/tmp/<sessionID>。
func (e *Executor) sessionTempDir(sessionID string) string {
	// 如果 sessionID 为空，则无法构造有效临时目录，返回空字符串。
	if sessionID == "" {
		return ""
	}
	// 使用 filepath.Join 拼接工作目录、.bma、tmp 和 sessionID。
	return filepath.Join(e.workDir, ".bma", "tmp", sessionID)
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

// resolvePath 将路径解析为相对于 Executor 工作目录的绝对路径。
// 如果传入的 path 已经是绝对路径，则原样返回。
func (e *Executor) resolvePath(path string) string {
	// 如果 path 已经是绝对路径，则无需拼接，直接返回。
	if filepath.IsAbs(path) {
		return path
	}
	// 否则将相对路径与工作目录拼接，得到绝对路径。
	return filepath.Join(e.workDir, path)
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
