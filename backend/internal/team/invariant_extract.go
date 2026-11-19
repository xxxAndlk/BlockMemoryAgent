package team

// invariant_extract.go 从用户原话确定性提取不变量（WP1 §3.3 第 6 条）。
//
// 为什么需要：事故第 20 轮用户明确"商品编码到商品名称 +30px、商品名称到下单时间 +60px"，
// 第 21 轮只说"修省略"，模型却把三列的 min-width/width/flex 一起改掉，第 22 轮用户
// 才发现间距被破坏。这些约束当时只存在于对话历史里（会被压缩掉），必须落成可机器
// 验证的不变量。
//
// 第一版纪律（计划 §3.3）：**不调用 LLM**，只用确定性、保守规则；识别不到就不产
// 生不变量（宁少不多）——凭空生成错误的"约束"比没有约束更糟。
//
// 已支持句式（中英混排、空白不敏感）：
//   - "给A到B的间距 +30px" / "A 到 B +30px" / "加 30px" / "再 +60 px"
//     → dom_metric 不变量（gap_after，Expected={"delta_px":30}）
//   - "不要动 X" / "X 不要改" / "X 不能改" / "保持 X 不动"
//     → file_hash 不变量（需要 BaselineRef，由调用方在快照后回填；无基线时记 unverified）
//   - "保持之前的距离" / "之前的距离保持不要动"
//     → 继承强化：不产生新项，由 InheritInvariants 的 active 保留逻辑覆盖
//
// 所有生成项都带 SourceEvent + SourceTaskID，便于审计"这条约束是谁在第几轮说的"。

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// pxGapPattern 匹配"A 到 B +30px"类间距指令。
// 依次捕获：左锚点（可空）、右锚点（可空）、数值、单位。
var pxGapPattern = regexp.MustCompile(`([\p{Han}A-Za-z0-9_\-]{0,24})\s*(?:到|至|和|与|->|→)\s*([\p{Han}A-Za-z0-9_\-]{0,24})\s*(?:的)?\s*(?:间距|距离|间隔)?\s*(?:再)?\s*[+＋加]\s*(\d+(?:\.\d+)?)\s*(px|Px|PX|像素)`)

// simpleGapPattern 匹配只有一侧锚点的"加 30px"。
var simpleGapPattern = regexp.MustCompile(`([\p{Han}A-Za-z0-9_\-]{2,24})\s*(?:再)?\s*[+＋加]\s*(\d+(?:\.\d+)?)\s*(px|Px|PX|像素)`)

// noTouchPattern 匹配"不要动 X"/"X 不要改"/"X 不能改"。
var noTouchPattern = regexp.MustCompile(`(?:不要|别|禁止|不准)(?:动|改|修改|碰)\s*([^\s，。；,;、]{1,24})|([^\s，。；,;、]{1,24})\s*(?:不要|不能|不准|别)(?:动|改|修改|碰)`)

// keepPattern 匹配"保持 X 不动"。
var keepPattern = regexp.MustCompile(`保持\s*([^\s，。；,;、]{1,24})\s*(?:不动|不变|原样)`)

// InvariantExtraction 提取结果。
type InvariantExtraction struct {
	Invariants []Invariant
	// Notes 记录"为什么没提取到"（审计/对拍用，不注入模型）。
	Notes []string
}

