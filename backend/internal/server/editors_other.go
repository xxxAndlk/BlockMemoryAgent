//go:build !windows

package server

// editors_other.go 非 Windows 平台（macOS / Linux）的编辑器探测与拉起实现。
//
// macOS：扫 /Applications 下的 *.app 包（编辑器是 .app 目录，exe 直接记包路径，
// 拉起用 `open -a <app> <file>`——等价于 Finder 双击打开）。
// Linux：exec.LookPath 按命令名探测（code/trae/cursor/sublime_text/notepad++/webstorm）。
//
// 拉起同样 Start 后不 Wait：open/xdg-open 立即返回，编辑器长驻不阻塞 HTTP 请求。

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
)

// darwinAppDir macOS 应用程序目录（只扫系统级 /Applications，用户级 ~/Applications
// 里的编辑器多数也会在 /Applications 有副本或软链；扫两处意义不大且徒增耗时）。
const darwinAppDir = "/Applications"

// probeOther 非 Windows 平台探测：darwin 看 .app 是否存在，Linux 走 LookPath。
func probeOther(e editorCatalogEntry) []string {
	if runtime.GOOS == "darwin" {
		var hits []string
		for _, app := range e.darwinApps {
			if p := filepath.Join(darwinAppDir, app); dirExists(p) {
				hits = append(hits, p)
			}
		}
		return hits
	}
	// Linux/其他 Unix：PATH 探测命令名。
	var hits []string
	for _, bin := range e.linuxBins {
		if p, err := exec.LookPath(bin); err == nil {
			hits = append(hits, p)
		}
	}
	return hits
}

// dirExists 目录存在（.app 是目录包）。
func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// detectInstalledEditors 非 Windows 入口：全目录扫码，按 id 去重、目录序。
func detectInstalledEditors() []EditorInfo {
	return scanEditors(probeOther)
}

// launchEditorDetached 以指定编辑器打开文件：
// darwin 的 exe 是 .app 包路径（open -a），Linux 的 exe 是二进制路径（直接执行）。
func launchEditorDetached(exe, filePath string) error {
	var cmd *exec.Cmd
	if runtime.GOOS == "darwin" {
		cmd = exec.Command("open", "-a", exe, filePath)
	} else {
		cmd = exec.Command(exe, filePath)
	}
	cmd.SysProcAttr = detachSysProcAttr()
	return cmd.Start()
}

// openDefaultApp 系统默认方式打开文件：macOS `open`、Linux `xdg-open`。
func openDefaultApp(filePath string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	cmd := exec.Command(name, filePath)
	cmd.SysProcAttr = detachSysProcAttr()
	return cmd.Start()
}

// detachSysProcAttr 脱离启动属性：darwin/linux 用 Setsid 另开会话（编辑器不随
// 服务进程组收信号）。仅 darwin/linux 用 syscall.SysProcAttr；其余 !windows
// 平台（plan9 等 SysProcAttr 字段不同）在 editors_sysattr_fallback.go 返回 nil。
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
