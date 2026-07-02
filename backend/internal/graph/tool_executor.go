package graph

import (
	"bytes"         // bytes.Buffer 用于捕获命令 stdout/stderr
	"context"       // 上下文与超时控制
	"encoding/json" // HTTPPost body 序列化
	"fmt"           // 格式化输出与错误信息
	"io"            // HTTP 响应体读取与 LimitReader
	"net/http"      // HTTP 工具实现
	"os"            // 文件读写与目录操作
	"os/exec"       // 子进程执行
	"path/filepath" // 路径解析、WalkDir、扩展名提取
	"runtime"       // GOOS 判断，区分 Windows/Unix 命令
	"slices"        // slices.Contains 判断扩展名白名单
	"strings"       // 字符串切分、前缀处理、大小写转换
	"time"          // 超时时长
)

// ToolResult 工具执行结果。
//
// 职责：统一描述一次工具调用的产出，供 blades Agent 回灌给 LLM、供 UI 展示、
//   供上层完成门控判断。
// 字段：
//   - Tool：工具名（已归一化为 CamelCase）。
//   - Success：是否执行成功。
//   - Output：截断后的标准输出 / 文件内容 / 命令输出。
//   - Error：失败时的错误信息（成功时为空）。
//   - Path：相关路径（文件路径 / URL / 工作目录），便于审计。
//   - SessionID：归属会话，避免跨会话事件泄漏。
type ToolResult struct {
	Tool      string `json:"tool"`
	Success   bool   `json:"success"`
	Output    string `json:"output"`
	Error     string `json:"error,omitempty"`
	Path      string `json:"path,omitempty"`
	ArgsJSON  string `json:"args_json,omitempty"`  // 入参 JSON 摘要（截断），用于日志展示
	SessionID string `json:"session_id,omitempty"` // 归属会话，避免跨会话事件泄漏
}

// ToolCallback 工具执行回调（用于通知UI）。
//
// 职责：每次工具执行完成后被调用，把结果推给订阅方（如 TUI 广播器）。
// 设计意图：解耦 ToolExecutor 与具体 UI 实现，executor 不关心谁在听。
type ToolCallback func(result *ToolResult)

// ProgressEvent 单步进度事件，用于把 Agent 的思考/意图/工具调用实时推给 UI。
// Kind 取值:
//   "think" | "intend" | "tool_call" | "tool_result" | "llm" | "wait" | "error"  (原有)
//   "prompt" | "agent_created" | "token_usage" | "graph_step"                     (新增调试类)
//
// 职责：承载一个 Agent 执行步骤的可观测信息，由 ProgressCallback 推送给 UI。
// 字段：
//   - SessionID：归属会话，避免跨会话事件泄漏。
//   - Kind：事件类型枚举（见上方注释）。
//   - Agent：节点名/角色名，用于事件归属。
//   - Message：人类可读描述。
//   - Detail：可选详情（LLM 原始输出 / 工具参数 / 错误堆栈 / prompt 摘要 / JSON 详情）。
type ProgressEvent struct {
	SessionID string // 归属会话，避免跨会话事件泄漏
	Kind      string // "think" | "intend" | "tool_call" | "tool_result" | "llm" | "llm_result" | "wait" | "error" | "prompt" | "agent_created" | "token_usage" | "graph_step"
	Agent     string // 节点名/角色名
	Tool      string // 工具名（tool_call 时携带，供前端直接展示，免正则推断）
	Message   string // 人类可读描述
	Detail    string // 可选：LLM 原始输出 / 工具参数 / 错误堆栈 / prompt 摘要 / JSON 详情
}

// ProgressCallback 进度回调。server 层注入，graph 各节点在每个关键步骤触发。
// 携带 context 以便下游按 sessionID 路由事件。
//
// 职责：把 Agent 执行过程中的关键事件转发给 UI（SSE / TUI 广播）。
// 设计意图：让 graph 节点不直接依赖 server 包，通过回调解耦。
type ProgressCallback func(ctx context.Context, ev ProgressEvent)

