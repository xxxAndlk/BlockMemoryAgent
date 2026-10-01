//go:build !windows

package server

// files_reveal_other.go 非 Windows 平台（macOS / Linux）的「在文件夹中显示」实现。
//
// macOS：open -R <path> 在 Finder 中显示并选中（与文件管理器"显示所在位置"语义一致）。
// Linux：xdg-open 没有 /select 等价物，退化为打开所在目录（xdg-open <dir>）。
// 其余 !windows 平台（plan9 等）：返回错误（前端 5xx 降级）。
// 同样 Start 后不 Wait：open/xdg-open 立即返回，文件管理器长驻不阻塞 HTTP 请求。

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
)

// revealInFileManager 在系统文件管理器中定位 path。
func revealInFileManager(path string) error {
	switch runtime.GOOS {
	case "darwin":
		cmd := exec.Command("open", "-R", path)
		cmd.SysProcAttr = detachSysProcAttr() // 复用 editors_other.go 的脱离启动属性
		return cmd.Start()
	case "linux":
		cmd := exec.Command("xdg-open", filepath.Dir(path))
		cmd.SysProcAttr = detachSysProcAttr()
		return cmd.Start()
	default:
		return fmt.Errorf("当前平台 %s 不支持在文件夹中显示", runtime.GOOS)
	}
}
