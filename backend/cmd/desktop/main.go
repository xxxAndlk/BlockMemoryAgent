//go:build windows

// Command desktop 是 BlockMemoryAgent 的桌面外壳（Windows / WebView2）。
//
// 行为：
//  1. 从安装目录读 app\current.json + .env 的 HTTP_ADDR；
//  2. /api/health 通 → 直接开窗口；不通 → 自举依赖（PostgreSQL/Redis，见 deps.go）
//    → 按版本指针拉起该版本的 bma-server.exe（隐藏控制台、stdout/stderr 落
//    logs\desktop_*.log）→ 等就绪；
//  3. 用 WebView2 开一个自带标题栏/任务栏图标的原生窗口指向 UI（**不再落到浏览器**）；
//     依赖自举期间窗口显示启动进度页，就绪后切到 UI；失败显示带「重试」的错误页。
//     WebView2 不可用时才退化为 Edge/Chrome 的 --app 模式。
//
// 关闭窗口只隐藏到托盘，后端继续在后台运行（与"关掉窗口≠关掉服务"的桌面习惯一致）。
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	ensureDepsOnly := flag.Bool("ensure-deps", false, "只做依赖自举后退出（供脚本启动器/安装流程复用）")
	depsOut := flag.String("deps-out", "", "依赖自举结果 JSON 输出路径")
	stopOnly := flag.Bool("stop", false, "只停掉本安装目录的后端与自带运行时后退出（安装器卸载前用）")
	flag.Parse()

	root, err := resolveRoot()
	if err != nil {
		showError(appTitle, err.Error())
		return
	}

	// CLI 模式：停掉后台（卸载/换版本前调用）。必须在单实例互斥之前——否则已运行的
	// 外壳会让本进程直接退出，后端与数据库就还活着（文件被占，目录删不掉）。
	if *stopOnly {
		stopBackend(root)
		stopEmbeddedRuntime(root)
		stopMediaService(root)
		stopLocalMediaBridge(root)
		return
	}

	// CLI 模式：只自举依赖、不开窗口。**必须在单实例互斥之前返回**——否则已运行
	// 的外壳会让本进程直接退出，调用方会把"没干活"误判成"依赖已就绪"。
	if *ensureDepsOnly {
		rep, derr := ensureAllDeps(root, nil)
		if werr := writeDepsReport(*depsOut, rep); werr != nil {
			rep.OK = false
			rep.Error = strings.TrimSpace(rep.Error + "；写结果文件失败：" + werr.Error())
		}
		if derr != nil || !rep.OK {
			os.Exit(1)
		}
		os.Exit(0)
	}

	// 单实例：已有外壳在跑（窗口可能已隐藏到托盘）→ 唤回它的窗口后退出。
	if _, first := acquireSingleInstance(root); !first {
		showMainWindow(findWindowByTitle(appTitle))
		return
	}
	base := baseURL(root)
	runApp(root, base)
}

// runApp 建窗口并进入消息循环；依赖自举与后端拉起在后台完成，就绪后窗口切到 UI。
//
// 只有一个窗口：启动期间显示进度页，就绪后导航到 UI，失败显示带重试的错误页——
// 避免"进度窗关了、主窗口又闪出来"的窗口抖动，也避免残留窗口挡着 UI。
//
// 注意：w.Run() 必须与建窗在**同一个 goroutine**——WebView2 控制器绑定创建它的
// 线程，消息泵跑到别的线程上会让窗口永远白屏（内容不渲染）。
func runApp(root, base string) {
	// 锁定当前 goroutine 到当前 OS 线程：WebView2 控制器绑定创建它的线程，
	// 消息泵必须跑在同一个线程上，否则窗口白屏（Go 的 goroutine 随时可能换线程）。
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

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
		// WebView2 不可用：退化为浏览器应用窗口；依赖与后端照旧自举。
		go func() {
			if err := bootstrap(root, base, nil); err != nil {
				showError(appTitle, launchErrorText(err))
			}
		}()
		openBrowserFallback(root, base)
		return
	}
	defer w.Destroy()

	hwnd := w.Window()
	setWindowIcon(hwnd, filepath.Join(root, "app", "icon.ico"))

	// 关窗 = 隐藏到托盘（后端继续跑）；真正退出走托盘菜单，并停掉该安装目录的后端
	// 与自带运行时（PostgreSQL/Redis）。
	interceptClose(hwnd)
	startTray(root, base, func() windows.Handle { return windows.Handle(uintptr(hwnd)) }, func() {
		stopBackend(root)
		stopEmbeddedRuntime(root)
		stopMediaService(root)
		stopLocalMediaBridge(root)
		quitTray()
		w.Terminate()
	})

	// 启动页：先显示进度，避免开窗即空白（首次可能要拉镜像，耗时几分钟）。
	w.SetHtml(startupHTML("正在准备启动 ..."))

	// 错误页的「重试」按钮 → 重走一遍自举。
	_ = w.Bind("bmaRetry", func() {
		go func() {
			if err := bootstrap(root, base, w); err != nil {
				w.Dispatch(func() { w.SetHtml(errorHTML(launchErrorText(err))) })
			}
		}()
	})

	// 首启向导已移除（2026-11-18）：启动不弹任何配置页，直接进 UI。模型由用户在
	// 模型页手动新增（新装机器给的是一份空注册表模板，不再预填火山模型）。这里只补
	// models.json（缺失或 0 字节时用包内模板铺一份），补不了也不阻断启动。
	seedModelsTemplate(root)

	// 依赖自举与后端拉起放后台：主 goroutine 留给 WebView2 消息泵。
	go func() {
		if err := bootstrap(root, base, w); err != nil {
			w.Dispatch(func() { w.SetHtml(errorHTML(launchErrorText(err))) })
		}
	}()

	w.Run()
}

