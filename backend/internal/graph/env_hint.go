package graph

import (
	"fmt"
	"os"
	"runtime"
	"strings"
)

// workdirHint 返回当前工作目录的绝对路径，供 LLM 在 prompt 中感知本地文件系统位置。
//
// ToolExecutor 默认用 os.Getwd() 作为工作目录，Assistant 写文件/跑命令都以此为根。
// 若不在 prompt 里告知 LLM，LLM 会瞎猜路径（如 /tmp、/home/user），导致 WriteFile
// 写到非预期位置，或 RunCommand 找不到文件。
func workdirHint() string {
	wd, err := os.Getwd()
	if err != nil {
		wd = "."
	}
	abs, err := absPath(wd)
	if err == nil {
		wd = abs
	}
	return "工作目录: " + wd
}

// envHint 返回数据库连接 + 运行时环境信息，供 LLM 在执行 DB 检查/脚本类任务时直接使用。
//
// 之前 LLM 不知道 Redis 密码 / PG DSN，写出的脚本连不上库。把 .env 里的连接信息
// 注入 prompt 后，Agent 可以直接写 python 脚本验证存储。
func envHint() string {
	var parts []string
	parts = append(parts, workdirHint())

	parts = append(parts, "操作系统: "+runtime.GOOS)

	if addr := os.Getenv("REDIS_ADDR"); addr != "" {
		pw := os.Getenv("REDIS_PASSWORD")
		if pw != "" {
			parts = append(parts, "Redis: "+addr+" (密码: "+pw+")")
		} else {
			parts = append(parts, "Redis: "+addr+" (无密码)")
		}
	}
	if dsn := os.Getenv("POSTGRES_DSN"); dsn != "" {
		parts = append(parts, "PostgreSQL DSN: "+dsn)
	}

	parts = append(parts, "可用运行时: python (Windows), go")
	parts = append(parts, "工作区子目录: workspace/ （可在此创建文件）")

	return strings.Join(parts, "\n")
}

// absPath 返回绝对路径（Windows 下也用反斜杠）。
func absPath(p string) (string, error) {
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "\\") || (len(p) >= 2 && p[1] == ':') {
		return p, nil
	}
	// 相对路径 → 用 os.Getwd 拼接
	wd, err := os.Getwd()
	if err != nil {
		return p, err
	}
	return joinPath(wd, p), nil
}

func joinPath(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	sep := string(os.PathSeparator)
	if !strings.HasSuffix(a, sep) {
		a += sep
	}
	return a + b
}

// fmtEnvSection 把 envHint 格式化成 prompt 段落。
func fmtEnvSection() string {
	return "【运行环境】\n" + envHint()
}

// ensureNoNil 防止 nil 字符串在 prompt 拼接时 panic（极小概率，但 LLM 输出不可控）。
func ensureNoNil(s string) string {
	if s == "" {
		return ""
	}
	return fmt.Sprintf("%s", s)
}
