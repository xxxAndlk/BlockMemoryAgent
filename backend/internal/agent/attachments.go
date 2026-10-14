package agent

// attachments.go 用户消息文件附件（Web 粘贴/上传 SQL、Excel 等）的线型 DTO 与消息拼装工具。
//
// 与图片（base64 内联）不同：文件由 server 层落盘到会话工作区，消息里只带工作区
// 相对路径文本（AppendAttachmentRefs 拼进 content——content 是唯一持久载体，重启后
// Agent 仍能从历史里看到文件；图片的 [image:N] 占位同款思路，见 types.go Message.Images）。
// 结构化列表（名/路径/大小）另经 MarshalAttachmentsJSON 挂到 user_message 事件的
// detail_json（前端渲染文件卡片，零迁移）；detail 缺失时前端退化为纯文本引用行。

import (
	"encoding/json"
	"fmt"
	"strings"
)

// 用户消息文件附件上限（与 server 层 multipart 落盘校验同源）：
// 单文件 64MiB / 单条消息最多 8 个 / 整请求 256MiB。
const (
	// MaxMessageAttachments 单条用户消息最多携带的文件个数。
	MaxMessageAttachments = 8
	// MaxMessageAttachmentBytes 单个文件最大字节数（64MiB）。
	MaxMessageAttachmentBytes = 64 << 20
	// MaxMessageAttachmentsTotal 单条消息全部附件总量上限（256MiB）。
	MaxMessageAttachmentsTotal = 256 << 20
)

// WireAttachment 是用户消息文件附件的线型 DTO：文件已由 server 层落盘到会话工作区，
// 这里只携带结构化的落盘结果（不携带内容本体）。
type WireAttachment struct {
	// Name 原始文件名（已消毒，消毒规则见 server 层 sanitizeUploadName）。
	Name string `json:"name"`
	// Path 工作区相对路径（正斜杠，如 .bma/uploads/data.sql）。
	Path string `json:"path"`
	// Size 落盘后字节数。
	Size int64 `json:"size"`
}

// AppendAttachmentRefs 把附件引用行拼到用户消息文本尾部：每个附件一行
// "[附件 N: 名 | 工作区相对路径 | 大小]"，Agent 从消息文本即可获得工作区路径
// （ReadFile 等按工作目录解析）。content 为空时只返回引用块；无附件原样返回。
func AppendAttachmentRefs(content string, atts []WireAttachment) string {
	if len(atts) == 0 {
		return content
	}
	lines := make([]string, 0, len(atts))
	for i, a := range atts {
		lines = append(lines, fmt.Sprintf("[附件 %d: %s | %s | %s]", i+1, a.Name, a.Path, formatAttachmentSize(a.Size)))
	}
	block := strings.Join(lines, "\n")
	if strings.TrimSpace(content) == "" {
		return block
	}
	return strings.TrimRight(content, "\n") + "\n\n" + block
}

// formatAttachmentSize 人类可读大小：<1KiB 字节、<1MiB 一位小数 KB、其余一位小数 MB。
func formatAttachmentSize(size int64) string {
	switch {
	case size < 1<<10:
		return fmt.Sprintf("%d B", size)
	case size < 1<<20:
		return fmt.Sprintf("%.1f KB", float64(size)/(1<<10))
	default:
		return fmt.Sprintf("%.1f MB", float64(size)/(1<<20))
	}
}

// MarshalAttachmentsJSON 把附件列表编成 user_message 事件 detail_json 的载荷
// {"attachments":[...]}；空列表返回 ""（调用方走无 detail 的 addEvent 路径）。
func MarshalAttachmentsJSON(atts []WireAttachment) string {
	if len(atts) == 0 {
		return ""
	}
	b, err := json.Marshal(struct {
		Attachments []WireAttachment `json:"attachments"`
	}{Attachments: atts})
	if err != nil {
		return "" // 理论不可达（结构体全部可编码）
	}
	return string(b)
}
