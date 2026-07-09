package graph

// 本文件为 ToolExecutor 的核心：类型定义 + 构造 + Execute 分发 + 路径解析 + sessionID 上下文。
// 具体工具实现按类别拆分（P0-3）：tool_files.go / tool_command.go / tool_http.go。

import (
	"bytes"        // marshalNoHTMLEscape buffer
	"context"      // 上下文与超时控制
	"encoding/json" // Execute 入参 JSON 摘要
	"fmt"          // 错误格式化
	"os"           // NewToolExecutor 回退 Getwd
	"path/filepath" // resolvePath
	"time"         // 默认超时
)

// marshalNoHTMLEscape 序列化 v 为 JSON，关闭 HTML 转义。
// 默认 json.Marshal 会把 < > & 转成 < > &，TUI/日志直接展示
// 这些转义序列就是乱码。用 json.Encoder + SetEscapeHTML(false) 避免。
func marshalNoHTMLEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// Encode 末尾会加换行，trim 掉以保持与 json.Marshal 一致
	out := buf.Bytes()
	if len(out) > 0 && out[len(out)-1] == '\n' {
		out = out[:len(out)-1]
	}
	return out, nil
}

// ToolResult 工具执行结果。
//
// 职责：统一描述一次工具调用的产出，供 blades Agent 回灌给 LLM、供 UI 展示、
//
//	供上层完成门控判断。
//
// 字段：
//   - Tool：工具名（已归一化为 CamelCase）。
//   - Success：是否执行成功。
//   - Output：截断后的标准输出 / 文件内容 / 命令输出。
//   - Error：失败时的错误信息（成功时为空）。
//   - Path：相关路径（文件路径 / URL / 工作目录），便于审计。
//   - SessionID：归属会话，避免跨会话事件泄漏。
//   - IsTemporary：该工具创建的文件是否为临时文件（会话结束后自动清理）。
//   - TempDir：临时文件存放的会话级目录（仅当 IsTemporary=true 时有效）。
type ToolResult struct {
	Tool        string `json:"tool"`
	Success     bool   `json:"success"`
	Output      string `json:"output"`
	Error       string `json:"error,omitempty"`
	Path        string `json:"path,omitempty"`
	ArgsJSON    string `json:"args_json,omitempty"`    // 入参 JSON 摘要（截断），用于日志展示
	SessionID   string `json:"session_id,omitempty"`   // 归属会话，避免跨会话事件泄漏
	IsTemporary bool   `json:"is_temporary,omitempty"` // 是否为临时文件
	TempDir     string `json:"temp_dir,omitempty"`     // 会话级临时目录
}

// ToolCallback 工具执行回调（用于通知UI）。
//
// 职责：每次工具执行完成后被调用，把结果推给订阅方（如 TUI 广播器）。
// 设计意图：解耦 ToolExecutor 与具体 UI 实现，executor 不关心谁在听。
type ToolCallback func(result *ToolResult)

// ProgressEvent 单步进度事件，用于把 Agent 的思考/意图/工具调用实时推给 UI。
// Kind 取值:
//
//	"think" | "intend" | "tool_call" | "tool_result" | "llm" | "wait" | "error"  (原有)
//	"prompt" | "agent_created" | "token_usage" | "graph_step"                     (新增调试类)
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
//
//	输出截断与回调通知。
//
// 字段：
//   - workDir：工具执行的基准目录，相对路径基于此解析。
//   - timeout：默认超时（命令/HTTP），可被入参覆盖（上限 60s）。
//   - callback：可选回调，每次 Execute 后触发。
//   - guards：工具执行前的业务策略守卫注册表。
//
// 并发安全：workDir/timeout/callback/guards 在 SetCallback 后不再变化；
//
//	Execute 可被多 goroutine 并发调用（无共享可变状态）。
type ToolExecutor struct {
	workDir  string          // 工具执行基准目录
	timeout  time.Duration   // 默认超时
	callback ToolCallback    // 工具执行回调
	sandbox  SandboxConfig   // 轻量级沙箱策略（命令黑名单 + 路径逃逸检测）
	guards   *GuardRegistry  // 业务策略守卫（写保护、命令拦截等）
}

// NewToolExecutor 创建工具执行器。
//
// 参数：
//   - workDir：基准工作目录；空串时回退到当前进程工作目录。
//
// 返回：初始化好的 *ToolExecutor，默认超时 30s，启用默认沙箱与默认守卫，callback 为 nil。
// 副作用：workDir 为空时调用 os.Getwd()。
func NewToolExecutor(workDir string) *ToolExecutor {
	// workDir 为空时回退到进程当前目录，避免相对路径解析失败
	if workDir == "" {
		workDir, _ = os.Getwd()
	}
	e := &ToolExecutor{
		workDir: workDir,
		timeout: 30 * time.Second, // 默认 30s 超时
		sandbox: DefaultSandboxConfig(),
	}
	e.guards = e.defaultGuardRegistry()
	return e
}

