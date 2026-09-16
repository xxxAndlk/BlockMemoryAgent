package bootstrap

// evolver.go 实现 SessionEvolver（2026-09-02 设计 §6）：
// 一次轻量模型调用产出三类沉淀（用户偏好增量/项目经验增量/技能包 0-2 个），
// 技能包落 config/skills_learned/<name>.md + learned_skills PG 元数据 + 向量，
// 同名（when_to_use+title 相似度超阈值）走更新（保留 use_count），全部写 evolution_log。

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/skill"
	"github.com/blockmemory/agent/backend/internal/store"
	"github.com/blockmemory/agent/backend/pkg/textutil"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// evolveSessionLLM SessionEvolver 轻量模型调用（设计 §6.1）：
// 输入 goal+summary+事件摘要+用户消息（rune 上限在 agent 侧截断），产出三类沉淀。
// 解析失败返回 error，调用方（evolveSession）降级纯画像提取。
func evolveSessionLLM(ctx context.Context, factory *model.ModelFactory, in agent.EvolveInput) (*agent.EvolveOutput, error) {
	prompt := fmt.Sprintf(`你是会话进化器。分析刚结束的会话，提取可复用经验（失败会话的踩坑经验价值更高）。

产出 JSON 对象（不要 markdown 围栏）:
{"user_prefs": ["..."], "project_lessons": ["..."], "skills": [{"name": "...", "title": "...", "when_to_use": "...", "steps": ["..."], "pitfalls": ["..."], "verify": "..."}]}

规则:
- user_prefs: 用户的稳定偏好（跨项目通用，如沟通风格/技术栈），0-3 条；不确定就不提取
- project_lessons: 本项目特有的工艺/经验/约定（如"渲染动画帧前先统一去白底""角色一致性需固定参考图+seed"），0-3 条；跨项目通用的工艺放 skills 不要放这里
- skills: 0-2 个结构化技能包（跨项目通用工艺）；name 用小写连字符英文；title 是中文短标题；when_to_use 描述何时使用；steps 是操作步骤；pitfalls 是踩过的坑；verify 是验证方法
- 技能包必须有明确的可复用价值，琐碎会话输出空数组
- skills 中缺 title 或 when_to_use 的条目会被丢弃

会话目标:
%s

会话结果:
%s

结果状态:
%s

事件流摘要:
%s

用户消息:
%s

现在输出 JSON：`,
		truncateForPrompt(in.Goal, 500),
		truncateForPrompt(in.Summary, 800),
		in.Outcome,
		truncateForPrompt(in.EventsDigest, 1500),
		truncateForPrompt(in.UserMessages, 4000))
	resp, err := factory.CallLightweightWithRetry(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("call lightweight: %w", err)
	}
	return parseEvolveOutput(resp)
}

// parseEvolveOutput 解析进化产出 JSON（剥围栏/截取首尾大括号）。
func parseEvolveOutput(resp string) (*agent.EvolveOutput, error) {
	cleaned := strings.TrimSpace(resp)
	cleaned = strings.TrimPrefix(cleaned, "```json")
	cleaned = strings.TrimPrefix(cleaned, "```")
	cleaned = strings.TrimSuffix(cleaned, "```")
	start := strings.Index(cleaned, "{")
	end := strings.LastIndex(cleaned, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("no JSON object in response")
	}
	var raw struct {
		UserPrefs      []string `json:"user_prefs"`
		ProjectLessons []string `json:"project_lessons"`
		Skills         []struct {
			Name      string   `json:"name"`
			Title     string   `json:"title"`
			WhenToUse string   `json:"when_to_use"`
			Steps     []string `json:"steps"`
			Pitfalls  []string `json:"pitfalls"`
			Verify    string   `json:"verify"`
		} `json:"skills"`
	}
	if err := json.Unmarshal([]byte(cleaned[start:end+1]), &raw); err != nil {
		return nil, fmt.Errorf("unmarshal evolve output: %w", err)
	}
	out := &agent.EvolveOutput{UserPrefs: raw.UserPrefs, ProjectLessons: raw.ProjectLessons}
	for _, sk := range raw.Skills {
		out.Skills = append(out.Skills, agent.EvolvedSkill{
			Name: sk.Name, Title: sk.Title, WhenToUse: sk.WhenToUse,
			Steps: sk.Steps, Pitfalls: sk.Pitfalls, Verify: sk.Verify,
		})
	}
	return out, nil
}

