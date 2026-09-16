package bootstrap

// skill_consolidate.go 经验技能库整理（C 库存治理，2026-09-16）。
//
// 动机：SessionEvolver 每次会话结束沉淀 0-2 个技能且只增不并，技能库长期膨胀
// 会稀释 meta 系统提示【可用技能】目录、拖低模型选技能准确率。本文件实现
// "达到阈值后由轻量模型整理一轮"：语义重复的技能合并（保 keep、并正文、禁用来源）、
// 零使用且被取代/琐碎的技能归档（enabled=false，文件不删可回溯）。
//
// 纪律：只动 enabled 技能；归档要求 use_count==0（用过的一律不自动归档）；
// 全部动作写 evolution_log（skill_merge / skill_archive / consolidate_run）可审计；
// 合并保留目标名与 use_count，来源技能文件原样留在磁盘（禁用即退池，可手工恢复）。

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/skill"
	"github.com/blockmemory/agent/backend/internal/store"
	"github.com/blockmemory/agent/backend/pkg/textutil"
)

// skillConsolidateTimeout 整理单次轻量模型调用预算（与 evolver 同档）。
const skillConsolidateTimeout = 90 * time.Second

// skillConsolidateMaxMerges / skillConsolidateMaxArchives 单轮动作上限（防模型输出失控）。
const (
	skillConsolidateMaxMerges   = 10
	skillConsolidateMaxArchives = 20
)

// skillConsolidator 经验技能库整理器。
type skillConsolidator struct {
	skills    *store.LearnedSkillStore
	pool      *skill.Pool
	factory   *model.ModelFactory
	threshold int // 启用技能数达到该值才自动整理；<=0 关闭自动（手动触发不受限）
	mu        sync.Mutex
}

func newSkillConsolidator(skills *store.LearnedSkillStore, pool *skill.Pool, factory *model.ModelFactory, threshold int) *skillConsolidator {
	return &skillConsolidator{skills: skills, pool: pool, factory: factory, threshold: threshold}
}

// consolidatePlan 轻量模型产出的整理方案。
type consolidatePlan struct {
	Merges []struct {
		Keep  string   `json:"keep"`
		Merge []string `json:"merge"`
		Note  string   `json:"note"`
	} `json:"merges"`
	Archives []struct {
		Name   string `json:"name"`
		Reason string `json:"reason"`
	} `json:"archives"`
}

// Run 执行一轮整理，返回人类可读摘要（跳过原因也在此）。manual=true 时忽略阈值
//（手动端点触发）；自动（每日 tick）时低于阈值直接跳过。
func (c *skillConsolidator) Run(ctx context.Context, manual bool) (string, error) {
	if c == nil || c.skills == nil || c.factory == nil {
		return "", fmt.Errorf("skill consolidator not wired")
	}
	if !c.mu.TryLock() {
		return "已有整理任务在执行，本次跳过", nil
	}
	defer c.mu.Unlock()
	enabled, err := c.skills.List(ctx, true)
	if err != nil {
		return "", fmt.Errorf("list learned skills: %w", err)
	}
	if !manual && (c.threshold <= 0 || len(enabled) < c.threshold) {
		return fmt.Sprintf("技能数 %d 未达整理阈值 %d，跳过", len(enabled), c.threshold), nil
	}
	if len(enabled) < 2 {
		return fmt.Sprintf("启用技能仅 %d 个，无需整理", len(enabled)), nil
	}
	plan, err := c.plan(ctx, enabled)
	if err != nil {
		return "", err
	}
	groups, sources, archived := c.apply(ctx, enabled, plan)
	summary := fmt.Sprintf("整理完成：合并 %d 组（吸收 %d 个技能）、归档 %d 个零使用技能；技能库 %d → %d 个",
		groups, sources, archived, len(enabled), len(enabled)-sources-archived)
	_ = c.skills.AppendEvolutionLog(ctx, "consolidate_run", "learned_skills", summary, "")
	log.Printf("[skill-consolidate] %s", summary)
	return summary, nil
}

