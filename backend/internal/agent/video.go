package agent

// video.go 实现用户消息视频附件的服务端处理，两条消费路径：
//   - native（默认）：≤ NativeMaxBytes 的 mp4/avi/mov 整个文件以 video/* 媒体项
//     直传，openai-chat 兼容端点映射 video_url 供 Ark doubao-seed/GLM 视频理解
//     模型原生消费（已对照火山文档 82379/1895586），模型获得完整时间维度信息；
//   - frames（显式退出 / webm·mkv·超限·直传失败回落）：ffprobe 读元数据 + ffmpeg
//     等时间隔抽关键帧，帧作为图片并入 Message.Images 走现有图片链路
//    （anthropic/openai-chat 完整支持，openai-responses/ollama 自然降级纯文本）。
//
// 元数据文本并入 Content 持久化。
//
// 降级原则（对齐 mcp 插件 uvx 缺失先例）：ffmpeg/ffprobe 缺失、解码失败、
// 超时均不报错，视频降级为仅元数据文本 + slog.Warn，消息照常发送。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
)

// resolveVideosFn 是 ResolveVideos 的可替换注入点（service 层测试 stub）。
// 二进制名注入走 VideoOptions.FFmpegBin/FFprobeBin（测试指向假二进制/脚本即可）。
var resolveVideosFn = ResolveVideos

// VideoOptions 抽帧参数（bootstrap 从 config 注入；零值字段逐个回落默认）。
type VideoOptions struct {
	// FFmpegBin ffmpeg 可执行文件名/路径，默认 "ffmpeg"（PATH 探测）。
	FFmpegBin string
	// FFprobeBin ffprobe 可执行文件名/路径，默认 "ffprobe"。
	FFprobeBin string
	// MaxVideoBytes 单视频大小上限兜底（ParseWireVideos 已拦，此处防御）。
	MaxVideoBytes int64
	// FrameCount 每个视频抽取的关键帧数，默认 6。
	FrameCount int
	// FrameMaxPixels 帧长边像素上限，默认 1024（不放大小图）。
	FrameMaxPixels int
	// JPEGQuality ffmpeg -q:v 值，默认 5（约 JPEG q80）。
	JPEGQuality int
	// ExtractTimeoutSec 抽帧整体超时，默认 25s（须 < HTTP WriteTimeout 30s，
	// 否则 handler 先被写超时掐断）。
	ExtractTimeoutSec int
	// NativeMaxBytes 原生视频直传的大小上限（base64 进 data URI）：
	// >0 时，≤ 该上限且 MIME 在 nativeVideoMIMES 内（mp4/avi/mov）的视频跳过
	// 抽帧，以 video/* 媒体项随消息直传（openai-chat 兼容端点映射为
	// video_url——Ark doubao/GLM 视频理解格式）；webm/mkv/超限的仍走抽帧。
	// 0 = 关闭原生直传，全部抽帧（bootstrap 侧仅显式 mode: frames 时保持 0；
	// anthropic 等无视频 API 的 provider 组合须配 frames，否则视频内容会被其
	// 白名单静默丢弃）。
	NativeMaxBytes int64
}

// DefaultVideoOptions 返回带默认值的 VideoOptions。
func DefaultVideoOptions() VideoOptions {
	return VideoOptions{
		MaxVideoBytes:     DefaultMaxVideoBytes,
		FrameCount:        6,
		FrameMaxPixels:    1024,
		JPEGQuality:       5,
		ExtractTimeoutSec: 25,
	}
}

