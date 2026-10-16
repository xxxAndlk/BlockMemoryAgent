package workflow

// check.go 程序校验器（免费，开发文档 §6.1）：按 deliverable.type 分派，
// recipe.check.program 声明式列出关键字（exists / path_template / format / dimensions）。
// 能程序判的绝不叫模型判；v1 尺寸判定实现到 png（Go stdlib image/png，无新依赖），
// video 类型只做 exists/path/format（帧数校验待"交付物形态"拍板后补，校验器留接口）。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg" // 尺寸校验支持 jpg（alpha 恒 false）
	"image/png"
	"os"
	"path/filepath"
	"strings"
)

// CheckItem 是一条校验结果。
type CheckItem struct {
	Key    string `json:"key"`
	Pass   bool   `json:"pass"`
	Reason string `json:"reason,omitempty"`
}

// ProgramResult 是程序校验的聚合结果（Pass = 全部通过）。
type ProgramResult struct {
	Items []CheckItem `json:"items"`
	Pass  bool        `json:"pass"`
}

// CheckProgram 对期望产物执行声明式程序校验。
//   - workdir：运行工作区（synth.OutputPath 是工作区相对路径）。
//   - keys：recipe.check.program 关键字；未知关键字按失败标记（配方校验已挡，防御兜底）。
func CheckProgram(workdir string, synth *Synthesis, deliv Deliverable, keys []string) ProgramResult {
	res := ProgramResult{Pass: true}
	if synth == nil {
		return ProgramResult{Pass: false, Items: []CheckItem{{Key: "synth", Pass: false, Reason: "无合成结果"}}}
	}
	abs := joinWorkdir(workdir, synth.OutputPath)
	for _, kw := range keys {
		var item CheckItem
		switch kw {
		case "exists":
			item = checkExists(abs, synth.OutputPath)
		case "path_template":
			item = checkPathTemplate(synth.OutputPath)
		case "format":
			item = checkFormat(abs, synth.OutputPath, deliv)
		case "dimensions":
			item = checkDimensions(abs, deliv)
		default:
			item = CheckItem{Key: kw, Pass: false, Reason: "未知程序校验器（配方校验应已拦截）"}
		}
		if !item.Pass {
			res.Pass = false
		}
		res.Items = append(res.Items, item)
	}
	return res
}

func checkExists(abs, rel string) CheckItem {
	st, err := os.Stat(abs)
	if err != nil {
		return CheckItem{Key: "exists", Pass: false, Reason: fmt.Sprintf("产物不存在: %s", rel)}
	}
	if st.IsDir() {
		return CheckItem{Key: "exists", Pass: false, Reason: fmt.Sprintf("产物是目录不是文件: %s", rel)}
	}
	if st.Size() == 0 {
		return CheckItem{Key: "exists", Pass: false, Reason: fmt.Sprintf("产物为空文件: %s", rel)}
	}
	return CheckItem{Key: "exists", Pass: true, Reason: fmt.Sprintf("%d bytes", st.Size())}
}

func checkPathTemplate(rel string) CheckItem {
	norm := normalizeRelPath(rel)
	if norm == "" || strings.Contains(norm, "..") {
		return CheckItem{Key: "path_template", Pass: false, Reason: fmt.Sprintf("产物路径不合规: %q", rel)}
	}
	return CheckItem{Key: "path_template", Pass: true, Reason: norm}
}

// checkFormat 扩展名 / 魔数匹配交付格式。
func checkFormat(abs, rel string, deliv Deliverable) CheckItem {
	want := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(deliv.Format)), ".")
	if want == "" {
		want = defaultFormat(deliv.Type)
	}
	got := strings.TrimPrefix(strings.ToLower(filepath.Ext(rel)), ".")
	if got == "jpeg" {
		got = "jpg"
	}
	if want == "jpeg" {
		want = "jpg"
	}
	if got != want {
		return CheckItem{Key: "format", Pass: false, Reason: fmt.Sprintf("扩展名 %q 与契约 %q 不符", got, want)}
	}
	ok, kind := fileMagicMatches(abs, got)
	if !ok {
		return CheckItem{Key: "format", Pass: false, Reason: fmt.Sprintf("文件魔数与 %s 不符（实测 %s）", want, kind)}
	}
	return CheckItem{Key: "format", Pass: true, Reason: want}
}

// fileMagicMatches 读文件头判魔数；未知扩展名不做魔数判定（放行）。
func fileMagicMatches(abs, ext string) (bool, string) {
	f, err := os.Open(abs)
	if err != nil {
		return false, "无法读取"
	}
	defer f.Close()
	head := make([]byte, 16)
	n, _ := f.Read(head)
	head = head[:n]
	switch ext {
	case "png":
		return bytes.HasPrefix(head, []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}), "非PNG"
	case "jpg":
		return len(head) >= 3 && head[0] == 0xFF && head[1] == 0xD8 && head[2] == 0xFF, "非JPEG"
	case "webp":
		return len(head) >= 12 && string(head[0:4]) == "RIFF" && string(head[8:12]) == "WEBP", "非WEBP"
	case "gif":
		return len(head) >= 4 && string(head[0:4]) == "GIF8", "非GIF"
	case "mp4", "m4v", "mov":
		return len(head) >= 8 && string(head[4:8]) == "ftyp", "非MP4"
	case "webm":
		return len(head) >= 4 && head[0] == 0x1A && head[1] == 0x45 && head[2] == 0xDF && head[3] == 0xA3, "非WEBM"
	default:
		return true, ""
	}
}

