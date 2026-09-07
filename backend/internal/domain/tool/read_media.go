// read_media.go 提供 ReadMedia 工具：按文件路径读取本地图片/视频并附加到对话，
// 供 domain Agent 验证 UI 时自查截图/设计稿、MetaAgent 返工直达时看截图核对。
// 图片原图直读（png/jpg/jpeg/gif/webp，单张 ≤4MiB）；视频复用 agent 层注入的
// 解析器（ffmpeg 抽帧，见 bootstrap SetMediaResolver 闭包），单文件 ≤200MB。
// 依赖方向约束：tool 层不能 import agent 层，视频解析经 MediaResolveFunc 注入回接。
// 降级不报错原则：超限/格式不支持/解析器未接线/ffmpeg 缺失均返回文本说明，不抛错中断循环。
package tool

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

const (
	// readMediaImageBytes 单张图片上限，镜像 agent.MaxMessageImageBytes（上传链路同值）。
	readMediaImageBytes = 4 << 20
	// readMediaVideoBytes 单视频上限，镜像 agent.DefaultMaxVideoBytes。
	readMediaVideoBytes = 200 << 20
)

// readMediaImageMIMEs 是图片扩展名到 MIME 的映射（与 provider 白名单对齐：
// anthropic/openai 均接受 png/jpeg/gif/webp）。
var readMediaImageMIMEs = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
}

// readMediaVideoExts 是视频扩展名白名单（镜像 agent/types.go 上传链路白名单）。
var readMediaVideoExts = map[string]bool{
	".mp4": true, ".webm": true, ".mov": true, ".mkv": true, ".avi": true,
}

// MediaResolveFunc 是视频文件解析器的注入签名：入参为沙箱校验后的绝对路径，
// 返回抽帧/元数据图片（ResultImage.Data 为 base64 ASCII）与文本注记（帧数/编解码器等）。
// bootstrap 以 agent.ResolveVideos 闭包实现（强制抽帧模式）。
type MediaResolveFunc func(ctx context.Context, path string) (images []ResultImage, notes []string)

// readMediaTool 是 ReadMedia 工具的封装。
type readMediaTool struct {
	exec     *Executor
	resolver MediaResolveFunc
}

// Name 返回工具标准名称 ReadMedia。
func (t *readMediaTool) Name() string { return "ReadMedia" }

// Aliases 返回 ReadMedia 的别名列表。
func (t *readMediaTool) Aliases() []string { return []string{"read_media"} }

// Description 返回工具的人类可读描述。
func (t *readMediaTool) Description() string { return readMediaToolDescription() }

// readMediaToolDescription 是 ReadMedia 的描述文本。
func readMediaToolDescription() string {
	return "按文件路径读取本地图片或视频并附加到对话，用于查看截图/设计稿/录屏核对 UI 与视觉产物。" +
		"图片支持 png/jpg/jpeg/gif/webp（单张 ≤4MiB）原图直读；" +
		"视频支持 mp4/webm/mov/mkv/avi（≤200MB），自动抽取关键帧（需要 ffmpeg，缺失时降级为元数据说明）。" +
		"路径相对当前工作目录解析，禁止越出沙箱；每次调用一个文件，多文件多次调用。"
}

// InputSchema 返回 LLM 可见的参数 schema。
func (t *readMediaTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"path": {
				Type:        "string",
				Description: "图片/视频文件路径（相对工作目录或绝对路径）",
			},
		},
		Required: []string{"path"},
	}
}