// ToolExecutor 本地工具执行器（沙箱）。
//
// 职责：在受限工作目录下执行 7 个内置工具，统一封装路径解析、超时控制、
//   输出截断与回调通知。
// 字段：
//   - workDir：工具执行的基准目录，相对路径基于此解析。
//   - timeout：默认超时（命令/HTTP），可被入参覆盖（上限 60s）。
//   - callback：可选回调，每次 Execute 后触发。
// 并发安全：workDir/timeout/callback 在 SetCallback 后不再变化；
//   Execute 可被多 goroutine 并发调用（无共享可变状态）。
type ToolExecutor struct {
	workDir  string        // 工具执行基准目录
	timeout  time.Duration // 默认超时
	callback ToolCallback  // 工具执行回调
}

// NewToolExecutor 创建工具执行器。
//
// 参数：
//   - workDir：基准工作目录；空串时回退到当前进程工作目录。
// 返回：初始化好的 *ToolExecutor，默认超时 30s，callback 为 nil。
// 副作用：workDir 为空时调用 os.Getwd()。
func NewToolExecutor(workDir string) *ToolExecutor {
	// workDir 为空时回退到进程当前目录，避免相对路径解析失败
	if workDir == "" {
		workDir, _ = os.Getwd()
	}
	return &ToolExecutor{
		workDir: workDir,
		timeout: 30 * time.Second, // 默认 30s 超时
	}
}

// SetCallback 设置工具执行回调。
//
// 参数：
//   - cb：每次 Execute 完成后调用的回调；传 nil 可清除。
// 副作用：覆盖既有 callback。
// 并发安全：非并发安全，预期在初始化阶段调用一次。
func (e *ToolExecutor) SetCallback(cb ToolCallback) {
	e.callback = cb
}

// Execute 执行工具调用。
//
// 职责：工具调用的统一入口，按工具名分发到对应实现，注入 sessionID，触发回调。
// 参数：
//   - ctx：请求上下文，携带 sessionID 与超时。
//   - toolName：工具名（支持 snake_case 别名，会自动归一化）。
//   - args：工具入参 map。
// 返回：填充好的 *ToolResult（始终非 nil，失败也通过 result.Error 表达）。
// 副作用：通过具体工具实现产生文件/命令/网络副作用；通过 callback 通知订阅方。
// 并发安全：可被多 goroutine 并发调用。
func (e *ToolExecutor) Execute(ctx context.Context, toolName string, args map[string]any) *ToolResult {
	// 兼容 snake_case 工具名（LLM 可能输出 skill_id 而非 ToolRef）。
	// 例如 write_file -> WriteFile。
	toolName = normalizeToolName(toolName)

	var result *ToolResult
	// 从 ctx 取 sessionID，后续填入 result 用于事件归属
	sessionID := SessionIDFromContext(ctx)
	// 按工具名分发到具体实现
	switch toolName {
	case "ReadFile":
		result = e.readFile(args)
	case "WriteFile":
		result = e.writeFile(args)
	case "ListDir":
		result = e.listDir(args)
	case "RunCommand":
		result = e.runCommand(ctx, args) // 命令需要 ctx 做超时控制
	case "SearchInFiles":
		result = e.searchInFiles(args)
	case "HTTPGet":
		result = e.httpGet(ctx, args) // HTTP 需要 ctx 做超时控制
	case "HTTPPost":
		result = e.httpPost(ctx, args)
	default:
		// 未知工具：返回带错误的空结果，不 panic
		result = &ToolResult{Tool: toolName, Error: fmt.Sprintf("unknown tool: %s", toolName)}
	}
	// 统一填入 sessionID，便于上层按会话过滤事件
	result.SessionID = sessionID
	// 统一填入入参 JSON 摘要（截断 300 字），便于日志展示工具调用上下文
	if argsJSON, mErr := json.Marshal(args); mErr == nil {
		argsStr := string(argsJSON)
		if len(argsStr) > 300 {
			argsStr = argsStr[:300] + "...(truncated)"
		}
		result.ArgsJSON = argsStr
	}
	// 回调通知（如 TUI 广播器）
	if e.callback != nil {
		e.callback(result)
	}
	return result
}

