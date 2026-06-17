package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// LoadEnvFile 从 .env 文件加载环境变量
// 已存在的环境变量不会被覆盖
func LoadEnvFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open .env file: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())

		// 跳过空行和注释
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// 解析 KEY=VALUE
		key, value, ok := parseEnvLine(line)
		if !ok {
			continue
		}

		// 已存在的环境变量不覆盖（命令行/系统环境优先）
		if os.Getenv(key) != "" {
			continue
		}

		os.Setenv(key, value)
	}

	return scanner.Err()
}

// parseEnvLine 解析单行环境变量
func parseEnvLine(line string) (string, string, bool) {
	idx := strings.Index(line, "=")
	if idx <= 0 {
		return "", "", false
	}

	key := strings.TrimSpace(line[:idx])
	value := strings.TrimSpace(line[idx+1:])

	// 去除引号包裹
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') ||
			(value[0] == '\'' && value[len(value)-1] == '\'') {
			value = value[1 : len(value)-1]
		}
	}

	return key, value, true
}
