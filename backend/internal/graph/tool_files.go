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

	// 解析为绝对路径（相对 workDir）并做沙箱读路径校验
	absPath, err := e.resolvePathWithSandbox(path)
	if err != nil {
		return &ToolResult{Tool: "ReadFile", Path: absPath, Error: err.Error()}
	}
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
	allowSpaces, _ := args["allow_spaces"].(bool)

	if path == "" {
		return &ToolResult{Tool: "WriteFile", Error: "path is required"}
	}

	// 工作区污染拦截：禁止 Agent 写到后端源码树 / 测试模块 / 根 go.mod / go.work。
	// 塔防 demo 事故中，Agent 把 Go 包写到 backend/internal/tdcombat/、改根 go.work、
	// 在 workspace/ 建 go.mod，污染项目结构。Agent 只能写 workspace/ 子目录下的用户产物。
	if err := rejectProtectedPath(path); err != nil {
		return &ToolResult{Tool: "WriteFile", Path: path, Error: err.Error()}
	}

	// 邮箱文件化拦截：Agent 偶尔把跨域邮箱当成 JSON 文件写到 workspace/.../mailbox/
	// 目录（参见塔防 demo 事故）。系统内置 runtime.Mailbox 投递，禁止用 WriteFile
	// 伪造邮箱消息。命中模式：路径段含 "mailbox" + 文件名 to-*/from-*/msg-*.json。
	if isMailboxFilePath(path) {
		return &ToolResult{Tool: "WriteFile", Path: path,
			Error: "禁止用 WriteFile 写邮箱消息文件。跨域协作请通过 runtime.Mailbox 投递（DomainAgent 内部 API），或在任务输出中声明『请把 X 发给 Y 领域』由 MetaAgent 转发。直接写 mailbox/*.json 不会被下游领域消费。"}
	}

	// 临时脚本反模式拦截：Agent 偶尔用 WriteFile 写 Python/Shell 脚本去读文件、
	// 列目录、搜文本，而不是直接用 ReadFile/ListDir/SearchInFiles（参见塔防 demo
	// 事故：写了 10+ 个 read_game_js.py / check_game_js.py / dump_game.py 浪费 token）。
	// 命中典型模式：.py 脚本含 open(...).read() + print，或 os.listdir + print。
	if reason := detectFileHelperScript(path, content); reason != "" {
		return &ToolResult{Tool: "WriteFile", Path: path,
			Error: "禁止写脚本做文件读取/列目录/搜索: " + reason +
				"。直接用 ReadFile / ListDir / SearchInFiles 工具，无需写中间脚本。" +
				"此反模式浪费 token 与执行时间（塔防事故中 Agent 写 10+ 个 .py 读 game.js）。"}
	}

	// 邮箱 Go 程序绕过拦截：Agent 写 Go 源码（send_interface.go 等）调用
	// runtime.Mailbox.Send 绕过 JSON 文件化检测（参见塔防 demo 事故：
	// Agent 写 backend/cmd/send_interface/main.go 伪造跨域邮箱投递）。
	// 这类 Go 程序不会被编译进后端二进制，纯浪费 token；且直接写 backend/ 已被
	// rejectProtectedPath 拦截，但 workspace/ 下的 .go 仍可能漏过。
	if reason := detectMailboxGoProgram(path, content); reason != "" {
		return &ToolResult{Tool: "WriteFile", Path: path,
			Error: "禁止写 Go 程序伪造邮箱投递: " + reason +
				"。跨域协作请通过 runtime.Mailbox 投递（DomainAgent 内部 API，Agent 不应直接调用），" +
				"或在任务输出中声明『请把 X 发给 Y 领域』由 MetaAgent 转发。" +
				"写 Go 源码绕过邮箱检测不会被编译，纯属浪费 token（塔防事故 Agent 写 send_interface.go）。"}
	}

	// 路径段空格校验：LLM 偶尔把 "docs/workspace" 错写成 "docs workspace"，
	// 导致创建带空格的错误目录。拒绝此类路径，强制 LLM 用 / 或 \ 分隔。
	// allow_spaces=true 时放行（罕见场景，如文件名确需含空格）。
	if !allowSpaces {
		if err := validateNoSpacesInSegments(path); err != nil {
			return &ToolResult{Tool: "WriteFile", Path: path, Error: err.Error()}
		}
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
	} else {
		// 非临时文件必须先通过沙箱路径校验，禁止写到工作目录外
		if err := e.sanitizeWritePath(absPath); err != nil {
			return &ToolResult{Tool: "WriteFile", Path: absPath, Error: err.Error()}
		}
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

// detectFileHelperScript 检测 WriteFile 是否在写"读文件/列目录/搜索"类辅助脚本。
// 命中条件：.py/.sh 文件 + 内容含 open(...).read() / os.listdir / grep / findstr 等模式。
// 这类脚本完全可以用 ReadFile / ListDir / SearchInFiles 替代，写脚本纯属浪费。
func detectFileHelperScript(path, content string) string {
	if path == "" || content == "" {
		return ""
	}
	lowerPath := strings.ToLower(path)
	isScript := strings.HasSuffix(lowerPath, ".py") || strings.HasSuffix(lowerPath, ".sh")
	if !isScript {
		return ""
	}
	lower := strings.ToLower(content)
	// Python: open(...).read() + 任意输出（print / sys.stdout.write / sys.stdout.buffer.write）
	// 塔防 demo 事故 v2：Agent 用 sys.stdout.write 绕过只查 print 的检测，
	// 写 _read_tail.py 反复读 game_core.js / game.js tail。补全所有输出方法。
	if strings.HasSuffix(lowerPath, ".py") {
		hasOpen := strings.Contains(lower, "open(") && (strings.Contains(lower, ".read(") || strings.Contains(lower, "readlines(") || strings.Contains(lower, "readline("))
		// 任意输出方式：print / sys.stdout.write / sys.stdout.buffer.write / sys.stderr.write / write(
		hasOutput := strings.Contains(lower, "print(") ||
			strings.Contains(lower, "sys.stdout.write") ||
			strings.Contains(lower, "sys.stdout.buffer.write") ||
			strings.Contains(lower, "sys.stderr.write") ||
			strings.Contains(lower, "sys.stdout.buffer.flush")
		if hasOpen && hasOutput {
			return "Python 脚本含 open(...).read() + 输出（读文件并打印，应直接用 ReadFile）"
		}
		if strings.Contains(lower, "os.listdir") && hasOutput {
			return "Python 脚本含 os.listdir + 输出（列目录并打印，应直接用 ListDir）"
		}
		if strings.Contains(lower, "os.walk") && hasOutput {
			return "Python 脚本含 os.walk + 输出（遍历目录并打印，应直接用 ListDir）"
		}
		if (strings.Contains(lower, "re.search") || strings.Contains(lower, "re.findall")) && hasOutput {
			return "Python 脚本用正则搜索并输出（应直接用 SearchInFiles）"
		}
		// 逐行读取 + 输出（readlines / for line in f）
		if (strings.Contains(lower, "for line in") || strings.Contains(lower, "readlines(")) && hasOutput {
			return "Python 脚本逐行读文件并输出（应直接用 ReadFile）"
		}
	}
	// Shell: cat / ls / grep + echo
	if strings.HasSuffix(lowerPath, ".sh") {
		if (strings.Contains(lower, "cat ") || strings.Contains(lower, "ls ") || strings.Contains(lower, "grep ")) && strings.Contains(lower, "echo ") {
			return "Shell 脚本含 cat/ls/grep + echo（应直接用 ReadFile/ListDir/SearchInFiles）"
		}
	}
	return ""
}

// detectMailboxGoProgram 检测 WriteFile 是否在写 Go 程序绕过邮箱投递。
// 命中条件：.go 文件 + 内容含 runtime.Mailbox / mailbox.Send / runtime.NewMailbox 等模式。
// Agent 不应直接调用 runtime.Mailbox（DomainAgent 内部 API），跨域协作走 MetaAgent 转发。
// 参见塔防 demo 事故：Agent 写 send_interface.go 调 runtime.Mailbox.Send 绕过 JSON 检测。
func detectMailboxGoProgram(path, content string) string {
	if path == "" || content == "" {
		return ""
	}
	lowerPath := strings.ToLower(path)
	if !strings.HasSuffix(lowerPath, ".go") {
		return ""
	}
	lower := strings.ToLower(content)
	// 命中模式：Go 源码引用 runtime.Mailbox 或类似邮箱 API
	hasMailboxRef := strings.Contains(lower, "runtime.mailbox") ||
		strings.Contains(lower, "mailbox.send") ||
		strings.Contains(lower, "mailbox.post") ||
		strings.Contains(lower, "mailbox.publish") ||
		strings.Contains(lower, "newmailbox(") ||
		strings.Contains(lower, "runtime.new(")
	if !hasMailboxRef {
		return ""
	}
	// 进一步确认是程序性调用（含 import 或 func main 或 .Send( 调用）
	hasProgramStructure := strings.Contains(lower, "package main") ||
		strings.Contains(lower, "func main()") ||
		strings.Contains(lower, ".send(") ||
		strings.Contains(lower, ".post(") ||
		strings.Contains(lower, ".publish(")
	if !hasProgramStructure {
		return ""
	}
	return "Go 源码引用 runtime.Mailbox 并含程序结构（package main / func main / .Send(）"
}

// rejectProtectedPath 拒绝 Agent 写到受保护的项目路径。
// 禁止写：
//   - 根 go.mod / go.work（污染 Go module 配置）
//   - backend/ 源码树（Agent 不应改后端代码）
//   - test/ 测试模块（同上）
//   - cmd/ 顶层命令目录
//   - config/ 配置目录（系统配置，非用户产物）
//   - migrations/ 数据库迁移
//   - .git/ / .github/ / .claude/ 等元数据目录
//
// 允许写：
//   - workspace/ 子目录（用户产物）
//   - workspace/ 下的任意子路径
//
// 塔防 demo 事故：Agent 在 backend/internal/tdcombat/ 写 Go 包、改根 go.work、
// 在 workspace/ 建 go.mod，全错。
func rejectProtectedPath(path string) error {
	if path == "" {
		return nil
	}
	cleaned := filepath.Clean(path)
	cleaned = strings.ReplaceAll(cleaned, "\\", "/")
	lower := strings.ToLower(cleaned)

	// 检查路径是否落在项目根的关键目录内
	segments := strings.Split(lower, "/")
	protectedRoots := []string{"backend", "test", "cmd", "config", "migrations", ".git", ".github", ".claude", ".idea", ".vscode", "doc", "docs", "scripts", "docker"}
	for i, seg := range segments {
		// 仅跳过盘符（Windows 绝对路径首段如 "c:"）或空段（Unix 绝对路径前导 /）或 "."
		// 相对路径首段（如 "backend/..."）不跳过，否则项目根受保护目录漏检
		if i == 0 && (seg == "" || strings.HasSuffix(seg, ":") || seg == ".") {
			continue
		}
		for _, root := range protectedRoots {
			if seg == root {
				// 允许 workspace/backend 等用户自建子目录，但禁止直接写项目根的 backend/
				// 判断：如果该 protected 段是项目根的直接子目录（前面只有盘符或空或 .），拒绝
				prev := ""
				if i > 0 {
					prev = segments[i-1]
				}
				if prev == "" || strings.HasSuffix(prev, ":") || prev == "." {
					return fmt.Errorf("禁止写入受保护目录 %s/（项目源码树/配置/元数据）。Agent 只能写 workspace/ 子目录下的用户产物。如确需修改后端代码请由人工操作", root)
				}
			}
		}
	}

	// 根 go.mod / go.work 拒绝
	base := strings.ToLower(filepath.Base(cleaned))
	if base == "go.mod" || base == "go.work" {
		// 允许 workspace/go.mod（用户子项目），拒绝根
		if !strings.Contains(lower, "workspace/") {
			return fmt.Errorf("禁止写入根 %s（污染 Go module 配置）。如需 Go 子项目请放到 workspace/ 下", base)
		}
	}

	return nil
}

// isMailboxFilePath 检测路径是否像 Agent 伪造的邮箱消息文件。
// 命中条件：路径中含 "mailbox" 段 + 文件名以 to-/from-/msg-/mailbox 开头且 .json 结尾。
// 这类文件不会被系统消费，应通过 runtime.Mailbox.Send 投递。
func isMailboxFilePath(path string) bool {
	if path == "" {
		return false
	}
	lower := strings.ToLower(path)
	if !strings.Contains(lower, "mailbox") {
		return false
	}
	// 提取 basename
	base := path
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			base = path[i+1:]
			break
		}
	}
	baseLower := strings.ToLower(base)
	if !strings.HasSuffix(baseLower, ".json") {
		return false
	}
	prefixes := []string{"to-", "from-", "msg-", "mailbox", "mail-", "send-", "notify-"}
	for _, p := range prefixes {
		if strings.HasPrefix(baseLower, p) {
			return true
		}
	}
	return false
}

