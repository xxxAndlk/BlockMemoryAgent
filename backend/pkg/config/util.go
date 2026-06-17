package config

import (
	"os"
	"strings"
)

// resolveEnv 解析环境变量引用，支持 ${VAR} 和 ${VAR:"default"}
func resolveEnv(s string) string {
	if len(s) > 3 && s[0] == '$' && s[1] == '{' && s[len(s)-1] == '}' {
		inner := s[2 : len(s)-1]
		if varName, defaultPart, ok := strings.Cut(inner, ":"); ok {
			defaultVal := strings.Trim(defaultPart, "\"")
			if v := os.Getenv(varName); v != "" {
				return v
			}
			return defaultVal
		}
		return os.Getenv(inner)
	}
	return s
}