// normalizeToolName 把 snake_case 工具名转为 CamelCase。
// 已是 CamelCase 的原样返回。
//
// 职责：兼容 LLM 输出的 snake_case 工具名（如 write_file），统一映射到内部 CamelCase。
// 参数：
//   - name：原始工具名。
// 返回：归一化后的工具名；未命中别名时原样返回。
// 副作用：无。
// 并发安全：纯函数（每次构建 map，无共享状态）。
//
// parseMkdirDir 解析 mkdir / mkdir -p 命令，返回目标目录。
// 不是 mkdir 命令时返回空串。
//
// 职责：跨平台兼容 mkdir 命令，绕过 Windows shell 不支持 -p 的问题。
// 参数：
//   - cmd：原始命令字符串。
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

// normalizeToolName 工具名归一化（snake_case → CamelCase）。
//
// 参数：
//   - name：原始工具名。
// 返回：归一化后的工具名；未命中别名时原样返回。
func normalizeToolName(name string) string {
	// 别名表：LLM 偶尔输出 snake_case，这里统一翻译回 CamelCase
	aliases := map[string]string{
		"read_file":       "ReadFile",
		"write_file":      "WriteFile",
		"list_dir":        "ListDir",
		"run_command":     "RunCommand",
		"search_in_files": "SearchInFiles",
		"http_get":        "HTTPGet",
		"http_post":       "HTTPPost",
	}
	if v, ok := aliases[name]; ok {
		return v
	}
	return name // 已是 CamelCase 或未知名，原样返回
}

// readFile 读取文件内容。
//
// 职责：读取指定路径文件，超过 10000 字符时截断，返回内容与绝对路径。
// 参数：
//   - args：必须含 "path" 字段。
// 返回：成功时 Output 为文件内容；失败时 Error 为错误信息。
// 副作用：只读，无写入。
func (e *ToolExecutor) readFile(args map[string]any) *ToolResult {
	// 取 path 参数，类型必须为 string 且非空
	path, ok := args["path"].(string)
	if !ok || path == "" {
		return &ToolResult{Tool: "ReadFile", Error: "path is required"}
	}

	// 解析为绝对路径（相对 workDir）
	absPath := e.resolvePath(path)
	// 读文件
	data, err := os.ReadFile(absPath)
	if err != nil {
		return &ToolResult{Tool: "ReadFile", Path: absPath, Error: err.Error()}
	}

	// 转 string 并截断超长内容，避免回灌 LLM 时上下文爆炸
	content := string(data)
	if len(content) > 10000 {
		content = content[:10000] + "\n... (truncated)"
	}

	return &ToolResult{Tool: "ReadFile", Success: true, Output: content, Path: absPath}
}

// writeFile 写入文件。
//
// 职责：把 content 写入指定路径，自动创建父目录。
// 参数：
//   - args：含 "path" 与 "content" 字段。
// 返回：成功时 Output 为写入字节数；失败时 Error 为错误信息。
// 副作用：创建目录 + 写文件（覆盖已有内容）。
func (e *ToolExecutor) writeFile(args map[string]any) *ToolResult {
	// 取 path 与 content，类型断言失败时取零值
	path, _ := args["path"].(string)
	content, _ := args["content"].(string)

	if path == "" {
		return &ToolResult{Tool: "WriteFile", Error: "path is required"}
	}

	// 解析为绝对路径
	absPath := e.resolvePath(path)

	// 创建父目录（支持嵌套创建），权限 0755
	dir := filepath.Dir(absPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return &ToolResult{Tool: "WriteFile", Path: absPath, Error: "mkdir: " + err.Error()}
	}

	// 写文件，权限 0644（覆盖已有内容）
	if err := os.WriteFile(absPath, []byte(content), 0644); err != nil {
		return &ToolResult{Tool: "WriteFile", Path: absPath, Error: err.Error()}
	}

	// 返回写入字节数，便于 LLM 判断是否完整落盘
	return &ToolResult{Tool: "WriteFile", Success: true, Output: fmt.Sprintf("wrote %d bytes", len(content)), Path: absPath}
}