// Execute 按路径读取媒体文件：图片读盘转 base64 附为 ResultImage；
// 视频交注入解析器抽帧。失败路径一律返回文本说明（validation_rejected / execution_failed）。
func (t *readMediaTool) Execute(ctx context.Context, args map[string]any) *Result {
	res := &Result{Tool: "ReadMedia"}
	path, _ := args["path"].(string)
	path = strings.TrimSpace(path)
	if path == "" {
		res.Error = "缺少 path 参数"
		res.Category = ResultCategoryValidationRejected
		return res
	}
	if t.exec == nil {
		res.Error = "executor 未接线"
		return res
	}
	absPath, err := t.exec.resolvePathWithSandbox(ctx, path)
	if err != nil {
		res.Error = err.Error()
		res.Category = ResultCategoryValidationRejected
		return res
	}
	res.Path = absPath

	ext := strings.ToLower(filepath.Ext(absPath))
	if mime, ok := readMediaImageMIMEs[ext]; ok {
		return t.readImage(res, absPath, mime)
	}
	if readMediaVideoExts[ext] {
		return t.readVideo(ctx, res, absPath)
	}
	res.Success = false
	res.Error = fmt.Sprintf("不支持的媒体格式 %q（支持图片 %v；视频 mp4/webm/mov/mkv/avi）",
		ext, supportedImageExts())
	res.Category = ResultCategoryValidationRejected
	return res
}

// readImage 读取图片文件并附加为单张 ResultImage。
func (t *readMediaTool) readImage(res *Result, absPath, mime string) *Result {
	info, err := os.Stat(absPath)
	if err != nil {
		res.Error = fmt.Sprintf("文件不可访问: %v", err)
		res.Category = ResultCategoryExecutionFailed
		return res
	}
	if info.Size() > readMediaImageBytes {
		res.Error = fmt.Sprintf("图片 %s 大小 %d 字节超过上限 %d（可先压缩或截图局部）",
			filepath.Base(absPath), info.Size(), readMediaImageBytes)
		res.Category = ResultCategoryValidationRejected
		return res
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		res.Error = fmt.Sprintf("读取失败: %v", err)
		res.Category = ResultCategoryExecutionFailed
		return res
	}
	res.Success = true
	res.Images = []ResultImage{{MIMEType: mime, Data: []byte(base64.StdEncoding.EncodeToString(data))}}
	res.Output = fmt.Sprintf("已读取图片 %s（%s，%d 字节），图像已附加于本工具结果。", absPath, mime, info.Size())
	return res
}

// readVideo 交注入解析器抽帧；解析器未接线或返回空时降级为文本说明。
func (t *readMediaTool) readVideo(ctx context.Context, res *Result, absPath string) *Result {
	info, err := os.Stat(absPath)
	if err != nil {
		res.Error = fmt.Sprintf("文件不可访问: %v", err)
		res.Category = ResultCategoryExecutionFailed
		return res
	}
	if info.Size() > readMediaVideoBytes {
		res.Error = fmt.Sprintf("视频 %s 大小 %d 字节超过上限 %d", filepath.Base(absPath), info.Size(), readMediaVideoBytes)
		res.Category = ResultCategoryValidationRejected
		return res
	}
	if t.resolver == nil {
		res.Success = false
		res.Output = fmt.Sprintf("视频解析器未接线（%s，%d 字节）：无法抽帧，请改用文字描述或让用户直接查看该文件。", absPath, info.Size())
		return res
	}
	images, notes := t.resolver(ctx, absPath)
	if len(images) == 0 {
		res.Success = false
		res.Output = strings.Join(append([]string{fmt.Sprintf("视频 %s 抽帧无产出：", absPath)}, notes...), "\n")
		return res
	}
	res.Success = true
	res.Images = images
	res.Output = fmt.Sprintf("已读取视频 %s（%d 字节），附加 %d 帧关键帧。\n%s",
		absPath, info.Size(), len(images), strings.Join(notes, "\n"))
	return res
}

// supportedImageExts 返回图片扩展名清单（错误提示用）。
func supportedImageExts() []string {
	exts := make([]string, 0, len(readMediaImageMIMEs))
	for ext := range readMediaImageMIMEs {
		exts = append(exts, strings.TrimPrefix(ext, "."))
	}
	return exts
}
