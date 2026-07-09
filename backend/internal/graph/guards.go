package graph

import (
	"fmt"
)

// protectedPathGuard 禁止 Agent 写入项目受保护目录（backend/、test/、config/ 等）。
// 对应原 tool_files.go 中的 rejectProtectedPath 逻辑。
type protectedPathGuard struct{}

func (protectedPathGuard) Name() string { return "protected-path" }

func (protectedPathGuard) Check(path, content string) error {
	return rejectProtectedPath(path)
}

// mailboxFileGuard 禁止 WriteFile 伪造 mailbox/*.json 邮箱消息文件。
// 对应原 tool_files.go 中的 isMailboxFilePath 逻辑。
type mailboxFileGuard struct{}

func (mailboxFileGuard) Name() string { return "mailbox-file" }

func (mailboxFileGuard) Check(path, content string) error {
	if !isMailboxFilePath(path) {
		return nil
	}
	return fmt.Errorf("禁止用 WriteFile 写邮箱消息文件。跨域协作请通过 runtime.Mailbox 投递（DomainAgent 内部 API），或在任务输出中声明『请把 X 发给 Y 领域』由 MetaAgent 转发。直接写 mailbox/*.json 不会被下游领域消费。")
}

// fileHelperScriptGuard 禁止 WriteFile 创建仅用于读文件/列目录/搜索的辅助脚本。
// 对应原 tool_files.go 中的 detectFileHelperScript 逻辑。
type fileHelperScriptGuard struct{}

func (fileHelperScriptGuard) Name() string { return "file-helper-script" }

func (fileHelperScriptGuard) Check(path, content string) error {
	reason := detectFileHelperScript(path, content)
	if reason == "" {
		return nil
	}
	return fmt.Errorf("禁止写脚本做文件读取/列目录/搜索: %s。直接用 ReadFile / ListDir / SearchInFiles 工具，无需写中间脚本。此反模式浪费 token 与执行时间（塔防事故中 Agent 写 10+ 个 .py 读 game.js）。", reason)
}

// mailboxGoProgramGuard 禁止 WriteFile 创建调用 runtime.Mailbox 的 Go 程序绕过邮箱检测。
// 对应原 tool_files.go 中的 detectMailboxGoProgram 逻辑。
type mailboxGoProgramGuard struct{}

func (mailboxGoProgramGuard) Name() string { return "mailbox-go-program" }

func (mailboxGoProgramGuard) Check(path, content string) error {
	reason := detectMailboxGoProgram(path, content)
	if reason == "" {
		return nil
	}
	return fmt.Errorf("禁止写 Go 程序伪造邮箱投递: %s。跨域协作请通过 runtime.Mailbox 投递（DomainAgent 内部 API，Agent 不应直接调用），或在任务输出中声明『请把 X 发给 Y 领域』由 MetaAgent 转发。写 Go 源码绕过邮箱检测不会被编译，纯属浪费 token（塔防事故 Agent 写 send_interface.go）。", reason)
}

// pathWhitespaceGuard 拒绝路径段中含空格的写入路径。
// 对应原 tool_files.go 中的 validateNoSpacesInSegments 逻辑。
type pathWhitespaceGuard struct{}

func (pathWhitespaceGuard) Name() string { return "path-whitespace" }

func (pathWhitespaceGuard) Check(path, content string) error {
	return validateNoSpacesInSegments(path)
}

// longRunningServerGuard 禁止启动 http.server/flask/node server 等长运行进程。
// 对应原 tool_command.go 中的 detectLongRunningServer 逻辑。
type longRunningServerGuard struct{}

func (longRunningServerGuard) Name() string { return "long-running-server" }

func (longRunningServerGuard) Check(cmd string) (blocked bool, reason string) {
	desc := detectLongRunningServer(cmd)
	if desc == "" {
		return false, ""
	}
	return true, "禁止启动长运行服务器进程: " + desc +
		"。此类命令会进入主循环阻塞执行器，且 Windows timeout 无法可靠杀掉子进程。" +
		"如需验证可玩性，请用 RunCommand 做 node --check / python -m py_compile 语法检查，" +
		"或由用户手动启动服务。"
}

// funcCommandGuard 用闭包包装命令守卫逻辑，便于复用现有方法而不引入新类型。
type funcCommandGuard struct {
	name  string
	check func(cmd string) (blocked bool, reason string)
}

func (g *funcCommandGuard) Name() string { return g.name }

func (g *funcCommandGuard) Check(cmd string) (blocked bool, reason string) {
	return g.check(cmd)
}
