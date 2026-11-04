package agent

// memory_eval.go C-05 同预算 baseline/treatment 真实模型评估。
// 基线/处理只差“是否注入候选经验”，任务、模型、次数与预算口径一致。
// 评估只产出报告与事件；晋升仍需人工（promotion_mode=enforce + 非 auto + verified）。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/blockmemory/agent/backend/internal/store"
)

// EvalCall 真实模型调用（bootstrap 接 ModelFactory.CallLightweightWithRetry）。
type EvalCall func(ctx context.Context, prompt string) (string, error)

// EvalTask 一条固定评测任务。
type EvalTask struct {
	ID     string `json:"id"`
	Prompt string `json:"prompt"`
}

// LLMCandidateEvaluator 真实 LLM baseline/treatment 评估器。
type LLMCandidateEvaluator struct {
	Tasks    []EvalTask
	Repeats  int
	MinTasks int
	Call     EvalCall
}

// EvalReport 评估报告（可持久化到 memory events）。
type EvalReport struct {
	Version     string   `json:"version"`
	CandidateID string   `json:"candidate_id"`
	Tasks       int      `json:"tasks"`
	Repeats     int      `json:"repeats"`
	Pass        bool     `json:"pass"`
	Wins        int      `json:"wins"`
	Losses      int      `json:"losses"`
	Ties        int      `json:"ties"`
	Reasons     []string `json:"reasons,omitempty"`
}

// Evaluate 实现 CandidateEvaluator。
func (e *LLMCandidateEvaluator) Evaluate(ctx context.Context, item store.MemoryItem) (string, error) {
	if e == nil || e.Call == nil {
		return "", ErrEvaluationUnavailable
	}
	if len(e.Tasks) == 0 {
		return "", fmt.Errorf("candidate evaluation: no eval tasks configured")
	}
	minTasks := e.MinTasks
	if minTasks <= 0 {
		minTasks = len(e.Tasks)
	}
	if len(e.Tasks) < minTasks {
		return "", fmt.Errorf("candidate evaluation: need >=%d eval tasks, got %d", minTasks, len(e.Tasks))
	}
	repeats := e.Repeats
	if repeats <= 0 {
		repeats = 1
	}
	rep := EvalReport{Version: "v1", CandidateID: item.ID, Tasks: len(e.Tasks), Repeats: repeats}
	for _, task := range e.Tasks {
		for i := 0; i < repeats; i++ {
			// 交替顺序降低位置偏差；两次调用同任务同预算，仅 treatment 多候选经验。
			baselinePrompt := task.Prompt
			treatmentPrompt := task.Prompt + "\n\n【候选项目经验】\n" + item.Content
			var aP, bP, aLabel, bLabel string
			if i%2 == 0 {
				aP, bP, aLabel, bLabel = baselinePrompt, treatmentPrompt, "baseline", "treatment"
			} else {
				aP, bP, aLabel, bLabel = treatmentPrompt, baselinePrompt, "treatment", "baseline"
			}
			a, err := e.Call(ctx, aP)
			if err != nil {
				return "", fmt.Errorf("eval run A: %w", err)
			}
			b, err := e.Call(ctx, bP)
			if err != nil {
				return "", fmt.Errorf("eval run B: %w", err)
			}
			score, reason, err := e.judge(ctx, task.Prompt, a, b)
			if err != nil {
				return "", fmt.Errorf("eval judge: %w", err)
			}
			// score: 1=A better, -1=B better, 0=tie；映射回 treatment 胜负。
			treatmentWins := (score > 0 && aLabel == "treatment") || (score < 0 && bLabel == "treatment")
			treatmentLoses := (score > 0 && aLabel == "baseline") || (score < 0 && bLabel == "baseline")
			switch {
			case treatmentWins:
				rep.Wins++
			case treatmentLoses:
				rep.Losses++
				rep.Reasons = append(rep.Reasons, fmt.Sprintf("%s: %s", task.ID, reason))
			default:
				rep.Ties++
			}
		}
	}
	rep.Pass = rep.Wins > rep.Losses && rep.Losses == 0
	out, _ := json.Marshal(rep)
	return string(out), nil
}

// judge 让同一轻量模型比较两份结果；返回 1=A 更好 / -1=B 更好 / 0=相当。
func (e *LLMCandidateEvaluator) judge(ctx context.Context, task, a, b string) (int, string, error) {
	prompt := "你是严格评审。任务：\n" + task + "\n\n结果 A：\n" + truncateForEval(a) +
		"\n\n结果 B：\n" + truncateForEval(b) +
		"\n\n只回答一行：A、B 或 TIE，后接 20 字内理由。优先正确性与可验证性；不要因为更长而偏好。"
	out, err := e.Call(ctx, prompt)
	if err != nil {
		return 0, "", err
	}
	up := strings.ToUpper(strings.TrimSpace(out))
	switch {
	case strings.HasPrefix(up, "A"):
		return 1, strings.TrimSpace(strings.TrimPrefix(up, "A")), nil
	case strings.HasPrefix(up, "B"):
		return -1, strings.TrimSpace(strings.TrimPrefix(up, "B")), nil
	default:
		return 0, strings.TrimSpace(out), nil
	}
}

func truncateForEval(s string) string {
	const max = 4000
	r := []rune(strings.TrimSpace(s))
	if len(r) <= max {
		return string(r)
	}
	return string(r[:max]) + "\n…(truncated)"
}
