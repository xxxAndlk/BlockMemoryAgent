// Package agent 视频附件处理测试。
package agent

// video_test.go 验证视频附件链路：
//   - ParseWireVideos：数量/扩展名白名单/文件存在/大小上限/mime 回填；
//   - ResolveVideos 降级：ffmpeg/ffprobe 缺失 → 仅元数据文本，不报错；
//   - ResolveVideos 真实轨（本机有 ffmpeg 才跑）：lavfi 生成测试视频 → 抽帧成功；
//   - CreateSession / sendMessageFull 接线：stub resolveVideosFn 验证帧并入
//     Images、元数据文本并入 goal/Content、Videos 不入库。

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/pkg/enums"
)

// writeTempVideo 写一个指定内容的假视频文件（只测 ParseWireVideos 的存在/大小校验，
// 不解析内容），返回路径。
func writeTempVideo(t *testing.T, name string, size int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
		t.Fatalf("写临时文件: %v", err)
	}
	return p
}

func TestParseWireVideos(t *testing.T) {
	ok := writeTempVideo(t, "a.mp4", 10)

	// 合法：mime 缺省按扩展名回填；大写扩展名大小写不敏感。
	got, err := ParseWireVideos([]WireVideo{{Path: ok}}, 0)
	if err != nil {
		t.Fatalf("合法视频应通过: %v", err)
	}
	if len(got) != 1 || got[0].MIMEType != "video/mp4" {
		t.Fatalf("mime 应按扩展名回填 video/mp4, got %+v", got)
	}

	upper := writeTempVideo(t, "b.MP4", 10)
	if _, err := ParseWireVideos([]WireVideo{{Path: upper}}, 0); err != nil {
		t.Fatalf("大写扩展名应通过: %v", err)
	}

	// 超数量上限（3 > 2）。
	paths := []WireVideo{{Path: ok}, {Path: ok}, {Path: ok}}
	if _, err := ParseWireVideos(paths, 0); err == nil {
		t.Fatal("3 个视频应被拒绝")
	}

	// 非法扩展名。
	if _, err := ParseWireVideos([]WireVideo{{Path: writeTempVideo(t, "x.exe", 1)}}, 0); err == nil {
		t.Fatal(".exe 应被拒绝")
	}

	// 文件不存在。
	if _, err := ParseWireVideos([]WireVideo{{Path: filepath.Join(t.TempDir(), "nope.mp4")}}, 0); err == nil {
		t.Fatal("不存在的文件应被拒绝")
	}

	// 超大小上限（maxBytes=1）。
	if _, err := ParseWireVideos([]WireVideo{{Path: ok}}, 1); err == nil {
		t.Fatal("超大小上限应被拒绝")
	}

	// 空路径。
	if _, err := ParseWireVideos([]WireVideo{{Path: ""}}, 0); err == nil {
		t.Fatal("空路径应被拒绝")
	}
}

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		sec  float64
		want string
	}{
		{0, "时长未知"},
		{-1, "时长未知"},
		{90, "01:30"},
		{61, "01:01"},
		{3661, "1:01:01"},
	}
	for _, c := range cases {
		if got := formatDuration(c.sec); got != c.want {
			t.Fatalf("formatDuration(%v) = %q, want %q", c.sec, got, c.want)
		}
	}
}

// TestResolveVideosDegradesWithoutFFmpeg 验证降级路径：二进制不可用时返回
// 零帧 + 元数据文本（不返回 error——签名即无 error，断言 notes 内容）。
// 显式传入不存在的二进制名，与宿主机是否装了 ffmpeg 无关。
func TestResolveVideosDegradesWithoutFFmpeg(t *testing.T) {
	p := writeTempVideo(t, "clip.mp4", 1<<20)
	opts := VideoOptions{FFmpegBin: "bma_no_such_ffmpeg_xx", FFprobeBin: "bma_no_such_ffprobe_xx"}
	frames, notes := ResolveVideos(context.Background(), []WireVideo{{Path: p}}, opts, 0)
	if len(frames) != 0 {
		t.Fatalf("ffmpeg 缺失应零帧, got %d", len(frames))
	}
	if len(notes) != 1 {
		t.Fatalf("应有 1 段元数据文本, got %d", len(notes))
	}
	// ffprobe 缺失时探测失败：note 走"未能读取视频信息"分支并含文件名。
	if !strings.Contains(notes[0], "clip.mp4") || !strings.Contains(notes[0], "未能") {
		t.Fatalf("降级文案不符: %q", notes[0])
	}
}

