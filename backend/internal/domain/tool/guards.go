package tool

// tool 包提供领域 Agent 可调用工具的封装与校验逻辑。
// guards.go 负责在写文件、执行命令前进行安全拦截，
// 防止 Agent 误写项目源码、伪造邮箱消息或启动长运行进程。

import (
	"fmt"           // fmt 用于格式化错误信息
	"os"            // os 提供操作系统功能，此处用于 PathSeparator
	"path/filepath" // filepath 提供跨平台路径处理
	"strings"       // strings 提供字符串匹配、切分等工具
)

// protectedPathGuard 用于阻止写入受保护的目录（版本控制/IDE 配置/构建产物）。
// workDir 本身即沙箱：Agent 可直接读写 workDir 内用户项目文件，
// 仅 VCS/IDE/产物根目录被挡；路径逃逸 workDir 由 sandbox.go 处理。
type protectedPathGuard struct{}

// Name 返回守卫的标识名，供外部注册与日志使用。
func (protectedPathGuard) Name() string { return "protected-path" }

// Check 对写文件路径执行保护目录校验。
// 参数 path 为目标文件路径；content 为待写入内容（本守卫仅校验路径，不使用内容）。
// 返回错误表示路径命中受保护区域，nil 表示通过校验。
func (protectedPathGuard) Check(path, content string) error {
	// 委托给 rejectProtectedPath 做具体路径校验。
	return rejectProtectedPath(path)
}

// mailboxFileGuard 用于阻止 WriteFile 工具伪造 mailbox JSON 消息文件。
// 邮箱消息必须由 runtime.Mailbox 统一投递，保证跨域协作的消息格式与路由一致。
type mailboxFileGuard struct{}

// Name 返回守卫标识名。
func (mailboxFileGuard) Name() string { return "mailbox-file" }

// Check 检查目标路径是否是 mailbox 消息文件。
// 参数 path 为目标路径；content 为待写入内容（本守卫仅校验路径）。
func (mailboxFileGuard) Check(path, content string) error {
	// 若路径不像 mailbox 文件，直接放行。
	if !isMailboxFilePath(path) {
		return nil
	}
	// 命中 mailbox 文件路径，返回禁止写入的错误提示。
	return fmt.Errorf("禁止用 WriteFile 写邮箱消息文件。跨域协作请通过 runtime.Mailbox 投递（DomainAgent 内部 API），或在任务输出中声明『请把 X 发给 Y 领域』由 MetaAgent 转发。直接写 mailbox/*.json 不会被下游领域消费。")
}

// fileHelperScriptGuard 用于阻止写入仅做文件读取、列目录或搜索的脚本。
// 这类脚本属于冗余反模式：既浪费 token 与执行时间，又容易出错，
// 应直接调用 ReadFile / ListDir / SearchInFiles 等现有工具。
type fileHelperScriptGuard struct{}

// Name 返回守卫标识名。
func (fileHelperScriptGuard) Name() string { return "file-helper-script" }

// Check 检查待写入的脚本是否属于文件 helper 反模式。
// 参数 path 为文件路径；content 为文件内容。
func (fileHelperScriptGuard) Check(path, content string) error {
	// 调用 detectFileHelperScript 得到具体违规原因字符串。
	// 若原因字符串为空，说明未检测到反模式。
	reason := detectFileHelperScript(path, content)
	if reason == "" {
		return nil
	}
	// 检测到反模式，返回带原因的错误提示。
	return fmt.Errorf("禁止写脚本做文件读取/列目录/搜索: %s。直接用 ReadFile / ListDir / SearchInFiles 工具，无需写中间脚本。", reason)
}

// mailboxGoProgramGuard 用于阻止写入调用 runtime.Mailbox 的 Go 程序。
// Agent 不应绕过 Mailbox 抽象直接投递或伪造消息。
type mailboxGoProgramGuard struct{}

// Name 返回守卫标识名。
func (mailboxGoProgramGuard) Name() string { return "mailbox-go-program" }

