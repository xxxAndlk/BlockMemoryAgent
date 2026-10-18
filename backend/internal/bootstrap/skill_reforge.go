package bootstrap

// skill_reforge.go 存量薄技能重铸（薄技能治理，2026-10-18）。
//
// 动机：历史沉淀的"百字小提示"技能（1-2 条空泛步骤、无锚点、无验证）占用技能库位置
// 却无法指导实践。重铸用 decision 槽模型按当前产出契约（background/steps≥3 带锚点/
// pitfalls/verify/example）重写技能正文——保留 name/title/when_to_use/use_count/tools
//（技能身份与积累的使用统计不动），只换内容。
//
// 纪律：重铸结果先过确定性结构门再过 B2 评分，任一不过拒绝写盘（旧内容原样保留）；
// 重铸写 evolution_log（skill_reforge）可审计；scripts/ 目录与 frontmatter tools 不动。

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/skill"
	"github.com/blockmemory/agent/backend/internal/store"
	"github.com/blockmemory/agent/backend/pkg/textutil"
)

// reforgeTimeout 重铸单次 decision 调用预算（与整理器同档；评分另计 20s，端点预算 150s）。
const reforgeTimeout = 90 * time.Second

// reforgeLearnedSkill 重铸单个经验技能：读现有全文 → decision 模型按契约重写 →
// 结构门+评分 → 重写文件 + 重嵌入 + 刷新技能池 + 审计。返回人类可读摘要。
// 未接线 factory / 技能不存在返回 error；重铸被拒（结构门/评分）返回带理由的 error，旧内容不动。
func reforgeLearnedSkill(ctx context.Context, skills learnedSkillStoreOps, pool *skill.Pool, factory *model.ModelFactory, name string) (string, error) {
	if skills == nil || factory == nil {
		return "", fmt.Errorf("skill reforge not wired")
	}
	rec, err := skills.Get(ctx, name)
	if err != nil {
		return "", fmt.Errorf("get learned skill: %w", err)
	}
	if rec == nil {
		return "", fmt.Errorf("skill not found: %s", name)
	}
	keepPath := textutil.ResolveSkillMDPathFromRef(rec.ContentPath)
	data, err := os.ReadFile(keepPath)
	if err != nil {
		return "", fmt.Errorf("read skill file: %w", err)
	}
	_, body := textutil.ParseFrontmatter(data)
	keepTools := textutil.ParseSkillTools(data)

	// decision 槽重写（复用 evolve 的解析器：让模型输出同样的 {"skills":[...]} 结构）。
	rctx, cancel := context.WithTimeout(ctx, reforgeTimeout)
	resp, err := factory.CallDecisionWithRetry(rctx, buildReforgePrompt(rec, strings.TrimSpace(body)))
	cancel()
	if err != nil {
		return "", fmt.Errorf("reforge call: %w", err)
	}
	out, err := parseEvolveOutput(resp)
	if err != nil || len(out.Skills) == 0 {
		if err == nil {
			err = fmt.Errorf("reforge output has no skill")
		}
		return "", fmt.Errorf("parse reforge output: %w", err)
	}
	sk := out.Skills[0]

	// 硬门：结构门先（确定性），评分殿后（失败降级放行）。
	if reason := thinSkillReason(sk); reason != "" {
		return "", fmt.Errorf("重铸结果未过结构门：%s（旧内容保留）", reason)
	}
	if res, serr := scoreEvolvedSkill(ctx, factory, sk); serr == nil && res.Score < 3 {
		return "", fmt.Errorf("重铸结果评分不足（%d/5）：%s（旧内容保留）", res.Score, res.Reason)
	}

	// 落盘：身份字段（name/title/when_to_use/use_count/outcome）沿用旧记录，只换内容。
	merged := agent.EvolvedSkill{
		Background: sk.Background, Steps: sk.Steps,
		Pitfalls: sk.Pitfalls, Verify: sk.Verify, Example: sk.Example,
	}
	content := renderLearnedSkillMD(rec.Name, rec.Title, rec.WhenToUse, rec.Outcome, merged, keepTools)
	if err := writeSkillFile(keepPath, content); err != nil {
		return "", fmt.Errorf("write reforged skill file: %w", err)
	}
	emb, err := skills.Embed(ctx, rec.Title+" "+rec.WhenToUse)
	if err != nil {
		log.Printf("[reforge] embed failed (non-fatal, vector dropped): name=%s err=%v", name, err)
	}
	if err := skills.Upsert(ctx, &store.LearnedSkill{
		Name: rec.Name, Title: rec.Title, WhenToUse: rec.WhenToUse,
		ContentPath: keepPath, Embedding: emb,
		Enabled: rec.Enabled, UseCount: rec.UseCount, HasTools: len(keepTools) > 0,
		SourceSession: rec.SourceSession, Outcome: rec.Outcome,
	}); err != nil {
		return "", fmt.Errorf("upsert reforged skill: %w", err)
	}
	if pool != nil && rec.Enabled {
		_, body := textutil.ParseFrontmatter([]byte(content))
		pool.Register(learnedSkillToPool(rec.Name, rec.Title, rec.WhenToUse, keepPath,
			body+textutil.SkillToolSection(keepTools)))
	}
	hasExample := "缺省"
	if strings.TrimSpace(sk.Example) != "" {
		hasExample = "齐备"
	}
	summary := fmt.Sprintf("重铸 %s（%s）：步骤 %d 条，背景/示例%s",
		rec.Name, rec.Title, len(sk.Steps), hasExample)
	_ = skills.AppendEvolutionLog(ctx, "skill_reforge", rec.Name, truncateForPrompt(summary, 200), "")
	log.Printf("[reforge] %s", summary)
	return summary, nil
}

