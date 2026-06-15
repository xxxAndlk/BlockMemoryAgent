package config

import (
	"os"
)

// resolveEnv 解析环境变量引用，如 ${OPENAI_API_KEY}
func resolveEnv(s string) string {
	if len(s) > 3 && s[0] == '$' && s[1] == '{' && s[len(s)-1] == '}' {
		return os.Getenv(s[2 : len(s)-1])
	}
	return s
}