// Check 检查待写入的 Go 源码是否尝试使用 runtime.Mailbox。
// 参数 path 为文件路径；content 为文件内容。
func (mailboxGoProgramGuard) Check(path, content string) error {
	// 调用 detectMailboxGoProgram 检测邮箱相关 Go 程序。
	reason := detectMailboxGoProgram(path, content)
	if reason == "" {
		return nil
	}
	// 命中规则，返回禁止写入的错误提示。
	return fmt.Errorf("禁止写 Go 程序伪造邮箱投递: %s。跨域协作请在任务输出中声明『请把 X 发给 Y 领域』由 MetaAgent 转发。", reason)
}

// pathWhitespaceGuard 用于拒绝非末尾路径段包含空格的写入路径。
// 中间段空格会导致命令行解析、URL 拼接、日志分割等场景出错。
type pathWhitespaceGuard struct{}

// Name 返回守卫标识名。
func (pathWhitespaceGuard) Name() string { return "path-whitespace" }

// Check 校验路径各段是否包含空格。
// 参数 path 为目标路径；content 为待写入内容（本守卫仅校验路径）。
func (pathWhitespaceGuard) Check(path, content string) error {
	// 委托给 validateNoSpacesInSegments 做具体校验。
	return validateNoSpacesInSegments(path)
}

// longRunningServerGuard 用于拦截启动长运行服务器进程的命令。
// 这些命令会进入主循环并阻塞执行器，且在 Windows 上 timeout 无法可靠终止子进程。
type longRunningServerGuard struct{}

// Name 返回守卫标识名。
func (longRunningServerGuard) Name() string { return "long-running-server" }

// Check 检查命令字符串是否疑似启动长运行服务器。
// 参数 cmd 为用户输入的命令行字符串。
// 返回值 blocked 为 true 表示命中拦截规则；reason 为拦截原因说明。
func (longRunningServerGuard) Check(cmd string) (blocked bool, reason string) {
	// detectLongRunningServer 返回命中的描述字符串，空表示未命中。
	desc := detectLongRunningServer(cmd)
	if desc == "" {
		// 未命中，允许执行。
		return false, ""
	}
	// 命中，返回禁止执行及原因。
	return true, "禁止启动长运行服务器进程: " + desc +
		"。此类命令会进入主循环阻塞执行器，且 Windows timeout 无法可靠杀掉子进程。" +
		"如需验证可玩性，请用 RunCommand 做 node --check / python -m py_compile 语法检查，" +
		"或由用户手动启动服务。"
}

// funcCommandGuard 通过闭包包装一个命令检查函数，使其满足 CommandGuard 接口。
// 适用于需要临时、轻量注册命令检查规则的场景。
type funcCommandGuard struct {
	name  string                                         // name 是守卫的标识名
	check func(cmd string) (blocked bool, reason string) // check 是实际的命令检查闭包
}

// Name 返回 funcCommandGuard 实例注册的名称。
func (g *funcCommandGuard) Name() string { return g.name }

// Check 调用闭包执行实际的命令检查逻辑。
// 参数 cmd 为待检查的命令字符串。
// 返回值 blocked 与 reason 由闭包决定。
func (g *funcCommandGuard) Check(cmd string) (blocked bool, reason string) {
	return g.check(cmd)
}