// checkDimensions 尺寸契约：sprite_sheet 按 cell × frames 网格推整张尺寸
// （宽高各被格子整除且格子乘积 == 帧数，条带 8×1 与网格 4×2 都成立）；
// alpha 需求随 PNG 颜色模型判定（调色板图全解码扫描）。非 png/未声明契约时跳过（Pass）。
func checkDimensions(abs string, deliv Deliverable) CheckItem {
	cw, ch, cellErr := parseCell(deliv.Cell)
	frames := deliv.Frames
	if cellErr != nil && deliv.Alpha == nil {
		return CheckItem{Key: "dimensions", Pass: true, Reason: "未声明尺寸契约，跳过"}
	}
	if cellErr == nil && frames <= 0 {
		frames = 1 // image + cell：按单格尺寸校验
	}
	ext := strings.ToLower(filepath.Ext(abs))
	if ext != ".png" {
		return CheckItem{Key: "dimensions", Pass: true, Reason: fmt.Sprintf("%s 类型 v1 不做尺寸校验", ext)}
	}
	f, err := os.Open(abs)
	if err != nil {
		return CheckItem{Key: "dimensions", Pass: false, Reason: "无法读取产物"}
	}
	defer f.Close()
	cfg, err := png.DecodeConfig(f)
	if err != nil {
		return CheckItem{Key: "dimensions", Pass: false, Reason: fmt.Sprintf("PNG 解码失败: %v", err)}
	}
	var reasons []string
	pass := true
	if cellErr == nil && frames > 0 {
		gridOK := cfg.Width%cw == 0 && cfg.Height%ch == 0 && (cfg.Width/cw)*(cfg.Height/ch) == frames
		if !gridOK {
			pass = false
			reasons = append(reasons, fmt.Sprintf("尺寸 %dx%d 不符 %d 帧 × %s 网格（格子乘积应为 %d）",
				cfg.Width, cfg.Height, frames, deliv.Cell, frames))
		} else {
			reasons = append(reasons, fmt.Sprintf("%dx%d = %d 格 × %s", cfg.Width, cfg.Height, frames, deliv.Cell))
		}
	}
	if deliv.Alpha != nil {
		hasAlpha, err := pngHasAlpha(abs, cfg.ColorModel)
		if err != nil {
			pass = false
			reasons = append(reasons, fmt.Sprintf("alpha 判定失败: %v", err))
		} else if hasAlpha != *deliv.Alpha {
			pass = false
			reasons = append(reasons, fmt.Sprintf("alpha=%v 与契约 alpha=%v 不符", hasAlpha, *deliv.Alpha))
		} else {
			reasons = append(reasons, fmt.Sprintf("alpha=%v", hasAlpha))
		}
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "通过")
	}
	return CheckItem{Key: "dimensions", Pass: pass, Reason: strings.Join(reasons, "；")}
}

// pngHasAlpha 判定 PNG 是否有透明通道：NRGBA 系模型直接为真；调色板图全解码扫描
// 是否存在非全不透明像素；其余（RGB/Gray 系）为不透明。
func pngHasAlpha(abs string, cm color.Model) (bool, error) {
	switch cm {
	case color.NRGBAModel, color.NRGBA64Model:
		return true, nil
	case color.GrayModel, color.Gray16Model, color.RGBAModel, color.RGBA64Model, color.YCbCrModel, color.CMYKModel:
		return false, nil
	}
	// 调色板等：全解码扫描 alpha。
	f, err := os.Open(abs)
	if err != nil {
		return false, err
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return false, err
	}
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a < 0xFFFF {
				return true, nil
			}
		}
	}
	return false, nil
}

// normalizeRelPath 归一化工作区相对路径（反斜杠→正斜杠、去 ./ 前缀、去首尾 /）。
func normalizeRelPath(p string) string {
	p = strings.ReplaceAll(strings.TrimSpace(p), "\\", "/")
	p = strings.TrimPrefix(p, "./")
	return strings.Trim(p, "/")
}

// CheckResultJSON 组装 unit 的 check_result JSON（程序 + 评委 [+ 漂移/升级] 三段）。
type CheckResultJSON struct {
	Program *ProgramResult `json:"program,omitempty"`
	Judge   *JudgeResult   `json:"judge,omitempty"`
	Drift   *DriftInfo     `json:"drift,omitempty"`
	Upgrade *UpgradeInfo   `json:"upgrade,omitempty"`
	Error   string         `json:"error,omitempty"`
}

// MarshalCheckResult 序列化 check_result（空结构返回 ""，写入时置 NULL 语义）。
func MarshalCheckResult(cr CheckResultJSON) string {
	if cr.Program == nil && cr.Judge == nil && cr.Drift == nil && cr.Upgrade == nil && cr.Error == "" {
		return ""
	}
	b, err := json.Marshal(cr)
	if err != nil {
		return ""
	}
	return string(b)
}

// decodeCheckResult 解析 check_result JSON（展示/前端聚合用，损坏返回零值）。
func decodeCheckResult(raw string) CheckResultJSON {
	var cr CheckResultJSON
	if strings.TrimSpace(raw) == "" {
		return cr
	}
	_ = json.Unmarshal([]byte(raw), &cr)
	return cr
}

// imageSizeOf 读取图片尺寸（测试/工具用）。
func imageSizeOf(abs string) (int, int, error) {
	f, err := os.Open(abs)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0, err
	}
	return cfg.Width, cfg.Height, nil
}
