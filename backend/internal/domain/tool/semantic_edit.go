package tool

// semantic_edit.go 语义 PatchPlan 的生产接线（Harness 根治计划 WP4 §8.3/§8.4）。
//
// 2026-11-19 验收提出的缺陷是"worker 就绪但没有进入任何生产执行路径"——EditFile 仍走
// 字面字符串替换，`semantic_edit_mode` 形同虚设。这里把它接上：
//
//	EditFile（带 semantic_* 参数）
//	  → worker 定位唯一目标（AST/CSSOM 级，0/多个目标一律拒绝）
//	  → 指纹校验（目标内容没变）
//	  → 生成单条 TextEdit 并应用（未修改区域逐字节不变）
//	  → 写入前仍经 Action 管线（授权 + 检查点 + 真实 diff + 回滚）
//
// 模式语义（agent.semantic_edit_mode）：
//   - off     ：完全不介入（与改造前逐字节一致，默认）；
//   - shadow  ：照旧执行旧的字面替换，但额外跑一遍 worker 并把"语义定位结果/偏差"
//               记录到结果输出（对拍用，不改变写入内容）；
//   - enforce ：必须拿到唯一语义目标并应用 TextEdit；worker 不可用、目标歧义、
//               指纹过期一律 fail closed（返回错误、不写文件）。
//
// 严格边界：
//   - worker 不写文件，写盘只发生在 Go 侧；
//   - 只接受单条 TextEdit（批量替换被 worker 与 Go 侧双重拒绝）；
//   - 不把 worker 失败静默降级为正则盲写（enforce 下直接失败）。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/blockmemory/agent/backend/internal/domain/edit"
)

// 语义编辑模式（与 config.HarnessMode* 同值；tool 包不依赖 config）。
const (
	SemanticEditOff     = "off"
	SemanticEditShadow  = "shadow"
	SemanticEditEnforce = "enforce"
)

// normalizeSemanticEditMode 归一化：非法值回落 off（最保守）。
func normalizeSemanticEditMode(m string) string {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case SemanticEditShadow:
		return SemanticEditShadow
	case SemanticEditEnforce:
		return SemanticEditEnforce
	default:
		return SemanticEditOff
	}
}

// literalNote 在 enforce 的受控字面通道下给结果加一句可审计标注（非受控返回空串）。
func literalNote(trusted bool, rationale string) string {
	if !trusted {
		return ""
	}
	return "；受控字面通道（enforce 下显式声明无法语义定位）：" + rationale
}

// SemanticEditConfig EditFile 语义编辑接线参数（bootstrap 从 cfg.Agent 派生注入）。
type SemanticEditConfig struct {
	Mode   string
	Worker *edit.Worker
}

// semanticEditRequest 从 EditFile 参数解析出的语义目标（空=未要求语义编辑）。
type semanticEditRequest struct {
	Kind           string // css_declaration | template_node
	Selector       string // css_declaration
	Property       string // css_declaration
	ExpectedValue  string // css_declaration（可选指纹）
	Tag            string // template_node
	Anchor         string // template_node
	Text           string // template_node
	NewValue       string // 覆盖 old_string/new_string 的替换值
	ExpectedHash   string // 目标文件内容指纹（可选，防读旧版）
	ExpectedMatchN int    // 必须为 1（显式声明批量替换时拒绝）
	// LiteralRationale 受控字面通道的声明理由（enforce 且未声明 semantic_kind 时必填）。
	// 不是「允许绕过」开关：它只让本次字面编辑可审计，仍受合同写范围/检查点/回滚约束。
	LiteralRationale string
}

// parseSemanticEditRequest 解析 EditFile 的 semantic_* 参数。
// 未声明 Kind 视为"不要求语义编辑"，返回 ok=false（完全走旧路径）。
func parseSemanticEditRequest(args map[string]any) (semanticEditRequest, bool) {
	kind := strings.TrimSpace(stringArg(args, "semantic_kind"))
	if kind == "" {
		return semanticEditRequest{}, false
	}
	req := semanticEditRequest{
		Kind:          kind,
		Selector:      strings.TrimSpace(stringArg(args, "semantic_selector")),
		Property:      strings.TrimSpace(stringArg(args, "semantic_property")),
		ExpectedValue: strings.TrimSpace(stringArg(args, "semantic_expected_value")),
		Tag:           strings.TrimSpace(stringArg(args, "semantic_tag")),
		Anchor:        strings.TrimSpace(stringArg(args, "semantic_anchor")),
		Text:          strings.TrimSpace(stringArg(args, "semantic_text")),
		NewValue:      strings.TrimSpace(stringArg(args, "semantic_new_value")),
		ExpectedHash:  strings.TrimSpace(stringArg(args, "semantic_expected_hash")),
		// LiteralRationale 受控字面通道的声明理由（enforce 下未声明 semantic_kind 时必填）。
		LiteralRationale: strings.TrimSpace(stringArg(args, "semantic_literal_rationale")),
	}
	if n, ok := args["semantic_expected_matches"].(float64); ok {
		req.ExpectedMatchN = int(n)
	} else {
		req.ExpectedMatchN = 1
	}
	if req.NewValue == "" {
		req.NewValue = stringArg(args, "new_string")
	}
	return req, true
}