// defaultGuardRegistry 返回工具执行器默认启用的守卫集合。
func (e *ToolExecutor) defaultGuardRegistry() *GuardRegistry {
	g := NewGuardRegistry()
	g.RegisterWriteGuard(protectedPathGuard{})
	g.RegisterWriteGuard(mailboxFileGuard{})
	g.RegisterWriteGuard(fileHelperScriptGuard{})
	g.RegisterWriteGuard(mailboxGoProgramGuard{})
	g.RegisterWriteGuard(pathWhitespaceGuard{})
	g.RegisterCommandGuard(longRunningServerGuard{})
	g.RegisterCommandGuard(&funcCommandGuard{
		name: "sandbox-block",
		check: func(cmd string) (blocked bool, reason string) {
			pattern, blocked := e.isCommandBlocked(cmd)
			if !blocked {
				return false, ""
			}
			return true, fmt.Sprintf("blocked command matches sandbox rule: %s", pattern)
		},
	})
	return g
}

// SetGuardRegistry 注入自定义守卫注册表；nil 时恢复默认。
func (e *ToolExecutor) SetGuardRegistry(g *GuardRegistry) {
	if g == nil {
		e.guards = e.defaultGuardRegistry()
		return
	}
	e.guards = g
}

// ensureGuardDefaults 保证 guards 字段非空。
func (e *ToolExecutor) ensureGuardDefaults() {
	if e.guards == nil {
		e.guards = e.defaultGuardRegistry()
	}
}

// SetCallback 设置工具执行回调。
//
// 参数：
//   - cb：每次 Execute 完成后调用的回调；传 nil 可清除。
//
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
//
// 返回：填充好的 *ToolResult（始终非 nil，失败也通过 result.Error 表达）。
// 副作用：通过具体工具实现产生文件/命令/网络副作用；通过 callback 通知订阅方。
// 并发安全：可被多 goroutine 并发调用。
func (e *ToolExecutor) Execute(ctx context.Context, toolName string, args map[string]any) *ToolResult {
	// 确保通过旧构造函数或未设置沙箱/守卫的 executor 仍有默认安全策略
	e.ensureSandboxDefaults()
	e.ensureGuardDefaults()

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
		result = e.writeFile(ctx, args)
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
	case "GitDiff":
		result = e.gitDiff(args)
	case "GitStatus":
		result = e.gitStatus(args)
	case "GitLog":
		result = e.gitLog(args)
	case "GitBlame":
		result = e.gitBlame(args)
	default:
		// 未知工具：返回带错误的空结果，不 panic
		result = &ToolResult{Tool: toolName, Error: fmt.Sprintf("unknown tool: %s", toolName)}
	}
	// 统一填入 sessionID，便于上层按会话过滤事件
	result.SessionID = sessionID
	// 统一填入入参 JSON 摘要（截断 300 字），便于日志展示工具调用上下文
	// 关闭 HTML 转义：默认 json.Marshal 会把 < > & 转成 < > &，
	// TUI/日志直接展示这些转义序列就是乱码（参见塔防 demo 事故：HTML 内容显示为
	// <!DOCTYPE html>，命令中的 && 显示为 &&）。
	if argsJSON, mErr := marshalNoHTMLEscape(args); mErr == nil {
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
//
// 返回：归一化后的工具名；未命中别名时原样返回。
// 副作用：无。
// 并发安全：纯函数（每次构建 map，无共享状态）。
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
		"git_diff":        "GitDiff",
		"git_status":      "GitStatus",
		"git_log":         "GitLog",
		"git_blame":       "GitBlame",
	}
	if v, ok := aliases[name]; ok {
		return v
	}
	return name // 已是 CamelCase 或未知名，原样返回
}

// resolvePath 把路径解析为绝对路径。
//
// 职责：相对路径基于 workDir 解析，绝对路径原样返回。
// 参数：
//   - path：原始路径。
//
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

// sessionTempDir 返回指定会话的临时文件目录。
//
// 职责：为每个会话分配独立的临时目录，便于会话结束后统一清理。
// 路径规则：<workDir>/.bma/tmp/<sessionID>。
// 参数：
//   - sessionID：会话 ID。
//
// 返回：临时目录绝对路径；sessionID 为空时返回空串。
// 并发安全：纯函数（workDir 只读）。
func (e *ToolExecutor) sessionTempDir(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	return filepath.Join(e.workDir, ".bma", "tmp", sessionID)
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
//
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
//
// 返回：sessionID 字符串；不存在时返回 ""。
// 副作用：无。
// 并发安全：context 读取安全。
func SessionIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(sessionIDKey{}).(string); ok {
		return v
	}
	return ""
}
