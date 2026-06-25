package graph

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/go-kratos/blades/tools"
)

// failureCounter 跨工具调用共享的连续失败计数器。
// 同一工具连续失败达 maxConsecutiveFailures 次后，通过 SetAction(tools.ActionLoopExit)
// 通知 blades.Agent 跳出 ReAct 循环，避免盲目重试。
type failureCounter struct {
	mu     sync.Mutex
	counts map[string]int
}

func newFailureCounter() *failureCounter {
	return &failureCounter{counts: make(map[string]int)}
}

func (f *failureCounter) fail(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts[name]++
	return f.counts[name]
}

func (f *failureCounter) reset(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.counts, name)
}

// 工具入参类型。json schema 由 tools.NewFunc 自动生成。
type (
	readFileInput struct {
		Path string `json:"path"`
	}
	writeFileInput struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	listDirInput struct {
		Path string `json:"path"`
	}
	runCommandInput struct {
		Command string  `json:"command"`
		Timeout float64 `json:"timeout,omitempty"`
	}
	searchInFilesInput struct {
		Pattern string `json:"pattern"`
		Dir     string `json:"dir,omitempty"`
	}
	httpGetInput struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers,omitempty"`
		Timeout float64           `json:"timeout,omitempty"`
	}
	httpPostInput struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers,omitempty"`
		Body    map[string]any    `json:"body,omitempty"`
		Timeout float64           `json:"timeout,omitempty"`
	}
)

// toolRunner 封装一次工具执行 + 进度事件 + 失败计数。
// 所有 blades.Tool 共用同一个 runner，通过 name 区分。
type toolRunner struct {
	executor *ToolExecutor
	progress ProgressCallback
	session  string
	agent    string
	failures *failureCounter
	results  *[]*ToolResult
	mu       sync.Mutex
}

func (r *toolRunner) emit(ctx context.Context, kind, msg, detail string) {
	if r.progress != nil {
		r.progress(ctx, ProgressEvent{SessionID: r.session, Kind: kind, Agent: r.agent, Message: msg, Detail: detail})
	}
}

// run 执行工具并返回 JSON 结果字符串。
// 失败达阈值时通过 ToolContext 设置 ActionLoopExit 跳出 Agent 循环。
func (r *toolRunner) run(ctx context.Context, name string, args map[string]any) string {
	argsStr, _ := json.Marshal(args)
	r.emit(ctx, "tool_call", "调用工具 "+name, string(argsStr))

	ctx = WithSessionID(ctx, r.session)
	result := r.executor.Execute(ctx, name, args)

	r.mu.Lock()
	*r.results = append(*r.results, result)
	r.mu.Unlock()

	out := result.Output
	if len(out) > 200 {
		out = out[:200] + "..."
	}
	if result.Success {
		r.failures.reset(name)
		r.emit(ctx, "tool_result", "工具 "+name+" 执行成功", out)
	} else {
		n := r.failures.fail(name)
		r.emit(ctx, "error", "工具 "+name+" 执行失败", result.Error)
		if n >= maxConsecutiveFailures {
			if tc, ok := tools.FromContext(ctx); ok {
				tc.SetAction(tools.ActionLoopExit, true)
			}
		}
	}

	b, _ := json.Marshal(result)
	return string(b)
}

// buildBladesTools 构造 7 个内置工具的 blades.Tool 集合。
// 复用 ToolExecutor 的沙箱实现，结果通过 toolRunner 推送 ProgressEvent。
func buildBladesTools(
	executor *ToolExecutor,
	progress ProgressCallback,
	sessionID, agentName string,
	results *[]*ToolResult,
) []tools.Tool {
	r := &toolRunner{
		executor: executor,
		progress: progress,
		session:  sessionID,
		agent:    agentName,
		failures: newFailureCounter(),
		results:  results,
	}

	// tools.NewFunc 是泛型函数，无法通过函数字面量包装，这里逐个构造。
	toolsList := make([]tools.Tool, 0, 7)

	if t, err := tools.NewFunc("ReadFile", "读取文件内容。", func(ctx context.Context, in readFileInput) (string, error) {
		return r.run(ctx, "ReadFile", map[string]any{"path": in.Path}), nil
	}); err == nil {
		toolsList = append(toolsList, t)
	}
	if t, err := tools.NewFunc("WriteFile", "写入文件。", func(ctx context.Context, in writeFileInput) (string, error) {
		return r.run(ctx, "WriteFile", map[string]any{"path": in.Path, "content": in.Content}), nil
	}); err == nil {
		toolsList = append(toolsList, t)
	}
	if t, err := tools.NewFunc("ListDir", "列出目录内容。", func(ctx context.Context, in listDirInput) (string, error) {
		return r.run(ctx, "ListDir", map[string]any{"path": in.Path}), nil
	}); err == nil {
		toolsList = append(toolsList, t)
	}
	if t, err := tools.NewFunc("RunCommand", "执行 shell 命令。", func(ctx context.Context, in runCommandInput) (string, error) {
		args := map[string]any{"command": in.Command}
		if in.Timeout > 0 {
			args["timeout"] = in.Timeout
		}
		return r.run(ctx, "RunCommand", args), nil
	}); err == nil {
		toolsList = append(toolsList, t)
	}
	if t, err := tools.NewFunc("SearchInFiles", "在文件中搜索文本。", func(ctx context.Context, in searchInFilesInput) (string, error) {
		return r.run(ctx, "SearchInFiles", map[string]any{"pattern": in.Pattern, "dir": in.Dir}), nil
	}); err == nil {
		toolsList = append(toolsList, t)
	}
	if t, err := tools.NewFunc("HTTPGet", "发起 HTTP GET 请求。", func(ctx context.Context, in httpGetInput) (string, error) {
		args := map[string]any{"url": in.URL, "headers": in.Headers}
		if in.Timeout > 0 {
			args["timeout"] = in.Timeout
		}
		return r.run(ctx, "HTTPGet", args), nil
	}); err == nil {
		toolsList = append(toolsList, t)
	}
	if t, err := tools.NewFunc("HTTPPost", "发起 HTTP POST 请求（默认 JSON body）。", func(ctx context.Context, in httpPostInput) (string, error) {
		args := map[string]any{"url": in.URL, "headers": in.Headers, "body": in.Body}
		if in.Timeout > 0 {
			args["timeout"] = in.Timeout
		}
		return r.run(ctx, "HTTPPost", args), nil
	}); err == nil {
		toolsList = append(toolsList, t)
	}

	return toolsList
}
