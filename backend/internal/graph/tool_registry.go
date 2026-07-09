package graph

import (
	"context"
	"fmt"
)

// Tool 是 ToolExecutor 内置工具的统一接口。
// 每个具体工具实现 Name / Aliases / Execute，把按工具名分发从 giant switch 中解耦。
type Tool interface {
	Name() string
	Aliases() []string
	Execute(ctx context.Context, args map[string]any) *ToolResult
}

// ToolRegistry 按工具名与别名维护工具实例，并提供统一执行入口。
type ToolRegistry struct {
	tools   map[string]Tool
	aliases map[string]string
}

// NewToolRegistry 创建空注册表。
func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{
		tools:   make(map[string]Tool),
		aliases: make(map[string]string),
	}
}

// Register 注册一个工具及其别名。
func (r *ToolRegistry) Register(t Tool) {
	if t == nil {
		return
	}
	r.tools[t.Name()] = t
	for _, alias := range t.Aliases() {
		r.aliases[alias] = t.Name()
	}
}

// Execute 按名查找并执行工具。
// 支持通过别名调用；未知工具返回 error，由调用方决定如何包装为 ToolResult。
func (r *ToolRegistry) Execute(ctx context.Context, name string, args map[string]any) (*ToolResult, error) {
	if canonical, ok := r.aliases[name]; ok {
		name = canonical
	}
	t, ok := r.tools[name]
	if !ok {
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
	return t.Execute(ctx, args), nil
}

// readFileTool 读取文件内容。
type readFileTool struct{ exec *ToolExecutor }

func (t *readFileTool) Name() string   { return "ReadFile" }
func (t *readFileTool) Aliases() []string { return []string{"read_file", "readFile"} }
func (t *readFileTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	return t.exec.readFile(args)
}

// writeFileTool 写入文件。
type writeFileTool struct{ exec *ToolExecutor }

func (t *writeFileTool) Name() string   { return "WriteFile" }
func (t *writeFileTool) Aliases() []string { return []string{"write_file", "writeFile"} }
func (t *writeFileTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	return t.exec.writeFile(ctx, args)
}

// listDirTool 列出目录。
type listDirTool struct{ exec *ToolExecutor }

func (t *listDirTool) Name() string   { return "ListDir" }
func (t *listDirTool) Aliases() []string { return []string{"list_dir", "listDir"} }
func (t *listDirTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	return t.exec.listDir(args)
}

// runCommandTool 执行命令。
type runCommandTool struct{ exec *ToolExecutor }

func (t *runCommandTool) Name() string   { return "RunCommand" }
func (t *runCommandTool) Aliases() []string { return []string{"run_command", "runCommand"} }
func (t *runCommandTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	return t.exec.runCommand(ctx, args)
}

// searchInFilesTool 在文件中搜索。
type searchInFilesTool struct{ exec *ToolExecutor }

func (t *searchInFilesTool) Name() string   { return "SearchInFiles" }
func (t *searchInFilesTool) Aliases() []string { return []string{"search_in_files", "searchInFiles"} }
func (t *searchInFilesTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	return t.exec.searchInFiles(args)
}

// httpGetTool 执行 HTTP GET。
type httpGetTool struct{ exec *ToolExecutor }

func (t *httpGetTool) Name() string   { return "HTTPGet" }
func (t *httpGetTool) Aliases() []string { return []string{"http_get", "httpGet"} }
func (t *httpGetTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	return t.exec.httpGet(ctx, args)
}

// httpPostTool 执行 HTTP POST。
type httpPostTool struct{ exec *ToolExecutor }

func (t *httpPostTool) Name() string   { return "HTTPPost" }
func (t *httpPostTool) Aliases() []string { return []string{"http_post", "httpPost"} }
func (t *httpPostTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	return t.exec.httpPost(ctx, args)
}

// gitDiffTool 输出 Git diff。
type gitDiffTool struct{ exec *ToolExecutor }

func (t *gitDiffTool) Name() string   { return "GitDiff" }
func (t *gitDiffTool) Aliases() []string { return []string{"git_diff", "gitDiff"} }
func (t *gitDiffTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	return t.exec.gitDiff(args)
}

// gitStatusTool 输出 Git status。
type gitStatusTool struct{ exec *ToolExecutor }

func (t *gitStatusTool) Name() string   { return "GitStatus" }
func (t *gitStatusTool) Aliases() []string { return []string{"git_status", "gitStatus"} }
func (t *gitStatusTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	return t.exec.gitStatus(args)
}

// gitLogTool 输出 Git log。
type gitLogTool struct{ exec *ToolExecutor }

func (t *gitLogTool) Name() string   { return "GitLog" }
func (t *gitLogTool) Aliases() []string { return []string{"git_log", "gitLog"} }
func (t *gitLogTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	return t.exec.gitLog(args)
}

// gitBlameTool 输出 Git blame。
type gitBlameTool struct{ exec *ToolExecutor }

func (t *gitBlameTool) Name() string   { return "GitBlame" }
func (t *gitBlameTool) Aliases() []string { return []string{"git_blame", "gitBlame"} }
func (t *gitBlameTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	return t.exec.gitBlame(args)
}