// plan 调轻量模型产出合并/归档方案（输入为技能元数据 + 正文摘要）。
func (c *skillConsolidator) plan(ctx context.Context, enabled []*store.LearnedSkill) (*consolidatePlan, error) {
	var b strings.Builder
	for _, sk := range enabled {
		excerpt := skillFileExcerpt(sk.ContentPath, 200)
		fmt.Fprintf(&b, "- name: %s\n  title: %s\n  when_to_use: %s\n  use_count: %d\n  摘要: %s\n",
			sk.Name, sk.Title, sk.WhenToUse, sk.UseCount, excerpt)
	}
	prompt := fmt.Sprintf(`你是技能库整理器。下面是经验技能库（名称/标题/适用场景/被使用次数/正文摘要）。
找出语义重复的技能提出合并方案，找出零使用且被取代或过于琐碎的技能提出归档方案。

规则:
- 只处理下面列出的技能，name 必须逐字来自列表
- merges: 同一工艺的重复技能，keep 选保留者（优先 use_count 高、标题覆盖全的），merge 列出被并入者（至少 1 个）
- archives: 仅限 use_count 为 0 且（已并入别处 / 内容琐碎无复用价值 / 与其他技能重复但无需保留补充内容）的技能
- 语义不同、各有价值的技能不要动；宁少勿多，没有可整理的输出空数组
- 输出 JSON 对象（不要 markdown 围栏）:
{"merges":[{"keep":"name","merge":["name2"],"note":"合并理由"}],"archives":[{"name":"name3","reason":"归档理由"}]}

技能列表:
%s

现在输出 JSON：`, b.String())
	resp, err := c.factory.CallLightweightWithRetry(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("call lightweight: %w", err)
	}
	return parseConsolidatePlan(resp)
}

// parseConsolidatePlan 解析整理方案 JSON（剥 markdown 围栏、截取首尾大括号）。
func parseConsolidatePlan(resp string) (*consolidatePlan, error) {
	cleaned := strings.TrimSpace(resp)
	cleaned = strings.TrimPrefix(cleaned, "```json")
	cleaned = strings.TrimPrefix(cleaned, "```")
	cleaned = strings.TrimSuffix(cleaned, "```")
	start := strings.Index(cleaned, "{")
	end := strings.LastIndex(cleaned, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("no JSON object in consolidate response")
	}
	var plan consolidatePlan
	if err := json.Unmarshal([]byte(cleaned[start:end+1]), &plan); err != nil {
		return nil, fmt.Errorf("unmarshal consolidate plan: %w", err)
	}
	return &plan, nil
}

// apply 校验并应用整理方案；返回（合并组数, 被吸收来源数, 归档数）。
func (c *skillConsolidator) apply(ctx context.Context, enabled []*store.LearnedSkill, plan *consolidatePlan) (int, int, int) {
	byName := map[string]*store.LearnedSkill{}
	for _, sk := range enabled {
		byName[sk.Name] = sk
	}
	used := map[string]bool{} // 已被方案占用的技能名（合并来源/归档/保留者）
	groups, sources, archived := 0, 0, 0
	// 合并：keep 保留原名与 use_count，正文追加各来源正文段，来源禁用退池。
	for _, g := range plan.Merges {
		if groups >= skillConsolidateMaxMerges {
			break
		}
		keep := byName[g.Keep]
		if keep == nil || used[g.Keep] {
			continue
		}
		var srcs []*store.LearnedSkill
		valid := true
		for _, n := range g.Merge {
			src := byName[n]
			if src == nil || n == g.Keep || used[n] {
				valid = false
				break
			}
			srcs = append(srcs, src)
		}
		if !valid || len(srcs) == 0 {
			continue
		}
		if err := c.mergeOne(ctx, keep, srcs, g.Note); err != nil {
			log.Printf("[skill-consolidate] merge failed: keep=%s err=%v", g.Keep, err)
			continue
		}
		used[g.Keep] = true
		for _, src := range srcs {
			used[src.Name] = true
		}
		groups++
		sources += len(srcs)
	}
	// 归档：仅零使用技能（安全下限），禁用退池不删文件。
	for _, a := range plan.Archives {
		if archived >= skillConsolidateMaxArchives {
			break
		}
		sk := byName[a.Name]
		if sk == nil || used[a.Name] || sk.UseCount > 0 {
			continue
		}
		if err := c.disableOne(ctx, sk, "skill_archive", a.Reason); err != nil {
			log.Printf("[skill-consolidate] archive failed: name=%s err=%v", a.Name, err)
			continue
		}
		used[a.Name] = true
		archived++
	}
	return groups, sources, archived
}

