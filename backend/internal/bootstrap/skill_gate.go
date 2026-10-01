package bootstrap

// skill_gate.go 经验技能写入质量治理（TODO 25 阶段 B2/B3/B4，2026-09-30）。
//
// B2 写入时质量门禁：轻量模型对产出技能打 1-5 分（具体性/可执行性/可复用性），
// <3 分丢弃并写 evolution_log（kind=skill_rejected）；评分调用/解析失败不阻塞（降级放行）。
//
// B3 写入时近重复预检：persist 前对 title+when_to_use 向量做相似度检索，
// cosine ≥0.9 的异名候选进 judge 确认；确认重复走 skill_update 合并（steps/pitfalls union
// 进保留者正文并重渲染，顺带消双层编号）；judge 失败保守合并（宁可合并不灌水）。
//
// B4 共用实现：judgeDuplicateMerge 同时供 persist（B3）与每日整理 mergeOne（B4）调用，
// 整理侧合并前同样经 judge 复核，防整理模型误并。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/store"
)

// learnedSkillStoreOps 技能落库/整理依赖的存储操作。
// *store.LearnedSkillStore 实现本接口；单测注入 fake 覆盖写入门与近重复路径。
type learnedSkillStoreOps interface {
	Get(ctx context.Context, name string) (*store.LearnedSkill, error)
	List(ctx context.Context, enabledOnly bool) ([]*store.LearnedSkill, error)
	SearchSkills(ctx context.Context, embedding []float32, limit int, maxDistance float64) ([]*store.LearnedSkill, error)
	CountEnabled(ctx context.Context) (int, error)
	SetEnabled(ctx context.Context, name string, enabled bool) error
	Upsert(ctx context.Context, rec *store.LearnedSkill) error
	AppendEvolutionLog(ctx context.Context, kind, target, summary, sourceSession string) error
	Embed(ctx context.Context, text string) ([]float32, error)
}

// ---- B2 质量门禁 ----

// skillScoreTimeout 评分单次轻量模型调用预算（evolveSession 总预算 90s 内的一小段）。
const skillScoreTimeout = 20 * time.Second

// skillScoreResult 质量评分输出（严格 JSON：{"score":1-5,"reason":"..."}）。
type skillScoreResult struct {
	Score  int    `json:"score"`
	Reason string `json:"reason"`
}

// scoreEvolvedSkill 轻量模型对单个产出技能评分（B2 写入门）。
// 评分维度：具体性（明确对象/命令/参数/路径）、可执行性（步骤可照做）、可复用性（跨项目通用）。
func scoreEvolvedSkill(ctx context.Context, factory *model.ModelFactory, sk agent.EvolvedSkill) (skillScoreResult, error) {
	if factory == nil {
		return skillScoreResult{}, fmt.Errorf("skill scorer not wired")
	}
	steps, _ := json.Marshal(sk.Steps)
	pitfalls, _ := json.Marshal(sk.Pitfalls)
	prompt := fmt.Sprintf(`你是经验技能质量评审。给技能打 1-5 分：1=空泛无用，3=一般可用，5=具体且高度可复用。
评分维度：具体性（有明确对象/命令/参数/路径）、可执行性（步骤可直接照做）、可复用性（跨项目通用）。
只输出 JSON（不要 markdown 围栏）：{"score": 4, "reason": "一句话理由"}

技能：
{"title": %q, "when_to_use": %q, "steps": %s, "pitfalls": %s, "verify": %q}

现在输出 JSON：`,
		sk.Title, sk.WhenToUse, steps, pitfalls, sk.Verify)
	sctx, cancel := context.WithTimeout(ctx, skillScoreTimeout)
	defer cancel()
	resp, err := factory.CallLightweightWithRetry(sctx, prompt)
	if err != nil {
		return skillScoreResult{}, err
	}
	return parseSkillScore(resp)
}

// parseSkillScore 解析评分 JSON（剥围栏/截取首尾大括号）；score 越界钳到 1-5。
func parseSkillScore(resp string) (skillScoreResult, error) {
	var r skillScoreResult
	if err := parseStrictJSON(resp, &r); err != nil {
		return r, err
	}
	if r.Score < 1 {
		r.Score = 1
	}
	if r.Score > 5 {
		r.Score = 5
	}
	return r, nil
}