// buildReforgePrompt 重铸 prompt：现有技能全文 + 产出契约，要求按契约重写。
// 输出结构与 evolve 一致（{"skills":[...]}），复用 parseEvolveOutput。
func buildReforgePrompt(rec *store.LearnedSkill, body string) string {
	return fmt.Sprintf(`你是技能重写器。下面是一条从历史会话沉淀的经验技能，但它太薄了——
步骤空泛（没有具体命令/路径/参数）、缺背景、缺验证，读者无法照做。请按产出契约把它重写成一条真正能指导实践的技能。

产出契约（与 SessionEvolver 相同，输出 JSON 对象，不要 markdown 围栏）：
{"skills": [{"name": "", "title": "", "when_to_use": "", "background": "...", "steps": ["..."], "pitfalls": ["..."], "verify": "...", "example": "..."}]}

规则:
- 只输出 skills 数组（恰好 1 个元素）；name/title/when_to_use 字段原样照抄下面给出的值
- background: 背景/原理 2-4 句（为什么这样做、前置条件、踩坑根因）
- steps: 3-7 条，每条动词开头，格式"动作 + 具体做法 + 预期结果"，且至少含一个具体锚点
  （完整命令/文件路径/参数名=值/URL/代码片段）；明确拒绝"检查/注意/确保/考虑"式空话
- pitfalls: 0-4 条真实坑点，没有就省略
- verify: 可执行命令或可观察的判别标准
- example: 1-3 句具体使用示例（场景+命令/操作+预期结果）
- 知识边界：只依据技能现有内容与通用工程知识重写，禁止编造未经验证的命令或路径；
  现有信息实在撑不起 3 条带锚点的步骤时，steps 尽量写透并如实标注不确定处
- 不要输出 tools 字段（配套脚本保留不动）

现有技能:
name: %s
title: %s
when_to_use: %s

现有正文:
%s

现在输出 JSON：`,
		rec.Name, rec.Title, rec.WhenToUse, truncateForPrompt(body, 3000))
}
