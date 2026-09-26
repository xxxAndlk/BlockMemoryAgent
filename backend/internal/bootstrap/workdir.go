package bootstrap

import (
	"fmt"           // 错误包装
	"os"            // 目录创建
	"path/filepath" // 路径拼接与绝对判定
	"strings"       // 配置值去空白

	"github.com/blockmemory/agent/backend/internal/config" // config.HomeDir 安装目录解析
)

// resolveDefaultWorkDir 解析 config agent.default_workdir → 可用的默认工作目录绝对路径。
//
// 语义：
//   - 空（含纯空白）→ 返回 ""，调用方回落进程 cwd（旧行为：服务从哪儿启动就用哪儿）；
//   - 绝对路径 → 清洗后原样使用；
//   - 相对路径 → 拼到安装目录（BMA_HOME，经 config.HomeDir 解析）下，如 workspace →
//     D:\WebApp\bma\workspace。
//
// 解析成功即 MkdirAll：默认目录是**新会话未选工作目录时的落盘根**（文件读写、
// .bma 临时目录/共享记忆、PROJECT.md 都落在这里），建不出必须在启动期硬失败，
// 否则错误要拖到 Agent 第一次写文件才暴露。返回值非空时保证目录已存在。
//
// 参数：
//   - value：config agent.default_workdir 原始值（未做环境变量展开，路径无需）。
//
// 返回：
//   - string：绝对路径；未配置返回 ""。
//   - error：安装目录无法解析或目录创建失败。
func resolveDefaultWorkDir(value string) (string, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return "", nil
	}
	dir := v
	if !filepath.IsAbs(dir) {
		home, err := config.HomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve agent.default_workdir %q: %w", v, err)
		}
		dir = filepath.Join(home, dir)
	}
	dir = filepath.Clean(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create agent.default_workdir %s: %w", dir, err)
	}
	return dir, nil
}