// bootstrap 自举依赖 → 起后端 → 等就绪；就绪后把窗口切到 UI。
//
// w 为 nil 表示没有窗口（WebView2 缺失的浏览器兜底路径），此时只做后台动作。
func bootstrap(root, base string, w webview.WebView) error {
	if healthy(base) {
		navigate(w, base)
		go runMediaSetup(root, base)
		return nil
	}

	setProgress(w, "检查依赖（PostgreSQL / Redis）...")
	// 依赖自举：安装包带了自带运行时就用它（目标机免装 Docker），否则走 Docker 路径。
	rep, err := ensureAllDeps(root, func(step string) { setProgress(w, step) })
	if err != nil {
		return &launchError{msg: rep.Error, hint: rep.Hint}
	}

	// 原生媒体服务（免 Docker 的生图/生视频）：失败只降级媒体能力，不阻断启动。
	if err := ensureMediaService(root, func(step string) { setProgress(w, step) }); err != nil {
		logMediaNotice(root, err)
	}

	setProgress(w, "启动后端 ...")
	logPath, pid, err := startServer(root)
	if err != nil {
		return &launchError{
			msg:  "启动后端失败：" + err.Error(),
			hint: "日志目录：" + filepath.Join(root, "logs"),
		}
	}

	setProgress(w, "等待后端就绪 ...")
	if !waitHealthyWithProc(base, pid, 90*time.Second) {
		return &launchError{
			msg:    "后端在 90 秒内未就绪",
			hint:   "日志：" + logPath,
			detail: tailFile(logPath, 20),
		}
	}

	navigate(w, base)
	// 媒体栈体积大、构建慢，放后台；失败只落日志（插件降级语义）。
	go runMediaSetup(root, base)
	// 启动后顺手检查一次更新（没配 BMA_UPDATE_URL 就静默跳过）。
	go runUpdateCheck(root, false)
	return nil
}

// launchError 启动失败的结构化描述（原因 + 建议 + 日志片段）。
type launchError struct {
	msg    string
	hint   string
	detail string
}

func (e *launchError) Error() string { return e.msg }

