// Package tool 定义并实现了内置工具（ReadFile / WriteFile / ListDir / SearchInFiles / RunCommand / HTTP / Git 等）的执行逻辑。
// 这些工具通过 Executor 提供统一入口，每个方法接收 map[string]any 参数并返回 *Result 结果对象。
package tool

import (
	// bytes 用于在 RunCommand 中缓存命令的标准输出与标准错误。
	"bytes"
	// context 用于控制工具执行的生命周期（超时、取消）。
	"context"
	// encoding/json 用于将 HTTP POST 请求体序列化为 JSON。
	"encoding/json"
	// fmt 用于格式化输出字符串、错误信息以及日志字段。
	"fmt"
	// io 用于读取 HTTP 响应体的限流读取器。
	"io"
	// log/slog 用于记录进程树清理失败等调试信息。
	"log/slog"
	// net/http 用于执行 HTTPGet 与 HTTPPost 网络请求。
	"net/http"
	// os 用于文件读写、环境变量读取以及目录创建。
	"os"
	// os/exec 用于执行外部命令（RunCommand 与 Git 工具）。
	"os/exec"
	// path/filepath 用于路径解析、清洗以及相对路径计算。
	"path/filepath"
	// runtime 用于根据操作系统选择命令解释器与进程管理策略。
	"runtime"
	// slices 用于判断文件扩展名是否命中搜索白名单。
	"slices"
	// strconv 用于将进程 PID 转换为字符串以执行 taskkill/kill。
	"strconv"
	// strings 用于字符串切分、大小写转换、前缀判断与拼接。
	"strings"
	// time 用于超时时间计算与 context.WithTimeout。
	"time"
	// unicode 用于 Git 子命令首字母大写转换。
	"unicode"
)

// 以下输入结构体用于 JSON Schema 生成与运行时参数反序列化。
type (
	// readFileInput 表示 ReadFile 工具的输入参数。
	readFileInput struct {
		// Path 为待读取文件的相对或绝对路径。
		Path string `json:"path"`
		// Offset 为起始行号（1-based，默认 1）。
		Offset float64 `json:"offset,omitempty"`
		// Limit 为本次读取的最大行数（默认 defaultReadFileLimit 行）。
		Limit float64 `json:"limit,omitempty"`
	}
	// writeFileInput 表示 WriteFile 工具的输入参数。
	writeFileInput struct {
		// Path 为待写入文件的目标路径。
		Path string `json:"path"`
		// Content 为要写入文件的文本内容。
		Content string `json:"content"`
		// Temporary 为 true 时表示写入到当前会话的临时目录，受会话上下文约束。
		Temporary bool `json:"temporary"`
	}
	// listDirInput 表示 ListDir 工具的输入参数。
	listDirInput struct {
		// Path 为待列出目录的路径，空字符串时默认使用当前工作目录。
		Path string `json:"path"`
	}
	// runCommandInput 表示 RunCommand 工具的输入参数。
	runCommandInput struct {
		// Command 为要执行的命令字符串。
		Command string `json:"command"`
		// Timeout 为命令最大执行时间（秒），0 或缺失时使用默认超时。
		Timeout float64 `json:"timeout,omitempty"`
	}
	// searchInFilesInput 表示 SearchInFiles 工具的输入参数。
	searchInFilesInput struct {
		// Pattern 为在文件中搜索的文本模式（大小写不敏感）。
		Pattern string `json:"pattern"`
		// Dir 为搜索起始目录，空字符串时默认使用当前工作目录。
		Dir string `json:"dir,omitempty"`
	}
	// httpGetInput 表示 HTTPGet 工具的输入参数。
	httpGetInput struct {
		// URL 为请求的完整目标地址。
		URL string `json:"url"`
		// Headers 为可选的请求头映射。
		Headers map[string]string `json:"headers,omitempty"`
		// Timeout 为可选的请求超时时间（秒）。
		Timeout float64 `json:"timeout,omitempty"`
	}
	// httpPostInput 表示 HTTPPost 工具的输入参数。
	httpPostInput struct {
		// URL 为请求的完整目标地址。
		URL string `json:"url"`
		// Headers 为可选的请求头映射。
		Headers map[string]string `json:"headers,omitempty"`
		// Body 为请求体，支持任意 JSON 可序列化结构或字符串。
		Body map[string]any `json:"body,omitempty"`
		// Timeout 为可选的请求超时时间（秒）。
		Timeout float64 `json:"timeout,omitempty"`
	}
	// gitDiffInput 表示 GitDiff 工具的输入参数。
	gitDiffInput struct {
		// Target 为可选的 diff 目标引用（分支、提交等）。
		Target string `json:"target,omitempty"`
		// Path 为可选的限制 diff 范围的文件或目录路径。
		Path string `json:"path,omitempty"`
	}
	// gitStatusInput 表示 GitStatus 工具的输入参数（当前无字段）。
	gitStatusInput struct{}
	// gitLogInput 表示 GitLog 工具的输入参数。
	gitLogInput struct {
		// Limit 为返回的最大提交条数，默认 20。
		Limit float64 `json:"limit,omitempty"`
		// Path 为可选的限制日志范围的文件或目录路径。
		Path string `json:"path,omitempty"`
	}
	// gitBlameInput 表示 GitBlame 工具的输入参数。
	gitBlameInput struct {
		// Path 为待执行 blame 的文件路径。
		Path string `json:"path"`
	}
	// refreshProjectDocInput 表示 RefreshProjectDoc 工具的输入参数（当前无字段）。
	refreshProjectDocInput struct{}
	// callSubAgentInput 表示 call_sub_agent 工具的输入参数。
	// 与 subagent 包内部入参结构保持一致（按字段名 JSON 解码）。
	callSubAgentInput struct {
		// RoleID 为被调用子 Agent 的角色标识。
		RoleID string `json:"role_id" description:"被调用子 Agent 的角色标识，从工具描述的角色清单中选。默认走 domain，仅单函数级、单文件、领域明确的任务直派固定助手。"`
		// Task 为交给子 Agent 执行的自包含任务描述。
		Task string `json:"task" description:"自包含任务描述（<=500 字）：背景、目标、相关文件路径、前置结论与验收标准。子 Agent 看不到当前对话历史，规格原文走 WriteSharedMemory。"`
		// Domain 为领域分类简称（如 金融/认证/UI/数据库），仅 role_id="domain" 时有效，
		// 用于 DomainAgent 展示名（如"金融领域Agent"）。空时回退到 task 首行兜底。
		Domain string `json:"domain" description:"领域分类简称（如 金融/认证/UI/数据库/配置），仅 role_id=domain 时填，用于子 Agent 展示名。固定助手忽略。"`
		// Responsibility 为职责边界描述，仅 role_id="domain" 时必填，
		// 注入 DomainAgent 系统提示词头部，防止长 ReAct 循环中越界实现他域文件。
		Responsibility string `json:"responsibility" description:"职责边界（<=200 字）：写明该领域 Agent 负责哪些文件/模块、不碰哪些。role_id=domain 时必填，会注入子 Agent 系统提示词。"`
		// Mode 为派发执行模式（TODO #29）：react（默认）/ reflection / plan_execute。
		// 空串按 react 处理，零行为变化。
		Mode string `json:"mode" description:"派发执行模式（可选）：react（默认，直接 ReAct 循环）/ reflection（产出后对照验收标准自检，不达标带反馈重试）/ plan_execute（先出步骤计划再逐步执行）。琐碎单步任务省略；正确性敏感任务用 reflection；多步骤长任务用 plan_execute。"`
	}
)

