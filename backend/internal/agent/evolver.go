package agent

// evolver.go 实现偏好与自进化（2026-09-02 设计 docs/superpowers/specs/
// 2026-09-02-preferences-self-evolution-design.md）的服务侧装配。
//
// 期 1（本文件）：偏好 Merge 整理——会话结束提取到偏好增量后，轻量模型对目标小节
// 做去重/冲突归档重写（人工行保护见 userprofile/merge.go）；合并不可用时降级直写
// 归档小节（v1 行为）。用户画像与项目偏好两个 Store 共用同一流程。
//
// 期 2：SessionEvolver（一次轻量模型调用产出三类沉淀），见 evolveSession。

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/logger"
	"github.com/blockmemory/agent/backend/internal/userprofile"
	"github.com/blockmemory/agent/backend/pkg/enums"
)

// PrefMerger 偏好合并回调：读当前自动行视图 + 增量，产出合并计划。
// 由 bootstrap 注入轻量模型实现；nil 时调用方降级直写归档小节。
type PrefMerger func(ctx context.Context, view userprofile.MergeView, increments []string) (userprofile.MergePlan, error)

// EvolveInput SessionEvolver 输入（设计 §6.1）：goal + 总结 + 事件流摘要 + 用户消息。
type EvolveInput struct {
	Goal         string
	Summary      string
	EventsDigest string
	UserMessages string
	Outcome      string // success|failed|mixed（失败会话的踩坑经验价值更高）
}

// EvolvedSkill 提取出的技能包（SKILL.md 式结构，设计 §6.2）。
type EvolvedSkill struct {
	Name      string   // 小写连字符全局唯一；缺省由 title 规范化
	Title     string   // 展示标题（必填）
	WhenToUse string   // 适用场景（必填，与 title 构成召回向量）
	Steps     []string // 步骤
	Pitfalls  []string // 坑点
	Verify    string   // 验证方法
}

// EvolveOutput 一次会话进化的三类沉淀（设计 §3）。
type EvolveOutput struct {
	UserPrefs      []string // ① 用户偏好增量（全局 user_profile.md，Merge 整理）
	ProjectLessons []string // ② 项目经验增量（per workDir project_preferences.md ## 项目经验）
	Skills         []EvolvedSkill // ③ 技能包 0-2 个（全局技能库）
}

// minEvolveEvents 事件数下限：琐碎问答不提取，防噪音（设计 §6.1）。
const minEvolveEvents = 5

// evolveDigestMaxRunes 事件流摘要 rune 上限。
const evolveDigestMaxRunes = 1500

// evolveTimeout SessionEvolver 单次轻量模型调用预算。
const evolveTimeout = 90 * time.Second

// evolveSession 会话结束进化入口（设计 §6.1）：一次轻量模型调用产出三类沉淀，
// 分别落用户画像（Merge）/ 项目偏好（Merge）/ 技能库（skillSink），全部写 evolution_log。
// 降级路径全部静默（记日志零副作用）：evolver 未接线回退纯画像提取（旧测试语义）；
// LLM 失败回退画像提取；Merge 失败直写归档小节（mergeIntoStore 内）。
// 用户主动取消不进此入口（setSessionError 对 context canceled 跳过）。
func (s *ReactService) evolveSession(session *reactInternalSession, outcome string) {
	if s.evolver == nil {
		s.extractProfilePreferences(session)
		return
	}
	if len(session.Events) < minEvolveEvents {
		return
	}
	in := EvolveInput{
		Goal:         strings.TrimSpace(session.Goal),
		Summary:      strings.TrimSpace(session.Result),
		EventsDigest: s.eventsDigest(session),
		UserMessages: userMessagesText(session),
		Outcome:      outcome,
	}
	if in.UserMessages == "" && in.Goal == "" {
		return
	}
	ctx, cancel := context.WithTimeout(
		logger.NewContext(context.Background(), s.sessionLogger(session.ID, "SessionEvolver")),
		evolveTimeout)
	defer cancel()
	out, err := s.evolver(ctx, in)
	if err != nil || out == nil {
		log.Printf("[evolver] evolve session failed, degrade to profile extraction: err=%v", err)
		s.extractProfilePreferences(session)
		return
	}
	agentName := "SessionEvolver"
	// ① 用户偏好增量：Merge 整理进画像 + evolution_log。
	if prefs := cleanIncrements(out.UserPrefs); len(prefs) > 0 && s.userProfile != nil {
		s.mergeIntoStore(session.ID, agentName, s.userProfile, userProfileTargets, prefs)
		s.logEvolution(session.ID, "user_pref", "用户画像", strings.Join(prefs, "；"))
	}
	// ② 项目经验增量：Merge 整理进项目偏好 + evolution_log。
	// S2：项目偏好按会话工作目录解析（session.workDir 空串回落 store 构造目录）。
	// ForContext 返回 nil（目录不可解析）时既未写入也不应记 project_lesson 审计流水。
	if lessons := cleanIncrements(out.ProjectLessons); len(lessons) > 0 && s.projectPrefs != nil {
		if st := s.projectPrefs.ForContext(tool.WithWorkDir(ctx, session.workDir)); st != nil {
			s.mergeIntoStore(session.ID, agentName, st, projectPrefsTargets, lessons)
			s.logEvolution(session.ID, "project_lesson", "项目经验", strings.Join(lessons, "；"))
		}
	}
	// ③ 技能包：skillSink 落文件+PG+evolution_log（含校验/同名更新）。
	if len(out.Skills) > 0 && s.skillSink != nil {
		sinkCtx, sinkCancel := context.WithTimeout(context.Background(), 60*time.Second)
		if err := s.skillSink(sinkCtx, session.ID, out.Skills, outcome); err != nil {
			log.Printf("[evolver] persist skills failed (non-fatal): err=%v", err)
		}
		sinkCancel()
	}
}