// detectFileHelperScript 检测脚本是否仅用于读取、列目录或搜索文件。
// 参数 path 为文件路径；content 为文件内容。
// 返回非空字符串表示命中反模式及原因，空字符串表示未命中。
func detectFileHelperScript(path, content string) string {
	// 路径或内容为空时无需检测，直接返回空。
	if path == "" || content == "" {
		return ""
	}

	// 将路径转小写，便于不区分大小写地匹配扩展名。
	lowerPath := strings.ToLower(path)
	// 仅对 Python 与 Shell 脚本做检测。
	isScript := strings.HasSuffix(lowerPath, ".py") || strings.HasSuffix(lowerPath, ".sh")
	if !isScript {
		return ""
	}

	// 将内容整体转小写，用于后续关键字匹配。
	lower := strings.ToLower(content)

	// 对 Python 脚本进行多种反模式检测。
	if strings.HasSuffix(lowerPath, ".py") {
		// hasOpen 表示脚本打开并读取文件。
		hasOpen := strings.Contains(lower, "open(") && (strings.Contains(lower, ".read(") || strings.Contains(lower, "readlines(") || strings.Contains(lower, "readline("))
		// hasOutput 表示脚本将结果输出到 stdout/stderr。
		hasOutput := strings.Contains(lower, "print(") ||
			strings.Contains(lower, "sys.stdout.write") ||
			strings.Contains(lower, "sys.stdout.buffer.write") ||
			strings.Contains(lower, "sys.stderr.write") ||
			strings.Contains(lower, "sys.stdout.buffer.flush")

		// open+read 且输出，说明用脚本替代 ReadFile 工具。
		if hasOpen && hasOutput {
			return "Python 脚本含 open(...).read() + 输出（读文件并打印，应直接用 ReadFile）"
		}
		// os.listdir 且输出，说明用脚本替代 ListDir 工具。
		if strings.Contains(lower, "os.listdir") && hasOutput {
			return "Python 脚本含 os.listdir + 输出（列目录并打印，应直接用 ListDir）"
		}
		// os.walk 且输出，说明用脚本遍历目录。
		if strings.Contains(lower, "os.walk") && hasOutput {
			return "Python 脚本含 os.walk + 输出（遍历目录并打印，应直接用 ListDir）"
		}
		// re.search/re.findall 且输出，说明用脚本替代 SearchInFiles 工具。
		if (strings.Contains(lower, "re.search") || strings.Contains(lower, "re.findall")) && hasOutput {
			return "Python 脚本用正则搜索并输出（应直接用 SearchInFiles）"
		}
		// 逐行读取文件并输出，也是 ReadFile 反模式。
		if (strings.Contains(lower, "for line in") || strings.Contains(lower, "readlines(")) && hasOutput {
			return "Python 脚本逐行读文件并输出（应直接用 ReadFile）"
		}
	}

	// 对 Shell 脚本进行反模式检测。
	if strings.HasSuffix(lowerPath, ".sh") {
		// cat/ls/grep 配合 echo，说明用脚本替代 ReadFile/ListDir/SearchInFiles 工具。
		if (strings.Contains(lower, "cat ") || strings.Contains(lower, "ls ") || strings.Contains(lower, "grep ")) && strings.Contains(lower, "echo ") {
			return "Shell 脚本含 cat/ls/grep + echo（应直接用 ReadFile/ListDir/SearchInFiles）"
		}
	}

	// 未命中任何反模式，返回空字符串。
	return ""
}

// detectMailboxGoProgram 检测 Go 程序是否尝试使用 runtime.Mailbox。
// 参数 path 为文件路径；content 为文件内容。
// 返回非空字符串表示命中及原因，空字符串表示未命中。
func detectMailboxGoProgram(path, content string) string {
	// 路径或内容为空时无需检测。
	if path == "" || content == "" {
		return ""
	}

	// 转小写后只处理 .go 文件。
	lowerPath := strings.ToLower(path)
	if !strings.HasSuffix(lowerPath, ".go") {
		return ""
	}

	// 转小写内容进行关键字匹配。
	lower := strings.ToLower(content)

	// hasMailboxRef 检测是否引用 Mailbox 相关 API。
	hasMailboxRef := strings.Contains(lower, "runtime.mailbox") ||
		strings.Contains(lower, "mailbox.send") ||
		strings.Contains(lower, "mailbox.post") ||
		strings.Contains(lower, "mailbox.publish") ||
		strings.Contains(lower, "newmailbox(") ||
		strings.Contains(lower, "runtime.new(")
	// 没有引用 mailbox，直接放行。
	if !hasMailboxRef {
		return ""
	}

	// hasProgramStructure 检测是否具备可执行程序结构或调用发送方法。
	hasProgramStructure := strings.Contains(lower, "package main") ||
		strings.Contains(lower, "func main()") ||
		strings.Contains(lower, ".send(") ||
		strings.Contains(lower, ".post(") ||
		strings.Contains(lower, ".publish(")
	// 仅有引用但无程序结构，暂不拦截。
	if !hasProgramStructure {
		return ""
	}

	// 同时满足 mailbox 引用与程序结构，判定为伪造邮箱投递。
	return "Go 源码引用 runtime.Mailbox 并含程序结构（package main / func main / .Send(）"
}