// skillUpdateDistance 同名判定：when_to_use+title 向量 cosine 距离 <= 该值走更新
//（设计 §6.4；对应相似度 >= 0.75——技能身份匹配要求高于块记忆召回阈值）。
const skillUpdateDistance = 0.25

// skillRecallDistance 召回阈值：goal/task 与技能向量 cosine 距离 <= 该值才提示
//（设计 §6.5 相似度阈值过滤；对应相似度 >= 0.6——比同名判定宽，宁多提示一行）。
const skillRecallDistance = 0.4

// learnedSkillSink 技能包落库器：文件 + learned_skills PG + 技能池 + evolution_log。
type learnedSkillSink struct {
	skills *store.LearnedSkillStore
	dir    string
	pool   *skill.Pool
	// maxCount enabled 技能上限（B 写入门，config skills.max_count）；<=0 不限。
	// 达到上限后新经验只允许走"近邻同名判定"合并进既有技能，不再新建（防无限膨胀）。
	maxCount int
}

func newLearnedSkillSink(skills *store.LearnedSkillStore, dir string, pool *skill.Pool, maxCount int) *learnedSkillSink {
	return &learnedSkillSink{skills: skills, dir: dir, pool: pool, maxCount: maxCount}
}

// persist 技能包批量落库（单条失败记日志跳过，不拖垮其余沉淀）。
func (s *learnedSkillSink) persist(ctx context.Context, sessionID string, skills []agent.EvolvedSkill, outcome string) error {
	for _, sk := range skills {
		if err := s.persistOne(ctx, sessionID, sk, outcome); err != nil {
			log.Printf("[evolver] persist skill failed (skip one): name=%q err=%v", sk.Name, err)
		}
	}
	return nil
}

