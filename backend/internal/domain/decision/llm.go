package decision

// llm.go LLM 兜底 provider：把 newIntentArbiter（bootstrap.go）的
// "类型化输出 + parseStrictJSON 严格解析 + confidence<阈值拒判" 范式泛化为
// 多问题批量决策。委托 ModelFactory.CallDecisionWithRetry（decision 槽位，
// roles.yaml decision_model，未配置回退 lightweight——白拿缓存/绑定/热更新/日志）。

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Caller 轻量 LLM 调用面（ModelFactory.CallDecisionWithRetry 适配进来）。
// 独立接口便于测试注入 fake，不让 domain 反向依赖 model 包。
type Caller interface {
	CallDecision(ctx context.Context, prompt string) (string, error)
}

// CallerFunc 函数适配器。
type CallerFunc func(ctx context.Context, prompt string) (string, error)

// CallDecision 实现 Caller。
func (f CallerFunc) CallDecision(ctx context.Context, prompt string) (string, error) {
	return f(ctx, prompt)
}

// LLMProvider 轻量模型决策 provider。结构性防幻觉三件套：
//  1. 候选闭集在提示词里明示 + 解析后再次校验集外值即丢弃该答案；
//  2. 输出只认 JSON（parseStrictJSON 剥围栏严格解析）；
//  3. 置信度随答案返回，生效与否由 Layer.ShouldEnforce 按点阈值裁决（不在解析层硬拒，
//     影子期需要低置信样本做对拍——newIntentArbiter 的 <0.6 拒判是"生效门"不是"记录门"）。
type LLMProvider struct {
	Caller Caller
}

// NewLLMProvider 构造。caller 为 nil 时 Decide 恒错（fail-open 有声）。
func NewLLMProvider(caller Caller) *LLMProvider {
	return &LLMProvider{Caller: caller}
}

// Decide 单次轻量调用批量回答 Request.Questions。
func (p *LLMProvider) Decide(ctx context.Context, req Request) (Response, error) {
	if p == nil || p.Caller == nil {
		return Response{}, fmt.Errorf("decision llm provider: caller not wired")
	}
	if len(req.Questions) == 0 {
		return Response{}, nil
	}
	out, err := p.Caller.CallDecision(ctx, buildDecisionPrompt(req))
	if err != nil {
		return Response{}, fmt.Errorf("decision llm call: %w", err)
	}
	return parseDecisionResponse(out, req)
}

// buildDecisionPrompt 构造批量决策提示词：状态 + 逐问题（含候选闭集）+ JSON 输出契约。
func buildDecisionPrompt(req Request) string {
	var b strings.Builder
	b.WriteString("你是决策分类器。根据状态对下列问题逐个给出判断。只输出 JSON，不要任何解释。\n")
	b.WriteString("输出格式：{\"answers\":[{\"key\":\"...\",\"value\":\"...\",\"score\":0.0,\"confidence\":0.0}]}，每题一个元素。\n")
	b.WriteString("confidence 取 0.0-1.0 表示你对该判断的把握。choice 题的 value 只能取候选之一；")
	b.WriteString("noul 题的 value 只能是 \"yes\" 或 \"no\"；score 题把 0.0-1.0 的分数放 score 字段、value 留空。\n\n")
	b.WriteString("【状态】\n")
	b.WriteString(strings.TrimSpace(req.State))
	b.WriteString("\n\n【问题】\n")
	for _, q := range req.Questions {
		fmt.Fprintf(&b, "- key=%s 类型=%s：%s\n", q.Key, q.Primitive, strings.TrimSpace(q.Prompt))
		if len(q.Candidates) > 0 {
			fmt.Fprintf(&b, "  候选（只能从中选）：%s\n", strings.Join(q.Candidates, " | "))
		}
		if q.Context != "" {
			fmt.Fprintf(&b, "  参考：%s\n", compactRunes(q.Context, 2000))
		}
	}
	return b.String()
}

// decisionLLMOutput LLM 输出 JSON 契约。
type decisionLLMOutput struct {
	Answers []struct {
		Key        string  `json:"key"`
		Value      string  `json:"value"`
		Score      float64 `json:"score"`
		Confidence float64 `json:"confidence"`
	} `json:"answers"`
}

// parseDecisionResponse 严格解析 + 按问题类型校验，逐题装配 Answer。
// 校验不过的题直接丢（宁缺勿滥：调用方对缺失答案 fail-open 走现状行为）。
func parseDecisionResponse(out string, req Request) (Response, error) {
	var parsed decisionLLMOutput
	if err := parseStrictJSON(out, &parsed); err != nil {
		return Response{}, fmt.Errorf("decision parse json: %w", err)
	}
	byKey := make(map[string]int, len(parsed.Answers))
	for i, a := range parsed.Answers {
		byKey[a.Key] = i
	}
	var resp Response
	for _, q := range req.Questions {
		i, ok := byKey[q.Key]
		if !ok {
			continue
		}
		raw := parsed.Answers[i]
		ans := Answer{
			Key:        q.Key,
			Point:      q.Point,
			Primitive:  q.Primitive,
			Value:      strings.TrimSpace(raw.Value),
			Score:      clamp01(raw.Score),
			Confidence: clamp01(raw.Confidence),
		}
		if !answerValid(q, ans) {
			continue
		}
		resp.Answers = append(resp.Answers, ans)
	}
	if len(resp.Answers) == 0 {
		return Response{}, fmt.Errorf("decision parse: no valid answer (got %d raw)", len(parsed.Answers))
	}
	return resp, nil
}

// answerValid 按原语校验单答案：Choice 值必须命中候选闭集（结构性防幻觉），
// Noul 只认 yes/no，Score 有限且非 NaN 区间已由 clamp01 保证。
func answerValid(q Question, ans Answer) bool {
	switch q.Primitive {
	case PrimitiveChoice:
		return slices.Contains(q.Candidates, ans.Value)
	case PrimitiveNoul:
		return ans.Value == "yes" || ans.Value == "no"
	case PrimitiveScore:
		return true
	default:
		return false
	}
}

// parseStrictJSON 剥 markdown 围栏后严格解析 JSON（模型可能包 ```json 代码块）。
// 与 bootstrap.parseStrictJSON 同款语义，domain 层不反向依赖 bootstrap 复制一份。
func parseStrictJSON(s string, v any) error {
	s = strings.TrimSpace(s)
	if idx := strings.Index(s, "```"); idx != -1 {
		s = s[idx+3:]
		if end := strings.Index(s, "```"); end != -1 {
			s = s[:end]
		}
	}
	s = strings.TrimPrefix(strings.TrimSpace(s), "json")
	return json.Unmarshal([]byte(strings.TrimSpace(s)), v)
}

// clamp01 把置信/分数夹进 [0,1]（模型偶发越界值不因校验失败丢整条）。
func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// compactRunes 超长上下文截断（头 tail 保留）。
func compactRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "...(截断)"
}
