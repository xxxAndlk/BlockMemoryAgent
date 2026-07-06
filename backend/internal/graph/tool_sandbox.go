package graph

// 本文件为 ToolExecutor 提供基础沙箱策略：命令黑名单 + 路径逃逸检测。
// 设计目标：在保持现有工具接口不变的前提下，防止 LLM 被注入后执行任意危险命令
// 或把文件写到工作目录之外。更严格的沙箱（如 chroot、seccomp、容器）需在部署层补齐。

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// SandboxConfig 定义 ToolExecutor 的轻量级沙箱策略。
//
// 所有路径在比较前都会通过 filepath.Clean 规范化；"工作目录"指创建 executor 时传入的
// workDir。会话级临时目录（workDir/.bma/tmp/<sessionID>）始终被允许读写，无需显式配置。
type SandboxConfig struct {
	// BlockedCmds 命令黑名单：RunCommand 的 command 字符串中若出现任一子串（不区分大小写、
	// 按单词边界匹配），则拒绝执行。默认包含删除、格式化、权限提升、网络外发等高危命令。
	BlockedCmds []string

	// AllowedPaths 允许读写的额外路径白名单（绝对路径）。例如当 workDir 之外的固定目录
	// 需要被 Agent 访问时，可在此列出；不存在的路径会被忽略。
	AllowedPaths []string

	// AllowWriteOutsideWorkDir 为 true 时关闭写路径逃逸检测（仅保留命令黑名单）。
	// 用于需要高灵活性的测试或受信任环境，生产环境建议保持 false。
	AllowWriteOutsideWorkDir bool
}

// DefaultSandboxConfig 返回生产环境推荐的默认沙箱配置。
func DefaultSandboxConfig() SandboxConfig {
	return SandboxConfig{
		BlockedCmds: []string{
			"rm", "rmdir", "rd", "del", "erase",       // 删除
			"mv", "move", "ren", "rename",             // 移动/重命名
			"cp", "copy", "xcopy", "robocopy",         // 复制到不可控位置
			"dd", "format", "mkfs", "fdisk", "diskpart", // 磁盘/分区操作
			"shutdown", "reboot", "poweroff", "halt", "init", // 系统控制
			"sudo", "su", "doas",                       // 权限提升
			"chmod", "chown", "chgrp", "setfacl",       // 权限修改
			"curl", "wget", "nc", "netcat", "telnet", "ssh", "scp", "ftp", // 网络外发
			"taskkill", "kill", "killall", "pkill", // 进程终止：Agent 不应杀任意 PID（参见塔防 demo 事故 v2）
		},
		AllowedPaths:             nil,
		AllowWriteOutsideWorkDir: false,
	}
}

// disabledSandboxConfig 返回关闭路径检测但保留命令黑名单的测试配置。
// 仅在显式调用 SetSandboxConfig 时由调用方选择使用。
func disabledSandboxConfig() SandboxConfig {
	cfg := DefaultSandboxConfig()
	cfg.AllowWriteOutsideWorkDir = true
	return cfg
}

// SetSandboxConfig 设置 executor 的沙箱策略；传 nil 时使用默认配置。
// 并发安全：预期初始化阶段调用一次。
func (e *ToolExecutor) SetSandboxConfig(cfg *SandboxConfig) {
	if cfg == nil {
		cfg = &SandboxConfig{}
	}
	e.sandbox = *cfg
	if len(e.sandbox.BlockedCmds) == 0 {
		// 未显式指定命令黑名单时仍启用默认高危命令拦截，避免调用方传空配置等于关闭安全
		e.sandbox.BlockedCmds = DefaultSandboxConfig().BlockedCmds
	}
}

// isCommandBlocked 判断命令字符串是否命中黑名单。
// 按不区分大小写的单词边界匹配，减少误伤（如 "normal" 不会命中 "rm"）。
func (e *ToolExecutor) isCommandBlocked(cmd string) (string, bool) {
	for _, pattern := range e.sandbox.BlockedCmds {
		if pattern == "" {
			continue
		}
		// 构造单词边界正则：pattern 作为字面量，前后为空白或 shell 分隔符
		re, err := regexp.Compile(`(?i)(^|[\s\;\|\&\(\)\<\>])` + regexp.QuoteMeta(pattern) + `([\s\;\|\&\(\)\<\>]|$)`)
		if err != nil {
			continue
		}
		if re.MatchString(cmd) {
			return pattern, true
		}
	}
	return "", false
}

// isPathAllowed 判断给定绝对路径是否落在允许读写的区域内。
// 允许区域包括：workDir、会话临时目录、AllowedPaths 白名单。
func (e *ToolExecutor) isPathAllowed(absPath string) bool {
	absPath = filepath.Clean(absPath)

	// 1. 必须在工作目录内
	workDirClean := filepath.Clean(e.workDir)
	if hasPathPrefix(absPath, workDirClean) {
		return true
	}

	// 2. 或在额外白名单内
	for _, p := range e.sandbox.AllowedPaths {
		if p == "" {
			continue
		}
		allowedClean := filepath.Clean(p)
		// 白名单路径本身或其子目录都允许
		if absPath == allowedClean || hasPathPrefix(absPath, allowedClean) {
			return true
		}
	}

	return false
}

// hasPathPrefix 安全地判断 child 是否以 parent 为前缀，且避免 /foo/bar 匹配 /foo/ba 这种前缀陷阱。
func hasPathPrefix(child, parent string) bool {
	if child == parent {
		return true
	}
	prefix := parent + string(filepath.Separator)
	return strings.HasPrefix(child, prefix)
}

// sanitizeWritePath 检查并返回安全的写路径。
// 当 AllowWriteOutsideWorkDir=false 且路径逃逸到工作目录外时返回错误。
func (e *ToolExecutor) sanitizeWritePath(absPath string) error {
	if e.sandbox.AllowWriteOutsideWorkDir {
		return nil
	}
	if e.isPathAllowed(absPath) {
		return nil
	}
	return fmt.Errorf("path escapes sandbox: %s (allowed base: %s)", absPath, filepath.Clean(e.workDir))
}

// resolvePathWithSandbox 解析路径并做读访问检查（可选，目前仅用于需要显式拦截读路径的场景）。
func (e *ToolExecutor) resolvePathWithSandbox(path string) (string, error) {
	absPath := e.resolvePath(path)
	if e.sandbox.AllowWriteOutsideWorkDir {
		// 写路径逃逸关闭时，读路径也同步放行（保持向后兼容）
		return absPath, nil
	}
	if e.isPathAllowed(absPath) {
		return absPath, nil
	}
	return "", fmt.Errorf("path escapes sandbox: %s (allowed base: %s)", absPath, filepath.Clean(e.workDir))
}

// isPathWithinTempDir 判断路径是否在指定会话的临时目录内。
func (e *ToolExecutor) isPathWithinTempDir(absPath, sessionID string) bool {
	if sessionID == "" {
		return false
	}
	tempDir := e.sessionTempDir(sessionID)
	return hasPathPrefix(filepath.Clean(absPath), filepath.Clean(tempDir))
}

// ensureSandboxDefaults 保证 sandbox 字段非空且有默认黑名单。
// 在 Execute 入口调用，防止通过旧构造函数创建的 executor 未设置沙箱。
func (e *ToolExecutor) ensureSandboxDefaults() {
	if len(e.sandbox.BlockedCmds) == 0 && !e.sandbox.AllowWriteOutsideWorkDir && len(e.sandbox.AllowedPaths) == 0 {
		// 全零值视为未初始化，启用默认沙箱
		e.sandbox = DefaultSandboxConfig()
	}
}


