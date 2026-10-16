package workflow

// freeze.go workflow_freeze 的配方提取流水线（开发文档 §7.1，设计报告 §4.2）：
// 抓本会话轨迹中成功且被采纳的生成调用 → 登记锚点（采纳产物路径）→ 轻量模型参数化
//（把提示词切"逐件变动段/不变段"，产出 slots 草案）→ 生成 YAML 草案。
// 关键约束：不让模型凭空写配方，只切分/参数化已有轨迹——配方 = 实际被确认过的那次做法。
// 草案的过目确认/落盘在 tools.go（ask_user 通道，可编辑）。

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// TraceLoader 是会话轨迹读取器（bootstrap 适配 agent_messages → TraceCall）。
type TraceLoader interface {
	LoadTrace(ctx context.Context, sessionID string) ([]TraceCall, error)
}

// TraceCall 是一次工具调用轨迹（入参 + 结果摘要）。
type TraceCall struct {
	Tool    string         // 工具名（含插件前缀，按注册名）
	Args    map[string]any // 入参
	Output  string         // 结果文本（截断态）
	Success bool           // 结果是否成功
	Seq     int            // 轨迹内序号（升序）
}

// FreezeSpec 是冻结（打样→配方）入参。
type FreezeSpec struct {
	SessionID string
	Workdir   string
	Title     string
	RecipeID  string // 缺省从 title slug
	Overrides string // 可选覆盖段（YAML/JSON，人工修正 deliverable/anchors/invariants/judge 等）
}

// FreezeInfo 是草案生成摘要。
type FreezeInfo struct {
	RecipeID    string   `json:"recipe_id"`
	Title       string   `json:"title"`
	Tool        string   `json:"tool"`
	Stages      []string `json:"stages"`
	Anchors     []string `json:"anchors"`
	SlotCount   int      `json:"slot_count"`
	SourceCalls int      `json:"source_calls"` // 轨迹里纳入的生成调用数
	Notes       []string `json:"notes,omitempty"`
}

// isGenerateTool 判定是否为生成类工具（图片/视频生成；吸取可复用的产物）。
func isGenerateTool(name string) bool {
	low := strings.ToLower(name)
	return strings.Contains(low, "generate") && (strings.Contains(low, "image") || strings.Contains(low, "video") || strings.Contains(low, "od_"))
}

