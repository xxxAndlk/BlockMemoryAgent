package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

// ToolResult 工具执行结果
type ToolResult struct {
	Tool    string `json:"tool"`
	Success bool   `json:"success"`
	Output  string `json:"output"`
	Error   string `json:"error,omitempty"`
	Path    string `json:"path,omitempty"`
}

// ToolCallback 工具执行回调（用于通知UI）
type ToolCallback func(result *ToolResult)

// ToolExecutor 本地工具执行器（沙箱）
type ToolExecutor struct {
	workDir  string
	timeout  time.Duration
	callback ToolCallback
}

// NewToolExecutor 创建工具执行器
func NewToolExecutor(workDir string) *ToolExecutor {
	if workDir == "" {
		workDir, _ = os.Getwd()
	}
	return &ToolExecutor{
		workDir: workDir,
		timeout: 30 * time.Second,
	}
}

// SetCallback 设置工具执行回调
func (e *ToolExecutor) SetCallback(cb ToolCallback) {
	e.callback = cb
}

// Execute 执行工具调用
func (e *ToolExecutor) Execute(ctx context.Context, toolName string, args map[string]any) *ToolResult {
	var result *ToolResult
	switch toolName {
	case "ReadFile":
		result = e.readFile(args)
	case "WriteFile":
		result = e.writeFile(args)
	case "ListDir":
		result = e.listDir(args)
	case "RunCommand":
		result = e.runCommand(ctx, args)
	case "SearchInFiles":
		result = e.searchInFiles(args)
	case "HTTPGet":
		result = e.httpGet(ctx, args)
	case "HTTPPost":
		result = e.httpPost(ctx, args)
	default:
		result = &ToolResult{Tool: toolName, Error: fmt.Sprintf("unknown tool: %s", toolName)}
	}
	if e.callback != nil {
		e.callback(result)
	}
	return result
}

// readFile 读取文件
func (e *ToolExecutor) readFile(args map[string]any) *ToolResult {
	path, ok := args["path"].(string)
	if !ok || path == "" {
		return &ToolResult{Tool: "ReadFile", Error: "path is required"}
	}

	absPath := e.resolvePath(path)
	data, err := os.ReadFile(absPath)
	if err != nil {
		return &ToolResult{Tool: "ReadFile", Path: absPath, Error: err.Error()}
	}

	content := string(data)
	if len(content) > 10000 {
		content = content[:10000] + "\n... (truncated)"
	}

	return &ToolResult{Tool: "ReadFile", Success: true, Output: content, Path: absPath}
}

// writeFile 写入文件
func (e *ToolExecutor) writeFile(args map[string]any) *ToolResult {
	path, _ := args["path"].(string)
	content, _ := args["content"].(string)

	if path == "" {
		return &ToolResult{Tool: "WriteFile", Error: "path is required"}
	}

	absPath := e.resolvePath(path)

	// 创建目录
	dir := filepath.Dir(absPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return &ToolResult{Tool: "WriteFile", Path: absPath, Error: "mkdir: " + err.Error()}
	}

	if err := os.WriteFile(absPath, []byte(content), 0644); err != nil {
		return &ToolResult{Tool: "WriteFile", Path: absPath, Error: err.Error()}
	}

	return &ToolResult{Tool: "WriteFile", Success: true, Output: fmt.Sprintf("wrote %d bytes", len(content)), Path: absPath}
}

// listDir 列出目录
func (e *ToolExecutor) listDir(args map[string]any) *ToolResult {
	path, _ := args["path"].(string)
	if path == "" {
		path = "."
	}

	absPath := e.resolvePath(path)
	entries, err := os.ReadDir(absPath)
	if err != nil {
		return &ToolResult{Tool: "ListDir", Path: absPath, Error: err.Error()}
	}

	var lines []string
	for _, entry := range entries {
		prefix := "  "
		if entry.IsDir() {
			prefix = "D "
		}
		info, _ := entry.Info()
		size := ""
		if info != nil {
			size = fmt.Sprintf("%8d", info.Size())
		}
		lines = append(lines, fmt.Sprintf("%s %s %s", prefix, size, entry.Name()))
	}

	return &ToolResult{Tool: "ListDir", Success: true, Output: strings.Join(lines, "\n"), Path: absPath}
}