// persistOne 单个技能 create-or-update（设计 §6.4）：
// 校验 name/title/when_to_use（缺即丢弃记日志）→ 生成召回向量 → 相似度超阈值更新既有技能
//（保留 use_count/enabled，刷新 title/when_to_use/内容/updated_at）→ 否则新建并注册进技能池。
func (s *learnedSkillSink) persistOne(ctx context.Context, sessionID string, sk agent.EvolvedSkill, outcome string) error {
	title := strings.TrimSpace(sk.Title)
	whenToUse := strings.TrimSpace(sk.WhenToUse)
	if title == "" || whenToUse == "" {
		return fmt.Errorf("invalid skill: missing title/when_to_use (dropped)")
	}
	name := normalizeSkillName(sk.Name, title)
	if name == "" {
		return fmt.Errorf("invalid skill: cannot derive name from %q", sk.Name)
	}

	emb, err := s.skills.Embed(ctx, title+" "+whenToUse)
	if err != nil {
		return fmt.Errorf("embed skill: %w", err)
	}

	// 同名判定：先精确名命中，再向量近邻（enabled 技能）。
	targetName := name
	kind := "skill_create"
	useCount := 0
	if existing, err := s.skills.Get(ctx, name); err == nil && existing != nil {
		targetName = existing.Name
		useCount = existing.UseCount
		kind = "skill_update"
	} else if hits, err := s.skills.SearchSkills(ctx, emb, 5, skillUpdateDistance); err == nil && len(hits) > 0 {
		targetName = hits[0].Name
		useCount = hits[0].UseCount
		kind = "skill_update"
	}
	// B 写入门：技能库达上限后拒绝新建（更新既有技能不受限——合并路径正是满库时
	// 期望的吸收方式）。拒绝也写 evolution_log（skill_dropped），在技能页可审计。
	if kind == "skill_create" && s.maxCount > 0 {
		if n, err := s.skills.CountEnabled(ctx); err == nil && n >= s.maxCount {
			reason := fmt.Sprintf("技能库已满（enabled %d/%d），经验未沉淀为新技能：%s（可合并进既有技能或调大 skills.max_count）", n, s.maxCount, title)
			log.Printf("[evolver] skill dropped: library full (%d/%d) name=%s", n, s.maxCount, name)
			_ = s.skills.AppendEvolutionLog(ctx, "skill_dropped", name, truncateForPrompt(reason, 200), sessionID)
			return nil
		}
	}

	contentPath := filepath.Join(s.dir, targetName+".md")
	content := renderLearnedSkillMD(name, title, whenToUse, outcome, sk)
	if err := writeSkillFile(contentPath, content); err != nil {
		return fmt.Errorf("write skill file: %w", err)
	}
	rec := &store.LearnedSkill{
		Name: targetName, Title: title, WhenToUse: whenToUse,
		ContentPath: contentPath, Embedding: emb,
		Enabled: true, UseCount: useCount,
		SourceSession: sessionID, Outcome: outcome,
	}
	if err := s.skills.Upsert(ctx, rec); err != nil {
		return fmt.Errorf("upsert learned skill: %w", err)
	}
	if s.pool != nil {
		s.pool.Register(learnedSkillToPool(name, title, whenToUse, contentPath, content))
	}
	if err := s.skills.AppendEvolutionLog(ctx, kind, targetName,
		fmt.Sprintf("%s（%s）：%s", title, outcome, truncateForPrompt(whenToUse, 120)), sessionID); err != nil {
		log.Printf("[evolver] write evolution log failed (non-fatal): err=%v", err)
	}
	log.Printf("[evolver] skill persisted: kind=%s name=%s outcome=%s", kind, targetName, outcome)
	return nil
}

// skillNameRe 技能名规范：小写字母数字连字符。
var skillNameRe = regexp.MustCompile(`[^a-z0-9-]+`)

// normalizeSkillName 规范化技能名：小写、非法字符折叠为连字符、压缩连续连字符。
// name 缺失时从 title 转写（非 ASCII 全折叠为连字符，产物为空则返回空串）。
func normalizeSkillName(name, title string) string {
	normalize := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		s = skillNameRe.ReplaceAllString(s, "-")
		for strings.Contains(s, "--") {
			s = strings.ReplaceAll(s, "--", "-")
		}
		return strings.Trim(s, "-")
	}
	if n := normalize(name); n != "" {
		return n
	}
	return normalize(title)
}

// renderLearnedSkillMD 渲染 SKILL.md 式技能文件（YAML frontmatter + 步骤/坑点/验证正文）。
func renderLearnedSkillMD(name, title, whenToUse, outcome string, sk agent.EvolvedSkill) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "name: %s\n", name)
	fmt.Fprintf(&b, "title: %s\n", title)
	fmt.Fprintf(&b, "when_to_use: %s\n", whenToUse)
	if outcome != "" {
		fmt.Fprintf(&b, "outcome: %s\n", outcome)
	}
	b.WriteString("---\n\n")
	if len(sk.Steps) > 0 {
		b.WriteString("## 步骤\n")
		for i, st := range sk.Steps {
			fmt.Fprintf(&b, "%d. %s\n", i+1, strings.TrimSpace(st))
		}
		b.WriteString("\n")
	}
	if len(sk.Pitfalls) > 0 {
		b.WriteString("## 坑点\n")
		for _, p := range sk.Pitfalls {
			fmt.Fprintf(&b, "- %s\n", strings.TrimSpace(p))
		}
		b.WriteString("\n")
	}
	if v := strings.TrimSpace(sk.Verify); v != "" {
		b.WriteString("## 验证\n")
		b.WriteString(v)
		b.WriteString("\n")
	}
	return b.String()
}