// launchErrorText 把错误渲染成错误页正文。
func launchErrorText(err error) string {
	var le *launchError
	if ok := errors.As(err, &le); ok {
		s := le.msg
		if le.hint != "" {
			s += "\n\n建议：" + le.hint
		}
		if le.detail != "" {
			s += "\n\n日志片段：\n" + le.detail
		}
		return s
	}
	return err.Error()
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

// waitHealthyWithProc 等后端就绪；进程提前退出（严格启动 fail-fast）立即判失败，
// 不再像旧版那样空等满 90 秒才报一句"未就绪"。
func waitHealthyWithProc(base string, pid uint32, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if healthy(base) {
			return true
		}
		if pid != 0 && !processAlive(pid) {
			return false
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// startServer 按版本指针拉起后端，返回日志路径与进程 pid。
func startServer(root string) (string, uint32, error) {
	raw, err := os.ReadFile(filepath.Join(root, "app", "current.json"))
	if err != nil {
		return "", 0, fmt.Errorf("读取版本指针失败: %w", err)
	}
	var cur struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &cur); err != nil || cur.Version == "" {
		return "", 0, fmt.Errorf("版本指针损坏: %v", err)
	}
	verDir := filepath.Join(root, "app", "versions", cur.Version)
	exe := filepath.Join(verDir, "bin", "bma-server.exe")
	if !fileExists(exe) {
		return "", 0, fmt.Errorf("版本 %s 缺少 bin\\bma-server.exe", cur.Version)
	}
	logDir := filepath.Join(root, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return "", 0, err
	}
	logPath := filepath.Join(logDir, "desktop_"+time.Now().Format("20060102_150405")+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return "", 0, err
	}

	cmd := exec.Command(exe, "-web-dist", filepath.Join(verDir, "web", "dist"))
	// 工作目录 = 安装目录下的 workspace：会话产物（媒体桥的 .bma\od-artifacts 等）
	// 落在用户可见的工作目录，而不是程序目录。
	workDir := filepath.Join(root, "workspace")
	if !fileExists(workDir) {
		workDir = root
	}
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(),
		"BMA_HOME="+root,
		// 插件配置里的路径占位符（整串形式，配置插值只认整串 ${VAR}）：
		// 随包 Node + 三个本地 MCP 桥（生图/视频、浏览器截图、真机 GUI 直控）——
		// 全部走 stdio，目标机不需要 Docker、不需要系统 Node。
		"BMA_NODE_EXE="+filepath.Join(root, runtimeDirName, "node", "node.exe"),
		"BMA_MEDIA_BRIDGE="+filepath.Join(root, runtimeDirName, "media", "open-design-mcp", "server.js"),
		"BMA_UI_PREVIEW_MCP="+filepath.Join(root, runtimeDirName, "media", "ui-preview-mcp", "node_modules", "@playwright", "mcp", "cli.js"),
		"BMA_COMPUTER_USE_MCP="+filepath.Join(root, runtimeDirName, "media", "computer-use-mcp", "node_modules", "@zavora-ai", "computer-use-mcp", "dist", "launch.js"),
		// Chromium 浏览器二进制随包（离线可用）：Playwright 按此环境变量找浏览器。
		"BMA_PLAYWRIGHT_BROWSERS="+filepath.Join(root, runtimeDirName, "media", "playwright-browsers"),
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindowFlag}
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return logPath, 0, fmt.Errorf("启动 bma-server: %w", err)
	}
	pid := uint32(cmd.Process.Pid)
	// 不等待子进程；外壳进程退出后后端继续运行。
	_ = cmd.Process.Release()
	return logPath, pid, nil
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

// startupHTML 启动进度页。
func startupHTML(step string) string {
	return "<html><head><meta charset=\"utf-8\"><style>" +
		"body{font-family:'Segoe UI',sans-serif;background:#0d1929;color:#e2e8f0;padding:32px;line-height:1.6}" +
		"h1{font-size:18px;color:#5eead4;margin:0 0 12px}pre{white-space:pre-wrap;background:#122434;padding:16px;border-radius:10px;font-size:12px}" +
		".spin{display:inline-block;width:14px;height:14px;border:2px solid #5eead4;border-top-color:transparent;border-radius:50%;" +
		"animation:sp 1s linear infinite;vertical-align:-2px;margin-right:8px}@keyframes sp{to{transform:rotate(360deg)}}" +
		"</style></head><body><h1>BlockMemoryAgent</h1><pre><span class=\"spin\"></span>" + htmlEscape(step) +
		"</pre></body></html>"
}

// errorHTML 错误页：原因 + 建议 + 日志片段 + 重试按钮。
func errorHTML(message string) string {
	return "<html><head><meta charset=\"utf-8\"><style>" +
		"body{font-family:'Segoe UI',sans-serif;background:#0d1929;color:#e2e8f0;padding:32px;line-height:1.6}" +
		"h1{font-size:18px;color:#f87171;margin:0 0 12px}pre{white-space:pre-wrap;background:#122434;padding:16px;border-radius:10px;font-size:12px}" +
		"button{margin-top:16px;padding:8px 18px;border:0;border-radius:8px;background:#5eead4;color:#0d1929;font-size:13px;cursor:pointer}" +
		"</style></head><body><h1>BlockMemoryAgent 启动失败</h1><pre>" + htmlEscape(message) +
		"</pre><button onclick=\"bmaRetry()\">重试启动</button></body></html>"
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

// showConfirm 是/否消息框（返回用户是否点了「是」）。
func showConfirm(title, message string) bool {
	text, _ := windows.UTF16PtrFromString(message)
	caption, _ := windows.UTF16PtrFromString(title)
	// 0x04 = MB_YESNO, 0x30 = MB_ICONQUESTION
	ret, _, _ := user32.NewProc("MessageBoxW").Call(0,
		uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(caption)), 0x04|0x30)
	return ret == 6 // IDYES
}
