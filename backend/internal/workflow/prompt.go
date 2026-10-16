package workflow

// prompt.go Prompter：不变量 + 变量槽 + 锚点 → 生成工具入参（开发文档 §5.2）。
// 配方不存"最终完整提示词"，逐件实际提示词在这里合成；锚点按阶段 anchor_in 声明装配
//（ui_design 插件的 i2i `reference` / 视频 `first_frame` 参数，2026-10-14 已落地）。

import (
	"fmt"
	"os"
	"path"
	"strings"
)

// Synthesis 是一次生成调用的合成结果。
type Synthesis struct {
	Tool        string         // 生成工具名（recipe.tool）
	Args        map[string]any // 生成工具入参（prompt/save_as/reference|first_frame/...）
	PromptText  string         // 合成提示词原文（评委与漂移检测复用）
	OutputPath  string         // 期望产物路径（工作区相对、正斜杠；程序校验按此对账）
	AnchorPaths []string       // 本次挂上的锚点（工作区相对路径，按 anchor_in 顺序）
}

// SynthArgs 合成单位执行的生成工具入参。
//   - anchors：已解析的锚点池（name → 工作区相对路径；静态配方锚点已做槽位替换，
//     运行期锚点[前置阶段采纳件]由引擎并池）。
//   - attempt：第几次尝试（>1 重摇微抖动；产物路径带 _aN 后缀避免覆盖被否 attempt 的行）。
//   - workdir：用于锚点存在性剔除（已删文件的锚点不塞给生成工具）。
func SynthArgs(recipe *Recipe, item RosterItem, stage *Stage, anchors map[string]string, attempt int, workdir string) (*Synthesis, error) {
	if recipe == nil || stage == nil {
		return nil, fmt.Errorf("synth: recipe/stage required")
	}
	deliv := recipe.EffectiveDeliverable(stage)
	promptText := composePrompt(recipe, item, stage, deliv, attempt)
	outPath := expectedOutputPath(deliv, item, stage.ID, attempt)

	// 锚点按 anchor_in 声明顺序装配；多锚点工具 v1 只吃第一张（od_image_generate.reference 单张）。
	var anchorPaths []string
	seen := map[string]bool{}
	for _, name := range stage.AnchorIn {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		p := strings.TrimSpace(anchors[name])
		if p == "" {
			continue
		}
		if workdir != "" {
			if _, err := os.Stat(joinWorkdir(workdir, p)); err != nil {
				continue // 锚点文件缺失（外部删除/模板未命中）静默剔除
			}
		}
		seen[name] = true
		anchorPaths = append(anchorPaths, p)
	}

	args := map[string]any{
		"prompt":  promptText,
		"save_as": outPath,
	}
	isVideo := strings.Contains(strings.ToLower(recipe.Tool), "video")
	if isVideo {
		if len(anchorPaths) > 0 {
			args["first_frame"] = anchorPaths[0]
		}
	} else {
		if len(anchorPaths) > 0 {
			args["reference"] = anchorPaths[0]
		}
		// remove_bg 默认 true（工具侧）；契约显式声明不需要透明通道时才关。
		if deliv.Alpha != nil && !*deliv.Alpha {
			args["remove_bg"] = false
		}
	}
	if aspect := aspectFor(deliv); aspect != "" {
		args["aspect"] = aspect
	}
	return &Synthesis{Tool: recipe.Tool, Args: args, PromptText: promptText, OutputPath: outPath, AnchorPaths: anchorPaths}, nil
}

// composePrompt 合成提示词：阶段指令 + 对象槽位 + 不变量 + 交付形态约束（+重摇抖动）。
func composePrompt(recipe *Recipe, item RosterItem, stage *Stage, deliv Deliverable, attempt int) string {
	var b strings.Builder
	// 1. 阶段指令（配方可覆写；缺省给通用指令）。
	inst := strings.TrimSpace(stage.Instruction)
	if inst == "" {
		inst = fmt.Sprintf("为下面这个对象产出一版【%s】阶段的素材。", stage.ID)
	}
	b.WriteString(inst)
	b.WriteString("\n\n")
	// 2. 对象与逐件槽位（name 在前，其余按配方 slots 声明顺序）。
	name := strings.TrimSpace(item.Name)
	if name == "" {
		name = item.ID
	}
	b.WriteString("对象：" + name)
	for _, s := range recipe.Slots {
		if s == "id" || s == "name" {
			continue
		}
		if v := strings.TrimSpace(item.Slot(s)); v != "" {
			b.WriteString("\n- " + s + "：" + v)
		}
	}
	// 3. 不变量（逐件相同的不变段）。
	if inv := strings.TrimSpace(recipe.Invariants); inv != "" {
		b.WriteString("\n\n【不可变规范】\n" + inv)
	}
	// 4. 交付形态约束（sprite_sheet 给网格口径，程序校验按 cell×frames 判整图尺寸）。
	if deliv.Type == "sprite_sheet" && deliv.Frames > 0 {
		grid := fmt.Sprintf("%d 帧等分画布、按播放顺序从左到右排布", deliv.Frames)
		if deliv.Cell != "" {
			grid += fmt.Sprintf("；每帧格子 %s 像素", deliv.Cell)
		}
		grid += fmt.Sprintf("（推荐 %d列×1行 条带或 %d 的因数网格）", deliv.Frames, deliv.Frames)
		b.WriteString("\n\n【交付形态】单张精灵表（sprite sheet）：" + grid + "。")
	}
	// 5. 重摇微抖动（seed_policy=reroll_jitter；上一版未过校验，换表达不等同改标准）。
	if attempt > 1 {
		b.WriteString(fmt.Sprintf("\n\n【重摇】上一版未通过校验，这是第 %d 次尝试：请保持全部约束不变，", attempt))
		b.WriteString("只更换构图/姿态细节或整体重画，避免重复上一版的缺陷。")
	}
	return b.String()
}

