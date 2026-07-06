package graph

import (
	"context"
	"sync"

	"github.com/go-kratos/blades/tools"
)

// failureCounter 跨工具调用共享的连续失败计数器。
// 同一工具连续失败达 maxConsecutiveFailures 次后，通过 SetAction(tools.ActionLoopExit)
// 通知 blades.Agent 跳出 ReAct 循环，避免盲目重试。
//
// 职责：在多次工具调用间累计每个工具的连续失败次数，达到阈值后触发循环退出。
// 并发安全：内部用 sync.Mutex 保护 counts map，可被多个 blades.Tool 回调并发调用。
type failureCounter struct {
	mu     sync.Mutex     // 保护 counts 的互斥锁
	counts map[string]int // 工具名 → 连续失败次数
}

// newFailureCounter 创建一个空的失败计数器。
//
// 返回：初始化好 counts map 的 *failureCounter。
// 副作用：无。
func newFailureCounter() *failureCounter {
	return &failureCounter{counts: make(map[string]int)}
}

// fail 把指定工具的连续失败计数 +1 并返回最新值。
//
// 参数：
//   - name：工具名。
//
// 返回：累计后的连续失败次数。
// 并发安全：持锁操作，安全。
func (f *failureCounter) fail(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts[name]++ // 累加失败计数
	return f.counts[name]
}

// reset 清除指定工具的失败计数（成功后调用，重新开始计数）。
//
// 参数：
//   - name：工具名。
//
// 并发安全：持锁操作，安全。
func (f *failureCounter) reset(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.counts, name) // 直接删除 key 等价于归零
}

// 工具入参类型。json schema 由 tools.NewFunc 自动生成。
//
// 设计意图：每个工具的入参用一个独立 struct 表达，配合 tools.NewFunc 泛型函数
//
//	自动生成 JSON Schema 与反序列化逻辑，避免手写 schema。
type (
	readFileInput struct {
		Path string `json:"path"` // 文件路径，相对工作目录或绝对路径
	}
	writeFileInput struct {
		Path      string `json:"path"`      // 目标文件路径
		Content   string `json:"content"`   // 写入内容（整体覆盖）
		Temporary bool   `json:"temporary"` // 是否为临时文件（默认 false）。仅用于中间执行/分析的产物；用户要求保留的文件保持 false
	}
	listDirInput struct {
		Path string `json:"path"` // 目录路径，空则取工作目录
	}
	runCommandInput struct {
		Command string  `json:"command"`           // shell 命令字符串
		Timeout float64 `json:"timeout,omitempty"` // 可选超时（秒），上限 60s
	}
	searchInFilesInput struct {
		Pattern string `json:"pattern"`       // 搜索关键字（大小写不敏感）
		Dir     string `json:"dir,omitempty"` // 搜索目录，空则取工作目录
	}
	httpGetInput struct {
		URL     string            `json:"url"`               // 请求 URL
		Headers map[string]string `json:"headers,omitempty"` // 自定义请求头
		Timeout float64           `json:"timeout,omitempty"` // 可选超时（秒）
	}
	httpPostInput struct {
		URL     string            `json:"url"`               // 请求 URL
		Headers map[string]string `json:"headers,omitempty"` // 自定义请求头
		Body    map[string]any    `json:"body,omitempty"`    // 请求体（默认 JSON 序列化）
		Timeout float64           `json:"timeout,omitempty"` // 可选超时（秒）
	}
	gitDiffInput struct {
		Target string `json:"target,omitempty"` // 空=未暂存；"--staged"=暂存区；"commit...commit"=历史对比
		Path   string `json:"path,omitempty"`   // 可选文件/目录路径
	}
	gitStatusInput struct{}
	gitLogInput    struct {
		Limit float64 `json:"limit,omitempty"` // 返回最近 N 条提交，默认 20
		Path  string  `json:"path,omitempty"`  // 可选文件/目录路径
	}
	gitBlameInput struct {
		Path string `json:"path"` // 目标文件路径
	}
)