// rejectProtectedPath 拒绝写入受保护的项目路径。
// 参数 path 为目标文件路径。
// 返回 nil 表示路径安全；返回错误表示禁止写入。
func rejectProtectedPath(path string) error {
	// 空路径无法判断，视为安全，直接放行。
	if path == "" {
		return nil
	}

	// 清理路径，移除 .、.. 等冗余段。
	cleaned := filepath.Clean(path)
	// 统一使用正斜杠，便于后续按段比较。
	cleaned = strings.ReplaceAll(cleaned, "\\", "/")
	// 转小写用于不区分大小写的目录名匹配。
	lower := strings.ToLower(cleaned)
	// 按 / 切分得到路径段。
	segments := strings.Split(lower, "/")

	// protectedRoots 列出禁止写入的根级目录名：仅版本控制、IDE 配置与构建产物。
	// 源码目录名（backend/test/cmd/config/doc/...）不再保护——workDir 是用户项目，
	// Agent 需直接编辑用户代码；仅挡 VCS/IDE/产物，防污染元数据与可重建目录。
	protectedRoots := []string{".git", ".github", ".idea", ".vscode", "node_modules", "dist", "build", "bin", "logs", ".cache"}

	// 遍历每一段，检查是否为受保护的根目录。
	for i, seg := range segments {
		// 跳过绝对路径开头的空段、Windows 盘符段或当前目录段。
		if i == 0 && (seg == "" || strings.HasSuffix(seg, ":") || seg == ".") {
			continue
		}
		// 与每个受保护根目录名比较。
		for _, root := range protectedRoots {
			if seg == root {
				// 获取当前段的前一段，判断当前段是否处于路径顶层。
				prev := ""
				if i > 0 {
					prev = segments[i-1]
				}
				// 若前一段为空、盘符或当前目录，说明该段是根级目录，禁止写入。
				if prev == "" || strings.HasSuffix(prev, ":") || prev == "." {
					return fmt.Errorf("禁止写入受保护目录 %s/（版本控制/IDE 配置/构建产物）。本工作目录即沙箱,可直接读写其余子目录;如确需改此目录请由人工操作", root)
				}
			}
		}
	}

	// 通过所有保护规则，允许写入。
	return nil
}

// isMailboxFilePath 检测路径是否看起来像伪造的 mailbox 消息文件。
// 参数 path 为目标路径。
// 返回 true 表示路径命中 mailbox 文件命名模式。
func isMailboxFilePath(path string) bool {
	// 空路径不可能是 mailbox 文件。
	if path == "" {
		return false
	}

	// 转小写便于不区分大小写匹配。
	lower := strings.ToLower(path)
	// 路径中必须包含 "mailbox" 关键字。
	if !strings.Contains(lower, "mailbox") {
		return false
	}

	// 从路径末尾向前查找最后一个路径分隔符，提取文件名。
	base := path
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			base = path[i+1:]
			break
		}
	}

	// 文件名转小写。
	baseLower := strings.ToLower(base)
	// mailbox 消息文件以 .json 结尾。
	if !strings.HasSuffix(baseLower, ".json") {
		return false
	}

	// 文件名前缀符合常见邮箱消息命名模式才判定命中。
	prefixes := []string{"to-", "from-", "msg-", "mailbox", "mail-", "send-", "notify-"}
	for _, p := range prefixes {
		if strings.HasPrefix(baseLower, p) {
			return true
		}
	}

	// 前缀不匹配，放行。
	return false
}

