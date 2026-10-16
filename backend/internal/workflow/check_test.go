package workflow

// check_test.go 程序校验器（exists/path_template/format/dimensions）单元测试（M1 验收面）。

import (
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTestPNG 生成一张测试 PNG：alpha=true 用半透明 NRGBA（保留透明通道），
// alpha=false 用全不透明 RGBA（编码为无 alpha 的真彩）。
func writeTestPNG(t *testing.T, path string, w, h int, alpha bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var img image.Image
	if alpha {
		m := image.NewNRGBA(image.Rect(0, 0, w, h))
		for i := 0; i+3 < len(m.Pix); i += 4 {
			m.Pix[i], m.Pix[i+1], m.Pix[i+2], m.Pix[i+3] = 10, 20, 30, 128
		}
		img = m
	} else {
		m := image.NewRGBA(image.Rect(0, 0, w, h))
		for i := 0; i+3 < len(m.Pix); i += 4 {
			m.Pix[i], m.Pix[i+1], m.Pix[i+2], m.Pix[i+3] = 10, 20, 30, 255
		}
		img = m
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

func TestCheckProgramSpriteSheet(t *testing.T) {
	dir := t.TempDir()
	alpha := true
	deliv := Deliverable{Type: "sprite_sheet", Frames: 2, Cell: "64x64", Alpha: &alpha, Format: "png"}
	// 128x64 = 2 格 × 64x64，带 alpha：全过。
	writeTestPNG(t, filepath.Join(dir, "assets", "a.png"), 128, 64, true)
	synth := &Synthesis{OutputPath: "assets/a.png"}
	res := CheckProgram(dir, synth, deliv, []string{"exists", "path_template", "format", "dimensions"})
	if !res.Pass {
		t.Fatalf("应全过: %+v", res.Items)
	}
	// 尺寸 ≠ 格子乘积：dimensions 失败。
	writeTestPNG(t, filepath.Join(dir, "assets", "b.png"), 128, 128, true)
	res = CheckProgram(dir, &Synthesis{OutputPath: "assets/b.png"}, deliv, []string{"dimensions"})
	if res.Pass || !strings.Contains(res.Items[0].Reason, "网格") {
		t.Fatalf("网格不符应失败: %+v", res.Items)
	}
	// alpha 缺失：dimensions 失败。
	writeTestPNG(t, filepath.Join(dir, "assets", "c.png"), 128, 64, false)
	res = CheckProgram(dir, &Synthesis{OutputPath: "assets/c.png"}, deliv, []string{"dimensions"})
	if res.Pass || !strings.Contains(res.Items[0].Reason, "alpha") {
		t.Fatalf("alpha 不符应失败: %+v", res.Items)
	}
}

func TestCheckProgramImageSingleCell(t *testing.T) {
	dir := t.TempDir()
	deliv := Deliverable{Type: "image", Cell: "256x256", Format: "png"}
	writeTestPNG(t, filepath.Join(dir, "hero.png"), 256, 256, false)
	res := CheckProgram(dir, &Synthesis{OutputPath: "hero.png"}, deliv, []string{"dimensions"})
	if !res.Pass {
		t.Fatalf("单格尺寸应通过: %+v", res.Items)
	}
	writeTestPNG(t, filepath.Join(dir, "hero2.png"), 256, 512, false)
	res = CheckProgram(dir, &Synthesis{OutputPath: "hero2.png"}, deliv, []string{"dimensions"})
	if res.Pass {
		t.Fatalf("尺寸不符应失败: %+v", res.Items)
	}
}

func TestCheckProgramExistsFormat(t *testing.T) {
	dir := t.TempDir()
	deliv := Deliverable{Type: "image", Format: "png"}
	// 不存在。
	res := CheckProgram(dir, &Synthesis{OutputPath: "nope.png"}, deliv, []string{"exists"})
	if res.Pass {
		t.Fatalf("缺失文件 exists 应失败")
	}
	// 扩展名不符。
	writeTestPNG(t, filepath.Join(dir, "a.jpg"), 8, 8, false)
	res = CheckProgram(dir, &Synthesis{OutputPath: "a.jpg"}, deliv, []string{"format"})
	if res.Pass || !strings.Contains(res.Items[0].Reason, "扩展名") {
		t.Fatalf("扩展名不符应失败: %+v", res.Items)
	}
	// 魔数不符（文本冒充 png）。
	if err := os.WriteFile(filepath.Join(dir, "bad.png"), []byte("not a png at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	res = CheckProgram(dir, &Synthesis{OutputPath: "bad.png"}, deliv, []string{"format"})
	if res.Pass || !strings.Contains(res.Items[0].Reason, "魔数") {
		t.Fatalf("魔数不符应失败: %+v", res.Items)
	}
	// 路径穿越。
	res = CheckProgram(dir, &Synthesis{OutputPath: "../evil.png"}, deliv, []string{"path_template"})
	if res.Pass {
		t.Fatalf("路径穿越应失败")
	}
	// 空文件 exists 失败。
	if err := os.WriteFile(filepath.Join(dir, "empty.png"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	res = CheckProgram(dir, &Synthesis{OutputPath: "empty.png"}, deliv, []string{"exists"})
	if res.Pass {
		t.Fatalf("空文件 exists 应失败")
	}
}

func TestCheckProgramVideoSkipsDimensions(t *testing.T) {
	dir := t.TempDir()
	// mp4 魔数：4-8 字节 ftyp。
	mp4 := []byte{0, 0, 0, 20, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm', 0, 0, 0, 1}
	if err := os.WriteFile(filepath.Join(dir, "clip.mp4"), mp4, 0o644); err != nil {
		t.Fatal(err)
	}
	deliv := Deliverable{Type: "video", Format: "mp4", Frames: 30, Cell: "64x64"}
	res := CheckProgram(dir, &Synthesis{OutputPath: "clip.mp4"}, deliv, []string{"exists", "format", "dimensions"})
	if !res.Pass {
		t.Fatalf("视频 exists/format 应过、dimensions 跳过: %+v", res.Items)
	}
}

func TestAspectFor(t *testing.T) {
	cases := []struct {
		deliv Deliverable
		want  string
	}{
		{Deliverable{Type: "image", Cell: "1024x1024"}, "1:1"},
		{Deliverable{Type: "sprite_sheet", Cell: "128x128", Frames: 8}, ""}, // 8:1 不在支持挡位
		{Deliverable{Type: "image", Cell: "1920x1080"}, "16:9"},
		{Deliverable{Type: "image", Cell: ""}, ""},
	}
	for _, tc := range cases {
		if got := aspectFor(tc.deliv); got != tc.want {
			t.Fatalf("aspectFor(%+v) = %q, want %q", tc.deliv, got, tc.want)
		}
	}
}