// ---- ReadFile（读取文件） ----

// defaultReadFileLimit 是 ReadFile 未显式指定 limit 时的默认读取行数。
// 与 ReadFileMaxChars 字符上限共同生效（先到先截），保证单页输出可控。
const defaultReadFileLimit = 120

// readFile 按行区间读取指定路径的文本内容（1-based offset + limit 分页）。
// 输出带行号（cat -n 风格），顶部首行放置分页头（总行数/本页区间/下一页 offset），
// 放在顶部是因为写历史的 tool_output_history_max_runes 截断保头不保尾，
// 页脚式分页信息会在历史里丢失，导致模型忘记如何翻页。
// 超出 ReadFileMaxChars 时按行截停，分页头中给出实际返回区间，保证"继续读"指引始终准确。
func (e *Executor) readFile(args map[string]any) *Result {
	// 从参数中取出 path，要求必须是非空字符串。
	path, ok := args["path"].(string)
	if !ok || path == "" {
		// 参数缺失或为空时直接返回错误结果。
		return &Result{Tool: "ReadFile", Error: "path is required"}
	}
	// 解析路径并校验沙箱约束。
	absPath, err := e.resolvePathWithSandbox(path)
	if err != nil {
		// 路径越界或解析失败时返回错误。
		return &Result{Tool: "ReadFile", Path: absPath, Error: err.Error()}
	}
	// 读取文件全部字节。
	data, err := os.ReadFile(absPath)
	if err != nil {
		// 文件不存在或无权限时返回错误。
		return &Result{Tool: "ReadFile", Path: absPath, Error: err.Error()}
	}

	// 解析分页参数：offset 为 1-based 起始行，limit 为本页最大行数。
	offset := 1
	if v, ok := args["offset"].(float64); ok && v > 0 {
		offset = int(v)
	}
	limit := defaultReadFileLimit
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}

	// 按行切分文件内容。
	lines := strings.Split(string(data), "\n")
	total := len(lines)
	// offset 越界时直接报错并告知总行数，引导模型给出合法区间。
	if offset > total {
		return &Result{Tool: "ReadFile", Path: absPath,
			Error: fmt.Sprintf("offset %d 超出文件总行数 %d，请用 1-%d 之间的 offset", offset, total, total)}
	}
	// 计算本页结束行（含），不超出文件末尾。
	end := offset - 1 + limit
	if end > total {
		end = total
	}

	// 逐行拼接待行号内容；超出字符上限时提前截停，actualEnd 记录实际返回到哪一行。
	maxChars := e.agentConfig().ReadFileMaxChars
	var b strings.Builder
	actualEnd := offset - 1
	for i := offset - 1; i < end; i++ {
		line := fmt.Sprintf("%6d\t%s\n", i+1, lines[i])
		// 至少保留第一行，避免 maxChars 极小时返回空内容。
		if b.Len()+len(line) > maxChars && actualEnd >= offset {
			break
		}
		b.WriteString(line)
		actualEnd = i + 1
	}

	// 分页头放在输出顶部：历史截断保留头部，模型始终知道文件规模与下一页起点。
	header := fmt.Sprintf("[共 %d 行 | 本页 %d-%d 行", total, offset, actualEnd)
	if actualEnd < total {
		header += fmt.Sprintf(" | 继续读请 ReadFile(path, offset=%d)]\n", actualEnd+1)
	} else {
		header += " | 已到文件末尾]\n"
	}
	// 返回成功结果，包含文件路径与分页头 + （可能截停后的）本页内容。
	return &Result{Tool: "ReadFile", Success: true, Output: header + b.String(), Path: absPath}
}