// FreezeDraft 从会话轨迹生成配方 YAML 草案（未落盘）。
func (e *Engine) FreezeDraft(ctx context.Context, spec FreezeSpec) (string, *FreezeInfo, error) {
	if e.trace == nil {
		return "", nil, fmt.Errorf("轨迹读取器未接线（bootstrap 需注入 TraceLoader）")
	}
	if strings.TrimSpace(spec.Workdir) == "" {
		return "", nil, fmt.Errorf("workdir 缺失（会话工作目录）")
	}
	trace, err := e.trace.LoadTrace(ctx, spec.SessionID)
	if err != nil {
		return "", nil, fmt.Errorf("读取会话轨迹失败: %w", err)
	}
	var calls []TraceCall
	for _, c := range trace {
		if !isGenerateTool(c.Tool) || !c.Success {
			continue
		}
		calls = append(calls, c)
	}
	if len(calls) == 0 {
		return "", nil, fmt.Errorf("本会话轨迹里没有成功的生成调用可提取——请先打样（用生成工具产出并确认一版），再冻结为标准")
	}
	// 取最近 ≤8 次（原型 = 最后一次）。
	if len(calls) > 8 {
		calls = calls[len(calls)-8:]
	}
	prototype := calls[len(calls)-1]
	info := &FreezeInfo{
		Title:       strings.TrimSpace(spec.Title),
		Tool:        prototype.Tool,
		SourceCalls: len(calls),
	}
	info.RecipeID = strings.TrimSpace(spec.RecipeID)
	if info.RecipeID == "" {
		info.RecipeID = slugifyID(spec.Title)
	}
	if info.RecipeID == "" {
		info.RecipeID = "workflow_recipe"
	}

	// 轻量模型参数化（失败回退确定性骨架）。
	param := e.parameterizeTrace(ctx, calls)
	if param == nil {
		info.Notes = append(info.Notes, "参数化模型调用失败，已回退确定性骨架（不变量/槽位需人工补）")
		param = &traceParam{}
	}

	draft := &Recipe{
		ID:      info.RecipeID,
		Version: 0, // SaveRecipeYAML 落盘时定版（新文件 1 / 已存在 +1）
		Title:   orDefault(param.Title, info.Title),
		Tool:    prototype.Tool,
		Deliverable: Deliverable{
			Type:   orDefault(param.DeliverableType, "image"),
			Frames: param.Frames,
			Cell:   param.Cell,
			Format: inferFormat(prototype),
			Path:   inferPathTemplate(prototype),
		},
		Invariants: param.Invariants,
		Slots:      param.Slots,
		Check:      CheckSpec{Judge: param.Judge, Program: []string{"exists", "path_template", "format"}},
		Params:     Params{Reroll: 2, SeedPolicy: "reroll_jitter"},
	}
	if draft.Deliverable.Format == "png" {
		alpha := true
		draft.Deliverable.Alpha = &alpha
	}
	if len(draft.Slots) == 0 {
		draft.Slots = []string{"id", "name", "element", "family"}
	}
	for _, st := range param.Stages {
		if strings.TrimSpace(st.ID) == "" {
			continue
		}
		draft.Stages = append(draft.Stages, &Stage{ID: strings.TrimSpace(st.ID), Instruction: strings.TrimSpace(st.Instruction)})
	}
	if len(draft.Stages) == 0 {
		draft.Stages = []*Stage{{ID: "main", Instruction: strings.TrimSpace(param.Invariants)}}
		info.Notes = append(info.Notes, "未识别出阶段划分，已生成单阶段 main（可人工在 YAML 中拆分）")
	}
	// 锚点：模型提议 + 轨迹兜底（首个产物=style，原型产物=concept）。
	anchors := map[string]string{}
	for name, p := range param.Anchors {
		if strings.TrimSpace(name) != "" && strings.TrimSpace(p) != "" {
			anchors[strings.TrimSpace(name)] = strings.TrimSpace(p)
		}
	}
	if len(anchors) == 0 {
		if p := traceOutputPath(calls[0]); p != "" {
			anchors["style"] = p
		}
		if p := traceOutputPath(prototype); p != "" && anchors["style"] != p {
			anchors["concept"] = p
		}
	}
	if len(anchors) > 0 {
		draft.Anchors = anchors
	}
	// 轨迹里显式 reference 参数也是锚点候选（打样时锁一致性用的参考图）。
	for _, c := range calls {
		if ref, ok := c.Args["reference"].(string); ok && strings.TrimSpace(ref) != "" {
			if _, exists := draft.Anchors["style"]; !exists {
				draft.Anchors["style"] = strings.TrimSpace(ref)
			}
			break
		}
	}

	yamlBytes, err := MarshalRecipe(draft)
	if err != nil {
		return "", nil, err
	}
	yamlText := string(yamlBytes)
	if strings.TrimSpace(spec.Overrides) != "" {
		merged, err := mergeYAMLOverrides(yamlText, spec.Overrides)
		if err != nil {
			return "", nil, fmt.Errorf("覆盖段合并失败: %w", err)
		}
		yamlText = merged
	}
	// 最终校验（草案必须是可落盘配方）。
	if _, err := ParseRecipe([]byte(yamlText)); err != nil {
		return "", nil, fmt.Errorf("草案校验失败（可加 overrides 修正）: %w", err)
	}
	for _, st := range draft.Stages {
		info.Stages = append(info.Stages, st.ID)
	}
	for name := range draft.Anchors {
		info.Anchors = append(info.Anchors, name)
	}
	info.SlotCount = len(draft.Slots)
	return yamlText, info, nil
}

