//go:build windows

package server

// files_reveal_windows.go Windows 平台的「在文件夹中显示」实现（编译约束：仅 windows 构建）。
//
// explorer /select,<path> 选中该文件（目录则打开该目录窗口）；/select 与路径是
// 同一个参数（逗号分隔），不能拆成两个参数——拆开后 explorer 只会收到 /select 本身。
// 脱离启动（hide 窗口、新建进程组、不 Wait）：explorer 是长驻进程，Wait 会挂死请求。

import "os/exec"

// revealInFileManager 在资源管理器中定位 path（/select 选中文件）。
func revealInFileManager(path string) error {
	cmd := exec.Command("explorer", "/select,"+path)
	cmd.SysProcAttr = editorSysProcAttr // 复用 editors_windows.go 的脱离启动属性
	return cmd.Start()
}