// ExtractInvariants 从用户输入提取不变量。
//
// targets 是当前已解析的语义目标（可为空）；为空时仍生成不变量，只是 Target.File 留空，
// 由后续 revision 补齐文件——这样"用户说了 +30px"不会因为当时没解析到文件而丢失。
func ExtractInvariants(text string, targets []SemanticTarget, sourceEvent int64, sourceTaskID string, rev int64) InvariantExtraction {
	var res InvariantExtraction
	t := strings.TrimSpace(text)
	if t == "" {
		return res
	}
	seq := 0
	add := func(kind, id, human string, expected any, target SemanticTarget) {
		payload, err := json.Marshal(expected)
		if err != nil {
			return
		}
		seq++
		res.Invariants = append(res.Invariants, Invariant{
			ID:           id,
			Kind:         kind,
			Status:       InvariantActive,
			Verification: kind,
			Target:       target,
			Expected:     payload,
			SourceEvent:  sourceEvent,
			SourceTaskID: sourceTaskID,
			Text:         human,
		})
	}
	// 间距指令：注意同一句里可能有两条（"+30px ... +60px"）。
	for _, m := range pxGapPattern.FindAllStringSubmatch(t, -1) {
		left := strings.TrimSpace(m[1])
		right := strings.TrimSpace(m[2])
		val, err := strconv.ParseFloat(m[3], 64)
		if err != nil {
			continue
		}
		human := fmt.Sprintf("%s→%s 间距 +%gpx（用户明确要求）", orDefault(left, "前一项"), orDefault(right, "后一项"), val)
		target, _ := matchTargetByHint(targets, right)
		id := slugID("gap", right, left, fmt.Sprintf("%g", val))
		add(InvariantKindDOMMetric, id, human, map[string]any{
			"metric":   "gap_after",
			"delta_px": val,
			"left":     left,
			"right":    right,
		}, target)
	}
	// 单侧"加 30px"：只有在没有命中配对句式时才计入（避免重复）。
	if len(res.Invariants) == 0 {
		for _, m := range simpleGapPattern.FindAllStringSubmatch(t, -1) {
			hint := strings.TrimSpace(m[1])
			val, err := strconv.ParseFloat(m[2], 64)
			if err != nil {
				continue
			}
			human := fmt.Sprintf("%s 与前一元素间距 +%gpx（用户明确要求）", hint, val)
			target, _ := matchTargetByHint(targets, hint)
			id := slugID("gap", hint, fmt.Sprintf("%g", val))
			add(InvariantKindDOMMetric, id, human, map[string]any{
				"metric": "gap_after", "delta_px": val, "hint": hint,
			}, target)
		}
	}
	// 禁止修改项 → file_hash 不变量（基线由调用方回填）。
	// 只接受能落到具体文件/语义目标的提示（无法落位的抽象说法不生成不变量，
	// 否则会凭空造出“禁止修改之前那个距离”这类无法验证的冻结项）。
	for _, m := range noTouchPattern.FindAllStringSubmatch(t, -1) {
		hint := strings.TrimSpace(m[1])
		if hint == "" {
			hint = strings.TrimSpace(m[2])
		}
		if hint == "" || isStopWord(hint) {
			continue
		}
		target, exact := matchTargetByHint(targets, hint)
		if !exact {
			res.Notes = append(res.Notes, "跳过无法定位的冻结项："+hint)
			continue
		}
		id := slugID("freeze", hint)
		add(InvariantKindFileHash, id,
			fmt.Sprintf("%s 不得被修改（用户明确要求冻结）", hint),
			map[string]any{"preserve": true, "hint": hint}, target)
	}
	for _, m := range keepPattern.FindAllStringSubmatch(t, -1) {
		hint := strings.TrimSpace(m[1])
		if hint == "" || isStopWord(hint) {
			continue
		}
		target, exact := matchTargetByHint(targets, hint)
		if !exact {
			res.Notes = append(res.Notes, "跳过无法定位的保持项："+hint)
			continue
		}
		id := slugID("keep", hint)
		add(InvariantKindFileHash, id,
			fmt.Sprintf("%s 保持原样（不动）", hint),
			map[string]any{"preserve": true, "hint": hint}, target)
	}
	if len(res.Invariants) == 0 {
		res.Notes = append(res.Notes, "未识别到可机器验证的约束（保守策略：识别不到就不生成）")
	}
	return res
}

// IsCorrection 判定用户输入是否像"纠正/延续"而非全新任务（保守规则）。
// 只用于审计与观测标签，不驱动状态机（状态机由任务终态决定，见 §3.3）。
func IsCorrection(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return false
	}
	for _, p := range []string{"不对", "不是", "改一下", "还是", "继续", "接着", "另外", "还要", "修一下", "保持", "不要动", "别动", "撤销", "重新"} {
		if strings.Contains(t, p) {
			return true
		}
	}
	return false
}

func orDefault(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}

func isStopWord(s string) bool {
	switch s {
	case "动", "改", "它", "这个", "那个", "了", "的":
		return true
	}
	return false
}

// slugID 生成稳定可读的不变量 ID（同一用户表述重复执行得到同一 ID，便于去重）。
func slugID(prefix string, parts ...string) string {
	var b strings.Builder
	b.WriteString(prefix)
	for _, p := range parts {
		p = strings.TrimSpace(strings.ReplaceAll(p, " ", ""))
		if p == "" {
			continue
		}
		b.WriteString("-")
		b.WriteString(p)
	}
	id := b.String()
	if len([]rune(id)) > 48 {
		id = string([]rune(id)[:48])
	}
	return id
}

// matchTargetByHint 按标签/锚点/ID/文件名精确匹配已解析目标。
// 第二个返回值表示"命中的是具体目标"（true）还是"只是回退到唯一文件"（false）。
//
// 为什么要区分：间距类指令可以用回退（用户说的是中文名，落不到英文 anchor 时，
// 挂到唯一文件比丢弃安全），但"不要动 X"/"保持 Y 不动"这类冻结项必须命中具体目标——
// 否则"保持之前的距离不要动"会被误读成"冻结整个文件"，凭空造出一个错误约束。
func matchTargetByHint(targets []SemanticTarget, hint string) (SemanticTarget, bool) {
	hint = strings.TrimSpace(hint)
	if hint == "" || len(targets) == 0 {
		return SemanticTarget{}, false
	}
	for _, t := range targets {
		if strings.Contains(t.ID, hint) || (t.Anchor != "" && strings.Contains(t.Anchor, hint)) ||
			(t.Label != "" && (strings.Contains(t.Label, hint) || strings.Contains(hint, t.Label))) {
			return t, true
		}
	}
	for _, t := range targets {
		if t.File != "" && strings.Contains(filepathBase(t.File), hint) {
			return t, true
		}
	}
	// 回退：所有目标同属一个文件 → 落该文件（保留清单里的语义目标信息能用的部分）。
	var file string
	for _, t := range targets {
		if t.File == "" {
			continue
		}
		if file == "" {
			file = t.File
			continue
		}
		if normalizeRelPath(t.File) != normalizeRelPath(file) {
			return SemanticTarget{}, false
		}
	}
	if file == "" {
		return SemanticTarget{}, false
	}
	return SemanticTarget{File: file, Kind: "dom_metric"}, false
}

// filepathBase 取路径最后一段（不 import path/filepath 以保持零依赖）。
func filepathBase(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
