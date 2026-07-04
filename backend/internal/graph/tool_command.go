package graph

// 本文件承载命令类工具实现：RunCommand + parseMkdirDir。
// 从 tool_executor.go 按工具类别拆出（P0-3）。方法挂在 *ToolExecutor 上，同 package。

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// parseMkdirDir 解析 mkdir / mkdir -p 命令，返回目标目录。
// 不是 mkdir 命令时返回空串。
//
// 职责：跨平台兼容 mkdir 命令，绕过 Windows shell 不支持 -p 的问题。
// 参数：
//   - cmd：原始命令字符串。
//
// 返回：mkdir 的目标目录；非 mkdir 命令返回空串。
// 副作用：无。
// 并发安全：纯函数。
func parseMkdirDir(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	// 支持 "mkdir dir"、"mkdir -p dir"、"mkdir -p a/b/c"
	if !strings.HasPrefix(cmd, "mkdir") {
		return ""
	}
	// 去掉 "mkdir" 前缀
	rest := strings.TrimSpace(strings.TrimPrefix(cmd, "mkdir"))
	// 去掉可选的 "-p" 标志
	rest = strings.TrimSpace(strings.TrimPrefix(rest, "-p"))
	return rest
}

// runCommand 执行命令。
//
// 职责：在 workDir 下执行 shell 命令（Windows 用 cmd /c，Unix 用 sh -c），
//
//	捕获 stdout/stderr，超时控制，输出截断。mkdir 命令走跨平台 fast path。
//
// 参数：
//   - ctx：用于超时控制；同时从中取 sessionID，注入 BMA_SESSION_TEMP_DIR 环境变量。
//   - args：含 "command" 字段，可选 "timeout"（秒，上限 60）。
//
// 返回：成功时 Output 含 stdout+stderr；失败时 Error 为错误信息。
// 副作用：执行任意 shell 命令（沙箱取决于 workDir 隔离程度）。
func (e *ToolExecutor) runCommand(ctx context.Context, args map[string]any) *ToolResult {
	// 取命令字符串
	cmdStr, _ := args["command"].(string)
	if cmdStr == "" {
		return &ToolResult{Tool: "RunCommand", Error: "command is required"}
	}

	// 命令黑名单检测
	if pattern, blocked := e.isCommandBlocked(cmdStr); blocked {
		return &ToolResult{Tool: "RunCommand", Error: fmt.Sprintf("blocked command matches sandbox rule: %s", pattern)}
	}

	// 跨平台 mkdir：直接用 os.MkdirAll，绕过 shell 差异（Windows mkdir 不支持 -p）
	if dir := parseMkdirDir(cmdStr); dir != "" {
		absDir := e.resolvePath(dir)
		if err := e.sanitizeWritePath(absDir); err != nil {
			return &ToolResult{Tool: "RunCommand", Error: err.Error()}
		}
		if err := os.MkdirAll(absDir, 0755); err != nil {
			return &ToolResult{Tool: "RunCommand", Error: "mkdir: " + err.Error()}
		}
		return &ToolResult{Tool: "RunCommand", Success: true, Output: "created: " + absDir}
	}

	// 计算超时：默认 e.timeout，入参可覆盖
	timeout := e.timeout
	if t, ok := args["timeout"].(float64); ok && t > 0 {
		timeout = time.Duration(t) * time.Second
	}
	// 强制上限 60s：LLM 可能传 300s+ 导致 curl/长命令卡死整个 session
	if timeout > 60*time.Second {
		timeout = 60 * time.Second
	}

	// 派生带超时的 ctx
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 按平台选择 shell：Windows 用 cmd，Unix 用 sh
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/c", cmdStr)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", cmdStr)
	}
	cmd.Dir = e.workDir // 工作目录设为沙箱目录

	// 注入会话级临时目录环境变量，方便命令将临时产物写到统一位置，
	// 会话结束后由 SessionManager 统一清理。
	if sessionID := SessionIDFromContext(ctx); sessionID != "" {
		cmd.Env = append(os.Environ(), "BMA_SESSION_TEMP_DIR="+e.sessionTempDir(sessionID))
	}

	// 捕获 stdout 与 stderr
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// 执行命令：Windows 下额外监听 ctx 取消，确保杀死整棵进程树，
	// 避免 cmd 被 kill 后 python/gui 子进程仍然挂起导致超时形同虚设。
	err := runCommandWithTreeKill(ctx, cmd)

	// 合并 stdout 与 stderr，Windows cmd 默认 CP936 输出需转 UTF-8
	output := SanitizeBytes(stdout.Bytes())
	if stderr.Len() > 0 {
		output += "\n[stderr]\n" + SanitizeBytes(stderr.Bytes())
	}

	// 截断超长输出
	if len(output) > 10000 {
		output = output[:10000] + "\n... (truncated)"
	}

	// 组装结果，成功与否由 err 决定
	result := &ToolResult{
		Tool:    "RunCommand",
		Path:    e.workDir,
		Output:  output,
		Success: err == nil,
	}

	if err != nil {
		result.Error = err.Error()
	}

	return result
}

// runCommandWithTreeKill 执行命令并在 ctx 取消时尝试杀死整棵进程树。
// Windows 下 Go 的 CommandContext 只杀直接子进程，对 cmd /c 拉起的 python/gui 子进程
// 无效，因此需要手动 taskkill /T /F。
func runCommandWithTreeKill(ctx context.Context, cmd *exec.Cmd) error {
	if runtime.GOOS != "windows" {
		return cmd.Run()
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case <-ctx.Done():
		if cmd.Process != nil {
			killProcessTree(cmd.Process.Pid)
		}
		// 等待 Wait 返回，避免僵尸进程；返回 ctx 错误以便上层识别超时
		<-done
		return ctx.Err()
	case err := <-done:
		return err
	}
}

// killProcessTree 使用系统命令强制结束指定 PID 及其子进程。
func killProcessTree(pid int) {
	if runtime.GOOS == "windows" {
		// /T 结束进程树，/F 强制结束
		_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run()
	} else {
		_ = exec.Command("kill", "-9", "-"+strconv.Itoa(pid)).Run()
	}
}