// ---- B3/B4 共用判重 judge ----

// skillJudgeTimeout judge 单次轻量模型调用预算。
const skillJudgeTimeout = 20 * time.Second

// duplicateJudgeResult 判重输出（严格 JSON：{"duplicate":true,"merge_into":"name","reason":"..."}）。
type duplicateJudgeResult struct {
	Duplicate bool   `json:"duplicate"`
	MergeInto string `json:"merge_into"`
	Reason    string `json:"reason"`
}

// judgeDuplicateMerge 共用近重复判定（B3 persist 预检 / B4 整理合并复核）：
// 轻量模型判断新技能与候选技能是否同一工艺的近重复。
// 调用失败/解析失败返回 error，调用方按既定降级策略处理（persist 保守合并、整理维持合并）。
func judgeDuplicateMerge(ctx context.Context, factory *model.ModelFactory, newTitle, newWhenToUse string, cand *store.LearnedSkill) (duplicateJudgeResult, error) {
	if factory == nil {
		return duplicateJudgeResult{}, fmt.Errorf("skill judge not wired")
	}
	if cand == nil {
		return duplicateJudgeResult{}, fmt.Errorf("judge candidate is nil")
	}
	prompt := fmt.Sprintf(`你是技能库判重器。判断"新技能"与"候选技能"是否同一工艺的近重复（语义相同，或一个是另一个的子集/特例）。
规则：二者解决同一类问题且做法高度重合才算重复；仅主题相近但工艺不同不算重复。
只输出 JSON（不要 markdown 围栏）：{"duplicate": true, "merge_into": "候选name", "reason": "判定理由"}

新技能：
{"title": %q, "when_to_use": %q}

候选技能：
{"name": %q, "title": %q, "when_to_use": %q}

现在输出 JSON：`,
		newTitle, newWhenToUse, cand.Name, cand.Title, cand.WhenToUse)
	jctx, cancel := context.WithTimeout(ctx, skillJudgeTimeout)
	defer cancel()
	resp, err := factory.CallLightweightWithRetry(jctx, prompt)
	if err != nil {
		return duplicateJudgeResult{}, err
	}
	return parseDuplicateJudge(resp)
}

// parseDuplicateJudge 解析判重 JSON（parseStrictJSON 剥围栏/严格解析，bootstrap.go 共用实现）。
func parseDuplicateJudge(resp string) (duplicateJudgeResult, error) {
	var r duplicateJudgeResult
	if err := parseStrictJSON(resp, &r); err != nil {
		return r, err
	}
	return r, nil
}

// ---- B3 合并渲染辅助 ----

// parseSkillSections 从渲染后的技能文件正文解析 步骤/坑点/验证 三段
//（近重复合并时把新技能的条目 union 进保留者正文；合并自 consolidate 的小节标题按
// "## " 边界天然跳过，保守不解析）。剥行首序号/符号，与 renderLearnedSkillMD 对齐。
func parseSkillSections(body string) (steps, pitfalls []string, verify string) {
	section := ""
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "## ") {
			section = trimmed
			continue
		}
		switch section {
		case "## 步骤":
			if st := stripLeadingMarker(trimmed); st != "" {
				steps = append(steps, st)
			}
		case "## 坑点":
			if p := stripLeadingMarker(trimmed); p != "" {
				pitfalls = append(pitfalls, p)
			}
		case "## 验证":
			if verify == "" {
				verify = trimmed
			} else {
				verify += "\n" + trimmed
			}
		}
	}
	return steps, pitfalls, verify
}

// appendUnique 追加不重复条目（去重键=去空白后精确匹配）；空条目跳过。返回新切片。
func appendUnique(list []string, items ...string) []string {
	seen := map[string]bool{}
	for _, s := range list {
		seen[strings.TrimSpace(s)] = true
	}
	for _, it := range items {
		it = strings.TrimSpace(it)
		if it == "" || seen[it] {
			continue
		}
		seen[it] = true
		list = append(list, it)
	}
	return list
}