// TestResolveVideosReal 真实轨：本机有 ffmpeg 才跑（CI 无 ffmpeg 自动跳过）。
// 用 lavfi 生成 1s 测试视频，验证抽 6 帧 JPEG + 元数据文本编号映射含 imageBase 偏移。
func TestResolveVideosReal(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("本机无 ffmpeg，跳过真实抽帧测试")
	}
	p := filepath.Join(t.TempDir(), "clip.mp4")
	gen := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=1:size=320x240:rate=10", "-y", p)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("生成测试视频失败: %v\n%s", err, out)
	}

	frames, notes := ResolveVideos(context.Background(), []WireVideo{{Path: p}}, VideoOptions{}, 1)
	if len(frames) != 6 {
		t.Fatalf("应抽 6 帧, got %d", len(frames))
	}
	for i, f := range frames {
		if f.MIMEType != "image/jpeg" {
			t.Fatalf("帧 %d MIMEType = %s, want image/jpeg", i, f.MIMEType)
		}
		raw, err := base64.StdEncoding.DecodeString(string(f.Data))
		if err != nil || len(raw) < 2 || raw[0] != 0xFF || raw[1] != 0xD8 {
			t.Fatalf("帧 %d 应为 base64 JPEG（FFD8 魔数）, err=%v", i, err)
		}
	}
	if len(notes) != 1 {
		t.Fatalf("应有 1 段元数据文本, got %d", len(notes))
	}
	// imageBase=1：帧编号映射应为 image:2..image:7。
	if !strings.Contains(notes[0], "已抽取 6 关键帧（对应 image:2..image:7）") {
		t.Fatalf("编号映射应含 imageBase 偏移: %q", notes[0])
	}
	if !strings.Contains(notes[0], "320x240") || !strings.Contains(notes[0], "00:01") {
		t.Fatalf("元数据应含分辨率与时长: %q", notes[0])
	}
}

// videoStubFrames / videoStubNote 是接线测试的固定 stub 产物。
func videoStubOutputs(imageBase int) ([]tool.ResultImage, []string) {
	frames := []tool.ResultImage{
		{MIMEType: "image/jpeg", Data: []byte("ZnJhbWUx")},
		{MIMEType: "image/jpeg", Data: []byte("ZnJhbWUy")},
	}
	return frames, []string{
		"[视频 1: clip.mp4 | 00:01 | 320x240 | 已抽取 2 关键帧（对应 image:" +
			strconv.Itoa(imageBase+1) + "..image:" + strconv.Itoa(imageBase+2) + "）]",
	}
}

// stubResolveVideos 替换 resolveVideosFn 为固定 stub（按收到的 imageBase 生成
// 编号映射），返回记录调用参数的闭包。
func stubResolveVideos(t *testing.T) *videoStubRecorder {
	t.Helper()
	rec := &videoStubRecorder{}
	old := resolveVideosFn
	resolveVideosFn = func(ctx context.Context, vids []WireVideo, opts VideoOptions, imageBase int) ([]tool.ResultImage, []string) {
		rec.calls++
		rec.lastImageBase = imageBase
		rec.lastVids = vids
		if len(vids) == 0 {
			return nil, nil
		}
		frames, notes := videoStubOutputs(imageBase)
		return frames, notes
	}
	t.Cleanup(func() { resolveVideosFn = old })
	return rec
}

type videoStubRecorder struct {
	calls         int
	lastImageBase int
	lastVids      []WireVideo
}