// ---- WriteFile（写入文件） ----

// writeFile 将内容写入指定路径，支持普通文件与会话临时文件两种模式。
func (e *Executor) writeFile(ctx context.Context, args map[string]any) *Result {
	// 从参数中取出各字段，缺失时使用零值。
	path, _ := args["path"].(string)
	content, _ := args["content"].(string)
	temporary, _ := args["temporary"].(bool)
	allowSpaces, _ := args["allow_spaces"].(bool)

	// path 为空时不允许写入，直接返回错误。
	if path == "" {
		return &Result{Tool: "WriteFile", Error: "path is required"}
	}

	// 调用写保护守卫进行策略校验（例如禁止写入某些目录、检查路径特征）。
	if err := e.guards.CheckWrite(path, content, allowSpaces); err != nil {
		return &Result{Tool: "WriteFile", Path: path, Error: err.Error()}
	}

	// 解析目标路径。
	absPath := e.resolvePath(path)
	// tempDir 用于保存当前会话的临时目录路径，仅在 temporary 模式下使用。
	tempDir := ""
	if temporary {
		// 临时文件需要会话上下文，否则无法确定保存位置。
		sessionID := SessionIDFromContext(ctx)
		if sessionID == "" {
			return &Result{Tool: "WriteFile", Error: "temporary file requires a session context"}
		}
		// 获取当前会话专属的临时目录。
		tempDir = e.sessionTempDir(sessionID)
		// 清洗传入路径，防止 .. 等导致越界。
		cleanPath := filepath.Clean(path)
		if filepath.IsAbs(cleanPath) {
			// 临时文件不接受绝对路径，仅使用文件名部分。
			cleanPath = filepath.Base(cleanPath)
		}
		// 组合为临时目录下的最终绝对路径。
		absPath = filepath.Join(tempDir, cleanPath)
	} else {
		// 非临时文件需要额外校验写入路径（禁止写到系统目录等）。
		if err := e.sanitizeWritePath(absPath); err != nil {
			return &Result{Tool: "WriteFile", Path: absPath, Error: err.Error()}
		}
		// 角色级写沙箱（Layer 4）：角色配了 allowed_write_paths 时进一步限制写入范围。
		// 未配置的角色（含 MetaAgent/DomainAgent/默认叶子助手）roleWritePaths 返空，跳过。
		if err := e.enforceRoleWritePath(ctx, absPath); err != nil {
			return &Result{Tool: "WriteFile", Path: absPath, Error: err.Error()}
		}
	}

	// 确保目标文件所在目录存在，不存在时递归创建。
	dir := filepath.Dir(absPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return &Result{Tool: "WriteFile", Path: absPath, Error: "mkdir: " + err.Error()}
	}
	// 执行文件写入。
	if err := os.WriteFile(absPath, []byte(content), 0644); err != nil {
		return &Result{Tool: "WriteFile", Path: absPath, Error: err.Error()}
	}

	// 构造成功结果对象。
	result := &Result{
		Tool:    "WriteFile",
		Success: true,
		Output:  fmt.Sprintf("wrote %d bytes", len(content)),
		Path:    absPath,
	}
	// 若为临时文件，标记结果并记录临时目录，便于会话结束后清理。
	if temporary {
		result.IsTemporary = true
		result.TempDir = tempDir
	}
	// 返回结果。
	return result
}

// ---- ListDir（列出目录） ----

