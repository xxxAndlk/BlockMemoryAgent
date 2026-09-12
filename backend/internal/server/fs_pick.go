package server

// fs_pick.go 系统原生目录选择：在**服务端主机**上弹出操作系统自带的文件夹选择框，
// 把用户选中的绝对路径回给前端（工作目录选择器的"用电脑工具选"入口）。
//
// 为什么必须在服务端弹：浏览器侧的 File System Access API（showDirectoryPicker）只返回
// 沙箱句柄，拿不到可用于服务端进程的绝对路径；而工作目录是服务端概念。本部署形态是
// "服务与浏览器同机"（localhost:10010），因此服务端弹窗就是用户眼前的原生选择器。
// 前置条件：服务进程跑在**交互式桌面会话**里。若被装成 Windows 服务（Session 0），
// 对话框不可见——此时请求会等到超时并回 504，前端自动退回网页版选择器。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	// pickDirTimeout 是等待用户在原生对话框里操作的上限：超时即杀进程并回 504
	// （用户可能把窗口丢在后台忘了，不能永久占着这个"单例"对话框）。
	pickDirTimeout = 5 * time.Minute
)

// pickDirBusy 保证同一时刻只有一个原生对话框：第二个请求直接 409 而不是又弹一个
// （多个窗口叠在一起用户根本分不清哪个是哪次请求的）。
var pickDirBusy sync.Mutex

// PickDirHandler 处理 POST /api/fs/pick-dir：弹系统目录选择框，返回 {path} 或取消时的空串。
func (h *APIHandler) PickDirHandler(c *gin.Context) {
	if !pickDirBusy.TryLock() {
		c.String(http.StatusConflict, "已有目录选择窗口打开，请先完成或关闭它")
		return
	}
	defer pickDirBusy.Unlock()

	ctx, cancel := context.WithTimeout(c.Request.Context(), pickDirTimeout)
	defer cancel()

	path, err := pickSystemDir(ctx)
	switch {
	case errors.Is(err, errPickCancelled):
		// 用户取消：不是错误，回空路径让前端静默处理。
		c.JSON(http.StatusOK, gin.H{"path": ""})
		return
	case errors.Is(err, errPickUnsupported):
		c.String(http.StatusNotImplemented, "当前平台不支持系统目录选择器（可在网页版选择器里输入路径）")
		return
	case errors.Is(err, context.DeadlineExceeded):
		c.String(http.StatusGatewayTimeout, "等待选择超时（对话框可能不在前台）")
		return
	case err != nil:
		c.String(http.StatusInternalServerError, "系统目录选择器调用失败: %v", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"path": path})
}

// errPickCancelled 用户取消选择；errPickUnsupported 当前平台无可用选择器。
var (
	errPickCancelled   = errors.New("pick dir cancelled")
	errPickUnsupported = errors.New("pick dir unsupported")
)

// pickSystemDir 按平台调用系统目录选择器，返回绝对路径。
func pickSystemDir(ctx context.Context) (string, error) {
	switch runtime.GOOS {
	case "windows":
		return pickDirWindows(ctx)
	case "darwin":
		return runPickCommand(ctx, "osascript", "-e", `POSIX path of (choose folder with prompt "选择工作目录")`)
	default:
		// Linux 桌面：优先 zenity（GNOME 系默认装），退回 kdialog（KDE）。
		if _, err := exec.LookPath("zenity"); err == nil {
			return runPickCommand(ctx, "zenity", "--file-selection", "--directory", "--title=选择工作目录")
		}
		if _, err := exec.LookPath("kdialog"); err == nil {
			return runPickCommand(ctx, "kdialog", "--getexistingdirectory", os.Getenv("HOME"), "--title", "选择工作目录")
		}
		return "", errPickUnsupported
	}
}

// pickDirWindows 用 Windows PowerShell 5.1 的 WinForms FolderBrowserDialog。
//
// 细节：
//   - `-STA` 必需：WinForms 对话框要求单线程单元，缺了会直接抛异常；
//   - 结果经**临时文件 UTF-8** 回传而不是 stdout：控制台代码页会把中文路径写成乱码
//     （本项目的目录名大量含中文）；
//   - 用户取消时 SelectedPath 为空 → 归一为 errPickCancelled。
func pickDirWindows(ctx context.Context) (string, error) {
	tmp, err := os.CreateTemp("", "bma-pickdir-*.txt")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	// 单引号包裹路径，脚本内不做变量展开（临时路径由 Go 生成，无注入面）。
	script := fmt.Sprintf(`
Add-Type -AssemblyName System.Windows.Forms | Out-Null
$dlg = New-Object System.Windows.Forms.FolderBrowserDialog
$dlg.Description = '选择工作目录'
$dlg.ShowNewFolderButton = $true
if ($dlg.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) {
  [System.IO.File]::WriteAllText('%s', $dlg.SelectedPath, [System.Text.Encoding]::UTF8)
}`, strings.ReplaceAll(tmpPath, "'", "''"))

	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-STA", "-Command", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}

	data, err := os.ReadFile(tmpPath)
	if err != nil {
		return "", err
	}
	// 去 UTF-8 BOM 与首尾空白；空内容 = 用户取消。
	got := strings.TrimSpace(strings.TrimPrefix(string(data), "\ufeff"))
	if got == "" {
		return "", errPickCancelled
	}
	return filepath.Clean(got), nil
}

// runPickCommand 跑一个外部选择器命令，stdout 首行即所选路径（取消/未选返回错误）。
func runPickCommand(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// zenity/kdialog 取消时以非零码退出。
		return "", errPickCancelled
	}
	got := strings.TrimSpace(string(out))
	if got == "" {
		return "", errPickCancelled
	}
	return filepath.Clean(got), nil
}