// listDir 列出目录。
//
// 职责：列出指定目录下的条目，区分目录/文件，附带文件大小。
// 参数：
//   - args：含 "path" 字段，空则取工作目录。
// 返回：成功时 Output 为多行条目列表；失败时 Error 为错误信息。
// 副作用：只读。
func (e *ToolExecutor) listDir(args map[string]any) *ToolResult {
	// path 可空，空则取当前工作目录
	path, _ := args["path"].(string)
	if path == "" {
		path = "."
	}

	// 解析为绝对路径
	absPath := e.resolvePath(path)
	// 读目录条目
	entries, err := os.ReadDir(absPath)
	if err != nil {
		return &ToolResult{Tool: "ListDir", Path: absPath, Error: err.Error()}
	}

	var lines []string
	// 逐条格式化：前缀（D 目录 / 空格文件）+ 大小 + 名称
	for _, entry := range entries {
		prefix := "  " // 文件前缀
		if entry.IsDir() {
			prefix = "D " // 目录前缀
		}
		info, _ := entry.Info()
		size := ""
		if info != nil {
			size = fmt.Sprintf("%8d", info.Size()) // 右对齐 8 位
		}
		lines = append(lines, fmt.Sprintf("%s %s %s", prefix, size, entry.Name()))
	}

	return &ToolResult{Tool: "ListDir", Success: true, Output: strings.Join(lines, "\n"), Path: absPath}
}

