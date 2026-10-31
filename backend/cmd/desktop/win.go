//go:build windows

// win32 辅助：窗口显隐/前台、WM_CLOSE 拦截（关窗=隐藏到托盘）、单实例互斥、
// 按安装目录停止后端进程。全部走 user32/kernel32 原始调用，不引 cgo。
package main

import (
	"fmt"
	"hash/fnv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	swHide    = 0
	swShow    = 5
	swRestore = 9

	wmClose = 0x0010

	gwlpWndProc = -4

	processQueryLimitedInformation = 0x1000
	processTerminate               = 0x0001
)

var (
	user32DLL   = windows.NewLazySystemDLL("user32.dll")
	kernel32DLL = windows.NewLazySystemDLL("kernel32.dll")

	procShowWindow        = user32DLL.NewProc("ShowWindow")
	procSetForeground     = user32DLL.NewProc("SetForegroundWindow")
	procIsIconic          = user32DLL.NewProc("IsIconic")
	procFindWindowW       = user32DLL.NewProc("FindWindowW")
	procSetWindowLongPtrW = user32DLL.NewProc("SetWindowLongPtrW")
	procCallWindowProcW   = user32DLL.NewProc("CallWindowProcW")
)

// 子类化 WndProc 时保留原过程与回调地址（全局持有，避免 Go 回调被 GC）。
var (
	origWndProc  uintptr
	closeWndProc uintptr
)

// interceptClose 把窗口的 WM_CLOSE 改成"隐藏窗口"：点 X 不退出、后端继续跑，
// 真正退出走托盘菜单。原 WndProc 其余消息原样转发。
func interceptClose(hwnd unsafe.Pointer) {
	if hwnd == nil {
		return
	}
	closeWndProc = syscall.NewCallback(func(h, msg, wp, lp uintptr) uintptr {
		if msg == wmClose {
			procShowWindow.Call(h, swHide)
			return 0
		}
		r, _, _ := procCallWindowProcW.Call(origWndProc, h, msg, wp, lp)
		return r
	})
	idx := gwlpWndProc
	prev, _, _ := procSetWindowLongPtrW.Call(uintptr(hwnd), uintptr(idx), closeWndProc)
	origWndProc = prev
}

// showMainWindow 从托盘唤回窗口（最小化则先还原）。
func showMainWindow(hwnd unsafe.Pointer) {
	if hwnd == nil {
		return
	}
	if iconic, _, _ := procIsIconic.Call(uintptr(hwnd)); iconic != 0 {
		procShowWindow.Call(uintptr(hwnd), swRestore)
	} else {
		procShowWindow.Call(uintptr(hwnd), swShow)
	}
	procSetForeground.Call(uintptr(hwnd))
}

// findWindowByTitle 找已有实例的主窗口（隐藏状态下也能找到）。
func findWindowByTitle(title string) unsafe.Pointer {
	p, err := windows.UTF16PtrFromString(title)
	if err != nil {
		return nil
	}
	h, _, _ := procFindWindowW.Call(0, uintptr(unsafe.Pointer(p)))
	if h == 0 {
		return nil
	}
	return unsafe.Pointer(h)
}

// acquireSingleInstance 按安装目录创建命名互斥体；返回 (句柄, 是否首个实例)。
// 已有实例时调用方应 showMainWindow(findWindowByTitle(...)) 后直接退出。
func acquireSingleInstance(root string) (windows.Handle, bool) {
	h := fnv.New32a()
	normalized := strings.ToLower(strings.ReplaceAll(root, "\\", "/"))
	_, _ = h.Write([]byte(normalized))
	name, err := windows.UTF16PtrFromString(fmt.Sprintf("Local\\BlockMemoryAgent.Desktop.%08x", h.Sum32()))
	if err != nil {
		return 0, true
	}
	handle, err := windows.CreateMutex(nil, false, name)
	if err != nil {
		if err == windows.ERROR_ALREADY_EXISTS {
			return handle, false
		}
		return 0, true // 互斥体创建失败不阻断启动
	}
	return handle, true
}

// stopBackend 终止安装目录下的 bma-server.exe（不误杀其他安装/开发实例）。
func stopBackend(root string) int {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0
	}
	defer windows.CloseHandle(snap)

	rootLower := strings.ToLower(strings.TrimRight(root, "\\")) + "\\"
	killed := 0
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	for err := windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		if !strings.EqualFold(syscall.UTF16ToString(pe.ExeFile[:]), "bma-server.exe") {
			continue
		}
		h, err := windows.OpenProcess(processQueryLimitedInformation|processTerminate, false, pe.ProcessID)
		if err != nil {
			continue
		}
		var buf [windows.MAX_PATH]uint16
		size := uint32(len(buf))
		if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err == nil {
			path := strings.ToLower(syscall.UTF16ToString(buf[:size]))
			if strings.HasPrefix(path, rootLower) {
				if err := windows.TerminateProcess(h, 1); err == nil {
					killed++
				}
			}
		}
		windows.CloseHandle(h)
	}
	return killed
}