// listDir 列出指定目录下的条目，并标注每个条目的类型与大小。
func (e *Executor) listDir(args map[string]any) *Result {
	// 从参数中读取目录路径，空字符串时默认当前目录。
	path, _ := args["path"].(string)
	if path == "" {
		path = "."
	}
	// 解析路径并校验沙箱约束。
	absPath, err := e.resolvePathWithSandbox(path)
	if err != nil {
		return &Result{Tool: "ListDir", Path: absPath, Error: err.Error()}
	}
	// 读取目录下所有条目。
	entries, err := os.ReadDir(absPath)
	if err != nil {
		return &Result{Tool: "ListDir", Path: absPath, Error: err.Error()}
	}
	// lines 用于拼接最终的目录列表文本。
	var lines []string
	// 遍历每个目录条目。
	for _, entry := range entries {
		// 默认前缀表示普通文件。
		prefix := "  "
		if entry.IsDir() {
			// 目录条目使用 D 前缀标识。
			prefix = "D "
		}
		// 获取条目详细信息，失败时不影响列出名称。
		info, _ := entry.Info()
		// size 默认空，若成功获取信息则格式化为 8 位宽数字字符串。
		size := ""
		if info != nil {
			size = fmt.Sprintf("%8d", info.Size())
		}
		// 将类型前缀、大小、名称组合成一行。
		lines = append(lines, fmt.Sprintf("%s %s %s", prefix, size, entry.Name()))
	}
	// 返回拼接后的目录列表。
	return &Result{Tool: "ListDir", Success: true, Output: strings.Join(lines, "\n"), Path: absPath}
}

// ---- SearchInFiles（文件内搜索） ----

// searchInFiles 在指定目录下按扩展名白名单递归搜索包含指定模式的文本行。
func (e *Executor) searchInFiles(args map[string]any) *Result {
	// 从参数中读取搜索模式与目录，目录缺失时使用当前目录。
	pattern, _ := args["pattern"].(string)
	dir, _ := args["dir"].(string)
	if dir == "" {
		dir = "."
	}
	// 解析并校验搜索目录的沙箱约束。
	absDir, err := e.resolvePathWithSandbox(dir)
	if err != nil {
		return &Result{Tool: "SearchInFiles", Path: absDir, Error: err.Error()}
	}
	// 将搜索模式转为小写，实现大小写不敏感匹配。
	patternLower := strings.ToLower(pattern)
	// exts 定义允许搜索的文件扩展名白名单。
	exts := []string{".go", ".py", ".js", ".ts", ".java", ".yaml", ".yml", ".md", ".txt", ".json", ".toml", ".css", ".html"}
	// lines 保存所有命中的搜索结果行。
	var lines []string
	// 递归遍历目录下的文件与目录。
	filepath.WalkDir(absDir, func(path string, d os.DirEntry, err error) error {
		// 遇到遍历错误或结果已满 500 条时跳过当前路径。
		if err != nil || len(lines) > 500 {
			return nil
		}
		// 对目录进行特殊处理：跳过常见的依赖/版本控制目录。
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		// 获取文件扩展名并转为小写。
		ext := strings.ToLower(filepath.Ext(d.Name()))
		// 如果扩展名不在白名单中则跳过该文件。
		if !slices.Contains(exts, ext) {
			return nil
		}
		// 读取文件全部内容。
		data, err := os.ReadFile(path)
		if err != nil {
			// 无权限或读取失败时忽略该文件。
			return nil
		}
		// 将字节转换为字符串以便逐行匹配。
		content := string(data)
		// 按行切分并逐行检查是否包含模式。
		for i, line := range strings.Split(content, "\n") {
			if strings.Contains(strings.ToLower(line), patternLower) {
				// 计算相对路径以提升结果可读性。
				relPath, _ := filepath.Rel(absDir, path)
				// 记录相对路径、行号、去空白后的内容。
				lines = append(lines, fmt.Sprintf("%s:%d: %s", relPath, i+1, strings.TrimSpace(line)))
				// 命中行数超过 500 时提前停止遍历当前文件。
				if len(lines) > 500 {
					break
				}
			}
		}
		return nil
	})
	// 构造结果对象。
	result := &Result{
		Tool:   "SearchInFiles",
		Path:   absDir,
		Output: strings.Join(lines, "\n"),
	}
	// 只要存在命中行，就将结果标记为成功。
	if len(lines) > 0 {
		result.Success = true
	}
	// 若输出过长，按配置的最大字符数截断。
	if maxChars := e.agentConfig().ReadFileMaxChars; len(result.Output) > maxChars {
		result.Output = result.Output[:maxChars] + "\n... (truncated)"
	}
	// 返回搜索结果。
	return result
}

// ---- RunCommand（执行命令） ----

// parseMkdirDir 解析形如 "mkdir -p /some/dir" 的命令字符串，尝试提取目标目录。
func parseMkdirDir(cmd string) string {
	// 去除首尾空白。
	cmd = strings.TrimSpace(cmd)
	// 如果命令不是以 mkdir 开头，则返回空表示无法解析。
	if !strings.HasPrefix(cmd, "mkdir") {
		return ""
	}
	// 去掉 "mkdir" 前缀。
	rest := strings.TrimSpace(strings.TrimPrefix(cmd, "mkdir"))
	// 去掉可选的 "-p" 参数。
	rest = strings.TrimSpace(strings.TrimPrefix(rest, "-p"))
	// 剩余部分即目标目录。
	return rest
}

