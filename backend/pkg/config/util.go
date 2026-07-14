package config

import (
	"os"
	"strings"
)

// resolveEnv 解析字符串中的环境变量引用。
//
// 支持两种语法:
//   - ${VAR}:          直接取环境变量 VAR，未设置返回空串。
//   - ${VAR:"default"}: 取 VAR，未设置或为空时返回 default(去掉外层双引号)。
//
// 参数:
//   - s: 待解析的字符串。若不以 ${ 开头、} 结尾，则原样返回。
//
// 返回:
//   - 解析后的字符串。非引用格式直接返回 s。
//
// 副作用: 仅读取环境变量，无写入。
func resolveEnv(s string) string {
	// 快速过滤: 长度至少 3 (${x}) 且首尾匹配 ${...}
	if len(s) > 3 && s[0] == '$' && s[1] == '{' && s[len(s)-1] == '}' {
		// 取出大括号内部内容。
		inner := s[2 : len(s)-1]
		// 尝试切分 "VAR:default" 形式。
		if varName, defaultPart, ok := strings.Cut(inner, ":"); ok {
			// 去掉 default 外层双引号。
			defaultVal := strings.Trim(defaultPart, "\"")
			// 环境变量优先; 非空则返回。
			if v := os.Getenv(varName); v != "" {
				return v
			}
			// 回退到默认值。
			return defaultVal
		}
		// 无默认值: 直接返回环境变量值(可能为空)。
		return os.Getenv(inner)
	}
	// 非引用格式: 原样返回。
	return s
}
