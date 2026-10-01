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

// buildEvolvePrompt 构造 SessionEvolver 生成 prompt（B1 收紧门槛：单会话最多 1 个技能、
// 三条件齐备才可产出、steps 必须动词开头且不带行首序号）。JSON 结构与 parseEvolveOutput 保持兼容。
func buildEvolvePrompt(in agent.EvolveInput) string {
	return fmt.Sprintf(`你是会话进化器。分析刚结束的会话，提取可复用经验（失败会话的踩坑经验价值更高）。

产出 JSON 对象（不要 markdown 围栏）:
{"user_prefs": ["..."], "project_lessons": ["..."], "skills": [{"name": "...", "title": "...", "when_to_use": "...", "steps": ["..."], "pitfalls": ["..."], "verify": "..."}]}

规则:
- user_prefs: 用户的稳定偏好（跨项目通用，如沟通风格/技术栈），0-3 条；不确定就不提取
- project_lessons: 本项目特有的工艺/经验/约定（如"渲染动画帧前先统一去白底""角色一致性需固定参考图+seed"），0-3 条；跨项目通用的工艺放 skills 不要放这里
- skills: 0 或 1 个结构化技能包（最多 1 个）。必须同时满足以下三者，缺一输出空数组：
  ① 跨项目通用（不是本项目独有约定）；② 非显而易见（不是工程师默认都知道的做法）；
  ③ 有具体可执行步骤（步骤里能指到具体命令/参数/文件/工具）
- name 用小写连字符英文；title 是中文短标题；when_to_use 描述何时使用；pitfalls 是踩过的坑；verify 是验证方法
- steps 每条必须是动词开头的可执行动作（如"运行 xxx --yyy 生成 zzz"）；明确拒绝"检查/注意/确保/考虑/记得"这类空泛条目
- steps 数组元素不要自带行首序号或符号（不要写"1. xxx""- xxx"，直接写动作本身）
- tools: 可选，本次会话中实际自写并执行成功的辅助脚本（最多 1 个）；每项
  {"filename": "scripts/xxx.py", "language": "py", "code": "...", "desc": "一句话用途"}。
  filename 限 scripts/ 下小写字母数字连字符加点扩展名（py/sh/js/ts）；code 必须 ≤60 行
  （超长不要给 code，把做法写进 steps）；language ∈ py|sh|js|ts。
  没有符合条件的脚本就省略 tools 字段；tools 不进入 steps，steps 照常写操作步骤
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
}

// evolveSessionLLM SessionEvolver 轻量模型调用（设计 §6.1）：
// 输入 goal+summary+事件摘要+用户消息（rune 上限在 agent 侧截断），产出三类沉淀。
// 解析失败返回 error，调用方（evolveSession）降级纯画像提取。
func evolveSessionLLM(ctx context.Context, factory *model.ModelFactory, in agent.EvolveInput) (*agent.EvolveOutput, error) {
	prompt := buildEvolvePrompt(in)
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
			Tools     []struct {
				Filename string `json:"filename"`
				Language string `json:"language"`
				Code     string `json:"code"`
				Desc     string `json:"desc"`
			} `json:"tools"`
		} `json:"skills"`
	}
	if err := json.Unmarshal([]byte(cleaned[start:end+1]), &raw); err != nil {
		return nil, fmt.Errorf("unmarshal evolve output: %w", err)
	}
	out := &agent.EvolveOutput{UserPrefs: raw.UserPrefs, ProjectLessons: raw.ProjectLessons}
	for _, sk := range raw.Skills {
		ev := agent.EvolvedSkill{
			Name: sk.Name, Title: sk.Title, WhenToUse: sk.WhenToUse,
			Steps: sk.Steps, Pitfalls: sk.Pitfalls, Verify: sk.Verify,
		}
		for _, t := range sk.Tools {
			ev.Tools = append(ev.Tools, agent.EvolvedSkillTool{
				Filename: t.Filename, Language: t.Language, Code: t.Code, Desc: t.Desc,
			})
		}
		out.Skills = append(out.Skills, ev)
	}
	return out, nil
}

// skillUpdateDistance 同名判定：when_to_use+title 向量 cosine 距离 <= 该值走更新
//（设计 §6.4；对应相似度 >= 0.75——技能身份匹配要求高于块记忆召回阈值）。
const skillUpdateDistance = 0.25

// skillRecallDistance 召回阈值：goal/task 与技能向量 cosine 距离 <= 该值才提示
//（设计 §6.5 相似度阈值过滤；对应相似度 >= 0.6——比同名判定宽，宁多提示一行）。
const skillRecallDistance = 0.4

// skillNearDupDistance 近重复预检阈值（B3）：cosine ≥0.9（距离 ≤0.1）的异名候选进 judge 确认。
const skillNearDupDistance = 0.1

// learnedSkillSink 技能包落库器：文件 + learned_skills PG + 技能池 + evolution_log。
type learnedSkillSink struct {
	skills learnedSkillStoreOps
	dir    string
	pool   *skill.Pool
	// maxCount enabled 技能上限（B 写入门，config skills.max_count）；<=0 不限。
	// 达到上限后新经验只允许走"近邻同名判定"合并进既有技能，不再新建（防无限膨胀）。
	maxCount int
	factory  *model.ModelFactory
	// judge/score 质量治理回调（B2/B3）；newLearnedSkillSink 装配真实现（轻量模型），
	// 单测注入 fake 覆盖"评分拒绝/放行""judge 合并/保守合并/新建"各路径。
	judge func(ctx context.Context, newTitle, newWhenToUse string, cand *store.LearnedSkill) (duplicateJudgeResult, error)
	score func(ctx context.Context, sk agent.EvolvedSkill) (skillScoreResult, error)
}

func newLearnedSkillSink(skills learnedSkillStoreOps, dir string, pool *skill.Pool, maxCount int, factory *model.ModelFactory) *learnedSkillSink {
	s := &learnedSkillSink{skills: skills, dir: dir, pool: pool, maxCount: maxCount, factory: factory}
	s.judge = func(ctx context.Context, title, whenToUse string, cand *store.LearnedSkill) (duplicateJudgeResult, error) {
		return judgeDuplicateMerge(ctx, factory, title, whenToUse, cand)
	}
	s.score = func(ctx context.Context, sk agent.EvolvedSkill) (skillScoreResult, error) {
		return scoreEvolvedSkill(ctx, factory, sk)
	}
	return s
}

// persist 技能包批量落库（单条失败记日志跳过，不拖垮其余沉淀）。
// B2 写入门：落库前逐条轻量评分，<3 分丢弃并写 evolution_log（skill_rejected）；
// 评分调用/解析失败不阻塞（降级放行）。
func (s *learnedSkillSink) persist(ctx context.Context, sessionID string, skills []agent.EvolvedSkill, outcome string) error {
	for _, sk := range skills {
		if s.score != nil {
			res, err := s.score(ctx, sk)
			switch {
			case err != nil:
				log.Printf("[evolver] skill score failed (pass, degrade): name=%q err=%v", sk.Name, err)
			case res.Score < 3:
				name := normalizeSkillName(sk.Name, sk.Title)
				log.Printf("[evolver] skill rejected by quality gate (score=%d): %s", res.Score, sk.Title)
				_ = s.skills.AppendEvolutionLog(ctx, "skill_rejected", name,
					fmt.Sprintf("%s：%s", truncateForPrompt(sk.Title, 80), truncateForPrompt(res.Reason, 150)), sessionID)
				continue
			}
		}
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

	// 同名判定：先精确名命中。
	targetName := name
	kind := "skill_create"
	useCount := 0
	if existing, err := s.skills.Get(ctx, name); err == nil && existing != nil {
		targetName = existing.Name
		useCount = existing.UseCount
		kind = "skill_update"
	}
	// B3 近重复预检（位于粗粒度向量更新之前）：cosine ≥0.9 的异名候选必然也命中
	// ≤0.25 的同名更新阈值，必须先判——确认重复走 union 合并（而非整篇覆盖更新）。
	// judge 失败/超时 → 保守合并进相似度最高的候选（宁可合并不灌水）；
	// judge 明确判"不重复" → 跳过粗粒度更新直接新建（TODO：不是重复才新建）。
	notDupConfirmed := false
	if kind == "skill_create" {
		if hits, err := s.skills.SearchSkills(ctx, emb, 3, skillNearDupDistance); err == nil {
			var cands []*store.LearnedSkill
			for _, h := range hits {
				if h.Name != name {
					cands = append(cands, h)
				}
			}
			if len(cands) > 0 {
				keep, reason, merged := s.confirmNearDup(ctx, title, whenToUse, sk, cands)
				switch {
				case merged:
					if err := s.applyNearDupMerge(ctx, sessionID, sk, keep, reason); err != nil {
						// 合并失败不丢经验：降级按新建流程继续。
						log.Printf("[evolver] near-dup merge failed (degrade to create): keep=%s err=%v", keep.Name, err)
					} else {
						return nil
					}
				default:
					notDupConfirmed = true
				}
			}
		}
	}
	// 粗粒度同名判定：向量近邻（enabled 技能，0.75 ≤ 相似度 < 0.9 区间）走覆盖更新——维持现状。
	if kind == "skill_create" && !notDupConfirmed {
		if hits, err := s.skills.SearchSkills(ctx, emb, 5, skillUpdateDistance); err == nil && len(hits) > 0 {
			targetName = hits[0].Name
			useCount = hits[0].UseCount
			kind = "skill_update"
		}
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

	// C3 脚本固化：校验+冒烟（10s），通过者写 <name>/scripts/ 并进 frontmatter tools；
	// 失败者降级正文末尾附录、has_tools=false；无运行时同样降级。
	solid, failed, toolNotes := solidifySkillTools(sk.Tools)

	// C1 目录式落盘：优先 <name>/SKILL.md（新建即目录式）；存量老格式 <name>.md 只在
	// 仍存在且本次无固化脚本时原地更新（老格式只读不写回——解析器不搬迁）。
	contentPath := textutil.ResolveSkillMDPath(s.dir, targetName)
	if len(solid) > 0 && filepath.Base(contentPath) != "SKILL.md" {
		contentPath = filepath.Join(s.dir, targetName, "SKILL.md")
	}
	content := renderLearnedSkillMD(name, title, whenToUse, outcome, sk, solidToolMetas(solid))
	content += renderFailedToolAppendix(failed)
	if err := writeSkillFile(contentPath, content); err != nil {
		return fmt.Errorf("write skill file: %w", err)
	}
	// 脚本写盘失败不阻塞技能本体（降级为无工具技能，附审计）。
	if err := writeSkillScripts(filepath.Dir(contentPath), solid); err != nil {
		log.Printf("[evolver] write skill scripts failed (degrade to no-tools): name=%s err=%v", targetName, err)
		solid = nil
		toolNotes = append(toolNotes, "scripts 写盘失败，降级为无工具")
		content = renderLearnedSkillMD(name, title, whenToUse, outcome, sk)
		content += renderFailedToolAppendix(failed)
		if err := writeSkillFile(contentPath, content); err != nil {
			return fmt.Errorf("rewrite skill file: %w", err)
		}
	}
	hasTools := len(solid) > 0
	rec := &store.LearnedSkill{
		Name: targetName, Title: title, WhenToUse: whenToUse,
		ContentPath: contentPath, Embedding: emb,
		Enabled: true, UseCount: useCount, HasTools: hasTools,
		SourceSession: sessionID, Outcome: outcome,
	}
	if err := s.skills.Upsert(ctx, rec); err != nil {
		return fmt.Errorf("upsert learned skill: %w", err)
	}
	if s.pool != nil {
		// C2 加载注入：技能池 Content = 正文 + 「配套工具」段（load_skill 全文可见执行方式）。
		_, body := textutil.ParseFrontmatter([]byte(content))
		s.pool.Register(learnedSkillToPool(name, title, whenToUse, contentPath,
			body+textutil.SkillToolSection(solidToolMetas(solid))))
	}
	logSuffix := ""
	if len(toolNotes) > 0 {
		logSuffix = " tools: " + strings.Join(toolNotes, "；")
	}
	if err := s.skills.AppendEvolutionLog(ctx, kind, targetName,
		fmt.Sprintf("%s（%s）：%s%s", title, outcome, truncateForPrompt(whenToUse, 120), logSuffix), sessionID); err != nil {
		log.Printf("[evolver] write evolution log failed (non-fatal): err=%v", err)
	}
	log.Printf("[evolver] skill persisted: kind=%s name=%s outcome=%s has_tools=%v%s", kind, targetName, outcome, hasTools, logSuffix)
	return nil
}

// confirmNearDup 对近重复候选做 judge 确认（B3）：
// duplicate=true → 返回候选与判定理由；judge 失败/超时 → 保守合并（返回最高相似度候选）；
// duplicate=false → 不合并（返回 merged=false，走新建）。
func (s *learnedSkillSink) confirmNearDup(ctx context.Context, title, whenToUse string, sk agent.EvolvedSkill, cands []*store.LearnedSkill) (keep *store.LearnedSkill, reason string, merged bool) {
	top := cands[0]
	if s.judge == nil {
		// 未接线（极端降级）：宁可合并不灌水。
		return top, "judge 未接线，保守合并", true
	}
	res, err := s.judge(ctx, title, whenToUse, top)
	if err != nil {
		log.Printf("[evolver] near-dup judge failed (conservative merge into %s): %v", top.Name, err)
		return top, fmt.Sprintf("judge 失败保守合并：%v", err), true
	}
	if !res.Duplicate {
		log.Printf("[evolver] near-dup judge: not duplicate vs %s: %s", top.Name, res.Reason)
		return nil, "", false
	}
	if res.Reason == "" {
		res.Reason = "judge 判定近重复"
	}
	return top, res.Reason, true
}

// applyNearDupMerge 把新技能合并进既有技能（B3 确认重复路径）：
// 解析保留者正文 → steps/pitfalls union 去重（新条目剥行首序号，renderLearnedSkillMD
// 重渲染顺带消编号）→ 保留 use_count/name/title → 重嵌入 + Upsert + 技能池刷新
// + evolution_log（skill_update）。judge 给出的 merge_into 候选由 confirmNearDup 选定。
func (s *learnedSkillSink) applyNearDupMerge(ctx context.Context, sessionID string, sk agent.EvolvedSkill, keep *store.LearnedSkill, reason string) error {
	// C1 统一路径解析：PG content_path 新老格式混存，以磁盘现状为准（目录式优先）。
	keepPath := textutil.ResolveSkillMDPathFromRef(keep.ContentPath)
	// C2：近重复合并保留既有 tools 清单（frontmatter 重渲染时带上，scripts/ 目录不动）。
	var keepTools []textutil.SkillTool
	if data, err := os.ReadFile(keepPath); err == nil {
		keepTools = textutil.ParseSkillTools(data)
	}
	steps, pitfalls, verify := parseSkillSections(skillFileBody(keepPath))
	for _, st := range sk.Steps {
		steps = appendUnique(steps, stripLeadingMarker(st))
	}
	for _, p := range sk.Pitfalls {
		pitfalls = appendUnique(pitfalls, stripLeadingMarker(p))
	}
	if verify == "" {
		verify = strings.TrimSpace(sk.Verify)
	}
	newWhen := strings.TrimSpace(keep.WhenToUse)
	if w := strings.TrimSpace(sk.WhenToUse); w != "" && !strings.Contains(newWhen, w) {
		if newWhen == "" {
			newWhen = w
		} else {
			newWhen += "；" + w
		}
	}
	newWhen = truncateForPrompt(newWhen, 300)
	merged := agent.EvolvedSkill{Steps: steps, Pitfalls: pitfalls, Verify: verify}
	content := renderLearnedSkillMD(keep.Name, keep.Title, newWhen, keep.Outcome, merged, keepTools)
	if err := writeSkillFile(keepPath, content); err != nil {
		return fmt.Errorf("write merged skill file: %w", err)
	}
	emb, err := s.skills.Embed(ctx, keep.Title+" "+newWhen)
	if err != nil {
		return fmt.Errorf("embed merged skill: %w", err)
	}
	rec := &store.LearnedSkill{
		Name: keep.Name, Title: keep.Title, WhenToUse: newWhen,
		ContentPath: keepPath, Embedding: emb,
		Enabled: true, UseCount: keep.UseCount, HasTools: len(keepTools) > 0,
		SourceSession: sessionID, Outcome: keep.Outcome,
	}
	if err := s.skills.Upsert(ctx, rec); err != nil {
		return fmt.Errorf("upsert merged skill: %w", err)
	}
	if s.pool != nil {
		_, body := textutil.ParseFrontmatter([]byte(content))
		s.pool.Register(learnedSkillToPool(keep.Name, keep.Title, newWhen, keepPath,
			body+textutil.SkillToolSection(keepTools)))
	}
	srcName := normalizeSkillName(sk.Name, sk.Title)
	_ = s.skills.AppendEvolutionLog(ctx, "skill_update", keep.Name,
		fmt.Sprintf("%s（近重复合并自 %s）：%s", keep.Title, srcName, truncateForPrompt(reason, 120)), sessionID)
	log.Printf("[evolver] near-dup merged: keep=%s source=%s", keep.Name, srcName)
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
// 可选 tools（C1）：非空时 frontmatter 渲染 tools 清单；正文步骤不含 tools（C3 规则）。
func renderLearnedSkillMD(name, title, whenToUse, outcome string, sk agent.EvolvedSkill, tools ...[]textutil.SkillTool) string {
	var b strings.Builder
	b.WriteString(textutil.RenderSkillFrontmatter(name, title, whenToUse, outcome, tools...))
	b.WriteString("\n")
	if len(sk.Steps) > 0 {
		b.WriteString("## 步骤\n")
		for i, st := range sk.Steps {
			fmt.Fprintf(&b, "%d. %s\n", i+1, stripLeadingMarker(st))
		}
		b.WriteString("\n")
	}
	if len(sk.Pitfalls) > 0 {
		b.WriteString("## 坑点\n")
		for _, p := range sk.Pitfalls {
			fmt.Fprintf(&b, "- %s\n", stripLeadingMarker(p))
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

// stripLeadingMarker 剥掉条目自带的行首序号/符号（模型常返回 "1. xxx"、"2、xxx"），
// 避免渲染时再拼一层编号出现 "1. 1. xxx"。
var leadingMarkerRe = regexp.MustCompile(`^\s*(?:\d+\s*[.、．!)]\s*|[-•*]\s*)`)

func stripLeadingMarker(s string) string {
	return strings.TrimSpace(leadingMarkerRe.ReplaceAllString(s, ""))
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

// parseLearnedSkillFile 解析 skills_learned/<name>.md 或 <name>/SKILL.md（YAML frontmatter + 正文）。
// frontmatter 缺 name/title/when_to_use 任一项返回错误（孤儿修复时跳过）。
// C2：frontmatter 的 tools 清单解析后渲染「配套工具」段追加进 Content——load_skill
// 取全文时 agent 可知每个脚本的相对路径/运行命令/用途。
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
	body += textutil.SkillToolSection(textutil.ParseSkillTools(data))
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
	// 第一遍（C1 目录式优先）：<name>/SKILL.md 新格式。
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name(), "SKILL.md")
		if _, err := os.Stat(path); err != nil {
			continue // 无 SKILL.md 的普通目录（如 scripts/ 不会被列在顶层，但防御）
		}
		sk, err := parseLearnedSkillFile(path)
		if err != nil {
			log.Printf("[bootstrap] skills_learned skip %s: %v", e.Name(), err)
			continue
		}
		files[sk.SkillID] = sk
	}
	// 第二遍（老格式回退）：<name>.md 仅当同名目录式不存在时读取（老格式只读，不写回）。
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		if _, taken := files[name]; taken {
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
			// C2 静态审计点：技能脚本执行发生在会话内 agent 的 RunCommand（后端无法直接
			// 拦截），注册/加载日志记录 tools 清单作为审计依据。
			if data, err := os.ReadFile(sk.Path); err == nil {
				if tools := textutil.ParseSkillTools(data); len(tools) > 0 {
					log.Printf("[bootstrap] learned skill registered with tools: name=%s tools=%v", name, tools)
				}
			}
		}
	}
	if len(pool.All()) > 0 {
		log.Printf("[bootstrap] learned skills registered: %d", len(pool.All()))
	}
}