// writeSkillFile 落盘技能文件（先建目录）。
func writeSkillFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// learnedSkillToPool 把技能元数据注册进共享技能池（渐进披露第一层：
// list_skills 见 name+description，load_skill 取全文）。
func learnedSkillToPool(name, title, whenToUse, path, content string) *types.Skill {
	return &types.Skill{
		SkillID:     name,
		Name:        title,
		Description: whenToUse,
		Source:      "learned",
		Path:        path,
		Content:     content,
	}
}

// parseLearnedSkillFile 解析 skills_learned/<name>.md（YAML frontmatter + 正文）。
// frontmatter 缺 name/title/when_to_use 任一项返回错误（孤儿修复时跳过）。
func parseLearnedSkillFile(path string) (*types.Skill, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fm, body := textutil.ParseFrontmatter(data)
	name := strings.TrimSpace(fm["name"])
	title := strings.TrimSpace(fm["title"])
	whenToUse := strings.TrimSpace(fm["when_to_use"])
	if name == "" || title == "" || whenToUse == "" {
		return nil, fmt.Errorf("frontmatter missing name/title/when_to_use: %s", path)
	}
	return learnedSkillToPool(name, title, whenToUse, path, body), nil
}

// reconcileLearnedSkills 启动重扫：skills_learned/ 目录与 learned_skills PG 互相修复
//（设计 §9：文件在 PG 无记录则补注册；PG enabled 但文件缺失则标记禁用），
// enabled 且文件在的注册进共享技能池（渐进披露）。
func reconcileLearnedSkills(ctx context.Context, skills *store.LearnedSkillStore, dir string, pool *skill.Pool) {
	if skills == nil {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		log.Printf("[bootstrap] scan skills_learned failed (non-fatal): %v", err)
		return
	}
	files := map[string]*types.Skill{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		sk, err := parseLearnedSkillFile(filepath.Join(dir, e.Name()))
		if err != nil {
			log.Printf("[bootstrap] skills_learned skip %s: %v", e.Name(), err)
			continue
		}
		files[sk.SkillID] = sk
	}
	rows, err := skills.List(ctx, false)
	if err != nil {
		log.Printf("[bootstrap] list learned skills failed (non-fatal): %v", err)
		return
	}
	rowByName := map[string]*store.LearnedSkill{}
	for _, r := range rows {
		rowByName[r.Name] = r
	}
	// 文件在 PG 无记录 → 补注册（生成向量，enabled=true）。
	for name, sk := range files {
		if _, ok := rowByName[name]; ok {
			continue
		}
		emb, err := skills.Embed(ctx, sk.Name+" "+sk.Description)
		if err != nil {
			log.Printf("[bootstrap] embed orphan skill %s failed (skip): %v", name, err)
			continue
		}
		if err := skills.Upsert(ctx, &store.LearnedSkill{
			Name: name, Title: sk.Name, WhenToUse: sk.Description,
			ContentPath: sk.Path, Embedding: emb, Enabled: true,
		}); err != nil {
			log.Printf("[bootstrap] register orphan skill %s failed (skip): %v", name, err)
			continue
		}
		rowByName[name] = &store.LearnedSkill{Name: name, Enabled: true}
		log.Printf("[bootstrap] learned skill orphan repaired: %s", name)
	}
	if pool == nil {
		return
	}
	// PG enabled 且文件在 → 注册进池；PG enabled 但文件缺失 → 标记禁用。
	for name, row := range rowByName {
		sk := files[name]
		if sk == nil {
			if row.Enabled {
				if err := skills.SetEnabled(ctx, name, false); err == nil {
					log.Printf("[bootstrap] learned skill file missing, disabled: %s", name)
				}
			}
			continue
		}
		if row.Enabled {
			pool.Register(sk)
		}
	}
	if len(pool.All()) > 0 {
		log.Printf("[bootstrap] learned skills registered: %d", len(pool.All()))
	}
}