// runCommand 执行外部命令或处理 mkdir 快捷命令，并返回输出结果。
func (e *Executor) runCommand(ctx context.Context, args map[string]any) *Result {
	// 从参数中读取命令字符串，空字符串表示未提供命令。
	cmdStr, _ := args["command"].(string)
	if cmdStr == "" {
		return &Result{Tool: "RunCommand", Error: "command is required"}
	}

	// 调用命令守卫检查命令是否被策略禁止。
	if blocked, reason := e.guards.CheckCommand(cmdStr); blocked {
		return &Result{Tool: "RunCommand", Error: reason}
	}

	// 如果命令是 mkdir，则直接本地处理而不调用外部 shell。
	if dir := parseMkdirDir(cmdStr); dir != "" {
		// 解析目标目录路径。
		absDir := e.resolvePath(dir)
		// 校验目录是否允许写入。
		if err := e.sanitizeWritePath(absDir); err != nil {
			return &Result{Tool: "RunCommand", Error: err.Error()}
		}
		// 递归创建目录。
		if err := os.MkdirAll(absDir, 0755); err != nil {
			return &Result{Tool: "RunCommand", Error: "mkdir: " + err.Error()}
		}
		// 返回目录创建成功的结果。
		return &Result{Tool: "RunCommand", Success: true, Output: "created: " + absDir}
	}

	// 默认使用 Executor 自身的超时时间。
	timeout := e.timeout
	// 若参数中显式提供了超时秒数且为正数，则覆盖默认超时。
	if t, ok := args["timeout"].(float64); ok && t > 0 {
		timeout = time.Duration(t) * time.Second
	}
	// 限制超时不超过 Agent 配置中允许的最大命令执行时间。
	if maxTimeout := time.Duration(e.agentConfig().RunCommandTimeoutSec) * time.Second; timeout > maxTimeout {
		timeout = maxTimeout
	}

	// 基于当前上下文创建带超时的子上下文。
	ctx, cancel := context.WithTimeout(ctx, timeout)
	// 函数返回时取消上下文，释放相关资源。
	defer cancel()

	// cmd 为待执行的外部命令对象。
	var cmd *exec.Cmd
	// 根据操作系统选择命令解释器：Windows 使用 powershell -Command，类 Unix 使用 sh -c。
	// powershell 比 cmd /c 更可靠：Write-Host 输出到 stdout 可捕获；$ 变量不会被错误展开；
	// 复合管道命令正确执行。
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "powershell", "-NoLogo", "-NoProfile", "-Command", cmdStr)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", cmdStr)
	}
	// 设置命令的工作目录为当前 Agent 工作目录。
	cmd.Dir = e.readWorkDir()
	// 若存在会话上下文，将临时目录通过环境变量暴露给子进程。
	if sessionID := SessionIDFromContext(ctx); sessionID != "" {
		cmd.Env = append(os.Environ(), "BMA_SESSION_TEMP_DIR="+e.sessionTempDir(sessionID))
	}

	// stdout 与 stderr 用于缓存命令输出。
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// 执行命令，Windows 下使用树形进程kill以处理超时。
	err := runCommandWithTreeKill(ctx, cmd)

	// 清理并合并标准输出与标准错误。
	output := SanitizeBytes(stdout.Bytes())
	if stderr.Len() > 0 {
		output += "\n[stderr]\n" + SanitizeBytes(stderr.Bytes())
	}
	// 检测输出是否暗示端口占用，并追加友好提示。
	if hint := detectPortConflictHint(cmdStr, output); hint != "" {
		output += "\n[port-conflict]\n" + hint
	}
	// 若输出超过最大限制，则截断并追加提示。
	if maxOut := e.agentConfig().RunCommandMaxOutput; len(output) > maxOut {
		output = output[:maxOut] + "\n... (truncated)"
	}

	// 构造结果对象，默认 Success 由命令是否出错决定。
	result := &Result{
		Tool:    "RunCommand",
		Path:    e.readWorkDir(),
		Output:  output,
		Success: err == nil,
	}
	// 若命令执行出错，将错误信息写入结果。
	if err != nil {
		result.Error = err.Error()
	}
	// 返回命令执行结果。
	return result
}

// runCommandWithTreeKill 运行命令并在超时或取消时尝试结束整个进程树。
func runCommandWithTreeKill(ctx context.Context, cmd *exec.Cmd) error {
	// 非 Windows 平台直接调用 cmd.Run，由 context 驱动取消。
	if runtime.GOOS != "windows" {
		return cmd.Run()
	}
	// Windows 平台需要手动启动并等待，以便在超时后杀掉进程树。
	if err := cmd.Start(); err != nil {
		return err
	}
	// done 通道用于接收命令结束信号，缓冲 1 防止 goroutine 泄漏。
	done := make(chan error, 1)
	// 启动 goroutine 等待命令结束，并将错误发送到 done。
	go func() {
		done <- cmd.Wait()
	}()
	// 同时监听上下文取消与命令结束事件。
	select {
	case <-ctx.Done():
		// 上下文已取消（通常因超时），若进程存在则杀掉整个进程树。
		if cmd.Process != nil {
			killProcessTree(cmd.Process.Pid)
		}
		// 等待 Wait 返回，避免 goroutine 泄漏与僵尸进程。
		<-done
		// 返回上下文错误。
		return ctx.Err()
	case err := <-done:
		// 命令自行结束，返回其执行错误（如有）。
		return err
	}
}

