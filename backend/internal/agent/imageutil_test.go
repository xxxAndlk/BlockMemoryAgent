// imageutil_test.go 覆盖 TODO 第9项④ 截图/图片降采样：
//   - scaleImageLongEdge 纯函数：非等比缩小、小图不放大、坏图/非缩放格式直通、确定性；
//   - downsampleResultImages 端到端：Data 替换、MIMEType 更新、原图落盘与 Output 标注；
//   - SetImageMaxEdge 包级旋钮读写。
package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
)

func encodePNG(t *testing.T, w, h int, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// TestScaleImageLongEdge_ScalesNonProportional 200x100 → 长边 100 时等比缩为 100x50。
func TestScaleImageLongEdge_ScalesNonProportional(t *testing.T) {
	raw := encodePNG(t, 200, 100, color.RGBA{R: 0x40, G: 0x80, B: 0xC0, A: 0xFF})
	out, format := scaleImageLongEdge(raw, 100)
	if out == nil || format != "png" {
		t.Fatalf("expected scaled png, got nil/%q", format)
	}
	src, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode scaled: %v", err)
	}
	b := src.Bounds()
	if b.Dx() != 100 || b.Dy() != 50 {
		t.Fatalf("scaled dims = %dx%d, want 100x50", b.Dx(), b.Dy())
	}
	// 纯色图缩放后均值即原色（box-filter 数学性质）。
	r, g, bl, a := src.At(50, 25).RGBA()
	if uint8(r>>8) != 0x40 || uint8(g>>8) != 0x80 || uint8(bl>>8) != 0xC0 || uint8(a>>8) != 0xFF {
		t.Fatalf("solid color must survive box filter, got #%02x%02x%02x%02x", r>>8, g>>8, bl>>8, a>>8)
	}
}

// TestScaleImageLongEdge_Deterministic 同源同参两次缩放逐字节一致（指纹去重前提）。
func TestScaleImageLongEdge_Deterministic(t *testing.T) {
	raw := encodePNG(t, 300, 220, color.RGBA{R: 0x10, G: 0x20, B: 0x30, A: 0xFF})
	a1, f1 := scaleImageLongEdge(raw, 128)
	a2, f2 := scaleImageLongEdge(raw, 128)
	if f1 != f2 || !bytes.Equal(a1, a2) {
		t.Fatal("scaling must be byte-deterministic for identical input")
	}
}

// TestScaleImageLongEdge_NoUpscale 小图与已达上限的图原样直通（返回 nil）。
func TestScaleImageLongEdge_NoUpscale(t *testing.T) {
	small := encodePNG(t, 64, 32, color.RGBA{G: 0xFF, A: 0xFF})
	if out, _ := scaleImageLongEdge(small, 128); out != nil {
		t.Fatal("small image must not be upscaled")
	}
	equal := encodePNG(t, 128, 128, color.RGBA{G: 0xFF, A: 0xFF})
	if out, _ := scaleImageLongEdge(equal, 128); out != nil {
		t.Fatal("image at max edge must pass through untouched")
	}
	// 坏字节直通。
	if out, _ := scaleImageLongEdge([]byte("not an image"), 128); out != nil {
		t.Fatal("garbage bytes must pass through")
	}
}

// TestDownsampleResultImages_E2E base64 入参下 Data 被替换、MIMEType 更新、
// 原图落盘 .bma/images 且 Output 追加「原图已落盘」行。
func TestDownsampleResultImages_E2E(t *testing.T) {
	dir := t.TempDir()
	raw := encodePNG(t, 800, 400, color.RGBA{R: 0x7F, A: 0xFF})
	res := &ToolResult{
		Tool:    "take_screenshot",
		Success: true,
		Images: []tool.ResultImage{{
			Data:     []byte(base64.StdEncoding.EncodeToString(raw)),
			MIMEType: "image/png",
		}},
	}
	ctx := tool.WithWorkDir(context.Background(), dir)
	downsampleResultImages(ctx, "take_screenshot", res, 256)
	if len(res.Images) != 1 {
		t.Fatalf("images count changed: %d", len(res.Images))
	}
	scaled, err := base64.StdEncoding.DecodeString(string(res.Images[0].Data))
	if err != nil {
		t.Fatalf("scaled data not base64: %v", err)
	}
	src, err := png.Decode(bytes.NewReader(scaled))
	if err != nil {
		t.Fatalf("scaled not png: %v", err)
	}
	if b := src.Bounds(); b.Dx() != 256 || b.Dy() != 128 {
		t.Fatalf("scaled dims = %dx%d, want 256x128", b.Dx(), b.Dy())
	}
	if res.Images[0].MIMEType != "image/png" {
		t.Fatalf("mimetype = %q", res.Images[0].MIMEType)
	}
	if !strings.Contains(res.Output, "原图已落盘") {
		t.Fatalf("output should record original path, got: %q", res.Output)
	}
	idx := strings.Index(res.Output, "原图已落盘: ")
	path := strings.TrimSpace(res.Output[idx+len("原图已落盘: "):])
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("original file missing: %v (path=%q)", err, path)
	}
	orig, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(orig, raw) {
		t.Fatalf("original file content mismatch: %v", err)
	}
	// 落盘目录约定。
	if dir := filepath.Dir(path); filepath.Base(dir) != "images" {
		t.Fatalf("original must live under .bma/images, got %q", path)
	}
}

// TestDownsampleResultImages_PassThrough 坏图与关闭态零行为。
func TestDownsampleResultImages_PassThrough(t *testing.T) {
	dir := t.TempDir()
	// 坏 base64：跳过不炸。
	bad := &ToolResult{Images: []tool.ResultImage{{Data: []byte("!!!not-base64!!!"), MIMEType: "image/png"}}}
	downsampleResultImages(tool.WithWorkDir(context.Background(), dir), "t", bad, 256)
	if string(bad.Images[0].Data) != "!!!not-base64!!!" {
		t.Fatal("undecodable image must be left untouched")
	}
	// 关闭（maxEdge<=0）零行为。
	raw := encodePNG(t, 800, 400, color.RGBA{B: 0xFF, A: 0xFF})
	off := &ToolResult{Images: []tool.ResultImage{{Data: []byte(base64.StdEncoding.EncodeToString(raw)), MIMEType: "image/png"}}}
	downsampleResultImages(tool.WithWorkDir(context.Background(), dir), "t", off, 0)
	if string(off.Images[0].Data) != string([]byte(base64.StdEncoding.EncodeToString(raw))) {
		t.Fatal("disabled downsampling must leave data untouched")
	}
	if strings.Contains(off.Output, "原图已落盘") {
		t.Fatal("disabled downsampling must not write originals")
	}
}

// TestSetImageMaxEdge 包级旋钮注入生效。
func TestSetImageMaxEdge(t *testing.T) {
	prev := int(imageMaxEdgeCfg.Load())
	defer SetImageMaxEdge(prev)
	SetImageMaxEdge(512)
	if got := int(imageMaxEdgeCfg.Load()); got != 512 {
		t.Fatalf("SetImageMaxEdge(512) -> %d", got)
	}
	SetImageMaxEdge(0)
	if got := int(imageMaxEdgeCfg.Load()); got != 0 {
		t.Fatalf("SetImageMaxEdge(0) -> %d", got)
	}
}
