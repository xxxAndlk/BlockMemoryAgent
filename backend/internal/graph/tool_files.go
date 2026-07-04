package graph

// 本文件承载文件类工具实现：ReadFile / WriteFile / ListDir / SearchInFiles。
// 从 tool_executor.go 按工具类别拆出（P0-3）。方法挂在 *ToolExecutor 上，同 package。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// readFile 读取文件内容。
//
// 职责：读取指定路径文件，超过 10000 字符时截断，返回内容与绝对路径。
// 参数：
//   - args：必须含 "path" 字段。
//
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
//
//   - ctx：上下文，用于取 sessionID 以计算会话级临时目录。
//
//   - args：含 "path" / "content" / "temporary" 字段；temporary=true 时文件写入
//
//     会话临时目录并在 ToolResult 中标记，会话结束后自动清理。
//
// 返回：成功时 Output 为写入字节数；失败时 Error 为错误信息。
// 副作用：创建目录 + 写文件（覆盖已有内容）。
func (e *ToolExecutor) writeFile(ctx context.Context, args map[string]any) *ToolResult {
	// 取 path 与 content，类型断言失败时取零值
	path, _ := args["path"].(string)
	content, _ := args["content"].(string)
	temporary, _ := args["temporary"].(bool)

	if path == "" {
		return &ToolResult{Tool: "WriteFile", Error: "path is required"}
	}

	// 解析为绝对路径；临时文件写入会话级临时目录，防止污染工作目录
	absPath := e.resolvePath(path)
	tempDir := ""
	if temporary {
		sessionID := SessionIDFromContext(ctx)
		if sessionID == "" {
			return &ToolResult{Tool: "WriteFile", Error: "temporary file requires a session context"}
		}
		tempDir = e.sessionTempDir(sessionID)
		// 临时文件路径统一收敛到会话临时目录，绝对路径仅保留文件名，
		// 防止 LLM 用绝对路径把临时文件写到预期之外的位置。
		cleanPath := filepath.Clean(path)
		if filepath.IsAbs(cleanPath) {
			cleanPath = filepath.Base(cleanPath)
		}
		absPath = filepath.Join(tempDir, cleanPath)
	}

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
	result := &ToolResult{
		Tool:    "WriteFile",
		Success: true,
		Output:  fmt.Sprintf("wrote %d bytes", len(content)),
		Path:    absPath,
	}
	if temporary {
		result.IsTemporary = true
		result.TempDir = tempDir
	}
	return result
}

// listDir 列出目录。
//
// 职责：列出指定目录下的条目，区分目录/文件，附带文件大小。
// 参数：
//   - args：含 "path" 字段，空则取工作目录。
//
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

// searchInFiles 在文件中搜索（跨平台，纯Go实现）。
//
// 职责：递归搜索指定目录下白名单扩展名文件中包含 pattern 的行，
//
//	大小写不敏感，跳过 .git/node_modules/vendor，最多返回 500 条。
//
// 参数：
//   - args：含 "pattern" 字段，可选 "dir"（默认 "."）。
//
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