// defaultFormat 交付格式默认：图 png / 视频 mp4。
func defaultFormat(dtype string) string {
	if dtype == "video" {
		return "mp4"
	}
	return "png"
}

// expectedOutputPath 按交付路径模板推期望产物路径（工作区相对）。
// 模板以 "/" 结尾（或为空）视为目录：拼 <item>_<stage>.<ext>；重摇 attempt≥2 加 _aN 后缀，
// 避免物理覆盖被否 attempt 的产物（底账 append-only 纪律）。
func expectedOutputPath(deliv Deliverable, item RosterItem, stageID string, attempt int) string {
	ext := strings.TrimPrefix(strings.TrimSpace(deliv.Format), ".")
	if ext == "" {
		ext = defaultFormat(deliv.Type)
	}
	base := item.ID + "_" + stageID
	if attempt > 1 {
		base += fmt.Sprintf("_a%d", attempt)
	}
	filename := base + "." + ext
	tpl := strings.Trim(strings.TrimSpace(expandTemplate(deliv.Path, item, stageID)), "/")
	if tpl == "" {
		return filename
	}
	last := tpl
	if idx := strings.LastIndex(tpl, "/"); idx >= 0 {
		last = tpl[idx+1:]
	}
	if strings.Contains(last, ".") && !strings.HasSuffix(deliv.Path, "/") {
		// 模板直接给了文件名：原样使用（重摇也只覆盖自己该写的那张由模板决定的名字）。
		return tpl
	}
	return path.Join(tpl, filename)
}

// aspectFor 从交付尺寸推生成画幅参数（仅命中工具支持的四挡时给出，否则交给模型默认）。
func aspectFor(deliv Deliverable) string {
	w, h, err := parseCell(deliv.Cell)
	if err != nil || w <= 0 || h <= 0 {
		return ""
	}
	if deliv.Type == "sprite_sheet" && deliv.Frames > 1 {
		w = w * deliv.Frames // 单行条带假设
	}
	ratio := float64(w) / float64(h)
	switch {
	case ratio > 0.9 && ratio < 1.11:
		return "1:1"
	case ratio > 1.7 && ratio < 1.85:
		return "16:9"
	case ratio > 0.54 && ratio < 0.59:
		return "9:16"
	case ratio > 1.28 && ratio < 1.39:
		return "4:3"
	case ratio > 0.72 && ratio < 0.78:
		return "3:4"
	}
	return ""
}

// parseCell 解析 "WxH" 单元尺寸。
func parseCell(cell string) (int, int, error) {
	cell = strings.TrimSpace(strings.ToLower(cell))
	if cell == "" {
		return 0, 0, fmt.Errorf("cell 未声明")
	}
	parts := strings.SplitN(strings.ReplaceAll(cell, "×", "x"), "x", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("cell 形状应为 WxH: %q", cell)
	}
	var w, h int
	if _, err := fmt.Sscanf(strings.TrimSpace(parts[0]), "%d", &w); err != nil {
		return 0, 0, fmt.Errorf("cell 宽非法: %q", cell)
	}
	if _, err := fmt.Sscanf(strings.TrimSpace(parts[1]), "%d", &h); err != nil {
		return 0, 0, fmt.Errorf("cell 高非法: %q", cell)
	}
	return w, h, nil
}

// joinWorkdir 拼工作区绝对路径（相对路径进 workdir；绝对路径原样）。
func joinWorkdir(workdir, rel string) string {
	rel = strings.ReplaceAll(rel, "\\", "/")
	if strings.HasPrefix(rel, "/") || (len(rel) > 1 && rel[1] == ':') {
		return rel
	}
	return workdir + string(os.PathSeparator) + strings.ReplaceAll(rel, "/", string(os.PathSeparator))
}
