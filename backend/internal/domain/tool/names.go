package tool

// names.go 工具名约定辅助。

import "strings"

// toolNameSep 是 MCP 插件工具本地注册名的分隔符："<插件ID>__<远端工具名>"。
// 由 mcpbridge 包装时拼接，保证跨插件全局不重名。
const toolNameSep = "__"

// BareToolName 剥掉 MCP 插件工具本地名的 "<插件ID>__" 前缀，返回远端原始工具名；
// 无前缀的名字（内置工具、改名前的历史记录）原样返回。
// 用途：机器校验（验收证据扫描等）按裸名匹配，同时兼容新旧两种记录格式。
// 已知限制：插件 ID 或远端工具名自身含 "__" 时剥离不准（当前两侧均不含）。
func BareToolName(name string) string {
	if i := strings.Index(name, toolNameSep); i >= 0 && i+len(toolNameSep) < len(name) {
		return name[i+len(toolNameSep):]
	}
	return name
}
