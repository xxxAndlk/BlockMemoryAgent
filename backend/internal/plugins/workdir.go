package plugins

import (
	"os"
	"strings"
)

// WorkDirPlaceholder 是 plugins.yaml 插件 settings 卷映射（volumes）中的占位符，
// 插件 Init 时展开为 Agent 工作目录（bootstrap 的 os.Getwd()）绝对路径。
// 用于 ui_preview/ui_design 等需要挂载工作目录的插件：落盘路径随启动目录解析，
// 配置不再钉死绝对路径，支持同机在多个目录多开实例互不串产物。
const WorkDirPlaceholder = "${WORKDIR}"

// ExpandWorkDir 将 s 中的 ${WORKDIR} 替换为 workDir（反斜杠统一为正斜杠，
// docker -v 与容器内 file:// 均接受；不依赖平台，filepath.ToSlash 在非 Windows 上
// 原样保留反斜杠，会让同配置在不同平台产出不同的卷映射）；无占位符原样返回。
// workDir 为空回退 os.Getwd()。
// 与配置加载期的 ${VAR} 环境变量插值（仅整串 ${...} 形态才展开）互不干扰：
// "${WORKDIR}/sub:/out" 非整串形态，会原样透传到此处展开。
func ExpandWorkDir(s, workDir string) string {
	if !strings.Contains(s, WorkDirPlaceholder) {
		return s
	}
	if workDir == "" {
		if wd, err := os.Getwd(); err == nil {
			workDir = wd
		}
	}
	return strings.ReplaceAll(s, WorkDirPlaceholder, strings.ReplaceAll(workDir, `\`, "/"))
}
