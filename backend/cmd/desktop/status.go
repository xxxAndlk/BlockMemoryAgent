//go:build windows

// 窗口状态呈现、启动日志摘取与后台媒体栈准备。
package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	webview "github.com/jchv/go-webview2"
)

// setProgress 在窗口里显示一条启动进度（没有窗口时忽略）。
func setProgress(w webview.WebView, step string) {
	if w == nil {
		return
	}
	w.Dispatch(func() { w.SetHtml(startupHTML(step)) })
}

// navigate 把窗口切到应用地址。
func navigate(w webview.WebView, base string) {
	if w == nil {
		return
	}
	w.Dispatch(func() { w.Navigate(base) })
}

// tailFile 读文件末尾 n 行——后端严格启动失败时，把真实报错摆到用户眼前，
// 而不是让人自己去翻日志目录。
func tailFile(path string, n int) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// runMediaSetup 后台准备媒体栈（open_design daemon + minimax-media 桥 + 插件镜像）。
//
// 媒体镜像体积大、构建慢（Playwright 镜像数百 MB），不能挡在启动关键路径上；
// 失败只落日志——插件侧按既有降级语义处理（插件不可用，不阻断主流程）。
func runMediaSetup(root, base string) {
	logDir := filepath.Join(root, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return
	}
	logPath := filepath.Join(logDir, "media_setup_"+time.Now().Format("20060102_150405")+".log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	summary := ensureMedia(root, f)
	fmt.Fprintln(f, "[media] "+summary)
	// 媒体栈真的动过才通知后端重载插件：共享 keeper 是按镜像创建并常驻的，
	// 镜像缺席时插件会以"不可用"降级，重载才能用上新镜像；而重载会掐断在途
	// MCP 会话，所以"无需变更"时不动它。
	if summary == "媒体栈已更新" {
		reloadPlugins(root, base)
	}
}

// reloadPlugins 通知后端重载插件（POST /api/plugins/reload）。
func reloadPlugins(root, base string) {
	req, err := http.NewRequest(http.MethodPost, base+"/api/plugins/reload", nil)
	if err != nil {
		return
	}
	if token := readEnvValue(filepath.Join(root, ".env"), "BMA_API_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}
