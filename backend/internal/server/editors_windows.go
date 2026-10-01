//go:build windows

package server

// editors_windows.go Windows 平台的编辑器探测与拉起实现（编译约束：仅 windows 构建）。
//
// 双通道探测：
//  1. 注册表 App Paths：HKLM/HKCU 的
//     SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\<exe> 默认值即安装路径；
//     64 位进程再补一遍 WOW64 32 位视图（32 位安装器写下的键在另一棵树）。
//  2. 常见安装路径探测：%ProgramFiles% / %ProgramFiles(x86)% / %LOCALAPPDATA%\Programs
//     下的固定目录名 + exe 名，存在即命中（覆盖"装了但没写注册表"的绿色版）。
//
// 拉起：GUI 编辑器直接 Start + CREATE_NEW_PROCESS_GROUP，hide 控制台窗口、不 Wait
// （不阻塞 HTTP 请求；编辑器是长驻进程，Wait 会挂死请求）。

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// appPathsBase App Paths 注册表子键（相对于 HKLM/HKCU 的 SOFTWARE）。
const appPathsBase = `SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths`

// appPathsViews 注册表视图：先默认视图（64 位进程=64 位树），再补 32 位视图。
var appPathsViews = []uint32{0, windows.KEY_WOW64_32KEY}

// registryAppPaths 读 HKLM+HKCU × 双注册表视图的 App Paths 默认值。
// 参数 names：exe 键名列表。返回值：键名 → 默认键值（去重后第一个非空值）。
func registryAppPaths(names []string) map[string]string {
	out := make(map[string]string, len(names))
	roots := []registry.Key{
		registry.LOCAL_MACHINE,
		registry.CURRENT_USER,
	}
	for _, root := range roots {
		for _, view := range appPathsViews {
			access := registry.READ | view
			for _, exeName := range names {
				if _, done := out[exeName]; done {
					continue // 已有命中（HKLM 优先于 HKCU，默认视图优先于 32 位视图）
				}
				k, err := registry.OpenKey(root, appPathsBase+`\`+exeName, access)
				if err != nil {
					continue // 键不存在/无权限：下一个
				}
				v, _, err := k.GetStringValue("")
				k.Close()
				if err == nil && v != "" {
					out[exeName] = v
				}
			}
		}
	}
	return out
}

// installRoots 常见安装根目录（去重 + 跳过不存在的，绿色版/便携版机器上部分根可能缺失）。
func installRoots() []string {
	seen := map[string]bool{}
	var roots []string
	for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "LOCALAPPDATA"} {
		v := os.Getenv(env)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		roots = append(roots, v)
	}
	// %LOCALAPPDATA% 下编辑器惯例装在 Programs 子目录。
	if v := os.Getenv("LOCALAPPDATA"); v != "" {
		p := filepath.Join(v, "Programs")
		if !seen[p] {
			roots = append(roots, p)
		}
	}
	return roots
}

// fileExists 文件存在且不是目录。
func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

// probeWindows Windows 双通道探测：注册表 App Paths → 系统目录 → 常见安装路径。
func probeWindows(e editorCatalogEntry) []string {
	var hits []string

	// 通道 ①：注册表（值可能是带引号/参数的原始命令行，取第一个 token 并去引号）。
	for _, raw := range registryAppPaths(e.winReg) {
		if exe := stringsFirstToken(raw); exe != "" && fileExists(exe) {
			hits = append(hits, exe)
		}
	}

	// 通道 ②a：%SystemRoot% 系统目录（记事本等系统自带）。
	if sysRoot := os.Getenv("SystemRoot"); sysRoot != "" {
		for _, rel := range e.winSystem {
			if p := filepath.Join(sysRoot, rel); fileExists(p) {
				hits = append(hits, p)
			}
		}
	}

	// 通道 ②b：常见安装路径。
	if e.winExe != "" {
		for _, root := range installRoots() {
			for _, dir := range e.winDirs {
				if p := filepath.Join(root, dir, e.winExe); fileExists(p) {
					hits = append(hits, p)
				}
			}
		}
	}
	return hits
}

// stringsFirstToken 从注册表默认键值里拆出可执行文件路径：
// 带引号（`"C:\...\Code.exe" --new-window`）取第一个引号段；不带引号则整体就是
// 路径（可能含空格，如 D:\app\Microsoft VS Code\Code.exe）——不能按空白切。
// 拆出的候选仍要过 fileExists，脏值自然被淘汰。
func stringsFirstToken(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if raw[0] == '"' {
		if i := strings.Index(raw[1:], `"`); i >= 0 {
			return raw[1 : 1+i]
		}
		return ""
	}
	return raw
}

// detectInstalledEditors Windows 入口：全目录双通道扫码，按 id 去重、目录序。
func detectInstalledEditors() []EditorInfo {
	return scanEditors(probeWindows)
}

// editorSysProcAttr 脱离启动属性：新建进程组 + 隐藏宿主控制台闪窗。
var editorSysProcAttr = &syscall.SysProcAttr{
	HideWindow:    true,
	CreationFlags: windows.CREATE_NEW_PROCESS_GROUP,
}

// launchEditorDetached 以指定编辑器打开文件：Start 后不 Wait（编辑器长驻，Wait 会挂死请求）。
func launchEditorDetached(exe, filePath string) error {
	cmd := exec.Command(exe, filePath)
	cmd.SysProcAttr = editorSysProcAttr
	return cmd.Start()
}

// openDefaultApp 系统默认方式打开文件（Windows：rundll32 url.dll,FileProtocolHandler，
// 内部走 ShellExecute，等价于资源管理器双击）。
func openDefaultApp(filePath string) error {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", filePath)
	cmd.SysProcAttr = editorSysProcAttr
	return cmd.Start()
}