// killProcessTree 根据操作系统杀掉指定 PID 及其子进程。
func killProcessTree(pid int) {
	// Windows 使用 taskkill /T /F /PID 强制结束进程树。
	if runtime.GOOS == "windows" {
		if err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run(); err != nil {
			slog.Debug("kill process tree failed", slog.Int("pid", pid), slog.String("error", err.Error()))
		}
	} else {
		// 类 Unix 使用 kill -9 -PGID 结束整个进程组。
		if err := exec.Command("kill", "-9", "-"+strconv.Itoa(pid)).Run(); err != nil {
			slog.Debug("kill process tree failed", slog.Int("pid", pid), slog.String("error", err.Error()))
		}
	}
}

// detectPortConflictHint 检查命令输出中是否包含端口占用相关关键词，并返回中文提示。
func detectPortConflictHint(cmdStr, output string) string {
	// 空输出无需检测。
	if output == "" {
		return ""
	}
	// 将输出转为小写以进行不敏感匹配。
	lowerOut := strings.ToLower(output)
	// conflictPatterns 列出常见的端口占用错误特征字符串。
	conflictPatterns := []string{
		"address already in use",
		"errno -98",
		"eaddrinuse",
		"bind: an attempt was made",
		"通常每个套接字地址",
		"端口已被占用",
		"端口被占用",
		"only one usage of each socket",
		"no permission to use port",
	}
	// matched 保存第一个命中的模式，用于后续提示。
	matched := ""
	// 遍历所有模式并检查输出中是否包含。
	for _, p := range conflictPatterns {
		if strings.Contains(lowerOut, p) {
			matched = p
			// 命中后立即停止，只取第一个。
			break
		}
	}
	// 没有任何模式命中则返回空。
	if matched == "" {
		return ""
	}
	// 尝试从命令字符串中提取端口号。
	port := extractPortFromCommand(cmdStr)
	// 构造中文提示，说明检测到端口占用。
	hint := fmt.Sprintf("检测到端口占用错误（模式: %q）", matched)
	if port != "" {
		// 若成功提取端口，提示用户换用其他端口或杀掉占用进程。
		hint += fmt.Sprintf("。端口 %s 已被占用，请换用 8001/8002/.../8009 重试，", port)
	} else {
		hint += "。请换用其他端口（如 8001-8009）重试，"
	}
	// 追加查找与释放端口的操作指引。
	hint += "或先执行 `netstat -ano | findstr :<port>` 找到占用进程 PID，再 `taskkill /F /PID <pid>` 释放。" +
		"注意：长运行服务器（http.server/flask/node dev server）会被系统拦截，建议改用 `node --check` / `python -m py_compile` 做语法验证。"
	// 返回完整的端口占用提示。
	return hint
}