// toolRunner 封装一次工具执行 + 进度事件 + 失败计数。
// 所有 blades.Tool 共用同一个 runner，通过 name 区分。
//
// 职责：作为内置工具的统一执行适配层，把 blades.Tool 的调用转发给
//
//	ToolExecutor，同时推送 ProgressEvent 并维护连续失败计数。
//
// 并发安全：results 切片通过 mu 保护；failures 内部自带锁；其余字段只读。
type toolRunner struct {
	executor *ToolExecutor    // 沙箱执行器，真正干活的人
	progress ProgressCallback // 进度回调，可空
	session  string           // 当前会话 ID，用于事件归属
	agent    string           // 当前 agent 名，用于事件归属
	failures *failureCounter  // 连续失败计数器
	results  *[]*ToolResult   // 指向外部 slice，收集所有工具结果
	mu       sync.Mutex       // 保护 results 切片的并发追加
}

// emit 推送一条进度事件（含 detail 字段）。
//
// 参数：
//   - ctx：上下文（保留以备下游扩展）。
//   - kind：事件类型，如 "tool_call" / "tool_result" / "error"。
//   - msg：人类可读描述。
//   - detail：可选详情（工具参数 / 错误信息 / 截断输出）。
//
// 副作用：progress 非 nil 时触发回调。
func (r *toolRunner) emit(ctx context.Context, kind, msg, detail string) {
	if r.progress != nil {
		// 携带 session/agent 归属信息，便于 UI 按会话与节点过滤
		r.progress(ctx, ProgressEvent{SessionID: r.session, Kind: kind, Agent: r.agent, Message: msg, Detail: detail})
	}
}

// emitTool 推送一条携带工具名的进度事件（用于 tool_call，让前端直接拿到工具名）。
func (r *toolRunner) emitTool(ctx context.Context, kind, tool, msg, detail string) {
	if r.progress != nil {
		r.progress(ctx, ProgressEvent{SessionID: r.session, Kind: kind, Agent: r.agent, Tool: tool, Message: msg, Detail: detail})
	}
}

// run 执行工具并返回 JSON 结果字符串。
// 失败达阈值时通过 ToolContext 设置 ActionLoopExit 跳出 Agent 循环。
//
// 职责：单次工具调用的核心流程——推送调用事件、执行工具、记录结果、推送结果事件、
//
//	必要时强制退出 Agent 循环。
//
// 参数：
//   - ctx：blades 传入的上下文，可能携带 tools.ToolContext。
//   - name：工具名（已归一化前的原名，用于事件展示与失败计数）。
//   - args：工具入参 map。
//
// 返回：工具结果序列化后的 JSON 字符串（成功失败都返回 JSON，错误不通过 error）。
// 副作用：通过 executor 产生文件/命令/网络副作用；通过 progress 推送事件；
//
//	达失败阈值时设置 tools.ActionLoopExit。
//
// 并发安全：results 追加持锁；failures 内部持锁；可被 Agent 并发调用。
func (r *toolRunner) run(ctx context.Context, name string, args map[string]any) string {
	argsStr, _ := marshalNoHTMLEscape(args) // 序列化参数用于事件展示（关闭 HTML 转义，避免 TUI 乱码）
	// 推送调用前事件（pending 态）：携带 Tool 名，前端无需正则推断
	r.emitTool(ctx, "tool_call", name, "调用工具 "+name, string(argsStr))

	// 把 sessionID 注入 ctx，让 executor 知道这是哪个会话的调用
	ctx = WithSessionID(ctx, r.session)
	// 真正执行工具，拿到结构化结果
	result := r.executor.Execute(ctx, name, args)

	// 把结果追加到共享 slice，持锁防止并发覆盖
	r.mu.Lock()
	*r.results = append(*r.results, result)
	r.mu.Unlock()

	// 工具结果统一由 ToolExecutor callback → handleToolResult → tool_exec 事件携带
	// （含完整 Tool/Path/Output/Error/Success/ArgsJSON），这里不再单独 emit
	// tool_result/error，避免前端一次调用收到多个结果事件导致重复卡片。
	if result.Success {
		// 成功：清零该工具的失败计数
		r.failures.reset(name)
	} else {
		// 失败：累加失败计数；达阈值时强制跳出 Agent 循环，避免盲目重试
		n := r.failures.fail(name)
		if n >= maxConsecutiveFailures {
			if tc, ok := tools.FromContext(ctx); ok {
				tc.SetAction(tools.ActionLoopExit, true)
			}
		}
	}

	// 结果序列化为 JSON 返回给 blades Agent，Agent 会把文本回灌给 LLM
	// 关闭 HTML 转义：result.Output 可能含 < > &（HTML/命令输出），转义后 LLM 看到
	// 乱码影响后续决策（参见塔防 demo 事故：HTML 内容回灌为 <!DOCTYPE>）
	b, _ := marshalNoHTMLEscape(result)
	return string(b)
}