// withDefaults 对零值字段逐个回落默认（config 约定"0=未配置"）。
func (o VideoOptions) withDefaults() VideoOptions {
	d := DefaultVideoOptions()
	if o.FFmpegBin == "" {
		o.FFmpegBin = d.FFmpegBin
	}
	if o.FFprobeBin == "" {
		o.FFprobeBin = d.FFprobeBin
	}
	if o.MaxVideoBytes <= 0 {
		o.MaxVideoBytes = d.MaxVideoBytes
	}
	if o.FrameCount <= 0 {
		o.FrameCount = d.FrameCount
	}
	if o.FrameMaxPixels <= 0 {
		o.FrameMaxPixels = d.FrameMaxPixels
	}
	if o.JPEGQuality <= 0 {
		o.JPEGQuality = d.JPEGQuality
	}
	if o.ExtractTimeoutSec <= 0 {
		o.ExtractTimeoutSec = d.ExtractTimeoutSec
	}
	return o
}

// nativeVideoMIMES 原生直传允许的视频 MIME（火山方舟文档 82379/1895586
// "视频格式说明"：MP4/AVI/MOV 三种；webm/mkv 不在列表，端点会拒绝，
// 走抽帧路径——ffmpeg 可正常解码）。
var nativeVideoMIMES = map[string]bool{
	"video/mp4":       true,
	"video/x-msvideo": true,
	"video/quicktime": true,
}

// VideoMeta 是单个视频的探测元数据（未知字段为零值）。
type VideoMeta struct {
	Codec       string
	Width       int
	Height      int
	DurationSec float64
	SizeBytes   int64
}

