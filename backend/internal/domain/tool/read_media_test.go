package tool

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadMedia_Image 验证：图片按路径读取，附单张 ResultImage（base64 可解码、
// MIME 按扩展名映射），Output 含路径注记。
func TestReadMedia_Image(t *testing.T) {
	dir := t.TempDir()
	r := NewBuiltinRegistry(dir, nil, nil)

	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x01, 0x02}
	if err := os.WriteFile(filepath.Join(dir, "shot.png"), png, 0o644); err != nil {
		t.Fatalf("seed png: %v", err)
	}

	res, err := r.Dispatch(context.Background(), "ReadMedia", map[string]any{"path": "shot.png"})
	if err != nil || !res.Success {
		t.Fatalf("read image failed: res=%v err=%v", res, err)
	}
	if len(res.Images) != 1 {
		t.Fatalf("expected 1 image, got %d", len(res.Images))
	}
	img := res.Images[0]
	if img.MIMEType != "image/png" {
		t.Fatalf("expected image/png, got %s", img.MIMEType)
	}
	if got, err := base64.StdEncoding.DecodeString(string(img.Data)); err != nil || string(got) != string(png) {
		t.Fatalf("image data mismatch: err=%v len=%d", err, len(got))
	}
	if !strings.Contains(res.Output, "shot.png") {
		t.Fatalf("output missing path note, got: %s", res.Output)
	}
}

// TestReadMedia_ImageOverLimit 验证：超过 4MiB 的图片拒绝并提示压缩。
func TestReadMedia_ImageOverLimit(t *testing.T) {
	dir := t.TempDir()
	r := NewBuiltinRegistry(dir, nil, nil)

	big := make([]byte, readMediaImageBytes+1)
	if err := os.WriteFile(filepath.Join(dir, "big.png"), big, 0o644); err != nil {
		t.Fatalf("seed big png: %v", err)
	}

	res, err := r.Dispatch(context.Background(), "ReadMedia", map[string]any{"path": "big.png"})
	if err != nil || res.Success {
		t.Fatalf("expected failure, got res=%v err=%v", res, err)
	}
	if res.Category != ResultCategoryValidationRejected {
		t.Fatalf("expected validation_rejected, got %q", res.Category)
	}
	if !strings.Contains(res.Error, "超过上限") {
		t.Fatalf("error should mention limit, got: %s", res.Error)
	}
}

// TestReadMedia_UnsupportedExt 验证：非媒体扩展名拒绝并列出支持格式。
func TestReadMedia_UnsupportedExt(t *testing.T) {
	dir := t.TempDir()
	r := NewBuiltinRegistry(dir, nil, nil)

	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("seed txt: %v", err)
	}

	res, err := r.Dispatch(context.Background(), "ReadMedia", map[string]any{"path": "note.txt"})
	if err != nil || res.Success {
		t.Fatalf("expected failure, got res=%v err=%v", res, err)
	}
	if res.Category != ResultCategoryValidationRejected || !strings.Contains(res.Error, "不支持的媒体格式") {
		t.Fatalf("unexpected rejection: res=%v", res)
	}
}

// TestReadMedia_SandboxEscape 验证：越出工作目录的路径被沙箱拒绝。
func TestReadMedia_SandboxEscape(t *testing.T) {
	dir := t.TempDir()
	r := NewBuiltinRegistry(dir, nil, nil)

	outside := filepath.Join(filepath.Dir(dir), "outside.png")
	if err := os.WriteFile(outside, []byte{0x89, 'P', 'N', 'G'}, 0o644); err != nil {
		t.Fatalf("seed outside png: %v", err)
	}

	res, err := r.Dispatch(context.Background(), "ReadMedia", map[string]any{"path": outside})
	if err != nil || res.Success {
		t.Fatalf("expected sandbox rejection, got res=%v err=%v", res, err)
	}
	if res.Category != ResultCategoryValidationRejected || !strings.Contains(res.Error, "sandbox") {
		t.Fatalf("expected sandbox error, got: %s", res.Error)
	}
}

// TestReadMedia_VideoWithResolver 验证：视频走注入解析器（fake，不依赖 ffmpeg），
// 帧图与注记并入结果；解析器未接线时降级文本说明。
func TestReadMedia_VideoWithResolver(t *testing.T) {
	dir := t.TempDir()
	r := NewBuiltinRegistry(dir, nil, nil)

	if err := os.WriteFile(filepath.Join(dir, "demo.mp4"), []byte("fake-video-bytes"), 0o644); err != nil {
		t.Fatalf("seed mp4: %v", err)
	}

	// 未注入 resolver：降级文本说明，不失败不 panic。
	res, err := r.Dispatch(context.Background(), "ReadMedia", map[string]any{"path": "demo.mp4"})
	if err != nil {
		t.Fatalf("no-resolver dispatch failed: %v", err)
	}
	if res.Success || !strings.Contains(res.Output, "解析器未接线") {
		t.Fatalf("expected degrade note, got res=%v", res)
	}

	// 注入 fake resolver：帧与注记透传。
	r.SetMediaResolver(func(ctx context.Context, path string) ([]ResultImage, []string) {
		if !strings.HasSuffix(path, "demo.mp4") {
			t.Errorf("resolver got unexpected path %s", path)
		}
		return []ResultImage{
			{MIMEType: "image/jpeg", Data: []byte("ZnJhbWUx")},
			{MIMEType: "image/jpeg", Data: []byte("ZnJhbWUy")},
		}, []string{"已抽取 2 帧（h264, 640x480）"}
	})

	res, err = r.Dispatch(context.Background(), "ReadMedia", map[string]any{"path": "demo.mp4"})
	if err != nil || !res.Success {
		t.Fatalf("video read failed: res=%v err=%v", res, err)
	}
	if len(res.Images) != 2 {
		t.Fatalf("expected 2 frames, got %d", len(res.Images))
	}
	if !strings.Contains(res.Output, "已抽取 2 帧") || !strings.Contains(res.Output, "2 帧关键帧") {
		t.Fatalf("output missing notes, got: %s", res.Output)
	}
}

// TestReadMedia_MissingPath 验证：空 path 参数按校验拒绝处理。
func TestReadMedia_MissingPath(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)

	res, err := r.Dispatch(context.Background(), "ReadMedia", map[string]any{})
	if err != nil || res.Success {
		t.Fatalf("expected failure, got res=%v err=%v", res, err)
	}
	if res.Category != ResultCategoryValidationRejected {
		t.Fatalf("expected validation_rejected, got %q", res.Category)
	}
}
