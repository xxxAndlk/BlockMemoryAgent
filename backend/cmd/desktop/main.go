//go:build windows

// Command desktop 是 BlockMemoryAgent 的桌面外壳（Windows / WebView2）。
//
// 行为：
//  1. 从安装目录读 app\current.json + .env 的 HTTP_ADDR；
//  2. /api/health 通 → 直接开窗口；不通 → 按版本指针拉起该版本的 bma-server.exe
//    （BMA_HOME=安装目录、隐藏控制台、stdout/stderr 落 logs\desktop_*.log）并等就绪；
//  3. 用 WebView2 开一个自带标题栏/任务栏图标的原生窗口指向 UI（**不再落到浏览器**）；
//     WebView2 不可用时才退化为 Edge/Chrome 的 --app 模式。
//
// 关闭窗口只结束外壳进程，后端继续在后台运行（与"关掉窗口≠关掉服务"的桌面习惯一致）。
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	webview "github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"
)

const (
	appTitle           = "BlockMemoryAgent"
	defaultHTTPAddr    = "127.0.0.1:10010"
	user32Name         = "user32.dll"
	wmSetIcon          = 0x0080
	iconSmall          = 0
	iconBig            = 1
	imageIcon          = 1
	lrLoadFromFile     = 0x0010
	lrDefaultSize      = 0x0040
	createNoWindowFlag = 0x08000000
)

var (
	user32           = windows.NewLazySystemDLL(user32Name)
	procSendMessageW = user32.NewProc("SendMessageW")
	procLoadImageW   = user32.NewProc("LoadImageW")
)

func main() {
	root, err := resolveRoot()
	if err != nil {
		showError(appTitle, err.Error())
		return
	}

	// 单实例：已有外壳在跑（窗口可能已隐藏到托盘）→ 唤回它的窗口后退出。
	if _, first := acquireSingleInstance(root); !first {
		showMainWindow(findWindowByTitle(appTitle))
		return
	}
	base := baseURL(root)
	logPath := ""

	if !healthy(base) {
		if p, err := startServer(root); err != nil {
			showError(appTitle, "启动后端失败："+err.Error())
			return
		} else {
			logPath = p
		}
		if !waitHealthy(base, 90*time.Second) {
			msg := fmt.Sprintf("后端在 90 秒内未就绪：%s\n\n日志：%s", base, logPath)
			showErrorWindow(base, msg)
			return
		}
	}

	openWindow(root, base)
}

// resolveRoot 推断安装目录：<root>\BMA-Desktop.exe 或 <root>\app\versions\<v>\bin\bma-desktop.exe。
func resolveRoot() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(exe)
	if fileExists(filepath.Join(dir, "app", "current.json")) {
		return dir, nil
	}
	// bin → <ver> → versions → app → <root>
	candidate := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(dir))))
	if fileExists(filepath.Join(candidate, "app", "current.json")) {
		return candidate, nil
	}
	if home := os.Getenv("BMA_HOME"); home != "" && fileExists(filepath.Join(home, "app", "current.json")) {
		return home, nil
	}
	return "", fmt.Errorf("找不到安装目录（app\\current.json）：请把本程序放在安装目录根下，或运行 bma-app.ps1 install")
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// baseURL 从 .env 的 HTTP_ADDR 推导 UI 地址（缺省 127.0.0.1:10010）。
func baseURL(root string) string {
	addr := readEnvValue(filepath.Join(root, ".env"), "HTTP_ADDR")
	if addr == "" {
		addr = defaultHTTPAddr
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
		addr = "http://" + addr
	}
	return addr
}

func readEnvValue(path, key string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(strings.ToUpper(line), strings.ToUpper(key)+"=") {
			return strings.TrimSpace(strings.TrimPrefix(line, key+"="))
		}
	}
	return ""
}

func healthy(base string) bool {
	c := http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(base + "/api/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func waitHealthy(base string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if healthy(base) {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// startServer 按版本指针拉起后端，返回日志路径。
func startServer(root string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(root, "app", "current.json"))
	if err != nil {
		return "", fmt.Errorf("读取版本指针失败: %w", err)
	}
	var cur struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &cur); err != nil || cur.Version == "" {
		return "", fmt.Errorf("版本指针损坏: %v", err)
	}
	verDir := filepath.Join(root, "app", "versions", cur.Version)
	exe := filepath.Join(verDir, "bin", "bma-server.exe")
	if !fileExists(exe) {
		return "", fmt.Errorf("版本 %s 缺少 bin\\bma-server.exe", cur.Version)
	}
	logDir := filepath.Join(root, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return "", err
	}
	logPath := filepath.Join(logDir, "desktop_"+time.Now().Format("20060102_150405")+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}

	cmd := exec.Command(exe, "-web-dist", filepath.Join(verDir, "web", "dist"))
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "BMA_HOME="+root)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindowFlag}
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return logPath, fmt.Errorf("启动 bma-server: %w", err)
	}
	// 不等待子进程；外壳进程退出后后端继续运行。
	_ = cmd.Process.Release()
	return logPath, nil
}