// runCommand 执行命令
func (e *ToolExecutor) runCommand(ctx context.Context, args map[string]any) *ToolResult {
	cmdStr, _ := args["command"].(string)
	if cmdStr == "" {
		return &ToolResult{Tool: "RunCommand", Error: "command is required"}
	}

	timeout := e.timeout
	if t, ok := args["timeout"].(float64); ok && t > 0 {
		timeout = time.Duration(t) * time.Second
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/c", cmdStr)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", cmdStr)
	}
	cmd.Dir = e.workDir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	output := stdout.String()
	if stderr.Len() > 0 {
		output += "\n[stderr]\n" + stderr.String()
	}

	if len(output) > 10000 {
		output = output[:10000] + "\n... (truncated)"
	}

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

// searchInFiles 在文件中搜索（跨平台，纯Go实现）
func (e *ToolExecutor) searchInFiles(args map[string]any) *ToolResult {
	pattern, _ := args["pattern"].(string)
	dir, _ := args["dir"].(string)
	if dir == "" {
		dir = "."
	}

	absDir := e.resolvePath(dir)
	patternLower := strings.ToLower(pattern)

	var exts = []string{".go", ".py", ".js", ".ts", ".java", ".yaml", ".yml", ".md", ".txt", ".json", ".toml", ".css", ".html"}
	var lines []string

	filepath.WalkDir(absDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || len(lines) > 500 {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		if !slices.Contains(exts, ext) {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		content := string(data)
		for i, line := range strings.Split(content, "\n") {
			if strings.Contains(strings.ToLower(line), patternLower) {
				relPath, _ := filepath.Rel(absDir, path)
				lines = append(lines, fmt.Sprintf("%s:%d: %s", relPath, i+1, strings.TrimSpace(line)))
				if len(lines) > 500 {
					break
				}
			}
		}
		return nil
	})

	result := &ToolResult{
		Tool:   "SearchInFiles",
		Path:   absDir,
		Output: strings.Join(lines, "\n"),
	}
	if len(lines) > 0 {
		result.Success = true
	}
	if len(result.Output) > 10000 {
		result.Output = result.Output[:10000] + "\n... (truncated)"
	}
	return result
}

// httpGet 执行 HTTP GET 请求
func (e *ToolExecutor) httpGet(ctx context.Context, args map[string]any) *ToolResult {
	url, _ := args["url"].(string)
	if url == "" {
		return &ToolResult{Tool: "HTTPGet", Error: "url is required"}
	}

	headers := parseStringMap(args["headers"])

	timeout := e.timeout
	if t, ok := args["timeout"].(float64); ok && t > 0 {
		timeout = time.Duration(t) * time.Second
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return &ToolResult{Tool: "HTTPGet", Path: url, Error: err.Error()}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return &ToolResult{Tool: "HTTPGet", Path: url, Error: err.Error()}
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 最多 1MB
	output := fmt.Sprintf("HTTP %d\n%s", resp.StatusCode, string(body))
	if len(output) > 10000 {
		output = output[:10000] + "\n... (truncated)"
	}

	return &ToolResult{
		Tool:    "HTTPGet",
		Success: resp.StatusCode >= 200 && resp.StatusCode < 300,
		Output:  output,
		Path:    url,
	}
}

// httpPost 执行 HTTP POST 请求（默认 JSON Body）
func (e *ToolExecutor) httpPost(ctx context.Context, args map[string]any) *ToolResult {
	url, _ := args["url"].(string)
	if url == "" {
		return &ToolResult{Tool: "HTTPPost", Error: "url is required"}
	}

	headers := parseStringMap(args["headers"])

	var bodyBytes []byte
	if raw, ok := args["body"]; ok {
		switch v := raw.(type) {
		case string:
			bodyBytes = []byte(v)
		default:
			b, err := json.Marshal(v)
			if err != nil {
				return &ToolResult{Tool: "HTTPPost", Path: url, Error: "marshal body: " + err.Error()}
			}
			bodyBytes = b
			if _, ok := headers["Content-Type"]; !ok {
				if headers == nil {
					headers = make(map[string]string)
				}
				headers["Content-Type"] = "application/json"
			}
		}
	}

	timeout := e.timeout
	if t, ok := args["timeout"].(float64); ok && t > 0 {
		timeout = time.Duration(t) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return &ToolResult{Tool: "HTTPPost", Path: url, Error: err.Error()}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return &ToolResult{Tool: "HTTPPost", Path: url, Error: err.Error()}
	}
	defer resp.Body.Close()

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

// parseStringMap 兼容 map[string]any / map[string]string
func parseStringMap(raw any) map[string]string {
	if raw == nil {
		return nil
	}
	switch m := raw.(type) {
	case map[string]string:
		return m
	case map[string]any:
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
	return nil
}

func (e *ToolExecutor) resolvePath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(e.workDir, path)
}