// SkillRecallHint MetaAgent 侧经验技能预筛提示（设计 §6.5：只注一行，不注全文）。
type SkillRecallHint struct {
	Name  string
	Title string
}

// injectSkillRecall 把向量预筛命中的经验技能提示拼到任务文本前（设计 §6.5：
// MetaAgent 收到新任务时用 goal 预筛 top-3 enabled 技能，命中只注一行提示）。
// 回调未接线/无命中/任务为空时原样返回（零注入）。
func (s *ReactService) injectSkillRecall(ctx context.Context, goal string) string {
	if s.skillRecall == nil || strings.TrimSpace(goal) == "" {
		return goal
	}
	hints := s.skillRecall(ctx, goal)
	if len(hints) == 0 {
		return goal
	}
	var b strings.Builder
	b.WriteString("【相关经验】\n以下经验技能与当前任务相关，可用 load_skill(名称) 获取完整工艺指引：\n")
	for _, h := range hints {
		b.WriteString("- ")
		b.WriteString(h.Name)
		b.WriteString(" —— ")
		b.WriteString(h.Title)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	return b.String() + goal
}

// logEvolution 写一条进化审计流水（evolutionLog 未接线时静默跳过）。
func (s *ReactService) logEvolution(sessionID, kind, target, summary string) {
	if s.evolutionLog == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.evolutionLog(ctx, sessionID, kind, target, summary); err != nil {
		log.Printf("[evolver] write evolution log failed (non-fatal): kind=%s err=%v", kind, err)
	}
}

// eventsDigest 事件流摘要：最近事件拼接（Agent: 消息），整体截断。
func (s *ReactService) eventsDigest(session *reactInternalSession) string {
	const keep = 30
	events := session.Events
	if len(events) > keep {
		events = events[len(events)-keep:]
	}
	var b strings.Builder
	for _, ev := range events {
		agent := ev.Agent
		if agent == "" {
			agent = ev.Type
		}
		line := strings.TrimSpace(ev.Message)
		if line == "" {
			continue
		}
		if ev.Tool != "" {
			line = ev.Tool + " " + line
		}
		b.WriteString(agent)
		b.WriteString(": ")
		b.WriteString(truncateRunesForEvolver(line, 120))
		b.WriteString("\n")
	}
	return truncateRunesForEvolver(strings.TrimRight(b.String(), "\n"), evolveDigestMaxRunes)
}

// userMessagesText 拼接会话用户消息（提取输入的一部分）。
func userMessagesText(session *reactInternalSession) string {
	var sb strings.Builder
	for _, m := range session.Messages {
		if m.Role == string(enums.ChatRoleUser) {
			sb.WriteString(m.Content)
			sb.WriteByte('\n')
		}
	}
	return truncateRunesForEvolver(strings.TrimSpace(sb.String()), 4000)
}

// cleanIncrements 去空白去空串。
func cleanIncrements(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func truncateRunesForEvolver(s string, max int) string {
	if len([]rune(s)) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max]) + "…"
}

// profileMergeTimeout 单次合并轻量模型调用预算（CallLightweightWithRetry 内部
// 单次流式 30s + 重试，外层预算需覆盖）。
const profileMergeTimeout = 90 * time.Second

// userProfileTargets 用户画像 Merge 的目标小节（设计 §4）。
var userProfileTargets = []string{"偏好", "技术栈", "沟通风格"}

// projectPrefsTargets 项目偏好 Merge 的目标小节（设计 §5：项目经验自动沉淀；
// 项目约定是人工小节，只作为 MergeView 上下文供模型参照，不写自动行）。
var projectPrefsTargets = []string{"项目经验"}

// mergeIntoStore 把偏好增量合并进目标 store（用户画像或项目偏好）：
// 1) prefMerger 可用 → 轻量模型产出 MergePlan → ApplyMerge（人工行保护在 store 侧）；
// 2) 合并调用失败/计划为空 → 降级直写归档小节（v1 行为，零信息丢失）。
// 目标小节维持 store 当前结构；计划为空且增量为空时零副作用。
func (s *ReactService) mergeIntoStore(sessionID, agentName string, store *userprofile.Store, targets []string, increments []string) {
	if store == nil || len(increments) == 0 {
		return
	}
	if s.prefMerger != nil {
		ctx, cancel := context.WithTimeout(
			logger.NewContext(context.Background(), s.sessionLogger(sessionID, agentName)),
			profileMergeTimeout)
		plan, err := s.prefMerger(ctx, store.MergeView(targets), increments)
		cancel()
		if err == nil {
			if err := store.ApplyMerge(plan, targets); err == nil {
				return
			}
			log.Printf("[evolver] apply merge plan failed, degrade to append: err=%v", err)
		} else {
			log.Printf("[evolver] merge llm failed, degrade to append: err=%v", err)
		}
	}
	for _, inc := range increments {
		if err := store.Append(store.ArchiveSection(), inc); err != nil {
			log.Printf("[evolver] append increment to archive failed: err=%v", err)
		}
	}
}
