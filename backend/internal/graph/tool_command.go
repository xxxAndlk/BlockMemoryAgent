package graph

// 本文件承载命令类工具实现：RunCommand + parseMkdirDir。
// 从 tool_executor.go 按工具类别拆出（P0-3）。方法挂在 *ToolExecutor 上，同 package。

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
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

	// 通过 GuardRegistry 统一执行命令前业务策略校验（黑名单 + 长运行服务器拦截）。
	if blocked, reason := e.guards.CheckCommand(cmdStr); blocked {
		return &ToolResult{Tool: "RunCommand", Error: reason}
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
	// 强制上限：LLM 可能传 300s+ 导致 curl/长命令卡死整个 session
	if maxTimeout := time.Duration(e.agentConfig().RunCommandTimeoutSec) * time.Second; timeout > maxTimeout {
		timeout = maxTimeout
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

	// 端口占用检测：Agent 起服务器或绑定端口失败时，输出含典型 port-busy 模式。
	// 注入换端口提示，引导 Agent 改用 8001/8002/... 重试，而非死磕原端口超时。
	// 参见塔防 demo 事故：Agent 跑 http.server 8000 超时后跑 netstat 发现占用，
	// 但 prompt 无指导，仍重试 8000。
	if hint := detectPortConflictHint(cmdStr, output); hint != "" {
		output += "\n[port-conflict]\n" + hint
	}

	// 截断超长输出
	if maxOut := e.agentConfig().RunCommandMaxOutput; len(output) > maxOut {
		output = output[:maxOut] + "\n... (truncated)"
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

// detectPortConflictHint 检测命令输出是否为端口占用错误，返回换端口提示。
// 命中条件：输出含 "Address already in use" / "端口已被占用" / "bind: An attempt..."
// 等典型 socket 绑定失败模式，且命令本身含端口号（如 :8000 / -p 8000 / --port 8000）。
// 提示 Agent 换用 8001-8009 重试，或用 netstat+taskkill 释放端口。
// 参见塔防 demo 事故：Agent http.server 8000 超时后 netstat 发现占用，仍死磕 8000。
func detectPortConflictHint(cmdStr, output string) string {
	if output == "" {
		return ""
	}
	lowerOut := strings.ToLower(output)
	// 端口占用典型错误模式（跨平台/跨语言）
	conflictPatterns := []string{
		"address already in use",          // Python/Node/Go 标准 socket 错误
		"errno -98",                       // Linux EADDRINUSE
		"eaddrinuse",                      // Node.js
		"bind: an attempt was made",       // Windows winsock
		"通常每个套接字地址",                  // Windows 中文
		"端口已被占用",                       // 中文
		"端口被占用",                        // 中文
		"only one usage of each socket",   // Windows winsock 英文
		"no permission to use port",       // 权限不足
	}
	matched := ""
	for _, p := range conflictPatterns {
		if strings.Contains(lowerOut, p) {
			matched = p
			break
		}
	}
	if matched == "" {
		return ""
	}
	// 提取命令中的端口号（如 "8000" / ":8000" / "-p 8000"）
	port := extractPortFromCommand(cmdStr)
	hint := fmt.Sprintf("检测到端口占用错误（模式: %q）", matched)
	if port != "" {
		// 建议换用相邻端口 8001-8009
		hint += fmt.Sprintf("。端口 %s 已被占用，请换用 8001/8002/.../8009 重试，", port)
	} else {
		hint += "。请换用其他端口（如 8001-8009）重试，"
	}
	hint += "或先执行 `netstat -ano | findstr :<port>` 找到占用进程 PID，再 `taskkill /F /PID <pid>` 释放。" +
		"注意：长运行服务器（http.server/flask/node dev server）会被系统拦截，建议改用 `node --check` / `python -m py_compile` 做语法验证。"
	return hint
}

// extractPortFromCommand 从命令字符串提取端口号。
// 支持模式：":8000" / "-p 8000" / "--port 8000" / "--port=8000" / "port=8000" / "http.server 8000" 裸数字。
// 优先级：--port > -p > :port > 裸数字（最后出现的 2-5 位数）。
// 返回端口号字符串；未找到返回空串。
func extractPortFromCommand(cmdStr string) string {
	if cmdStr == "" {
		return ""
	}
	// --port=8000 / --port 8000
	if idx := strings.Index(strings.ToLower(cmdStr), "--port"); idx >= 0 {
		rest := cmdStr[idx+6:]
		rest = strings.TrimLeft(rest, " =")
		num := ""
		for _, r := range rest {
			if r >= '0' && r <= '9' {
				num += string(r)
				if len(num) >= 5 {
					break
				}
			} else {
				if num != "" {
					break
				}
			}
		}
		if len(num) >= 2 {
			return num
		}
	}
	// -p 8000
	if idx := strings.Index(cmdStr, "-p "); idx >= 0 {
		rest := cmdStr[idx+3:]
		num := ""
		for _, r := range rest {
			if r >= '0' && r <= '9' {
				num += string(r)
				if len(num) >= 5 {
					break
				}
			} else {
				if num != "" {
					break
				}
			}
		}
		if len(num) >= 2 {
			return num
		}
	}
	// :8000 (URL 或 host:port)
	idx := strings.Index(cmdStr, ":")
	for idx >= 0 {
		rest := cmdStr[idx+1:]
		num := ""
		for _, r := range rest {
			if r >= '0' && r <= '9' {
				num += string(r)
				if len(num) >= 5 {
					break
				}
			} else {
				if num != "" {
					break
				}
			}
		}
		if len(num) >= 2 && len(num) <= 5 {
			return num
		}
		next := strings.Index(cmdStr[idx+1:], ":")
		if next < 0 {
			break
		}
		idx = idx + 1 + next
	}
	// 裸数字回退：扫描命令中所有 2-5 位数，返回最后一个（通常是端口）
	// 例如 "python -m http.server 8000" → "8000"
	var lastNum string
	var inNum bool
	curNum := ""
	for _, r := range cmdStr {
		if r >= '0' && r <= '9' {
			curNum += string(r)
			inNum = true
			if len(curNum) > 5 {
				curNum = ""
				inNum = false
			}
		} else {
			if inNum && len(curNum) >= 2 {
				lastNum = curNum
			}
			curNum = ""
			inNum = false
		}
	}
	if inNum && len(curNum) >= 2 {
		lastNum = curNum
	}
	return lastNum
}

// killProcessTree 使用系统命令强制结束指定 PID 及其子进程。
func killProcessTree(pid int) {
	if runtime.GOOS == "windows" {
		// /T 结束进程树，/F 强制结束
		if err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run(); err != nil {
			slog.Debug("kill process tree failed", slog.Int("pid", pid), slog.String("error", err.Error()))
		}
	} else {
		if err := exec.Command("kill", "-9", "-"+strconv.Itoa(pid)).Run(); err != nil {
			slog.Debug("kill process tree failed", slog.Int("pid", pid), slog.String("error", err.Error()))
		}
	}
}

// detectLongRunningServer 检测命令是否启动长运行服务器进程。
// 命中返回原因说明，未命中返回空串。
// 防止 Agent 启动 http.server / flask / node dev server 等阻塞主循环的进程。
// 也拦截 Agent 编译并启动自定义服务器的绕过行为（参见塔防 demo 事故 v2）：
// - `go run *.go`（Agent 写 server.go 后用 go run 启动）
// - `start "" *.exe`（Agent 编译 server.exe 后用 start 启动）
// - `*.exe` 直接启动（server.exe / app.exe / main.exe 等服务器类可执行文件）
func detectLongRunningServer(cmdStr string) string {
	if cmdStr == "" {
		return ""
	}
	lower := strings.ToLower(cmdStr)
	patterns := []struct {
		pat  string
		desc string
	}{
		{"python -m http.server", "python http.server"},
		{"python3 -m http.server", "python http.server"},
		{"py -m http.server", "python http.server"},
		{"-m http.server", "http.server"},
		{"http.server ", "http.server"},
		{"flask run", "flask dev server"},
		{"uvicorn ", "uvicorn asgi server"},
		{"gunicorn ", "gunicorn wsgi server"},
		{"npm start", "npm start (dev server)"},
		{"npm run dev", "npm run dev"},
		{"yarn dev", "yarn dev server"},
		{"yarn start", "yarn start server"},
		{"pnpm dev", "pnpm dev server"},
		{"vite ", "vite dev server"},
		{"webpack serve", "webpack dev server"},
		{"ng serve", "angular dev server"},
		{"rails server", "rails server"},
		{"rails s ", "rails server"},
		{"django runserver", "django dev server"},
		{"manage.py runserver", "django dev server"},
		{"node server.js", "node server"},
		{"node app.js", "node app"},
		{"node .", "node app"},
		{"nodemon ", "nodemon watcher"},
		{"pm2 start", "pm2 daemon"},
		{"docker compose up", "docker compose (long running)"},
		{"docker-compose up", "docker compose (long running)"},
		{"tail -f", "tail -f (follows forever)"},
		{"less ", "less pager (interactive)"},
		{"man ", "man pager (interactive)"},
		{"vim ", "vim editor (interactive)"},
		{"nano ", "nano editor (interactive)"},
		{"python -m pygame", "pygame main loop"},
		{"python -m tkinter", "tkinter main loop"},
		{"start http://", "open browser (interactive)"},
		{"open http://", "open browser (interactive)"},
		{"xdg-open http", "open browser (interactive)"},
		// 塔防 demo 事故 v2：Agent 写 server.go 编译 server.exe 绕过 http.server 拦截
		// go run 直接执行 Go 代码（可能长运行），禁止；go build 只编译不运行，允许
		{"go run ", "go run (执行 Go 程序，可能长运行服务器)"},
		// start 命令启动服务器类 exe（Windows）：start "" server.exe / start server.exe
		// 只拦截服务器类名称，避免误伤 start notepad 等无害命令
		{"start \"\" server", "start server*.exe (自定义服务器)"},
		{"start server", "start server*.exe (自定义服务器)"},
		{"start \"\" app", "start app*.exe (可能长运行)"},
		{"start \"\" main", "start main*.exe (可能长运行)"},
		{"start \"\" serve", "start serve*.exe (自定义服务器)"},
		{"start \"\" httpd", "start httpd.exe (apache)"},
		{"start \"\" nginx", "start nginx.exe (nginx)"},
		// 直接执行服务器类 exe（无 start 前缀）
		{"server.exe", "server.exe (自定义服务器)"},
		{"serve.exe", "serve.exe (自定义服务器)"},
		{"httpd.exe", "httpd.exe (apache server)"},
		{"nginx.exe", "nginx.exe (nginx server)"},
	}
	for _, p := range patterns {
		if strings.Contains(lower, p.pat) {
			return p.desc
		}
	}
	return ""
}
