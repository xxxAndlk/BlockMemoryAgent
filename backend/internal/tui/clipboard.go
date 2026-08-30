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
	"path/filepath"
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

// pasteImageCmd 已并入 pasteClipboardCmd（视频优先 + 图片回落），原独立入口随
// 视频通道上线移除；图片读取逻辑经 readClipboardImageFn 在回落分支复用。

// clipVideoMsg 是剪贴板文件列表中识别出的视频附件（tea.Cmd 产出，Update 消费）。
type clipVideoMsg struct {
	paths []string // 视频文件的宿主机绝对路径
}

// readClipboardFileDropListFn 是 readClipboardFileDropList 的可替换注入点（测试 stub）。
var readClipboardFileDropListFn = readClipboardFileDropList

// readClipboardFileDropList 读取剪贴板中的文件路径列表（Windows 资源管理器
// 复制/剪切文件产生 FileDropList）。无文件列表返回 (nil, nil)，调用方回落
// 图片粘贴逻辑；非 Windows 平台返回 ErrClipboardUnsupported。
func readClipboardFileDropList(ctx context.Context) ([]string, error) {
	if runtime.GOOS != "windows" {
		return nil, ErrClipboardUnsupported
	}
	ctx, cancel := context.WithTimeout(ctx, clipboardTimeout)
	defer cancel()

	// PS 5.1：FileDropList 返回 StringCollection，无文件时为 $null；
	// 输出 EMPTY 哨兵与图片读取路径同模式。NTFS 文件名不含换行，按行切安全。
	script := `$fl = Get-Clipboard -Format FileDropList; if ($null -eq $fl -or $fl.Count -eq 0) { 'EMPTY' } else { $fl -join "` + "`n" + `" }`
	out, err := exec.CommandContext(ctx, "powershell", "-NoLogo", "-NoProfile", "-Command", script).Output()
	if err != nil {
		return nil, fmt.Errorf("读取剪贴板失败: %w", err)
	}
	return parseFileDropOutput(string(out)), nil
}

// parseFileDropOutput 解析 powershell FileDropList 输出："EMPTY" 哨兵 → nil；
// 其余按行切（TrimSpace 去行尾 \r），跳过空行。
func parseFileDropOutput(out string) []string {
	got := strings.TrimSpace(out)
	if got == "" || got == "EMPTY" {
		return nil
	}
	var paths []string
	for _, line := range strings.Split(got, "\n") {
		if p := strings.TrimSpace(line); p != "" {
			paths = append(paths, p)
		}
	}
	return paths
}

// filterVideoPaths 从文件路径列表中筛出视频文件（按扩展名白名单，
// 与服务端 ParseWireVideos 同源）。全非视频返回 nil。
func filterVideoPaths(paths []string) []string {
	var vids []string
	for _, p := range paths {
		if _, ok := agent.VideoMIMEByExt(filepath.Ext(p)); ok {
			vids = append(vids, p)
		}
	}
	return vids
}

// pasteClipboardCmd 返回异步读取剪贴板的 tea.Cmd：优先识别视频文件
// （FileDropList 命中视频扩展名 → clipVideoMsg），否则回落图片读取
//（clipImageMsg，原 Alt+V 逻辑不变）。
func (m Model) pasteClipboardCmd() tea.Cmd {
	return func() tea.Msg {
		paths, err := readClipboardFileDropListFn(context.Background())
		if vids := filterVideoPaths(paths); len(vids) > 0 {
			return clipVideoMsg{paths: vids}
		}
		_ = err // 无文件列表（或全非视频）时静默回落图片逻辑，由图片侧统一报错
		png, imgErr := readClipboardImageFn(context.Background())
		return clipImageMsg{png: png, err: imgErr}
	}
}

// applyClipVideo 消费剪贴板视频识别结果：限流校验通过后暂存路径引用并在
// 光标处插入 [video:N] 占位符（N=粘贴顺序号，与 pendingVideos 顺序对齐）。
func (m *Model) applyClipVideo(msg clipVideoMsg) {
	for _, p := range msg.paths {
		if len(m.inputBar.pendingVideos) >= agent.MaxMessageVideos {
			m.flashMsg(fmt.Sprintf("单条消息最多 %d 个视频，多余视频未粘贴", agent.MaxMessageVideos))
			return
		}
		mime, _ := agent.VideoMIMEByExt(filepath.Ext(p))
		m.inputBar.pendingVideos = append(m.inputBar.pendingVideos, agent.WireVideo{Path: p, MIMEType: mime})
		n := len(m.inputBar.pendingVideos)
		m.inputBar.insertRunes([]rune(fmt.Sprintf("[video:%d]", n)))
		m.flashMsg(fmt.Sprintf("已添加视频 [video:%d] %s（随下条消息发送，服务端抽帧）", n, filepath.Base(p)))
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