// ffprobeStream / ffprobeFormat 对应 ffprobe -of json 的输出结构。
type ffprobeStream struct {
	CodecName string `json:"codec_name"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
}

type ffprobeFormat struct {
	Duration string `json:"duration"`
	Size     string `json:"size"`
}

type ffprobeOutput struct {
	Streams []ffprobeStream `json:"streams"`
	Format  ffprobeFormat   `json:"format"`
}

// ResolveVideos 把用户消息携带的视频转成帧图片 + 元数据文本段。
// frames 依次追加到 Message.Images，notes 逐视频文本块拼入 Content。
// imageBase 是该消息已有图片数（元数据文本据此给出帧与 image 编号的
// 权威映射：帧追加在已有图片之后）。
// 任一环节失败：该视频降级为仅元数据文本（尽力版），记 slog.Warn，不返回 error。
// 整体用 WithoutCancel 隔离（TUI postJSON 客户端 3s 超时断连不能取消 ffmpeg），
// 再套 ExtractTimeoutSec 硬超时。
func ResolveVideos(ctx context.Context, vids []WireVideo, opts VideoOptions, imageBase int) ([]tool.ResultImage, []string) {
	if len(vids) == 0 {
		return nil, nil
	}
	opts = opts.withDefaults()
	// TUI postJSON 客户端 3s 超时会断连取消 ctx；抽帧必须继续完成
	//（消息已送达服务端），故与请求生命周期解耦，仅受自身硬超时约束。
	ctx = context.WithoutCancel(ctx)
	ctx, cancel := context.WithTimeout(ctx, time.Duration(opts.ExtractTimeoutSec)*time.Second)
	defer cancel()

	var frames []tool.ResultImage
	notes := make([]string, 0, len(vids))
	for i, vid := range vids {
		meta, probeErr := probeVideo(ctx, opts.FFprobeBin, vid.Path)
		if meta.SizeBytes == 0 {
			if info, err := os.Stat(vid.Path); err == nil {
				meta.SizeBytes = info.Size()
			}
		}

		// 原生视频直传分支（Ark/GLM 等 OpenAI 兼容端点的 video_url 理解）：
		// ≤ NativeMaxBytes 的视频跳过抽帧，整个视频以 video/* 媒体项直传
		//（ToBladesMessages 与图片同机制：仅最后一条 user 消息挂 DataPart，
		// openai-chat 层按 MIME 映射 video_url）。模型由此获得时间维度信息
		//（动作顺序/过程），抽帧只给离散画面。
		// MIME 须在端点支持列表内（火山文档 82379/1895586 视频格式说明仅
		// mp4/avi/mov）——webm/mkv 等直传会被端点拒绝，回落抽帧（ffmpeg 可解）。
		if opts.NativeMaxBytes > 0 && meta.SizeBytes > 0 && meta.SizeBytes <= opts.NativeMaxBytes && nativeVideoMIMES[vid.MIMEType] {
			if data, err := os.ReadFile(vid.Path); err == nil {
				frames = append(frames, tool.ResultImage{
					MIMEType: vid.MIMEType,
					// Data 链路内约定存 base64 ASCII（与图片同语义）。
					Data: []byte(base64.StdEncoding.EncodeToString(data)),
				})
				notes = append(notes, nativeVideoNote(i, vid, meta, imageBase))
				imageBase++
				continue
			}
			slog.Warn("原生视频读取失败，回落抽帧", "path", vid.Path)
		}

		vframes, frameErr := extractFrames(ctx, opts, vid.Path, meta.DurationSec)
		if probeErr != nil {
			slog.Warn("视频元数据探测失败，降级为尽力版元数据", "path", vid.Path, "err", probeErr)
		}
		if frameErr != nil {
			slog.Warn("视频抽帧失败，降级为仅元数据", "path", vid.Path, "err", frameErr)
		}
		if len(vframes) > 0 {
			frames = append(frames, vframes...)
		}
		notes = append(notes, videoNote(i, vid, meta, len(vframes), imageBase, probeErr, frameErr))
		imageBase += len(vframes)
	}
	return frames, notes
}

// nativeVideoNote 生成原生直传视频的元数据文本块（拼入 Content 持久化）。
// 编号与 frames 下标对齐：一个直传视频占一个 image 编号（imageBase+1）。
func nativeVideoNote(idx int, vid WireVideo, meta VideoMeta, imageBase int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[视频 %d: %s | %s", idx+1, filepath.Base(vid.Path), formatDuration(meta.DurationSec))
	if meta.Width > 0 && meta.Height > 0 {
		fmt.Fprintf(&b, " | %dx%d", meta.Width, meta.Height)
	}
	if meta.SizeBytes > 0 {
		fmt.Fprintf(&b, " | %.1fMB", float64(meta.SizeBytes)/(1<<20))
	}
	if meta.Codec != "" {
		fmt.Fprintf(&b, " | %s", meta.Codec)
	}
	fmt.Fprintf(&b, " | 完整视频已直传（含全部时间维度信息，对应 image:%d）]", imageBase+1)
	return b.String()
}

// probeVideo 用 ffprobe 读取首个视频流的元数据。
func probeVideo(ctx context.Context, bin, path string) (VideoMeta, error) {
	var meta VideoMeta
	if _, err := exec.LookPath(bin); err != nil {
		return meta, fmt.Errorf("ffprobe 不可用: %w", err)
	}
	out, err := exec.CommandContext(ctx, bin,
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=codec_name,width,height:format=duration,size",
		"-of", "json", path,
	).Output()
	if err != nil {
		return meta, fmt.Errorf("ffprobe 执行失败: %w", err)
	}
	var probe ffprobeOutput
	if err := json.Unmarshal(out, &probe); err != nil {
		return meta, fmt.Errorf("ffprobe 输出解析失败: %w", err)
	}
	if len(probe.Streams) > 0 {
		s := probe.Streams[0]
		meta.Codec = s.CodecName
		meta.Width = s.Width
		meta.Height = s.Height
	}
	if probe.Format.Duration != "" {
		if d, err := strconv.ParseFloat(probe.Format.Duration, 64); err == nil {
			meta.DurationSec = d
		}
	}
	if probe.Format.Size != "" {
		if n, err := strconv.ParseInt(probe.Format.Size, 10, 64); err == nil {
			meta.SizeBytes = n
		}
	}
	return meta, nil
}

// extractFrames 等时间隔抽取 N 关键帧（-ss 放 -i 前 = 输入侧 seek，
// 长视频免全量解码）。返回的帧 Data 为 base64 ASCII（与图片链路约定一致，
// ToBladesMessages 按 base64 解码）。单帧失败不中断；全部失败才返回错误。
func extractFrames(ctx context.Context, opts VideoOptions, path string, durationSec float64) ([]tool.ResultImage, error) {
	if _, err := exec.LookPath(opts.FFmpegBin); err != nil {
		return nil, fmt.Errorf("ffmpeg 不可用: %w", err)
	}
	tmpDir, err := os.MkdirTemp("", "bma_vid_")
	if err != nil {
		return nil, fmt.Errorf("创建抽帧临时目录失败: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	var frames []tool.ResultImage

	n := opts.FrameCount
	scale := fmt.Sprintf("scale='min(%d,iw)':'min(%d,ih)':force_original_aspect_ratio=decrease",
		opts.FrameMaxPixels, opts.FrameMaxPixels)
	okCount := 0
	var lastErr error
	for i := 0; i < n; i++ {
		// t=0 首帧起均匀铺满时长（短于 N 秒的视频可能重复抽到相邻帧，可接受）。
		t := durationSec * float64(i) / float64(n)
		out := filepath.Join(tmpDir, fmt.Sprintf("frame_%02d.jpg", i))
		args := []string{
			"-hide_banner", "-loglevel", "error",
			"-ss", strconv.FormatFloat(t, 'f', 2, 64),
			"-i", path,
			"-frames:v", "1",
			"-vf", scale,
			"-q:v", strconv.Itoa(opts.JPEGQuality),
			"-f", "image2", "-y", out,
		}
		if err := exec.CommandContext(ctx, opts.FFmpegBin, args...).Run(); err != nil {
			lastErr = err
			continue
		}
		data, err := os.ReadFile(out)
		if err != nil {
			lastErr = err
			continue
		}
		okCount++
		frames = append(frames, tool.ResultImage{
			MIMEType: "image/jpeg",
			// Data 链路内约定存 base64 ASCII（与 TUI 粘贴 / MCP 图片同语义，
			// ToBladesMessages 按 base64 解码）。
			Data: []byte(base64.StdEncoding.EncodeToString(data)),
		})
	}
	if okCount == 0 {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("抽帧超时（%ds）", opts.ExtractTimeoutSec)
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("未产生任何帧")
		}
		return nil, fmt.Errorf("抽帧失败: %w", lastErr)
	}
	return frames, nil
}

// videoNote 生成单个视频的元数据文本块（拼入 Content 持久化）。
// 成功时给出帧与 image 编号的权威映射（TUI [image:N] 占位与 Images 下标对齐，
// imageBase 为该消息已有图片数，帧追加在其后）。
func videoNote(idx int, vid WireVideo, meta VideoMeta, frameCount, imageBase int, probeErr, frameErr error) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[视频 %d: %s | %s", idx+1, filepath.Base(vid.Path), formatDuration(meta.DurationSec))
	if meta.Width > 0 && meta.Height > 0 {
		fmt.Fprintf(&b, " | %dx%d", meta.Width, meta.Height)
	}
	if meta.SizeBytes > 0 {
		fmt.Fprintf(&b, " | %.1fMB", float64(meta.SizeBytes)/(1<<20))
	}
	if meta.Codec != "" {
		fmt.Fprintf(&b, " | %s", meta.Codec)
	}
	b.WriteString(" | ")
	switch {
	case frameCount > 0:
		start := imageBase + 1
		fmt.Fprintf(&b, "已抽取 %d 关键帧（对应 image:%d..image:%d）", frameCount, start, start+frameCount-1)
	case probeErr != nil:
		fmt.Fprintf(&b, "未能读取视频信息（原因: %v），仅提供文件名", probeErr)
	default:
		fmt.Fprintf(&b, "未能抽取帧（原因: %v），仅提供元数据", frameErr)
	}
	b.WriteString("]")
	return b.String()
}

// formatDuration 把秒格式化为 mm:ss（超 1 小时为 h:mm:ss）。
func formatDuration(sec float64) string {
	if sec <= 0 {
		return "时长未知"
	}
	total := int(sec + 0.5)
	h, rem := total/3600, total%3600
	m, s := rem/60, rem%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}