// openWindow 用 WebView2 打开原生窗口；失败时退化为浏览器的应用窗口模式。
func openWindow(root, base string) {
	w := webview.NewWithOptions(webview.WebViewOptions{
		DataPath: filepath.Join(root, "app", "webview2"),
		WindowOptions: webview.WindowOptions{
			Title:  appTitle,
			Width:  1320,
			Height: 880,
			Center: true,
		},
	})
	if w == nil {
		openBrowserFallback(root, base)
		return
	}
	defer w.Destroy()

	hwnd := w.Window()
	setWindowIcon(hwnd, filepath.Join(root, "app", "icon.ico"))

	// 关窗 = 隐藏到托盘（后端继续跑）；真正退出走托盘菜单，并停掉该安装目录的后端。
	interceptClose(hwnd)
	startTray(root, base, func() unsafe.Pointer { return hwnd }, func() {
		stopBackend(root)
		quitTray()
		w.Terminate()
	})

	w.Navigate(base)
	w.Run()
}

// setWindowIcon 给原生窗口设置大/小图标（任务栏 + 标题栏）。
func setWindowIcon(hwnd unsafe.Pointer, icoPath string) {
	if hwnd == nil || !fileExists(icoPath) {
		return
	}
	p, err := windows.UTF16PtrFromString(icoPath)
	if err != nil {
		return
	}
	h, _, _ := procLoadImageW.Call(0, uintptr(unsafe.Pointer(p)), imageIcon, 0, 0, lrLoadFromFile|lrDefaultSize)
	if h == 0 {
		return
	}
	procSendMessageW.Call(uintptr(hwnd), wmSetIcon, iconBig, h)
	procSendMessageW.Call(uintptr(hwnd), wmSetIcon, iconSmall, h)
}

// openBrowserFallback WebView2 缺失时的兜底：优先 Edge/Chrome 的 --app 无边框窗口。
func openBrowserFallback(root, base string) {
	candidates := []string{
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
	}
	profile := filepath.Join(root, "app", "browser-profile")
	for _, exe := range candidates {
		if !fileExists(exe) {
			continue
		}
		cmd := exec.Command(exe, "--app="+base, "--user-data-dir="+profile, "--no-first-run", "--window-size=1320,880")
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: false}
		if err := cmd.Start(); err == nil {
			_ = cmd.Process.Release()
			return
		}
	}
	// 最后兜底：系统默认浏览器
	_ = exec.Command("cmd", "/c", "start", "", base).Start()
}

// showErrorWindow 用 WebView2 直接显示错误页（避免控制台窗口）。
func showErrorWindow(base, message string) {
	w := webview.New(false)
	if w == nil {
		showError(appTitle, message)
		return
	}
	defer w.Destroy()
	w.SetTitle(appTitle)
	w.SetSize(760, 420, webview.HintNone)
	html := "<html><head><meta charset=\"utf-8\"><style>" +
		"body{font-family:'Segoe UI',sans-serif;background:#0d1929;color:#e2e8f0;padding:32px;line-height:1.6}" +
		"h1{font-size:18px;color:#5eead4;margin:0 0 12px}pre{white-space:pre-wrap;background:#122434;padding:16px;border-radius:10px;font-size:12px}" +
		"a{color:#7dd3fc}</style></head><body><h1>BlockMemoryAgent 启动失败</h1><pre>" + htmlEscape(message) +
		"</pre><p>目标：<a href=\"" + base + "\">" + base + "</a></p></body></html>"
	w.SetHtml(html)
	w.Run()
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;")
	return r.Replace(s)
}

// showError 最后一层兜底：系统消息框。
func showError(title, message string) {
	text, _ := windows.UTF16PtrFromString(message)
	caption, _ := windows.UTF16PtrFromString(title)
	user32.NewProc("MessageBoxW").Call(0,
		uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(caption)), 0x10)
}