// FeedbackDraft 聚合本 run 全部驳回/失败原因 → 轻量模型生成配方 v+1 草案（YAML 文本，未落盘；
// 落盘走 SaveRecipeYAML，version 落盘侧 +1）。设计报告 §4.3 的配方演化回路。
func (e *Engine) FeedbackDraft(ctx context.Context, runID string) (string, error) {
	run, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return "", err
	}
	units, err := e.store.ListUnits(ctx, runID)
	if err != nil {
		return "", err
	}
	var reasons []string
	seen := map[string]bool{}
	for _, u := range latestCells(units) {
		r := strings.TrimSpace(u.RejectReason)
		if r == "" || (u.Status != UnitStatusFailed && u.Status != UnitStatusNeedsReview) {
			continue
		}
		line := fmt.Sprintf("- [%s/%s] %s", u.ItemID, u.Stage, truncateStr(r, 200))
		if !seen[line] {
			seen[line] = true
			reasons = append(reasons, line)
		}
		if len(reasons) >= 30 {
			break
		}
	}
	if len(reasons) == 0 {
		return "", fmt.Errorf("run %s 没有驳回/失败记录，无需回灌", runID)
	}
	if e.models == nil {
		return "", fmt.Errorf("轻量模型未接线，无法生成配方草案")
	}
	var b strings.Builder
	b.WriteString("下面是量产审查中收集到的驳回/失败原因，以及当前配方全文。请把驳回原因回灌为配方的修订版：\n")
	b.WriteString("- 只改与驳回原因直接相关的部分（补充不变量/负向约束、细化阶段指令、调整判定清单）；\n")
	b.WriteString("- 不要大改整体结构；保持 id 不变（version 号由系统落盘时递增）；\n")
	b.WriteString("- 只输出完整配方 YAML 原文（不要代码围栏、不要解释）。\n\n【驳回/失败原因】\n")
	b.WriteString(strings.Join(reasons, "\n"))
	b.WriteString("\n\n【当前配方】\n")
	b.WriteString(run.RecipeYAML)
	resp, err := e.models.CallLightweightWithRetry(ctx, b.String())
	if err != nil {
		return "", fmt.Errorf("生成配方草案失败: %w", err)
	}
	yamlText := strings.TrimSpace(resp)
	yamlText = strings.TrimPrefix(yamlText, "```yaml")
	yamlText = strings.TrimPrefix(yamlText, "```")
	yamlText = strings.TrimSuffix(yamlText, "```")
	yamlText = strings.TrimSpace(yamlText)
	if _, err := ParseRecipe([]byte(yamlText)); err != nil {
		return "", fmt.Errorf("草案未通过校验（可人工修正后保存）: %w", err)
	}
	return yamlText, nil
}

// traceParam 是参数化模型的 JSON 产出。
type traceParam struct {
	Title      string   `json:"title"`
	Invariants string   `json:"invariants"`
	Slots      []string `json:"slots"`
	Stages     []struct {
		ID          string `json:"id"`
		Instruction string `json:"instruction"`
	} `json:"stages"`
	Anchors         map[string]string `json:"anchors"`
	Judge           []string          `json:"judge"`
	DeliverableType string            `json:"deliverable_type"`
	Frames          int               `json:"frames"`
	Cell            string            `json:"cell"`
}

// parameterizeTrace 调轻量模型把轨迹切成"逐件变化/不变"两部分并提阶段与锚点。
func (e *Engine) parameterizeTrace(ctx context.Context, calls []TraceCall) *traceParam {
	if e.models == nil {
		return nil
	}
	var b strings.Builder
	b.WriteString("你是量产配方（recipe）参数化助手。下面是同一会话里一组成功生成调用（打样轨迹），")
	b.WriteString("请把它参数化为可复用的量产配方：逐件会变的内容抽成变量槽（slots），逐件不变的内容成为不变量（invariants），识别出阶段（stages）与锚点（anchors）。\n\n【打样轨迹】\n")
	for i, c := range calls {
		fmt.Fprintf(&b, "— 第 %d 次：工具 %s\n", i+1, c.Tool)
		if p, ok := c.Args["prompt"].(string); ok && p != "" {
			fmt.Fprintf(&b, "提示词：%s\n", truncateStr(p, 1500))
		}
		if ref, ok := c.Args["reference"].(string); ok && ref != "" {
			fmt.Fprintf(&b, "参考图：%s\n", ref)
		}
		if out := traceOutputPath(c); out != "" {
			fmt.Fprintf(&b, "产物：%s\n", out)
		}
	}
	b.WriteString(`
【要求】
- 只做切分与参数化，不要凭空发明轨迹里没有的约束；不变量写轨迹里反复出现、逐件保持一致的美术规范。
- stages：从提示词差异识别阶段（如 concept/walk/attack/vfx），id 用英文短名，instruction 逐字或忠实改写该阶段的指令。
- anchors：锁一致性的参考图，取轨迹里真实出现过的路径（产物路径或参考图路径），命名如 style/concept/family。
- slots：变量槽字段名（英文小写，如 id/name/element/family），不要包含 stage。
- judge：可判可核对的产物一致性条目（2~5 条）。
只输出 JSON，不要其他文字：
{"title":"...","invariants":"...","slots":["id","name"],"stages":[{"id":"concept","instruction":"..."}],"anchors":{"style":"路径"},"judge":["..."],"deliverable_type":"image|sprite_sheet|video","frames":0,"cell":""}`)
	resp, err := e.models.CallLightweightWithRetry(ctx, b.String())
	if err != nil {
		return nil
	}
	cleaned := strings.TrimSpace(resp)
	cleaned = strings.TrimPrefix(cleaned, "```json")
	cleaned = strings.TrimPrefix(cleaned, "```")
	cleaned = strings.TrimSuffix(cleaned, "```")
	start := strings.Index(cleaned, "{")
	end := strings.LastIndex(cleaned, "}")
	if start < 0 || end <= start {
		return nil
	}
	var p traceParam
	if err := json.Unmarshal([]byte(cleaned[start:end+1]), &p); err != nil {
		return nil
	}
	return &p
}