// validateNoSpacesInSegments 拒绝非末尾路径段包含空格的路径。
// 参数 path 为待校验路径。
// 返回 nil 表示路径合法；返回错误表示存在中间段空格。
func validateNoSpacesInSegments(path string) error {
	// 空路径直接视为合法。
	if path == "" {
		return nil
	}

	// 清理路径冗余段。
	cleaned := filepath.Clean(path)
	// 构造路径分隔符集合，包含当前系统的 PathSeparator 和正斜杠。
	seps := string(os.PathSeparator) + "/"
	// 按分隔符切分路径为各段。
	parts := strings.FieldsFunc(cleaned, func(r rune) bool {
		return strings.ContainsRune(seps, r)
	})

	// 逐段检查是否包含空格。
	for i, seg := range parts {
		if strings.Contains(seg, " ") {
			// 最后一段允许包含空格（例如文件名本身可以有空格）。
			if i == len(parts)-1 {
				continue
			}
			// 中间段含空格，返回错误提示，建议使用 / 或当前系统分隔符，或设置 allow_spaces=true 覆盖。
			return fmt.Errorf("path segment contains space: %q in path %q (use / or %c as separator, or set allow_spaces=true to override)", seg, path, os.PathSeparator)
		}
	}

	// 所有中间段均无空格，校验通过。
	return nil
}

// detectLongRunningServer 检测命令字符串是否疑似启动长运行服务器进程。
// 参数 cmdStr 为用户输入的命令行字符串。
// 返回命中的描述字符串，空字符串表示未命中。
func detectLongRunningServer(cmdStr string) string {
	// 空命令不可能是服务器进程。
	if cmdStr == "" {
		return ""
	}

	// 转小写便于不区分大小写匹配。
	lower := strings.ToLower(cmdStr)

	// patterns 定义长运行服务器或交互式进程的命令模式及对应描述。
	patterns := []struct {
		pat  string // pat 是命令子串模式
		desc string // desc 是命中后的可读描述
	}{
		{"python -m http.server", "python http.server"},
		{"python3 -m http.server", "python http.server"},
		{"py -m http.server", "python http.server"},
		{"-m http.server", "http.server"},
		{"http.server ", "http.server"},
		{"flask run", "flask dev server"},
		{"uvicorn ", "uvicorn asgi server"},
		{"gunicorn ", "gunicorn wsgi server"},
		{"npm start", "npm start (dev server)"},
		{"npm run dev", "npm run dev"},
		{"yarn dev", "yarn dev server"},
		{"yarn start", "yarn start server"},
		{"pnpm dev", "pnpm dev server"},
		{"vite ", "vite dev server"},
		{"webpack serve", "webpack dev server"},
		{"ng serve", "angular dev server"},
		{"rails server", "rails server"},
		{"rails s ", "rails server"},
		{"django runserver", "django dev server"},
		{"manage.py runserver", "django dev server"},
		{"node server.js", "node server"},
		{"node app.js", "node app"},
		{"node .", "node app"},
		{"nodemon ", "nodemon watcher"},
		{"pm2 start", "pm2 daemon"},
		{"docker compose up", "docker compose (long running)"},
		{"docker-compose up", "docker compose (long running)"},
		{"tail -f", "tail -f (follows forever)"},
		{"less ", "less pager (interactive)"},
		{"man ", "man pager (interactive)"},
		{"vim ", "vim editor (interactive)"},
		{"nano ", "nano editor (interactive)"},
		{"python -m pygame", "pygame main loop"},
		{"python -m tkinter", "tkinter main loop"},
		{"start http://", "open browser (interactive)"},
		{"open http://", "open browser (interactive)"},
		{"xdg-open http", "open browser (interactive)"},
		{"go run ", "go run (执行 Go 程序，可能长运行服务器)"},
		{"start \"\" server", "start server*.exe (自定义服务器)"},
		{"start server", "start server*.exe (自定义服务器)"},
		{"start \"\" app", "start app*.exe (可能长运行)"},
		{"start \"\" main", "start main*.exe (可能长运行)"},
		{"start \"\" serve", "start serve*.exe (自定义服务器)"},
		{"start \"\" httpd", "start httpd.exe (apache)"},
		{"start \"\" nginx", "start nginx.exe (nginx)"},
		{"server.exe", "server.exe (自定义服务器)"},
		{"serve.exe", "serve.exe (自定义服务器)"},
		{"httpd.exe", "httpd.exe (apache server)"},
		{"nginx.exe", "nginx.exe (nginx server)"},
	}

	// 遍历模式列表，任一子串命中即返回对应描述。
	for _, p := range patterns {
		if strings.Contains(lower, p.pat) {
			return p.desc
		}
	}

	// 未命中任何模式，返回空字符串。
	return ""
}
