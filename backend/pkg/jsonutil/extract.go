package jsonutil

import "strings"

// ExtractOptions 控制 ExtractJSON 的容错行为。
// 不同调用点需要不同严格度：
//   - role_factory / meta_domain / plan：需要完整修复（注释、单引号、尾逗号、数组）。
//   - agent_common 的 extractJSONBlock：只需剥离代码块并提取对象，保持最小干预。
type ExtractOptions struct {
	StripComments     bool
	FixSingleQuotes   bool
	FixTrailingCommas bool
	AllowArray        bool
}

// ExtractJSON 从可能包含 markdown 代码块、注释、单引号、尾部逗号的文本中提取 JSON。
// 总是先剥离 ```json / ``` 围栏；随后按 opts 进行可选修复。
// 无法提取时返回剥离后的文本本身，供调用方进一步兜底。
func ExtractJSON(input string, opts ExtractOptions) string {
	s := strings.TrimSpace(input)

	// 1. 剥离代码块
	if idx := strings.Index(s, "```json"); idx != -1 {
		s = s[idx+7:]
		if end := strings.Index(s, "```"); end != -1 {
			s = s[:end]
		}
	} else if idx := strings.Index(s, "```"); idx != -1 {
		s = s[idx+3:]
		if end := strings.Index(s, "```"); end != -1 {
			s = s[:end]
		}
	}
	s = strings.TrimSpace(s)

	// 2. 删除注释
	if opts.StripComments {
		s = removeComments(s)
	}

	// 3. 提取 JSON 主体
	var body string
	if opts.AllowArray {
		if start := strings.Index(s, "["); start != -1 {
			if end := strings.LastIndex(s, "]"); end > start {
				body = s[start : end+1]
			}
		}
	}
	if body == "" {
		start := strings.Index(s, "{")
		end := strings.LastIndex(s, "}")
		if start != -1 && end > start {
			body = s[start : end+1]
		}
	}
	if body == "" {
		return s
	}

	// 4. 修复尾部逗号
	if opts.FixTrailingCommas {
		body = fixTrailingCommas(body)
	}

	// 5. 单引号 → 双引号
	if opts.FixSingleQuotes {
		body = normalizeJSONQuotes(body)
	}

	return body
}

// removeComments 删除 s 中的 // 行注释与 /* */ 块注释。
func removeComments(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); {
		if i+1 < len(s) && s[i] == '/' && s[i+1] == '/' {
			for i < len(s) && s[i] != '\n' {
				i++
			}
			continue
		}
		if i+1 < len(s) && s[i] == '/' && s[i+1] == '*' {
			i += 2
			for i+1 < len(s) && !(s[i] == '*' && s[i+1] == '/') {
				i++
			}
			if i+1 < len(s) {
				i += 2
			}
			continue
		}
		out.WriteByte(s[i])
		i++
	}
	return out.String()
}

// fixTrailingCommas 删除对象/数组最后一个元素后的多余逗号。
func fixTrailingCommas(s string) string {
	s = strings.ReplaceAll(s, ",]", "]")
	s = strings.ReplaceAll(s, ",}", "}")
	return s
}

// normalizeJSONQuotes 把作为 JSON 字符串边界的单引号统一替换为双引号。
// 在双引号字符串内部的单引号原样保留（如缩写）。
func normalizeJSONQuotes(s string) string {
	runes := []rune(s)
	var out strings.Builder
	inDouble := false
	inSingle := false
	for i := 0; i < len(runes); {
		r := runes[i]
		switch r {
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
			out.WriteRune(r)
		case '\'':
			if inDouble {
				out.WriteRune(r)
				i++
				continue
			}
			inSingle = !inSingle
			out.WriteRune('"')
		case '\\':
			out.WriteRune(r)
			if i+1 < len(runes) {
				out.WriteRune(runes[i+1])
				i += 2
				continue
			}
		default:
			out.WriteRune(r)
		}
		i++
	}
	return out.String()
}