// validateNoSpacesInSegments 拒绝路径段中含空格的路径。
// LLM 偶尔把 "docs/workspace" 错写成 "docs workspace"，filepath.Clean 不会修正，
// 反而会创建名为 "docs workspace" 的目录。此函数把这类路径挡在写入前。
// 允许：纯文件名含空格（最后一段）—— 但仍不推荐，由调用方决定是否放行。
func validateNoSpacesInSegments(path string) error {
	if path == "" {
		return nil
	}
	// 标准化分隔符后再切分
	cleaned := filepath.Clean(path)
	seps := string(os.PathSeparator) + "/"
	// 按系统分隔符或 / 切分
	parts := strings.FieldsFunc(cleaned, func(r rune) bool {
		return strings.ContainsRune(seps, r)
	})
	for i, seg := range parts {
		if strings.Contains(seg, " ") {
			// 最后一段是文件名，含空格时仅警告不拒绝（许多场景合理）
			if i == len(parts)-1 {
				continue
			}
			return fmt.Errorf("path segment contains space: %q in path %q (use / or %c as separator, or set allow_spaces=true to override)", seg, path, os.PathSeparator)
		}
	}
	return nil
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

	// 解析为绝对路径并做沙箱读路径校验
	absPath, err := e.resolvePathWithSandbox(path)
	if err != nil {
		return &ToolResult{Tool: "ListDir", Path: absPath, Error: err.Error()}
	}
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

	// 解析为绝对目录并做沙箱读路径校验
	absDir, err := e.resolvePathWithSandbox(dir)
	if err != nil {
		return &ToolResult{Tool: "SearchInFiles", Path: absDir, Error: err.Error()}
	}
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
