package graph

// 环境提示生成器：把工作目录、操作系统、数据库连接等运行时信息
// 拼成一段 prompt 段落，注入 Assistant 的系统提示词。
// 目的：让 LLM 知道当前实际环境，避免瞎猜路径 / 连接串导致工具调用失败。

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
// 返回：形如 "工作目录: /abs/path" 的字符串。
func workdirHint() string {
	wd, err := os.Getwd() // 取当前工作目录
	if err != nil {
		wd = "." // 取不到时回退相对路径
	}
	abs, err := absPath(wd) // 转绝对路径
	if err == nil {
		wd = abs
	}
	return "工作目录: " + wd
}

// envHint 返回数据库连接 + 运行时环境信息，供 LLM 在执行 DB 检查/脚本类任务时直接使用。
//
// 之前 LLM 不知道 Redis 密码 / PG DSN，写出的脚本连不上库。把 .env 里的连接信息
// 注入 prompt 后，Agent 可以直接写 python 脚本验证存储。
// 返回：多行字符串，每行一项环境信息。
func envHint() string {
	var parts []string
	parts = append(parts, workdirHint()) // 工作目录（必需）

	parts = append(parts, "操作系统: "+runtime.GOOS) // GOOS：windows/linux/darwin

	// Redis 连接信息（仅当配置了 REDIS_ADDR 时输出）
	if addr := os.Getenv("REDIS_ADDR"); addr != "" {
		pw := os.Getenv("REDIS_PASSWORD")
		if pw != "" {
			parts = append(parts, "Redis: "+addr+" (密码: "+pw+")")
		} else {
			parts = append(parts, "Redis: "+addr+" (无密码)")
		}
	}
	// PostgreSQL DSN（仅当配置了 POSTGRES_DSN 时输出）
	if dsn := os.Getenv("POSTGRES_DSN"); dsn != "" {
		parts = append(parts, "PostgreSQL DSN: "+dsn)
	}

	// 可用运行时：告知 LLM 可直接调用的解释器/编译器
	parts = append(parts, "可用运行时: python (Windows), go")
	// 工作区子目录：约定 LLM 在此创建文件，便于清理
	parts = append(parts, "工作区子目录: workspace/ （可在此创建文件）")

	return strings.Join(parts, "\n")
}

// absPath 返回绝对路径（Windows 下也用反斜杠）。
// 已是绝对路径（Unix / 开头、Windows 盘符开头）则原样返回；否则拼接 os.Getwd()。
// 参数：p 待判断的路径。
// 返回：绝对路径；获取 cwd 失败时返回原路径 + error。
func absPath(p string) (string, error) {
	// 已是绝对路径的判定：
	//   - Unix 风格 /xxx
	//   - 反斜杠开头 \xxx
	//   - Windows 盘符 X:...
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

// joinPath 拼接两段路径，保证中间有且仅有一个分隔符。
// 用 os.PathSeparator 而非硬编码 "/"，保证 Windows 兼容。
// 参数：a 前缀路径，b 后缀路径。
// 返回：拼接后的路径。
func joinPath(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	sep := string(os.PathSeparator)
	if !strings.HasSuffix(a, sep) {
		a += sep // 前缀不以分隔符结尾则补一个
	}
	return a + b
}

// fmtEnvSection 把 envHint 格式化成 prompt 段落。
// 加上"【运行环境】"标题，便于 LLM 在系统提示词中识别这一段。
// 返回：可直接拼入 system_prompt 的多行字符串。
func fmtEnvSection() string {
	return "【运行环境】\n" + envHint()
}

// ensureNoNil 防止 nil 字符串在 prompt 拼接时 panic（极小概率，但 LLM 输出不可控）。
// 当前实现：空串原样返回，非空串用 fmt.Sprintf 格式化（确保是 string 类型）。
// 参数：s 待保护字符串。
// 返回：安全的字符串。
func ensureNoNil(s string) string {
	if s == "" {
		return "" // 空串直接返回
	}
	return fmt.Sprintf("%s", s)
}