// extractPortFromCommand 从命令字符串中尝试提取可能绑定的端口号。
func extractPortFromCommand(cmdStr string) string {
	// 空命令直接返回空。
	if cmdStr == "" {
		return ""
	}
	// 优先匹配 "--port" 参数形式。
	if idx := strings.Index(strings.ToLower(cmdStr), "--port"); idx >= 0 {
		// 取 "--port" 之后的子串。
		rest := cmdStr[idx+6:]
		// 去掉可能存在的空格或等号分隔符。
		rest = strings.TrimLeft(rest, " =")
		// num 用于累积连续数字。
		num := ""
		// 遍历后续字符提取数字。
		for _, r := range rest {
			if r >= '0' && r <= '9' {
				num += string(r)
				// 端口号最多 5 位，达到上限即可停止。
				if len(num) >= 5 {
					break
				}
			} else {
				// 一旦遇到非数字且已累积到数字，即可停止。
				if num != "" {
					break
				}
			}
		}
		// 至少 2 位数字才认为是端口号。
		if len(num) >= 2 {
			return num
		}
	}
	// 其次匹配 "-p " 参数形式。
	if idx := strings.Index(cmdStr, "-p "); idx >= 0 {
		// 取 "-p " 之后的子串。
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
	// 兜底匹配冒号形式（如 :8000）。
	idx := strings.Index(cmdStr, ":")
	for idx >= 0 {
		// 取冒号之后的子串。
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
		// 端口号长度在 2 到 5 位之间才接受。
		if len(num) >= 2 && len(num) <= 5 {
			return num
		}
		// 查找下一个冒号继续尝试。
		next := strings.Index(cmdStr[idx+1:], ":")
		if next < 0 {
			break
		}
		idx = idx + 1 + next
	}
	// 最后尝试提取命令中最后一个长度合理的连续数字作为端口候选。
	var lastNum string
	var inNum bool
	curNum := ""
	for _, r := range cmdStr {
		if r >= '0' && r <= '9' {
			curNum += string(r)
			inNum = true
			// 超过 5 位则放弃当前数字段。
			if len(curNum) > 5 {
				curNum = ""
				inNum = false
			}
		} else {
			// 遇到非数字且当前数字段长度合法，则记录为 lastNum。
			if inNum && len(curNum) >= 2 {
				lastNum = curNum
			}
			curNum = ""
			inNum = false
		}
	}
	// 若遍历结束正处于数字段且长度合法，也作为候选。
	if inNum && len(curNum) >= 2 {
		lastNum = curNum
	}
	// 返回最后一个候选数字，可能为空。
	return lastNum
}

// ---- HTTP 工具 ----

// parseStringMap 将任意类型的输入转换为 map[string]string，用于解析 headers 等字段。
func parseStringMap(raw any) map[string]string {
	// 空输入直接返回 nil。
	if raw == nil {
		return nil
	}
	// 根据实际类型分支处理。
	switch m := raw.(type) {
	case map[string]string:
		// 已经是目标类型，直接返回。
		return m
	case map[string]any:
		// 将 map[string]any 的每个值转为字符串。
		out := make(map[string]string, len(m))
		for k, v := range m {
			if s, ok := v.(string); ok {
				// 值为字符串时直接使用。
				out[k] = s
			} else {
				// 非字符串值使用 fmt.Sprint 进行默认格式化。
				out[k] = fmt.Sprint(v)
			}
		}
		return out
	}
	// 其他不支持的类型返回 nil。
	return nil
}

// httpGet 执行 HTTP GET 请求并返回响应状态码与体内容。
func (e *Executor) httpGet(ctx context.Context, args map[string]any) *Result {
	// 读取目标 URL，空 URL 直接返回错误。
	url, _ := args["url"].(string)
	if url == "" {
		return &Result{Tool: "HTTPGet", Error: "url is required"}
	}
	// 解析可选的请求头。
	headers := parseStringMap(args["headers"])
	// 默认使用 Executor 的超时时间。
	timeout := e.timeout
	// 若参数中显式设置超时则覆盖。
	if t, ok := args["timeout"].(float64); ok && t > 0 {
		timeout = time.Duration(t) * time.Second
	}
	// 创建带超时的子上下文。
	ctx, cancel := context.WithTimeout(ctx, timeout)
	// 函数返回时取消上下文。
	defer cancel()
	// 构造 GET 请求。
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return &Result{Tool: "HTTPGet", Path: url, Error: err.Error()}
	}
	// 设置请求头。
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// 发送请求。
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return &Result{Tool: "HTTPGet", Path: url, Error: err.Error()}
	}
	// 函数返回时关闭响应体，防止连接泄漏。
	defer resp.Body.Close()
	// 读取响应体，限制最大 1MB。
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	// 拼接状态码与响应体作为输出。
	output := fmt.Sprintf("HTTP %d\n%s", resp.StatusCode, string(body))
	// 若输出超过 10000 字符则截断。
	if len(output) > 10000 {
		output = output[:10000] + "\n... (truncated)"
	}
	// 返回结果，2xx 视为成功。
	return &Result{
		Tool:    "HTTPGet",
		Success: resp.StatusCode >= 200 && resp.StatusCode < 300,
		Output:  output,
		Path:    url,
	}
}

// httpPost 执行 HTTP POST 请求，支持字符串或 JSON 对象作为请求体。
func (e *Executor) httpPost(ctx context.Context, args map[string]any) *Result {
	// 读取目标 URL，空 URL 直接返回错误。
	url, _ := args["url"].(string)
	if url == "" {
		return &Result{Tool: "HTTPPost", Error: "url is required"}
	}
	// 解析可选的请求头。
	headers := parseStringMap(args["headers"])
	// bodyBytes 用于保存最终序列化后的请求体字节。
	var bodyBytes []byte
	// 若参数中包含 body，则根据类型处理。
	if raw, ok := args["body"]; ok {
		switch v := raw.(type) {
		case string:
			// body 为字符串时直接转字节。
			bodyBytes = []byte(v)
		default:
			// 其他类型序列化为 JSON。
			b, err := json.Marshal(v)
			if err != nil {
				return &Result{Tool: "HTTPPost", Path: url, Error: "marshal body: " + err.Error()}
			}
			bodyBytes = b
			// 若请求头中未设置 Content-Type，则默认设置为 application/json。
			if _, ok := headers["Content-Type"]; !ok {
				if headers == nil {
					headers = make(map[string]string)
				}
				headers["Content-Type"] = "application/json"
			}
		}
	}
	// 默认使用 Executor 的超时时间。
	timeout := e.timeout
	// 若参数中显式设置超时则覆盖。
	if t, ok := args["timeout"].(float64); ok && t > 0 {
		timeout = time.Duration(t) * time.Second
	}
	// 创建带超时的子上下文。
	ctx, cancel := context.WithTimeout(ctx, timeout)
	// 函数返回时取消上下文。
	defer cancel()
	// 构造 POST 请求，将 bodyBytes 作为请求体。
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return &Result{Tool: "HTTPPost", Path: url, Error: err.Error()}
	}
	// 设置请求头。
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// 发送请求。
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return &Result{Tool: "HTTPPost", Path: url, Error: err.Error()}
	}
	// 函数返回时关闭响应体。
	defer resp.Body.Close()
	// 读取响应体，限制最大 1MB。
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	// 拼接状态码与响应体作为输出。
	output := fmt.Sprintf("HTTP %d\n%s", resp.StatusCode, string(body))
	// 若输出超过 10000 字符则截断。
	if len(output) > 10000 {
		output = output[:10000] + "\n... (truncated)"
	}
	// 返回结果，2xx 视为成功。
	return &Result{
		Tool:    "HTTPPost",
		Success: resp.StatusCode >= 200 && resp.StatusCode < 300,
		Output:  output,
		Path:    url,
	}
}