func stringArg(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

// planSemanticEdit 让 worker 定位唯一目标并生成 TextEdit（不写盘）。
func planSemanticEdit(ctx context.Context, w *edit.Worker, file string, src []byte,
	req semanticEditRequest) (*edit.EditPlan, error) {
	if w == nil || !w.Configured() {
		return nil, &edit.WorkerError{Code: -32099, Message: "semantic_worker_not_configured"}
	}
	if req.ExpectedMatchN != 1 {
		return nil, &edit.WorkerError{Code: -32004,
			Message: fmt.Sprintf("expected_matches must be 1 (got %d); bulk replace is forbidden", req.ExpectedMatchN)}
	}
	source := string(src)
	switch req.Kind {
	case "css_declaration":
		if req.Selector == "" || req.Property == "" {
			return nil, &edit.WorkerError{Code: -32602, Message: "semantic_selector and semantic_property required"}
		}
		return w.PlanCSSEdit(ctx, edit.CSSDeclarationParams{
			File: file, Source: source, Selector: req.Selector, Property: req.Property,
			ExpectedValue: req.ExpectedValue, NewValue: req.NewValue,
		})
	case "template_node":
		if req.Tag == "" || (req.Anchor == "" && req.Text == "") {
			return nil, &edit.WorkerError{Code: -32602,
				Message: "semantic_tag plus semantic_anchor or semantic_text required"}
		}
		params := map[string]any{
			"file": file, "source": source, "tag": req.Tag,
			"anchor": req.Anchor, "text": req.Text, "new_text": req.NewValue,
			"expected_matches": 1,
		}
		raw, err := w.Call(ctx, "plan_template_edit", params)
		if err != nil {
			return nil, err
		}
		var plan edit.EditPlan
		if err := json.Unmarshal(raw, &plan); err != nil {
			return nil, &edit.WorkerError{Code: -32099, Message: "decode plan: " + err.Error()}
		}
		if len(plan.Edits) != 1 {
			return nil, &edit.WorkerError{Code: -32050,
				Message: fmt.Sprintf("expected exactly 1 edit, got %d", len(plan.Edits))}
		}
		return &plan, nil
	default:
		return nil, &edit.WorkerError{Code: -32602, Message: "unsupported semantic_kind: " + req.Kind}
	}
}

// applySemanticEdit 走完“定位 → 应用 → 复验”并返回新内容。
//
// 复验语义（I-EDIT-2，2026-11-19 再次验收 P1-1 重写）：
//   - oracle = **新目标 + 新值**，而不是“旧目标仍能定位”：
//     把 ExpectedValue 换成 NewValue 重新定位，必须**唯一命中**；命中不了就是复验失败。
//   - 未修改区域必须逐字节保留：用 plan 的字节范围把 src 切成“前/编/后”三段，
//     out 必须恰好是 `前 + NewText + 后`，否则视为写错位置。
//   - **任何复验错误一律 fail closed**（worker 崩溃/解析失败/目标消失/歧义/新值未命中）：
//     旧实现把所有复验错误当成功返回，等于没有复验。
func applySemanticEdit(ctx context.Context, w *edit.Worker, file string, src []byte,
	req semanticEditRequest, plan *edit.EditPlan) ([]byte, error) {
	if req.ExpectedHash != "" && plan.ContentHash != "" && req.ExpectedHash != plan.ContentHash {
		return nil, &edit.WorkerError{Code: -32007,
			Message: fmt.Sprintf("content_hash_mismatch: expected %s, actual %s", req.ExpectedHash, plan.ContentHash)}
	}
	start, end, err := edit.ByteRangeOfEdit(src, plan)
	if err != nil {
		return nil, err
	}
	out, err := edit.ApplyUTF16Edits(src, plan)
	if err != nil {
		return nil, err
	}
	// 1) 未修改区域校验：out 必须严格等于 前 + NewText + 后。
	if int(end) > len(src) {
		return nil, &edit.WorkerError{Code: -32051, Message: "edit range exceeds source length"}
	}
	want := make([]byte, 0, len(src)-int(end-start)+len(plan.Edits[0].NewText))
	want = append(want, src[:start]...)
	want = append(want, plan.Edits[0].NewText...)
	want = append(want, src[end:]...)
	if string(want) != string(out) {
		return nil, &edit.WorkerError{Code: -32052,
			Message: "post_edit_verification_failed: untouched regions changed (edit applied outside the planned range)"}
	}
	// 2) 新目标/新值 oracle：应用后必须能按**新值**唯一定位到同一属性。
	if err := verifySemanticEdit(ctx, w, file, out, req); err != nil {
		return nil, err
	}
	return out, nil
}

// verifySemanticEdit 用“新目标 + 新值”做复验（任何错误均 fail closed）。
//
// 旧目标指纹（ExpectedValue）已被替换掉，因此 oracle 必须以新值正向定位：
// css_declaration → 同 selector/property 且期望值为 NewValue；
// template_node  → 同 tag/anchor 且文本包含新值。
func verifySemanticEdit(ctx context.Context, w *edit.Worker, file string, out []byte,
	req semanticEditRequest) error {
	newValue := strings.TrimSpace(req.NewValue)
	if newValue == "" {
		return &edit.WorkerError{Code: -32052,
			Message: "post_edit_verification_failed: new value is empty, cannot prove the result"}
	}
	verify := req
	verify.ExpectedValue = newValue
	verify.NewValue = newValue // 只用于定位，不会再写盘
	if verify.Kind == "template_node" {
		// 模板节点复验：整节点替换后锚点可能已不存在，改用新文本作为定位依据。
		verify.Anchor = ""
		verify.Text = newValue
		verify.NewValue = newValue
	}
	verifyPlan, err := planSemanticEdit(ctx, w, file, out, verify)
	if err != nil {
		return &edit.WorkerError{Code: -32052,
			Message: "post_edit_verification_failed（新值未能唯一定位）: " + err.Error()}
	}
	if verifyPlan == nil || len(verifyPlan.Edits) != 1 {
		return &edit.WorkerError{Code: -32052,
			Message: "post_edit_verification_failed: expected exactly 1 match for the new value"}
	}
	return nil
}

// semanticEditReplace 执行语义编辑接线（mode != off 时由 editFile 调用）。
//
// 返回：
//   - (result, note, nil)：enforce 下已成功应用并写盘 → result 非空；
//     shadow 下只做"影子定位" → result 为空、note 说明判定结果；
//   - (nil, "", err)：enforce 下失败（调用方必须 fail closed，不回退字面替换）。
func (e *Executor) semanticEditReplace(ctx context.Context, absPath, displayPath string,
	data []byte, req semanticEditRequest, mode string) (*Result, string, error) {
	cfg := e.semanticEditConfig()
	if cfg.Worker == nil || !cfg.Worker.Configured() {
		if mode == SemanticEditEnforce {
			return nil, "", &edit.WorkerError{Code: -32099,
				Message: "semantic_worker_not_configured（enforce 下不允许静默回退字面替换）"}
		}
		return nil, "；语义定位（shadow）：worker 未配置，未参与", nil
	}
	plan, err := planSemanticEdit(ctx, cfg.Worker, displayPath, data, req)
	if err != nil {
		return nil, "", err
	}
	if req.ExpectedHash != "" && plan.ContentHash != "" && req.ExpectedHash != plan.ContentHash {
		return nil, "", &edit.WorkerError{Code: -32007,
			Message: fmt.Sprintf("content_hash_mismatch: expected %s, actual %s",
				shortHashText(req.ExpectedHash), shortHashText(plan.ContentHash))}
	}
	if mode != SemanticEditEnforce {
		// shadow：只报告"如果 enforce 会改成什么范围"，绝不改动写入内容。
		ed := plan.Edits[0]
		return nil, fmt.Sprintf("；语义定位（shadow）：命中 %s [%d,%d) 可由语义编辑精确替换（未启用）",
			req.Kind, ed.Start, ed.End), nil
	}
	out, err := applySemanticEdit(ctx, cfg.Worker, displayPath, data, req, plan)
	if err != nil {
		return nil, "", err
	}
	// 与旧路径一致的行尾纪律：原文以 CRLF 为主时按 CRLF 写回（语义编辑只替换
	// 目标片段，未修改区域逐字节保留，因此这里不做行尾归一化）。
	if err := os.WriteFile(absPath, out, 0o644); err != nil {
		return nil, "", err
	}
	ed := plan.Edits[0]
	return &Result{
		Tool:    "EditFile",
		Success: true,
		Path:    absPath,
		Output: fmt.Sprintf("semantic edit applied (%s): replaced [%d,%d) (%d -> %d bytes), 未修改区域逐字节保留",
			req.Kind, ed.Start, ed.End, len(data), len(out)),
	}, "", nil
}

// shortHashText 摘要展示（错误消息不刷屏）。
func shortHashText(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// SetSemanticEdit 注入语义编辑接线（Registry 级入口，bootstrap 装配）。
func (r *Registry) SetSemanticEdit(cfg SemanticEditConfig) {
	if r == nil || r.exec == nil {
		return
	}
	r.exec.SetSemanticEdit(cfg)
}

// SemanticEditMode 返回当前生效模式（测试/管理面只读访问）。
func (r *Registry) SemanticEditMode() string {
	if r == nil || r.exec == nil {
		return SemanticEditOff
	}
	return normalizeSemanticEditMode(r.exec.semanticEditConfig().Mode)
}
