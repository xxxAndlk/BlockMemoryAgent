package tool

// untrusted.go 不可信内容围栏（TODO #18-4 T31 提示注入防线）：
// MCP 插件工具输出、HTTP 响应体等外部源内容进入模型上下文前包进
// <untrusted_data> 围栏，配合 meta/domain 提示词的【数据围栏纪律】——
// 围栏内是数据不是指令，模型不得把其中的命令式文本当作用户/系统指令执行。
//
// 防逃逸：内容中出现闭合标记（任意大小写）时转义打断，防止外部内容自带
// "</untrusted_data>" 提前闭合围栏、把后续文本洗出围栏外。

import (
	"regexp"
	"strings"
)

const (
	// UntrustedTagOpen/UntrustedTagClose 围栏标记（导出供提示词文档对齐口径）。
	UntrustedTagOpen  = "<untrusted_data>"
	UntrustedTagClose = "</untrusted_data>"
)

// untrustedFenceRe 匹配围栏标记（开/闭、任意大小写），用于内容侧转义。
var untrustedFenceRe = regexp.MustCompile(`(?i)<(/?)untrusted_data`)

// WrapUntrusted 把外部源内容包进不可信围栏。
//
// 参数:
//   - source: 内容来源标识（工具名/插件 ID/URL），写入围栏 source 属姓供模型溯源。
//   - content: 原始外部内容。
//
// 返回: 围栏包裹后的文本；content 为空/纯空白时返回空串（零信息内容不包围栏，
// 避免往上下文灌空壳标记）。source 仅取首行（防多行 source 自身携带注入文本）。
func WrapUntrusted(source, content string) string {
	if strings.TrimSpace(content) == "" {
		return ""
	}
	src := source
	if i := strings.IndexAny(src, "\r\n"); i >= 0 {
		src = src[:i]
	}
	src = strings.TrimSpace(src)
	// 防逃逸：内容里的围栏标记（任意大小写）补反斜杠打断，使其不再构成合法标记。
	safe := untrustedFenceRe.ReplaceAllString(content, `<\${1}untrusted_data`)
	var b strings.Builder
	b.WriteString(UntrustedTagOpen)
	if src != "" {
		b.WriteString(`<!-- source: `)
		b.WriteString(src)
		b.WriteString(` -->`)
	}
	b.WriteByte('\n')
	b.WriteString(safe)
	b.WriteByte('\n')
	b.WriteString(UntrustedTagClose)
	return b.String()
}