// ---- Git 工具 ----

// gitMaxOutput 限制 Git 工具返回文本的最大长度。
const gitMaxOutput = 10000

// runGit 执行 git 子命令并封装为 *Result 返回。
func (e *Executor) runGit(args []string, path string) *Result {
	// 构造 git 命令，参数已包含子命令与选项。
	cmd := exec.Command("git", args...)
	// 设置命令的工作目录。
	cmd.Dir = e.readWorkDir()
	// 执行命令并捕获合并后的标准输出与错误。
	out, err := cmd.CombinedOutput()
	// 根据子命令名称构造工具名，如 GitDiff / GitStatus。
	toolName := "Git" + capitalizeFirst(args[0])
	// 初始化结果对象，记录工具名与路径。
	result := &Result{Tool: toolName, Path: path}
	// 命令失败且没有任何输出时，将错误信息放入 Error 字段。
	if err != nil && len(out) == 0 {
		result.Error = err.Error()
		return result
	}
	// 只要存在输出（即使 err 非空也视为 git 返回了错误信息），将结果标记为成功。
	result.Success = true
	// 对输出进行长度截断。
	result.Output = truncateGitOutput(string(out))
	return result
}

// capitalizeFirst 将字符串首字母转为大写，用于构造 Git 工具名。
func capitalizeFirst(s string) string {
	// 空字符串无需处理。
	if s == "" {
		return ""
	}
	// 转为 rune 切片以正确处理 Unicode。
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	// 返回首字母大写后的字符串。
	return string(r)
}

// truncateGitOutput 对过长的 Git 输出进行截断并追加提示。
func truncateGitOutput(s string) string {
	// 如果输出超过最大长度限制，截取前段并追加截断提示。
	if len(s) > gitMaxOutput {
		return s[:gitMaxOutput] + "\n... (truncated)"
	}
	// 未超过限制时原样返回。
	return s
}

// gitDiff 执行 git diff，支持指定目标引用与路径范围。
func (e *Executor) gitDiff(args map[string]any) *Result {
	// 读取可选参数 target 与 path。
	target, _ := args["target"].(string)
	path, _ := args["path"].(string)
	// 构造 git diff 参数列表。
	gitArgs := []string{"diff"}
	// 若指定目标引用，则加入参数。
	if target != "" {
		gitArgs = append(gitArgs, target)
	}
	// 若指定路径，则解析为绝对路径后加入参数。
	if path != "" {
		absPath, err := e.resolvePathWithSandbox(path)
		if err != nil {
			return &Result{Tool: "GitDiff", Path: absPath, Error: err.Error()}
		}
		gitArgs = append(gitArgs, absPath)
	}
	// 调用统一的 git 执行入口。
	return e.runGit(gitArgs, path)
}

// gitStatus 执行 git status -sb 返回仓库精简状态。
func (e *Executor) gitStatus(args map[string]any) *Result {
	// 直接调用 git status 短格式命令。
	return e.runGit([]string{"status", "-sb"}, "")
}

// gitLog 执行 git log，支持限制条数与路径范围。
func (e *Executor) gitLog(args map[string]any) *Result {
	// 默认返回最近 20 条提交。
	limit := 20
	// 若参数中显式设置 limit 且为正数，则覆盖默认值。
	if v, ok := args["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}
	// 读取可选路径参数。
	path, _ := args["path"].(string)
	// 构造 git log 参数：单行格式并限制数量。
	gitArgs := []string{"log", "--oneline", "-n", fmt.Sprintf("%d", limit)}
	// 若指定路径，则解析为绝对路径后加入。
	if path != "" {
		absPath, err := e.resolvePathWithSandbox(path)
		if err != nil {
			return &Result{Tool: "GitLog", Path: absPath, Error: err.Error()}
		}
		gitArgs = append(gitArgs, "--", absPath)
	}
	// 调用统一的 git 执行入口。
	return e.runGit(gitArgs, path)
}

// gitBlame 执行 git blame --line-porcelain 并返回指定文件的逐行作者信息。
func (e *Executor) gitBlame(args map[string]any) *Result {
	// 读取文件路径，空字符串直接返回错误。
	path, _ := args["path"].(string)
	if path == "" {
		return &Result{Tool: "GitBlame", Error: "path is required"}
	}
	// 解析路径并校验沙箱约束。
	absPath, err := e.resolvePathWithSandbox(path)
	if err != nil {
		return &Result{Tool: "GitBlame", Path: absPath, Error: err.Error()}
	}
	// 构造 git blame 参数，使用 line-porcelain 格式。
	gitArgs := []string{"blame", "--line-porcelain", absPath}
	// 调用统一的 git 执行入口。
	return e.runGit(gitArgs, absPath)
}
