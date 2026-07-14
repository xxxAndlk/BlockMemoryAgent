package jsonutil

import "strings"

// ExtractOptions 控制 ExtractJSON 的容错行为。
// 不同调用点需要不同严格度：
//   - role_factory / meta_domain / plan：需要完整修复（注释、单引号、尾逗号、数组）。
//   - agent_common 的 extractJSONBlock：只需剥离代码块并提取对象，保持最小干预。
type ExtractOptions struct {
	StripComments     bool // 是否删除 // 与 /* */ 注释
	FixSingleQuotes   bool // 是否将 JSON 字符串边界的单引号替换为双引号
	FixTrailingCommas bool // 是否删除对象/数组最后一个元素后的多余逗号
	AllowArray        bool // 是否允许提取 JSON 数组（默认只提取对象）
}

// ExtractJSON 从可能包含 markdown 代码块、注释、单引号、尾部逗号的文本中提取 JSON。
// 总是先剥离 ```json / ``` 围栏；随后按 opts 进行可选修复。
// 无法提取时返回剥离后的文本本身，供调用方进一步兜底。
//
// 参数:
//   - input: 原始文本。
//   - opts: 修复选项。
//
// 返回: 提取（并修复）后的 JSON 字符串；若未找到 JSON 则返回清理后的文本。
func ExtractJSON(input string, opts ExtractOptions) string {
	// 去除首尾空白，便于后续识别代码块前缀。
	s := strings.TrimSpace(input)

	// 1. 剥离 markdown 代码块：优先匹配 ```json ... ```。
	if idx := strings.Index(s, "```json"); idx != -1 {
		// 跳过 ```json 前缀。
		s = s[idx+7:]
		// 若存在结束围栏 ```，则截取到该位置。
		if end := strings.Index(s, "```"); end != -1 {
			s = s[:end]
		}
	} else if idx := strings.Index(s, "```"); idx != -1 {
		// 否则匹配无语言标识的 ``` 围栏。
		s = s[idx+3:]
		if end := strings.Index(s, "```"); end != -1 {
			s = s[:end]
		}
	}
	// 剥离围栏后再次去除首尾空白。
	s = strings.TrimSpace(s)

	// 2. 根据选项删除注释。
	if opts.StripComments {
		s = removeComments(s)
	}

	// 3. 提取 JSON 主体。
	var body string
	// 若允许数组，先尝试提取方括号包裹的数组。
	if opts.AllowArray {
		if start := strings.Index(s, "["); start != -1 {
			if end := strings.LastIndex(s, "]"); end > start {
				body = s[start : end+1]
			}
		}
	}
	// 数组未命中或不允许数组时，尝试提取花括号包裹的对象。
	if body == "" {
		start := strings.Index(s, "{")
		end := strings.LastIndex(s, "}")
		if start != -1 && end > start {
			body = s[start : end+1]
		}
	}
	// 仍未找到 JSON 主体，则返回清理后的文本供调用方兜底处理。
	if body == "" {
		return s
	}

	// 4. 根据选项修复尾部逗号。
	if opts.FixTrailingCommas {
		body = fixTrailingCommas(body)
	}

	// 5. 根据选项将单引号字符串边界替换为双引号。
	if opts.FixSingleQuotes {
		body = normalizeJSONQuotes(body)
	}

	// 返回修复后的 JSON 主体。
	return body
}

// removeComments 删除 s 中的 // 行注释与 /* */ 块注释。
// 会保留注释外的所有字符（包括换行）。
func removeComments(s string) string {
	// 使用 strings.Builder 避免反复分配字符串。
	var out strings.Builder
	// 逐字节扫描输入字符串。
	for i := 0; i < len(s); {
		// 检测到 // 行注释：跳到行尾或字符串末尾。
		if i+1 < len(s) && s[i] == '/' && s[i+1] == '/' {
			for i < len(s) && s[i] != '\n' {
				i++
			}
			// 继续下一轮循环；换行符会在后续被写入。
			continue
		}
		// 检测到 /* 块注释：跳到 */ 之后。
		if i+1 < len(s) && s[i] == '/' && s[i+1] == '*' {
			// 跳过 /* 两个字符。
			i += 2
			// 寻找 */ 结束标记。
			for i+1 < len(s) && !(s[i] == '*' && s[i+1] == '/') {
				i++
			}
			// 若找到结束标记，跳过 * 和 /。
			if i+1 < len(s) {
				i += 2
			}
			continue
		}
		// 普通字符直接写入输出。
		out.WriteByte(s[i])
		i++
	}
	return out.String()
}

// fixTrailingCommas 删除对象/数组最后一个元素后的多余逗号。
// 例如将 "[1,2,]" 修复为 "[1,2]"，将 "{\"a\":1,}" 修复为 "{\"a\":1}"。
func fixTrailingCommas(s string) string {
	// 直接全局替换 ",]" 与 ",}" 即可覆盖常见尾部逗号场景。
	s = strings.ReplaceAll(s, ",]", "]")
	s = strings.ReplaceAll(s, ",}", "}")
	return s
}

// normalizeJSONQuotes 把作为 JSON 字符串边界的单引号统一替换为双引号。
// 在双引号字符串内部的单引号原样保留（如缩写）。
func normalizeJSONQuotes(s string) string {
	// 按 rune 切片处理，避免多字节字符被错误拆分。
	runes := []rune(s)
	var out strings.Builder
	// inDouble 表示当前是否处于双引号字符串内部。
	inDouble := false
	// inSingle 表示当前是否处于单引号字符串内部。
	inSingle := false
	// 逐个 rune 处理。
	for i := 0; i < len(runes); {
		r := runes[i]
		switch r {
		case '"':
			// 只有在不处于单引号字符串时，双引号才切换 inDouble 状态。
			if !inSingle {
				inDouble = !inDouble
			}
			// 双引号本身原样输出。
			out.WriteRune(r)
		case '\'':
			// 若处于双引号字符串内部，该单引号只是普通字符，原样保留。
			if inDouble {
				out.WriteRune(r)
				i++
				continue
			}
			// 否则切换单引号状态，并将边界单引号替换为双引号。
			inSingle = !inSingle
			out.WriteRune('"')
		case '\\':
			// 转义字符原样输出，避免破坏转义序列。
			out.WriteRune(r)
			// 若存在后续字符，一并输出并跳过两个 rune。
			if i+1 < len(runes) {
				out.WriteRune(runes[i+1])
				i += 2
				continue
			}
		default:
			// 其他字符原样输出。
			out.WriteRune(r)
		}
		i++
	}
	return out.String()
}