// runCommand 执行命令。
//
// 职责：在 workDir 下执行 shell 命令（Windows 用 cmd /c，Unix 用 sh -c），
//   捕获 stdout/stderr，超时控制，输出截断。mkdir 命令走跨平台 fast path。
// 参数：
//   - ctx：用于超时控制。
//   - args：含 "command" 字段，可选 "timeout"（秒，上限 60）。
// 返回：成功时 Output 含 stdout+stderr；失败时 Error 为错误信息。
// 副作用：执行任意 shell 命令（沙箱取决于 workDir 隔离程度）。
func (e *ToolExecutor) runCommand(ctx context.Context, args map[string]any) *ToolResult {
	// 取命令字符串
	cmdStr, _ := args["command"].(string)
	if cmdStr == "" {
		return &ToolResult{Tool: "RunCommand", Error: "command is required"}
	}

	// 跨平台 mkdir：直接用 os.MkdirAll，绕过 shell 差异（Windows mkdir 不支持 -p）
	if dir := parseMkdirDir(cmdStr); dir != "" {
		absDir := e.resolvePath(dir)
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

	// 捕获 stdout 与 stderr
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// 执行命令
	err := cmd.Run()
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

// searchInFiles 在文件中搜索（跨平台，纯Go实现）。
//
// 职责：递归搜索指定目录下白名单扩展名文件中包含 pattern 的行，
//   大小写不敏感，跳过 .git/node_modules/vendor，最多返回 500 条。
// 参数：
//   - args：含 "pattern" 字段，可选 "dir"（默认 "."）。
// 返回：命中时 Success=true，Output 为 "相对路径:行号: 行内容" 多行；无命中时 Success=false。
// 副作用：只读。
func (e *ToolExecutor) searchInFiles(args map[string]any) *ToolResult {
	// 取 pattern 与 dir
	pattern, _ := args["pattern"].(string)
	dir, _ := args["dir"].(string)
	if dir == "" {
		dir = "."
	}

	// 解析为绝对目录
	absDir := e.resolvePath(dir)
	// 大小写不敏感匹配：pattern 与行都转小写比较
	patternLower := strings.ToLower(pattern)

	// 扩展名白名单：只在代码/配置/文档类文件中搜，避免扫描二进制
	var exts = []string{".go", ".py", ".js", ".ts", ".java", ".yaml", ".yml", ".md", ".txt", ".json", ".toml", ".css", ".html"}
	var lines []string

	// 递归遍历目录
	filepath.WalkDir(absDir, func(path string, d os.DirEntry, err error) error {
		// 遍历出错或已收集足够结果则跳过
		if err != nil || len(lines) > 500 {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			// 跳过常见大目录，减少无效扫描
			if name == ".git" || name == "node_modules" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		// 非白名单扩展名跳过
		ext := strings.ToLower(filepath.Ext(d.Name()))
		if !slices.Contains(exts, ext) {
			return nil
		}

		// 读文件内容
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		// 逐行匹配 pattern
		content := string(data)
		for i, line := range strings.Split(content, "\n") {
			if strings.Contains(strings.ToLower(line), patternLower) {
				// 转为相对路径便于阅读
				relPath, _ := filepath.Rel(absDir, path)
				lines = append(lines, fmt.Sprintf("%s:%d: %s", relPath, i+1, strings.TrimSpace(line)))
				if len(lines) > 500 {
					break // 达上限提前退出
				}
			}
		}
		return nil
	})

	// 组装结果
	result := &ToolResult{
		Tool:   "SearchInFiles",
		Path:   absDir,
		Output: strings.Join(lines, "\n"),
	}
	// 有命中才算成功
	if len(lines) > 0 {
		result.Success = true
	}
	// 截断超长输出
	if len(result.Output) > 10000 {
		result.Output = result.Output[:10000] + "\n... (truncated)"
	}
	return result
}

// httpGet 执行 HTTP GET 请求。
//
// 职责：发起 GET 请求，附带自定义 header 与超时，最多读 1MB 响应体。
// 参数：
//   - ctx：用于超时控制。
//   - args：含 "url" 字段，可选 "headers"/"timeout"。
// 返回：Output 为 "HTTP <status>\n<body>"；2xx 时 Success=true。
// 副作用：发起网络请求。
func (e *ToolExecutor) httpGet(ctx context.Context, args map[string]any) *ToolResult {
	// 取 URL
	url, _ := args["url"].(string)
	if url == "" {
		return &ToolResult{Tool: "HTTPGet", Error: "url is required"}
	}

	// 解析 headers（兼容 map[string]any / map[string]string）
	headers := parseStringMap(args["headers"])

	// 计算超时
	timeout := e.timeout
	if t, ok := args["timeout"].(float64); ok && t > 0 {
		timeout = time.Duration(t) * time.Second
	}

	// 派生带超时的 ctx
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 构造请求
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return &ToolResult{Tool: "HTTPGet", Path: url, Error: err.Error()}
	}
	// 设置自定义 header
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	// 发起请求
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return &ToolResult{Tool: "HTTPGet", Path: url, Error: err.Error()}
	}
	defer resp.Body.Close()

	// 最多读 1MB 响应体，避免大响应撑爆内存
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 最多 1MB
	// 拼装输出：状态码 + 响应体
	output := fmt.Sprintf("HTTP %d\n%s", resp.StatusCode, string(body))
	if len(output) > 10000 {
		output = output[:10000] + "\n... (truncated)"
	}

	return &ToolResult{
		Tool:    "HTTPGet",
		Success: resp.StatusCode >= 200 && resp.StatusCode < 300, // 2xx 算成功
		Output:  output,
		Path:    url,
	}
}

// httpPost 执行 HTTP POST 请求（默认 JSON Body）。
//
// 职责：发起 POST 请求，body 默认按 JSON 序列化并自动补 Content-Type，
//   最多读 1MB 响应体。
// 参数：
//   - ctx：用于超时控制。
//   - args：含 "url" 字段，可选 "headers"/"body"/"timeout"。
// 返回：Output 为 "HTTP <status>\n<body>"；2xx 时 Success=true。
// 副作用：发起网络请求。
func (e *ToolExecutor) httpPost(ctx context.Context, args map[string]any) *ToolResult {
	// 取 URL
	url, _ := args["url"].(string)
	if url == "" {
		return &ToolResult{Tool: "HTTPPost", Error: "url is required"}
	}

	// 解析 headers
	headers := parseStringMap(args["headers"])

	// 序列化 body：string 原样使用，其他类型走 JSON
	var bodyBytes []byte
	if raw, ok := args["body"]; ok {
		switch v := raw.(type) {
		case string:
			bodyBytes = []byte(v) // 字符串 body 直接用
		default:
			// 其他类型（map/struct 等）序列化为 JSON
			b, err := json.Marshal(v)
			if err != nil {
				return &ToolResult{Tool: "HTTPPost", Path: url, Error: "marshal body: " + err.Error()}
			}
			bodyBytes = b
			// 自动补 Content-Type: application/json（用户未显式设置时）
			if _, ok := headers["Content-Type"]; !ok {
				if headers == nil {
					headers = make(map[string]string)
				}
				headers["Content-Type"] = "application/json"
			}
		}
	}

	// 计算超时
	timeout := e.timeout
	if t, ok := args["timeout"].(float64); ok && t > 0 {
		timeout = time.Duration(t) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 构造 POST 请求
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return &ToolResult{Tool: "HTTPPost", Path: url, Error: err.Error()}
	}
	// 设置 header
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	// 发起请求
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return &ToolResult{Tool: "HTTPPost", Path: url, Error: err.Error()}
	}
	defer resp.Body.Close()

	// 最多读 1MB 响应体
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	output := fmt.Sprintf("HTTP %d\n%s", resp.StatusCode, string(body))
	if len(output) > 10000 {
		output = output[:10000] + "\n... (truncated)"
	}

	return &ToolResult{
		Tool:    "HTTPPost",
		Success: resp.StatusCode >= 200 && resp.StatusCode < 300,
		Output:  output,
		Path:    url,
	}
}

