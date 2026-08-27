package tui

// clipboard.go 实现剪贴板图片读取（Alt+V 粘贴）。
//
// Windows：经 powershell Get-Clipboard -Format Image 取位图存临时 PNG 再读回
//（与 domain/tool.RunCommand 的 powershell 调用模式一致）；非 Windows 平台
// 返回 ErrClipboardUnsupported（结构留位，按需补 pbpaste/osascript/xclip）。

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
)

// ErrClipboardUnsupported 表示当前平台不支持剪贴板图片读取。
var ErrClipboardUnsupported = errors.New("当前平台暂不支持剪贴板图片粘贴")

// clipboardTimeout 单次剪贴板读取超时（powershell 冷启动可能达数秒）。
const clipboardTimeout = 10 * time.Second

// clipImageMsg 是异步剪贴板读取的结果（tea.Cmd 产出，Update 消费）。
type clipImageMsg struct {
	png []byte // PNG 原始字节；失败时为 nil
	err error
}

// readClipboardImageFn 是 readClipboardImage 的可替换注入点（测试 stub）。
var readClipboardImageFn = readClipboardImage

// readClipboardImage 读取系统剪贴板位图，返回 PNG 字节。
func readClipboardImage(ctx context.Context) ([]byte, error) {
	if runtime.GOOS != "windows" {
		return nil, ErrClipboardUnsupported
	}
	ctx, cancel := context.WithTimeout(ctx, clipboardTimeout)
	defer cancel()

	// 临时 PNG 落盘路径由 Go 侧生成（随机名防并发碰撞）。
	tmp, err := os.CreateTemp("", "bma_clip_*.png")
	if err != nil {
		return nil, fmt.Errorf("创建临时文件失败: %w", err)
	}
	path := tmp.Name()
	tmp.Close()
	defer os.Remove(path)

	// powershell 脚本：剪贴板无图片输出 EMPTY，否则存 PNG 并回显路径。
	script := fmt.Sprintf(`$img = Get-Clipboard -Format Image; if ($null -eq $img) { 'EMPTY' } else { $img.Save('%s', [System.Drawing.Imaging.ImageFormat]::Png); '%s' }`, path, path)
	out, err := exec.CommandContext(ctx, "powershell", "-NoLogo", "-NoProfile", "-Command", script).Output()
	if err != nil {
		return nil, fmt.Errorf("读取剪贴板失败: %w", err)
	}
	got := strings.TrimSpace(string(out))
	if got != path {
		if strings.Contains(got, "EMPTY") {
			return nil, errors.New("剪贴板中没有图片")
		}
		return nil, fmt.Errorf("读取剪贴板失败: 未知输出 %q", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取临时 PNG 失败: %w", err)
	}
	if len(data) == 0 {
		return nil, errors.New("剪贴板图片为空")
	}
	return data, nil
}

// pasteImageCmd 返回异步读取剪贴板的 tea.Cmd（产出 clipImageMsg）。
func (m Model) pasteImageCmd() tea.Cmd {
	return func() tea.Msg {
		png, err := readClipboardImageFn(context.Background())
		return clipImageMsg{png: png, err: err}
	}
}

// applyClipImage 消费剪贴板读取结果：限流校验通过后暂存图片并在光标处
// 插入 [image:N] 占位符（N=粘贴顺序号，与图片顺序对齐）。
func (m *Model) applyClipImage(msg clipImageMsg) {
	if msg.err != nil {
		m.flashMsg(fmt.Sprintf("粘贴图片失败: %v", msg.err))
		return
	}
	if len(m.inputBar.pendingImages) >= agent.MaxMessageImages {
		m.flashMsg(fmt.Sprintf("单条消息最多 %d 张图片", agent.MaxMessageImages))
		return
	}
	if len(msg.png) > agent.MaxMessageImageBytes {
		m.flashMsg("图片超过大小上限（4MiB），未粘贴")
		return
	}
	n := len(m.inputBar.pendingImages) + 1
	// Data 链路内约定存 base64 ASCII（与 MCP ImageContent.Data / tool 图片分支同语义，
	// ToBladesMessages 按 base64 解码）：此处存 raw PNG 会被静默丢弃（解码失败跳过）。
	m.inputBar.pendingImages = append(m.inputBar.pendingImages, tool.ResultImage{
		MIMEType: "image/png",
		Data:     []byte(base64.StdEncoding.EncodeToString(msg.png)),
	})
	m.inputBar.insertRunes([]rune(fmt.Sprintf("[image:%d]", n)))
	m.flashMsg(fmt.Sprintf("已粘贴图片 [image:%d]（随下条消息发送）", n))
}
