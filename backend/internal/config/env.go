package config

import (
	"bufio"   // 提供按行扫描 .env 文件的能力
	"fmt"     // 用于包装错误信息
	"os"      // 用于打开文件、读写环境变量
	"strings" // 用于裁剪与切分字符串
)

// LoadEnvFile 从指定路径的 .env 文件加载环境变量到进程环境。
// 职责：逐行解析 KEY=VALUE，跳过空行与注释；已存在的环境变量不会被覆盖
// （保证命令行或系统环境注入的变量优先级最高）。
// 参数：path 为 .env 文件路径。
// 返回：文件打开或扫描出错时返回包装后的 error；正常解析返回 nil。
// 副作用：调用 os.Setenv 写入进程环境变量，影响后续配置插值与程序运行。
func LoadEnvFile(path string) error {
	// 打开 .env 文件，失败时包装错误返回
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open .env file: %w", err)
	}
	// 确保函数退出时关闭文件句柄
	defer file.Close()

	// 创建按行扫描器，逐行读取文件内容
	scanner := bufio.NewScanner(file)
	// 行号计数器，便于定位解析问题
	lineNum := 0
	for scanner.Scan() {
		// 递增行号
		lineNum++
		// 去除首尾空白，得到当前行内容
		line := strings.TrimSpace(scanner.Text())

		// 跳过空行和以 # 开头的注释行
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// 解析 KEY=VALUE 格式，返回键值与是否成功
		key, value, ok := parseEnvLine(line)
		if !ok {
			// 格式不合法的行直接跳过，保证解析鲁棒性
			continue
		}

		// 已存在的环境变量不覆盖（命令行/系统环境优先）
		if os.Getenv(key) != "" {
			continue
		}

		// 写入进程环境变量，供后续配置插值使用
		os.Setenv(key, value)
	}

	// 返回扫描过程中遇到的错误（如行过长等），无错则返回 nil
	return scanner.Err()
}

// parseEnvLine 解析单行 KEY=VALUE 格式的环境变量定义。
// 职责：按第一个等号切分键值，去除首尾空白与包裹引号。
// 参数：line 为已去除首尾空白的单行文本。
// 返回：key、value 与是否解析成功；无等号或等号在首位时返回 false。
// 副作用：无。
func parseEnvLine(line string) (string, string, bool) {
	// 查找第一个等号位置
	idx := strings.Index(line, "=")
	// 等号不存在或位于行首（键为空）则解析失败
	if idx <= 0 {
		return "", "", false
	}

	// 等号左侧为键，去除首尾空白
	key := strings.TrimSpace(line[:idx])
	// 等号右侧为值，去除首尾空白
	value := strings.TrimSpace(line[idx+1:])

	// 去除值两侧的成对引号（双引号或单引号）
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') ||
			(value[0] == '\'' && value[len(value)-1] == '\'') {
			value = value[1 : len(value)-1]
		}
	}

	// 返回解析结果
	return key, value, true
}
