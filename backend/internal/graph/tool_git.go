package graph

// 本文件承载 Git 类工具实现（P3-2）：GitDiff / GitStatus / GitLog / GitBlame。
// 方法挂在 *ToolExecutor 上，与 tool_files.go / tool_command.go 同属 graph 包。
// 所有工具均通过在工作目录执行 git 命令实现，并做沙箱路径校验。

import (
	"fmt"
	"os/exec"
	"unicode"
)

// gitMaxOutput 限制 Git 工具输出长度，避免 LLM 上下文爆炸。
const gitMaxOutput = 10000

// runGit 在工作目录执行 git 命令并返回截断输出。
func (e *ToolExecutor) runGit(args []string, path string) *ToolResult {
	cmd := exec.Command("git", args...)
	cmd.Dir = e.workDir
	out, err := cmd.CombinedOutput()
	toolName := "Git" + capitalizeFirst(args[0])
	result := &ToolResult{Tool: toolName, Path: path}
	if err != nil && len(out) == 0 {
		result.Error = err.Error()
		return result
	}
	result.Success = true
	result.Output = truncateGitOutput(string(out))
	return result
}

// capitalizeFirst 将字符串首字母大写。
func capitalizeFirst(s string) string {
	if s == "" {
		return ""
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// truncateGitOutput 截断超长 Git 输出。
func truncateGitOutput(s string) string {
	if len(s) > gitMaxOutput {
		return s[:gitMaxOutput] + "\n... (truncated)"
	}
	return s
}

// gitDiff 输出工作区与暂存区、或指定提交的差异。
// 参数：
//   - target：可选，空表示未暂存变更；"--staged" 表示暂存区；"commit...commit" 表示历史对比。
func (e *ToolExecutor) gitDiff(args map[string]any) *ToolResult {
	target, _ := args["target"].(string)
	path, _ := args["path"].(string)
	gitArgs := []string{"diff"}
	if target != "" {
		gitArgs = append(gitArgs, target)
	}
	if path != "" {
		absPath, err := e.resolvePathWithSandbox(path)
		if err != nil {
			return &ToolResult{Tool: "GitDiff", Path: absPath, Error: err.Error()}
		}
		gitArgs = append(gitArgs, absPath)
	}
	return e.runGit(gitArgs, path)
}

// gitStatus 输出工作区状态（简短格式）。
func (e *ToolExecutor) gitStatus(args map[string]any) *ToolResult {
	return e.runGit([]string{"status", "-sb"}, "")
}

// gitLog 输出提交历史（默认最近 20 条，可自定义 limit）。
func (e *ToolExecutor) gitLog(args map[string]any) *ToolResult {
	limit := 20
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}
	path, _ := args["path"].(string)
	gitArgs := []string{"log", "--oneline", "-n", fmt.Sprintf("%d", limit)}
	if path != "" {
		absPath, err := e.resolvePathWithSandbox(path)
		if err != nil {
			return &ToolResult{Tool: "GitLog", Path: absPath, Error: err.Error()}
		}
		gitArgs = append(gitArgs, "--", absPath)
	}
	return e.runGit(gitArgs, path)
}

// gitBlame 输出指定文件每行最后修改者。
func (e *ToolExecutor) gitBlame(args map[string]any) *ToolResult {
	path, _ := args["path"].(string)
	if path == "" {
		return &ToolResult{Tool: "GitBlame", Error: "path is required"}
	}
	absPath, err := e.resolvePathWithSandbox(path)
	if err != nil {
		return &ToolResult{Tool: "GitBlame", Path: absPath, Error: err.Error()}
	}
	gitArgs := []string{"blame", "--line-porcelain", absPath}
	return e.runGit(gitArgs, absPath)
}