// traceOutputPath 从轨迹调用的结果/入参推产物路径。
func traceOutputPath(c TraceCall) string {
	if out := strings.TrimSpace(c.Output); out != "" {
		start := strings.Index(out, "{")
		end := strings.LastIndex(out, "}")
		if start >= 0 && end > start {
			var payload map[string]any
			if err := json.Unmarshal([]byte(out[start:end+1]), &payload); err == nil {
				if p, ok := payload["path"].(string); ok && strings.TrimSpace(p) != "" {
					return normalizeRelPath(p)
				}
			}
		}
	}
	if p, ok := c.Args["save_as"].(string); ok && strings.TrimSpace(p) != "" {
		return normalizeRelPath(p)
	}
	return ""
}

// inferFormat 从原型调用推交付格式（save_as 扩展名 / 产物扩展名）。
func inferFormat(c TraceCall) string {
	if p := traceOutputPath(c); p != "" {
		if ext := strings.TrimPrefix(strings.ToLower(path.Ext(p)), "."); ext != "" {
			return ext
		}
	}
	if strings.Contains(strings.ToLower(c.Tool), "video") {
		return "mp4"
	}
	return "png"
}

// inferPathTemplate 从原型产物的目录推路径模板（<dir>/{{id}}_{{stage}}.ext 由引擎拼）。
func inferPathTemplate(c TraceCall) string {
	p := traceOutputPath(c)
	if p == "" {
		return ""
	}
	dir := path.Dir(p)
	if dir == "." || dir == "" {
		return ""
	}
	return dir + "/"
}

// slugifyID 从标题生成配方 id（保留字母数字下划线连字符；中文标题产出空则调用方兜底）。
func slugifyID(title string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(title)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '_' || r == '-' || r == ' ':
			b.WriteRune('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	return out
}

// mergeYAMLOverrides 把覆盖段（YAML/JSON 顶层键）合并进草案 YAML。
func mergeYAMLOverrides(base, overrides string) (string, error) {
	var baseMap map[string]any
	if err := yaml.Unmarshal([]byte(base), &baseMap); err != nil {
		return "", err
	}
	var ovMap map[string]any
	if err := yaml.Unmarshal([]byte(overrides), &ovMap); err != nil {
		// 尝试 JSON。
		if err2 := json.Unmarshal([]byte(overrides), &ovMap); err2 != nil {
			return "", err
		}
	}
	for k, v := range ovMap {
		if k == "id" {
			continue // id 不因覆盖而漂移
		}
		baseMap[k] = v
	}
	var sb strings.Builder
	enc := yaml.NewEncoder(&sb)
	enc.SetIndent(2)
	if err := enc.Encode(baseMap); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	return sb.String(), nil
}

func orDefault(v, fallback string) string {
	if strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}

// looksLikeRecipeYAML 判断用户答复是否是手改的配方 YAML（freeze 确认卡的可编辑通道）。
func looksLikeRecipeYAML(s string) bool {
	t := strings.TrimSpace(s)
	if len(t) < 20 {
		return false
	}
	hasID := strings.Contains(t, "id:") || strings.Contains(t, "\"id\"")
	hasKey := strings.Contains(t, "deliverable") || strings.Contains(t, "stages") || strings.Contains(t, "tool:")
	return hasID && hasKey
}
