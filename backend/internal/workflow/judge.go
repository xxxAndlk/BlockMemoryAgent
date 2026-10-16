package workflow

// judge.go 模型评委（便宜，开发文档 §6.2）：复用 ModelFactory.CallLightweightWithRetry +
// evolver 的 ```json 围栏解析套路，按 check.judge 逐条判定，输出 {items:[{item,pass,reason}]}。
//
// v1 限制（如实记录）：轻量调用通道是纯文本（model.LLMClient.Generate(prompt)），评委
// 看不了像素——提示词给出产物与锚点的工作区路径、生成提示词原文与交付契约，评委据此判
// "清单与提示词/契约是否自洽、要求是否被明确覆盖"；纯视觉品味类判定按设计归人审联系表
//（§6.3），视觉评委（抽帧喂多模态）留 M3。判不了就标 needs_review 由人审，不静默放行。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// LightModel 是评委与配方参数化所需的轻量模型调用能力（*model.ModelFactory 满足）。
type LightModel interface {
	CallLightweightWithRetry(ctx context.Context, prompt string) (string, error)
}

// Embedder 是漂移检测所需的向量化能力（embed.Embedder 满足）。
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	Dim() int
}

// JudgeItem 是评委对清单一条的判定。
type JudgeItem struct {
	Item   string `json:"item"`
	Pass   bool   `json:"pass"`
	Reason string `json:"reason,omitempty"`
}

// JudgeResult 是评委的聚合结果（Pass = 全部 pass；Err 非空表示评委不可用）。
type JudgeResult struct {
	Items    []JudgeItem `json:"items"`
	Pass     bool        `json:"pass"`
	Raw      string      `json:"-"`                  // 原始响应（排查用，不落库）
	Err      string      `json:"err,omitempty"`      // 调用/解析失败原因（评委不可用 → 人审）
	Delegate bool        `json:"delegate,omitempty"` // 模型自述无法判定（要求人审）
}

// JudgeUnit 对一次采纳候补执行模型评委。清单为空时跳过（Pass=true, Skipped 语义）。
func JudgeUnit(ctx context.Context, models LightModel, timeout time.Duration, recipe *Recipe, item RosterItem, stage *Stage, synth *Synthesis) JudgeResult {
	items := recipe.Check.Judge
	if len(items) == 0 {
		return JudgeResult{Pass: true}
	}
	if models == nil {
		return JudgeResult{Pass: false, Err: "评委未接线（ModelFactory 缺失）"}
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	jctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	prompt := buildJudgePrompt(recipe, item, stage, synth, items)
	resp, err := models.CallLightweightWithRetry(jctx, prompt)
	if err != nil {
		return JudgeResult{Pass: false, Err: fmt.Sprintf("评委调用失败: %v", err)}
	}
	res, err := parseJudgeOutput(resp)
	if err != nil {
		return JudgeResult{Pass: false, Err: err.Error(), Raw: resp}
	}
	res.Raw = resp
	return res
}

// buildJudgePrompt 组装评委提示词：清单 + 生成提示词原文 + 交付契约 + 产物/锚点路径。
func buildJudgePrompt(recipe *Recipe, item RosterItem, stage *Stage, synth *Synthesis, items []string) string {
	var b strings.Builder
	b.WriteString("你是量产流水线的质量评委。产物由生成模型按下面的提示词产出，请对判定清单逐条判定。\n")
	name := item.Name
	if name == "" {
		name = item.ID
	}
	fmt.Fprintf(&b, "\n【任务】件 %s（%s）· 阶段 %s\n", name, item.ID, stage.ID)
	deliv := recipe.EffectiveDeliverable(stage)
	fmt.Fprintf(&b, "【交付契约】type=%s frames=%d cell=%s alpha=%s format=%s\n",
		deliv.Type, deliv.Frames, orDash(deliv.Cell), alphaText(deliv.Alpha), orDash(deliv.Format))
	fmt.Fprintf(&b, "\n【产物】%s（工作区相对路径；评委看不了像素，按下列信息判定）\n", synth.OutputPath)
	if len(synth.AnchorPaths) > 0 {
		b.WriteString("【锚点（对照标准）】\n")
		for _, p := range synth.AnchorPaths {
			fmt.Fprintf(&b, "- %s\n", p)
		}
	}
	b.WriteString("\n【生成提示词（模型实际收到的指令）】\n")
	b.WriteString(synth.PromptText)
	b.WriteString("\n\n【判定清单】\n")
	for i, it := range items {
		fmt.Fprintf(&b, "%d. %s\n", i+1, it)
	}
	b.WriteString(`
【判定纪律】
- 依据上文的生成提示词、交付契约与清单逐条判定：要求已被明确覆盖且无自相矛盾记 pass；缺失、矛盾或与契约冲突记 false。
- 本条清单是纯视觉像素判定（如"轮廓与概念锚一致"）而上方信息不足以核对时：pass 记 true 并在 reason 写明"需人审目检"。
- reason 一句话给出依据（≤60 字），不要复述清单。
只输出 JSON，不要其他文字：
{"items":[{"item":"<清单原文>","pass":true,"reason":"<依据>"}]}`)
	return b.String()
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func alphaText(a *bool) string {
	if a == nil {
		return "-"
	}
	if *a {
		return "true"
	}
	return "false"
}

// parseJudgeOutput 解析评委响应（剥 ```json 围栏 / 截取首尾大括号，evolver 套路）。
func parseJudgeOutput(resp string) (JudgeResult, error) {
	cleaned := strings.TrimSpace(resp)
	cleaned = strings.TrimPrefix(cleaned, "```json")
	cleaned = strings.TrimPrefix(cleaned, "```")
	cleaned = strings.TrimSuffix(cleaned, "```")
	start := strings.Index(cleaned, "{")
	end := strings.LastIndex(cleaned, "}")
	if start < 0 || end <= start {
		return JudgeResult{}, fmt.Errorf("评委响应无可解析 JSON")
	}
	var raw struct {
		Items []struct {
			Item   string `json:"item"`
			Pass   any    `json:"pass"`
			Reason string `json:"reason"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(cleaned[start:end+1]), &raw); err != nil {
		return JudgeResult{}, fmt.Errorf("评委 JSON 解析失败: %w", err)
	}
	if len(raw.Items) == 0 {
		return JudgeResult{}, fmt.Errorf("评委 JSON 无 items")
	}
	res := JudgeResult{Pass: true}
	for _, it := range raw.Items {
		pass := false
		switch v := it.Pass.(type) {
		case bool:
			pass = v
		case string:
			pass = strings.EqualFold(strings.TrimSpace(v), "true")
		}
		res.Items = append(res.Items, JudgeItem{Item: it.Item, Pass: pass, Reason: it.Reason})
		if !pass {
			res.Pass = false
		}
	}
	return res, nil
}
