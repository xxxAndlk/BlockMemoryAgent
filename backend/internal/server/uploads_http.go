package server

// uploads_http.go 对话输入文件附件（Web 粘贴/上传 SQL、Excel 等）的服务端落盘与
// 双模式请求解析。
//
// 双模式：POST /api/sessions/:id/message 与 POST /api/sessions 在**带文件**时以
// multipart/form-data 提交（content/goal 等文本字段 + images 为 JSON 字符串字段 +
// files 为文件 part），不带文件时保持既有 JSON 请求体（TUI、测试与旧客户端不受影响）。
// 文件落盘到会话生效工作目录的 .bma/uploads/（.bma 是 BMA 应用数据既有约定，不污染
// 用户项目目录、不与工作区内同名目录冲突），消息里只带引用文本与结构化列表
//（见 agent.WireAttachment / agent.AppendAttachmentRefs）。

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/blockmemory/agent/backend/internal/agent"
)

// 上传上限：单文件 64MiB / 最多 8 个 / 整请求 256MiB（常量与 agent 包同源，
// 供前端与直连调用方对齐；30s ReadTimeout 下 256MiB ≈ 8.5MB/s，本机/局域网可行）。
const (
	maxUploadFiles        = agent.MaxMessageAttachments
	maxUploadFileBytes    = agent.MaxMessageAttachmentBytes
	maxUploadRequestBytes = agent.MaxMessageAttachmentsTotal
	// uploadsDirName 附件落盘子目录（工作区相对路径，正斜杠；wire 引用与磁盘拼接共用）。
	uploadsDirName = ".bma/uploads"
	// multipartMemoryBytes multipart 解析的内存驻留上限：以内驻内存，超出溢写临时
	// 文件（net/http 请求结束后自动清理；defer RemoveAll 兜底 httptest 直调场景）。
	multipartMemoryBytes = 32 << 20
)

// defaultWorkDirProvider 是"进程默认工作目录"能力的鸭子类型接口（同
// sessionWorkDirProvider 模式）。由 *agent.ReactService 实现。创建会话携带附件时
// 会话尚未存在，落盘目标目录经此解析（req.WorkDir 为空时回落进程默认）。
type defaultWorkDirProvider interface {
	DefaultWorkDir() string
}

// isMultipartRequest 判定请求体是否为 multipart/form-data（双模式分支依据）。
// 必须在 DecodeBody 之前判定——二进制 part 过不了 DecodeBody 的 UTF-8 校验。
func isMultipartRequest(c *gin.Context) bool {
	return strings.HasPrefix(c.GetHeader("Content-Type"), "multipart/form-data")
}

// parseMultipartForm 解析 multipart 请求体并返回表单。失败时已写响应（413/400），
// ok=false。整请求体积用 http.MaxBytesReader 兜底（超限 ParseMultipartForm 报错）。
// 调用方拿到 form 后应 defer form.RemoveAll()（生产链路 net/http 会自动清理，
// 此处为 httptest 直调 ServeHTTP 的场景兜底）。
func parseMultipartForm(c *gin.Context) (*multipart.Form, bool) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxUploadRequestBytes)
	if err := c.Request.ParseMultipartForm(multipartMemoryBytes); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			c.String(http.StatusRequestEntityTooLarge, "上传内容超过大小上限（%dMB）", maxUploadRequestBytes>>20)
		} else {
			c.String(http.StatusBadRequest, "multipart 请求体无效")
		}
		return nil, false
	}
	return c.Request.MultipartForm, true
}

// formValue 取 multipart 表单字段首个值（缺失返回空串）。
func formValue(form *multipart.Form, key string) string {
	if form == nil {
		return ""
	}
	if vs := form.Value[key]; len(vs) > 0 {
		return vs[0]
	}
	return ""
}

// parseMultipartImages 解析 multipart 表单里的 images 字段（WireImage 数组的
// JSON 字符串，可空；图片本体仍走既有 base64 链路）。
func parseMultipartImages(raw string) ([]agent.WireImage, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var imgs []agent.WireImage
	if err := json.Unmarshal([]byte(raw), &imgs); err != nil {
		return nil, fmt.Errorf("images 字段不是合法 JSON 数组")
	}
	return imgs, nil
}

// parseMultipartStrings 解析 multipart 表单里的字符串数组字段（JSON 字符串，可空）。
func parseMultipartStrings(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("字段不是合法 JSON 字符串数组")
	}
	return out, nil
}