// mergeOne 把一个来源技能并入保留者：重写保留者文件（追加来源正文段 + 合并 when_to_use）、
// 重嵌入向量、刷新技能池注册；来源全部禁用退池；写 skill_merge 审计。
func (c *skillConsolidator) mergeOne(ctx context.Context, keep *store.LearnedSkill, srcs []*store.LearnedSkill, note string) error {
	var b strings.Builder
	b.WriteString(skillFileBody(keep.ContentPath))
	whenParts := []string{}
	if w := strings.TrimSpace(keep.WhenToUse); w != "" {
		whenParts = append(whenParts, w)
	}
	for _, src := range srcs {
		fmt.Fprintf(&b, "\n\n## 合并自 %s（%s）\n%s", src.Name, src.Title, skillFileBody(src.ContentPath))
		if w := strings.TrimSpace(src.WhenToUse); w != "" && !strings.Contains(strings.Join(whenParts, "；"), w) {
			whenParts = append(whenParts, w)
		}
	}
	newWhen := truncateForPrompt(strings.Join(whenParts, "；"), 300)
	content := renderSkillFile(keep.Name, keep.Title, newWhen, keep.Outcome, strings.TrimSpace(b.String()))
	if err := writeSkillFile(keep.ContentPath, content); err != nil {
		return fmt.Errorf("write merged skill file: %w", err)
	}
	emb, err := c.skills.Embed(ctx, keep.Title+" "+newWhen)
	if err != nil {
		return fmt.Errorf("embed merged skill: %w", err)
	}
	rec := &store.LearnedSkill{
		Name: keep.Name, Title: keep.Title, WhenToUse: newWhen,
		ContentPath: keep.ContentPath, Embedding: emb,
		Enabled: true, UseCount: keep.UseCount,
		SourceSession: keep.SourceSession, Outcome: keep.Outcome,
	}
	if err := c.skills.Upsert(ctx, rec); err != nil {
		return fmt.Errorf("upsert merged skill: %w", err)
	}
	if c.pool != nil {
		c.pool.Register(learnedSkillToPool(keep.Name, keep.Title, newWhen, keep.ContentPath, content))
	}
	for _, src := range srcs {
		if err := c.disableOne(ctx, src, "skill_merge", fmt.Sprintf("合并进 %s：%s", keep.Name, truncateForPrompt(note, 120))); err != nil {
			log.Printf("[skill-consolidate] disable merged source failed: name=%s err=%v", src.Name, err)
		}
	}
	log.Printf("[skill-consolidate] merged: keep=%s sources=%d", keep.Name, len(srcs))
	return nil
}

// disableOne 禁用单条技能并退池（文件保留）+ 审计。
func (c *skillConsolidator) disableOne(ctx context.Context, sk *store.LearnedSkill, kind, reason string) error {
	if err := c.skills.SetEnabled(ctx, sk.Name, false); err != nil {
		return err
	}
	if c.pool != nil {
		c.pool.Remove(sk.Name)
	}
	if err := c.skills.AppendEvolutionLog(ctx, kind, sk.Name,
		fmt.Sprintf("%s（use_count=%d）：%s", sk.Title, sk.UseCount, truncateForPrompt(reason, 150)), ""); err != nil {
		log.Printf("[skill-consolidate] evolution log failed (non-fatal): name=%s err=%v", sk.Name, err)
	}
	return nil
}

// renderSkillFile 渲染技能文件：frontmatter（name/title/when_to_use/outcome）+ 正文。
func renderSkillFile(name, title, whenToUse, outcome, body string) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "name: %s\n", name)
	fmt.Fprintf(&b, "title: %s\n", title)
	fmt.Fprintf(&b, "when_to_use: %s\n", whenToUse)
	if outcome != "" {
		fmt.Fprintf(&b, "outcome: %s\n", outcome)
	}
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimSpace(body))
	b.WriteString("\n")
	return b.String()
}

// skillFileExcerpt 读技能文件正文前 n 个 rune（剥 frontmatter、压平空白），供整理模型判重。
func skillFileExcerpt(path string, n int) string {
	body := strings.Join(strings.Fields(skillFileBody(path)), " ")
	return truncateForPrompt(body, n)
}

// skillFileBody 读技能文件正文（剥 YAML frontmatter）；读失败返回空串（best-effort）。
func skillFileBody(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	_, body := textutil.ParseFrontmatter(data)
	return strings.TrimSpace(body)
}