// TestCreateSessionWithVideos 验证 CreateSession 接线：帧并入 firstTurnImages
// （经 Goal 里的元数据文本间接验证编号映射）、notes 并入 Goal、Videos 不入库。
func TestCreateSessionWithVideos(t *testing.T) {
	rec := stubResolveVideos(t)
	svc := newAskUserTestService(t, &mockReactModelProvider{})

	created, err := svc.CreateSession(context.Background(), CreateRequest{
		Goal:   "分析这段视频",
		Images: []tool.ResultImage{{MIMEType: "image/png", Data: []byte("aQ==")}},
		Videos: []WireVideo{{Path: "D:/clip/a.mp4", MIMEType: "video/mp4"}},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if rec.calls != 1 {
		t.Fatalf("resolveVideosFn 应被调用 1 次, got %d", rec.calls)
	}
	if rec.lastImageBase != 1 {
		t.Fatalf("imageBase 应为已有图片数 1, got %d", rec.lastImageBase)
	}
	// 元数据文本并入 Goal（持久化），编号映射反映已有图片偏移。
	if !strings.Contains(created.Goal, "分析这段视频") || !strings.Contains(created.Goal, "[视频 1: clip.mp4") {
		t.Fatalf("Goal 应并入视频元数据文本: %q", created.Goal)
	}
	if !strings.Contains(created.Goal, "image:2..image:3") {
		t.Fatalf("编号映射应含偏移: %q", created.Goal)
	}
}

// TestSendMessageWithVideos 验证 sendMessageFull 接线：帧并入该条 Message.Images、
// 元数据文本并入 Content、Videos 不随 Message 入 session.Messages。
func TestSendMessageWithVideos(t *testing.T) {
	stubResolveVideos(t)
	svc := newAskUserTestService(t, &mockReactModelProvider{})
	ctx := context.Background()

	created, err := svc.CreateSession(ctx, CreateRequest{Goal: "x"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// 等首轮跑完，避免并发追加干扰断言。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s, _ := svc.Get(ctx, created.ID)
		if s.Status == string(enums.SessionStatusCompleted) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	vids := []WireVideo{{Path: "D:/clip/a.mp4", MIMEType: "video/mp4"}}
	if err := svc.sendMessageFull(ctx, created.ID, "看视频继续", vids,
		tool.ResultImage{MIMEType: "image/png", Data: []byte("aQ==")}); err != nil {
		t.Fatalf("sendMessageFull: %v", err)
	}

	// 用户消息同步追加：Content 含元数据文本、Images = 原有 1 张 + 帧 2 张、无 Videos。
	var found *Message
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && found == nil {
		s, _ := svc.Get(ctx, created.ID)
		for i := range s.Messages {
			m := s.Messages[i]
			if m.Role == string(enums.ChatRoleUser) && strings.Contains(m.Content, "看视频继续") {
				found = &s.Messages[i]
				break
			}
		}
		if found == nil {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if found == nil {
		t.Fatal("未找到带视频的用户消息")
	}
	if !strings.Contains(found.Content, "[视频 1: clip.mp4") {
		t.Fatalf("Content 应并入元数据文本: %q", found.Content)
	}
	if len(found.Images) != 3 {
		t.Fatalf("Images 应为原有 1 张 + 帧 2 张 = 3, got %d", len(found.Images))
	}
	if found.Videos != nil {
		t.Fatalf("Videos 转换后应即弃（不随 Message 入库）, got %+v", found.Videos)
	}
}

// TestResolveVideosNative 验证 native 模式：≤ NativeMaxBytes 的视频跳过抽帧，
// 整个文件以 video/* 媒体项直传（base64 往返还原原字节）。无需 ffmpeg——
// probe 失败时 SizeBytes 由 os.Stat 兜底，native 分支不依赖 ffprobe。
func TestResolveVideosNative(t *testing.T) {
	p := writeTempVideo(t, "clip.mp4", 1024)
	frames, notes := ResolveVideos(context.Background(),
		[]WireVideo{{Path: p, MIMEType: "video/mp4"}},
		VideoOptions{NativeMaxBytes: 1 << 20}, 1)

	if len(frames) != 1 {
		t.Fatalf("native 模式应直传 1 个媒体项, got %d", len(frames))
	}
	f := frames[0]
	if f.MIMEType != "video/mp4" {
		t.Fatalf("MIMEType = %s, want video/mp4", f.MIMEType)
	}
	raw, err := base64.StdEncoding.DecodeString(string(f.Data))
	if err != nil || len(raw) != 1024 {
		t.Fatalf("base64 解码应还原原文件字节, err=%v len=%d", err, len(raw))
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "完整视频已直传") {
		t.Fatalf("native 文案不符: %q", notes)
	}
	if !strings.Contains(notes[0], "对应 image:2") { // imageBase=1 → image:2
		t.Fatalf("native 编号映射不符: %q", notes[0])
	}
}

// TestResolveVideosNativeUnsupportedMIME 验证 native 模式 MIME 守卫：webm/mkv
// 不在 Ark 支持列表（仅 mp4/avi/mov），即使体积达标也不直传，回落抽帧
//（无 ffmpeg → 降级元数据）。
func TestResolveVideosNativeUnsupportedMIME(t *testing.T) {
	p := writeTempVideo(t, "clip.webm", 1024)
	frames, notes := ResolveVideos(context.Background(),
		[]WireVideo{{Path: p, MIMEType: "video/webm"}},
		VideoOptions{NativeMaxBytes: 1 << 20, FFmpegBin: "bma_no_such_ffmpeg_xx", FFprobeBin: "bma_no_such_ffprobe_xx"}, 0)

	if len(frames) != 0 {
		t.Fatalf("webm 不应直传, got %d 个媒体项", len(frames))
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "未能") {
		t.Fatalf("应回落抽帧并降级为元数据文本: %q", notes)
	}
}

// TestResolveVideosNativeFallback 验证超 native 上限回落抽帧（ffmpeg 缺失 → 降级元数据）。
func TestResolveVideosNativeFallback(t *testing.T) {
	p := writeTempVideo(t, "big.mp4", 2<<20)
	frames, notes := ResolveVideos(context.Background(),
		[]WireVideo{{Path: p, MIMEType: "video/mp4"}},
		VideoOptions{NativeMaxBytes: 1 << 20, FFmpegBin: "bma_no_such_ffmpeg_xx", FFprobeBin: "bma_no_such_ffprobe_xx"}, 0)

	if len(frames) != 0 {
		t.Fatalf("超限且无 ffmpeg 应零媒体项, got %d", len(frames))
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "未能") {
		t.Fatalf("应降级为元数据文本: %q", notes)
	}
}