// sanitizeUploadName 把浏览器提交的文件名消毒为可安全落盘的名字：
// 取 basename（剥任何目录成分，防路径穿越）；剔除控制字符与 Windows 非法字符
// \/:*?"<>|（保留中文等 Unicode）；去首尾空白与点；非 UTF-8 或空名回落 "file"；
// 字节长度封顶 120（按 rune 截断，超长文件名打不进文件系统）。
func sanitizeUploadName(name string) string {
	if i := strings.LastIndexAny(name, "/\\"); i >= 0 {
		name = name[i+1:]
	}
	if name == "" || !utf8.ValidString(name) {
		return "file"
	}
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`/\:*?"<>|`, r) {
			continue
		}
		b.WriteRune(r)
	}
	out := strings.Trim(b.String(), " .")
	if out == "" {
		return "file"
	}
	if len(out) > 120 {
		runes := []rune(out)
		for len(runes) > 0 && len(string(runes)) > 120 {
			runes = runes[:len(runes)-1]
		}
		out = strings.TrimRight(string(runes), " .")
		if out == "" {
			return "file"
		}
	}
	return out
}

// createUploadFile 在 dir 下创建目标文件（O_CREATE|O_EXCL 防并发覆盖），
// 重名自动加 -1/-2… 后缀（最多重试 100 次）；返回已打开文件与最终文件名。
func createUploadFile(dir, name string) (*os.File, string, error) {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 0; i <= 100; i++ {
		candidate := name
		if i > 0 {
			candidate = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		f, err := os.OpenFile(filepath.Join(dir, candidate), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			return f, candidate, nil
		}
		if !os.IsExist(err) {
			return nil, "", err
		}
	}
	return nil, "", fmt.Errorf("同名文件过多（%s）", name)
}

// saveUploadedFiles 把 multipart 文件落盘到 <workDir>/.bma/uploads/ 并返回结构化
// 落盘结果：逐个流式拷贝（LimitReader 计数，单文件/总量超限即报错并清理半截文件）；
// 返回的 error 由调用方直接作为 400 文案。已成功落盘的先前文件不回滚（与"产物不
// 迁移"同容忍度——重发同名文件自然得到 -1 后缀副本，不静默覆盖原文件）。
func saveUploadedFiles(workDir string, fhs []*multipart.FileHeader) ([]agent.WireAttachment, error) {
	if len(fhs) > maxUploadFiles {
		return nil, fmt.Errorf("单条消息最多携带 %d 个文件", maxUploadFiles)
	}
	dir := filepath.Join(workDir, filepath.FromSlash(uploadsDirName))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("上传目录创建失败: %v", err)
	}
	atts := make([]agent.WireAttachment, 0, len(fhs))
	var total int64
	for _, fh := range fhs {
		if fh.Size > maxUploadFileBytes {
			return nil, fmt.Errorf("文件 %s 超过大小上限（%dMB）", fh.Filename, maxUploadFileBytes>>20)
		}
		name := sanitizeUploadName(fh.Filename)
		src, err := fh.Open()
		if err != nil {
			return nil, fmt.Errorf("文件 %s 读取失败: %v", fh.Filename, err)
		}
		dst, finalName, err := createUploadFile(dir, name)
		if err != nil {
			src.Close()
			return nil, fmt.Errorf("文件 %s 保存失败: %v", fh.Filename, err)
		}
		n, cpErr := io.Copy(dst, io.LimitReader(src, maxUploadFileBytes+1))
		src.Close()
		closeErr := dst.Close()
		if cpErr == nil && closeErr != nil {
			cpErr = closeErr
		}
		if cpErr == nil && n > maxUploadFileBytes {
			cpErr = fmt.Errorf("超过大小上限（%dMB）", maxUploadFileBytes>>20)
		}
		if cpErr == nil && total+n > maxUploadRequestBytes {
			cpErr = fmt.Errorf("附件总量超过上限（%dMB）", maxUploadRequestBytes>>20)
		}
		if cpErr != nil {
			os.Remove(dst.Name()) // 半截/超限文件不保留
			return nil, fmt.Errorf("文件 %s 保存失败: %v", fh.Filename, cpErr)
		}
		total += n
		atts = append(atts, agent.WireAttachment{Name: name, Path: path.Join(uploadsDirName, finalName), Size: n})
	}
	return atts, nil
}