// parseStringMap 兼容 map[string]any / map[string]string。
//
// 职责：把 LLM 传来的 headers（可能是任意 map 类型）统一转为 map[string]string。
// 参数：
//   - raw：原始值，预期为 map[string]string 或 map[string]any。
// 返回：归一化后的 map[string]string；nil 输入返回 nil。
// 副作用：无。
// 并发安全：纯函数。
func parseStringMap(raw any) map[string]string {
	if raw == nil {
		return nil
	}
	switch m := raw.(type) {
	case map[string]string:
		return m // 已是目标类型，直接返回
	case map[string]any:
		// 逐键转 string，非 string 值用 fmt.Sprint 兜底
		out := make(map[string]string, len(m))
		for k, v := range m {
			if s, ok := v.(string); ok {
				out[k] = s
			} else {
				out[k] = fmt.Sprint(v)
			}
		}
		return out
	}
	return nil // 未知类型返回 nil
}

// resolvePath 把路径解析为绝对路径。
//
// 职责：相对路径基于 workDir 解析，绝对路径原样返回。
// 参数：
//   - path：原始路径。
// 返回：绝对路径。
// 副作用：无。
// 并发安全：纯函数（workDir 只读）。
func (e *ToolExecutor) resolvePath(path string) string {
	if filepath.IsAbs(path) {
		return path // 绝对路径直接用
	}
	// 相对路径拼接 workDir
	return filepath.Join(e.workDir, path)
}

// sessionIDKey 用于在 context 中传递会话 ID 的非导出键类型。
//
// 设计意图：用未导出的空 struct 作 key 类型，避免与其他包的 context value 冲突。
type sessionIDKey struct{}

// WithSessionID 把会话 ID 写入 context。
//
// 职责：返回一个携带 sessionID 的新 context，供下游 executor 取用。
// 参数：
//   - ctx：原 context。
//   - sessionID：会话 ID。
// 返回：携带 sessionID 的新 context。
// 副作用：无（仅 context 派生）。
// 并发安全：context 派生安全。
func WithSessionID(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, sessionIDKey{}, sessionID)
}

// SessionIDFromContext 从 context 读取会话 ID。
//
// 职责：取出 WithSessionID 写入的 sessionID；未设置时返回空串。
// 参数：
//   - ctx：携带 sessionID 的 context。
// 返回：sessionID 字符串；不存在时返回 ""。
// 副作用：无。
// 并发安全：context 读取安全。
func SessionIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(sessionIDKey{}).(string); ok {
		return v
	}
	return ""
}
