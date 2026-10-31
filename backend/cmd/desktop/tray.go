//go:build windows

// 系统托盘：左键唤回窗口；右键菜单「打开主窗口 / 在浏览器中打开 / 退出（停止后台）」。
// 托盘关闭 = 真正退出（并停止该安装目录下的后端）；窗口的 X 只是隐藏（见 win.go）。
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"unsafe"

	"github.com/energye/systray"
)

// startTray 创建托盘图标（非阻塞：内部有独立消息泵线程）。
// hwnd 用函数是为了拿"当前"窗口句柄（窗口可能被隐藏/重建）。
func startTray(root, base string, hwnd func() unsafe.Pointer, onExit func()) {
	start, _ := systray.RunWithExternalLoop(func() {
		if ico, err := os.ReadFile(filepath.Join(root, "app", "icon.ico")); err == nil {
			systray.SetIcon(ico)
		}
		systray.SetTitle("")
		systray.SetTooltip("BlockMemoryAgent（点击打开，右键菜单）")

		openItem := systray.AddMenuItem("打开主窗口", "显示 BlockMemoryAgent 窗口")
		openItem.Click(func() { showMainWindow(hwnd()) })

		browserItem := systray.AddMenuItem("在浏览器中打开", "用默认浏览器打开 UI")
		browserItem.Click(func() { _ = exec.Command("cmd", "/c", "start", "", base).Start() })

		systray.AddSeparator()

		quitItem := systray.AddMenuItem("退出（停止后台）", "停止后端服务并退出")
		quitItem.Click(onExit)

		// 左键单击托盘图标 = 打开窗口；右键仍弹默认菜单。
		systray.SetOnClick(func(systray.IMenu) { showMainWindow(hwnd()) })
	}, func() {})
	start()
}

// quitTray 关闭托盘图标（退出流程里调用）。
func quitTray() { systray.Quit() }