// buildBladesTools 构造内置工具的 blades.Tool 集合（P3-2 扩展为 11 个）。
// 复用 ToolExecutor 的沙箱实现，结果通过 toolRunner 推送 ProgressEvent。
//
// 职责：为 blades Agent 装配工具（ReadFile/WriteFile/ListDir/RunCommand/
//
//	SearchInFiles/HTTPGet/HTTPPost/GitDiff/GitStatus/GitLog/GitBlame），每个工具的入参用独立 struct 描述，
//	实际执行统一委托给 toolRunner.run → ToolExecutor.Execute。
//
// 参数：
//   - executor：本地工具沙箱执行器。
//   - progress：进度回调，可空。
//   - sessionID：当前会话 ID，用于事件归属。
//   - agentName：当前 agent 名，用于事件归属。
//   - results：指向外部 slice，用于收集所有工具结果。
//
// 返回：长度 ≤11 的 blades.Tool 切片（某个工具构造失败时会跳过）。
// 副作用：无（仅构造工具定义，不执行）。
// 并发安全：返回的工具集合由调用方独占使用；内部 toolRunner 自带锁。
func buildBladesTools(
	executor *ToolExecutor,
	progress ProgressCallback,
	sessionID, agentName string,
	results *[]*ToolResult,
) []tools.Tool {
	// 共享一个 runner，所有工具回调都走它，便于统一计数与结果收集
	r := &toolRunner{
		executor: executor,
		progress: progress,
		session:  sessionID,
		agent:    agentName,
		failures: newFailureCounter(),
		results:  results,
	}

	// tools.NewFunc 是泛型函数，无法通过函数字面量包装，这里逐个构造。
	toolsList := make([]tools.Tool, 0, 11)

	// ReadFile：读文件，委托给 executor.readFile
	if t, err := tools.NewFunc("ReadFile", "读取文件内容。", func(ctx context.Context, in readFileInput) (string, error) {
		return r.run(ctx, "ReadFile", map[string]any{"path": in.Path}), nil
	}); err == nil {
		toolsList = append(toolsList, t)
	}
	// WriteFile：写文件，委托给 executor.writeFile
	if t, err := tools.NewFunc("WriteFile", "写入文件。若文件仅作为临时产物使用（例如运行脚本、中间分析、一次性计算），请设置 temporary=true，文件会写入会话级临时目录并在会话结束后自动清理；用户明确要求保留的文件请保持 temporary=false（默认）。", func(ctx context.Context, in writeFileInput) (string, error) {
		return r.run(ctx, "WriteFile", map[string]any{"path": in.Path, "content": in.Content, "temporary": in.Temporary}), nil
	}); err == nil {
		toolsList = append(toolsList, t)
	}
	// ListDir：列目录，委托给 executor.listDir
	if t, err := tools.NewFunc("ListDir", "列出目录内容。", func(ctx context.Context, in listDirInput) (string, error) {
		return r.run(ctx, "ListDir", map[string]any{"path": in.Path}), nil
	}); err == nil {
		toolsList = append(toolsList, t)
	}
	// RunCommand：执行 shell 命令，按需带上 timeout 参数
	if t, err := tools.NewFunc("RunCommand", "执行 shell 命令。命令执行时环境变量 BMA_SESSION_TEMP_DIR 指向本会话的临时目录，如需创建临时文件请写入该目录，会话结束后会自动清理。", func(ctx context.Context, in runCommandInput) (string, error) {
		args := map[string]any{"command": in.Command}
		if in.Timeout > 0 {
			args["timeout"] = in.Timeout // 仅当 LLM 显式指定时才传
		}
		return r.run(ctx, "RunCommand", args), nil
	}); err == nil {
		toolsList = append(toolsList, t)
	}
	// SearchInFiles：在文件中搜索文本，委托给 executor.searchInFiles
	if t, err := tools.NewFunc("SearchInFiles", "在文件中搜索文本。", func(ctx context.Context, in searchInFilesInput) (string, error) {
		return r.run(ctx, "SearchInFiles", map[string]any{"pattern": in.Pattern, "dir": in.Dir}), nil
	}); err == nil {
		toolsList = append(toolsList, t)
	}
	// HTTPGet：发起 GET 请求，按需带上 timeout
	if t, err := tools.NewFunc("HTTPGet", "发起 HTTP GET 请求。", func(ctx context.Context, in httpGetInput) (string, error) {
		args := map[string]any{"url": in.URL, "headers": in.Headers}
		if in.Timeout > 0 {
			args["timeout"] = in.Timeout
		}
		return r.run(ctx, "HTTPGet", args), nil
	}); err == nil {
		toolsList = append(toolsList, t)
	}
	// HTTPPost：发起 POST 请求（默认 JSON body），按需带上 timeout
	if t, err := tools.NewFunc("HTTPPost", "发起 HTTP POST 请求（默认 JSON body）。", func(ctx context.Context, in httpPostInput) (string, error) {
		args := map[string]any{"url": in.URL, "headers": in.Headers, "body": in.Body}
		if in.Timeout > 0 {
			args["timeout"] = in.Timeout
		}
		return r.run(ctx, "HTTPPost", args), nil
	}); err == nil {
		toolsList = append(toolsList, t)
	}
	// GitDiff：查看 Git 差异（工作区/暂存区/历史对比）
	if t, err := tools.NewFunc("GitDiff", `查看 Git 差异。target 为空时显示未暂存变更；"--staged" 显示暂存区变更；"HEAD~1..HEAD" 等显示历史区间差异。`, func(ctx context.Context, in gitDiffInput) (string, error) {
		return r.run(ctx, "GitDiff", map[string]any{"target": in.Target, "path": in.Path}), nil
	}); err == nil {
		toolsList = append(toolsList, t)
	}
	// GitStatus：查看 Git 工作区状态
	if t, err := tools.NewFunc("GitStatus", "查看 Git 工作区状态（简短格式）。", func(ctx context.Context, in gitStatusInput) (string, error) {
		return r.run(ctx, "GitStatus", map[string]any{}), nil
	}); err == nil {
		toolsList = append(toolsList, t)
	}
	// GitLog：查看 Git 提交历史
	if t, err := tools.NewFunc("GitLog", "查看 Git 提交历史。limit 控制返回条数（默认 20），path 可限定文件/目录。", func(ctx context.Context, in gitLogInput) (string, error) {
		return r.run(ctx, "GitLog", map[string]any{"limit": in.Limit, "path": in.Path}), nil
	}); err == nil {
		toolsList = append(toolsList, t)
	}
	// GitBlame：查看文件每行最后修改者
	if t, err := tools.NewFunc("GitBlame", "查看指定文件每行的最后修改者（git blame）。", func(ctx context.Context, in gitBlameInput) (string, error) {
		return r.run(ctx, "GitBlame", map[string]any{"path": in.Path}), nil
	}); err == nil {
		toolsList = append(toolsList, t)
	}

	return toolsList
}
