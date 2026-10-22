package agent

// decision_ledger.go —— 会话决策账本（2026-10-22 上下文事故修复）。
//
// 事故复盘（session-1792579233967105600-0766b099-61）：
//  1. 首条 user 消息（session.Goal）被 windowMessages 永久保留，后续"改用图集"的
//     路线变更从不改写它，模型重新规划时永远从陈旧目标起笔；
//  2. 用户经 ask_user 给出的关键裁决（"帧渲染是正确做法""要到这种类型"）只作为
//     tool result 存在，压缩器按工具输出截断/丢弃；
//  3. 历史压缩摘要里的【已定决策】在 400 条滑窗裁剪时被整段扔掉。
// 结果：正确指令在上下文里权重最低，错误路线每轮全文注入，模型在错误方向上跑了 3 小时。
//
// 本文件把"用户说过什么/验收基准是什么/什么已被否决"从对话历史中独立出来：
// append-only 落 PG（session_decisions），每轮以固定 system 块注入，不参与压缩。
// 它不替代压缩摘要，只提供一个不可被裁剪的锚。

import (
	"context"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/blockmemory/agent/backend/internal/store"
	"github.com/blockmemory/agent/backend/pkg/textutil"
)

// sessionDecisionCache 是会话内账本缓存（权威副本在 PG session_decisions）。
// 独立成指针类型：reactInternalSession 会被浅拷贝，内嵌 mutex 触发 copylocks。
type sessionDecisionCache struct {
	mu     sync.RWMutex
	items  []store.SessionDecision
	loaded bool
}

// newSessionDecisionCache 创建空缓存（createSession/buildRestoredSession 初始化）。
func newSessionDecisionCache() *sessionDecisionCache { return &sessionDecisionCache{} }

// decisionCache 返回会话缓存，nil 时惰性初始化（测试/老结构兜底）。
func sessionDecisionCacheOf(session *reactInternalSession) *sessionDecisionCache {
	if session.decisions == nil {
		session.decisions = newSessionDecisionCache()
	}
	return session.decisions
}

// decisionCacheMax 单会话内存账本上限（防长会话无限增长；权威账本在 PG）。
const decisionCacheMax = 96

// decisionBlockMaxRunes 注入块总长上限（长会话也不能把上下文挤爆）。
const decisionBlockMaxRunes = 1400

// decisionContentMaxRunes 单条账本内容上限。取值放宽到 4000：任务书全文要能进账本
// （首条 user 消息被压缩掉时，账本是唯一还带路线原文的锚）；注入侧只渲染 routeHeadline
// 短摘录，不会把 4000 字灌回上下文。
const decisionContentMaxRunes = 4000

// routeDirectiveMarkers 路线级改向标记：命中即视为"用户在改方向"，会废止上一条路线。
// 保守取值：只认明确的改向/选型语气，避免"渲染给我看"这类动作句误判成新路线。
var routeDirectiveMarkers = []string{
	"改用", "换成", "改成", "不要用", "别用", "不再用", "不用", "排除", "取消", "作废",
	"不是", "而是", "正确做法", "要到这种", "这种类型", "继续之前", "使用", "采用",
	"要用", "必须用", "得用", "按这个做", "按这个来", "就用", "替换为", "替代",
}

// negativeFeedbackMarkers 用户否定当前成果/路线的标记。命中即写 rejection 并把
// 现行路线标为 rejected——防止模型"用户骂了但继续打补丁"。
var negativeFeedbackMarkers = []string{
	"做的什么玩意", "什么玩意", "为什么这么差", "为什么做的这么差", "太差", "垃圾",
	"重做", "做错", "搞错", "跑偏", "不满意", "难看", "不能用",
}

// routeTopicGroups 路线主题分组：同一组内的词视为同一方案，跨组即互斥。
// 仅用于计划审批前的冲突提示（不是硬拦截）。
var routeTopicGroups = [][]string{
	{"骨架", "拼接", "骨骼", "spine", "montage"},
	{"图集", "序列帧", "整帧", "spriteatlas", "atlas"},
	{"ai视频", "视频生成", "文生视频", "i2v", "首帧"},
}

// strongNegationMarkers 命中即整分句丢弃：任务书里"明确排除/已否决/不要…"的分句
// 不能参与路线主题判定（否则被排除的方案反而成了现行路线）。
var strongNegationMarkers = []string{
	"排除", "否决", "禁止", "不要", "不用", "别用", "不得", "不再", "放弃", "严禁", "不做",
	"废弃", "过时", "淘汰", "不适合", "不采用", "不建议", "早期", "口头",
}

// weakNegationMarkers 只删除"标记→下一个顿号/逗号/句号"这一小段，保留同句正面内容。
// 例：「用已有素材（禁 AI 重生图、禁拆部位）交付 16 帧图集」只去掉两个"禁…"片段。
var weakNegationMarkers = []string{"禁"}

// ledgerEllipsis 账本行内截断统一用省略号，绝不用 "(truncated)"：
// 权威块里出现"被截断"字样会让模型以为服务端还藏着一份全文，转而满文件系统找
// （2026-10-22 23:00-23:06 实证：连续 20 轮 grep/cat /tmp/bma/ledger_block.txt，任务停摆）。
func ledgerEllipsis(s string, n int) string {
	return textutil.TruncateRunes(s, n, "…")
}

// positiveTopicText 去掉否定内容并归一化空白/标点，供主题词匹配。
// 例：「明确排除：骨架拼接、部位拼接」整句丢弃；「16 帧图集」归一为「16帧图集」；
// 「（禁 AI 重生图、禁拆部位）」只删两个否定片段，保留同句的正面路线。
func positiveTopicText(s string) string {
	s = strings.ToLower(s)
	clauses := strings.FieldsFunc(s, func(r rune) bool {
		switch r {
		case '。', '；', ';', '，', ',', '\n', '\r', '！', '!', '？', '?':
			return true
		}
		return false
	})
	kept := make([]string, 0, len(clauses))
	for _, c := range clauses {
		if containsAnyMarker(c, strongNegationMarkers) {
			continue
		}
		kept = append(kept, stripWeakNegations(c))
	}
	var b strings.Builder
	for _, r := range strings.Join(kept, " ") {
		switch r {
		case ' ', '\t', '\r', '\n', '　', '*', '`', '#', '>', '|', '-', '~', '"', '\'', '：', ':', '（', '）', '(', ')', '、', '「', '」', '“', '”':
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func containsAnyMarker(s string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// stripWeakNegations 删除"否定标记 → 下一个顿号/逗号/句号"的小片段（按 rune 安全切除）。
func stripWeakNegations(s string) string {
	rs := []rune(s)
	for _, m := range weakNegationMarkers {
		mr := []rune(m)
		for i := 0; i+len(mr) <= len(rs); {
			if string(rs[i:i+len(mr)]) != m {
				i++
				continue
			}
			end := len(rs)
			for k := i + len(mr); k < len(rs); k++ {
				if strings.ContainsRune("、，,;；。)", rs[k]) {
					end = k
					break
				}
			}
			rs = append(rs[:i], rs[end:]...)
		}
	}
	return string(rs)
}

// containsGroupWord 判断文本是否仍正面提到该组的关键词（正面提到=计划覆盖了现行路线）。
func containsGroupWord(text string, group int) bool {
	if group < 0 || group >= len(routeTopicGroups) {
		return false
	}
	for _, kw := range routeTopicGroups[group] {
		if strings.Contains(text, kw) {
			return true
		}
	}
	return false
}

// routeHeadline 把任务书/路线指令压成注入用的短摘录：首行标题 + 首个「路线/目标」小节，
// 在行/句边界截断。完整任务书由首条 user 消息承载（resume 时由 ensureGoalMessage 保证
// 在场），账本只做不可裁剪的锚，不再重复整篇任务书。
func routeHeadline(content string, max int) string {
	lines := strings.Split(strings.TrimSpace(content), "\n")
	var title string
	var body []string
	inRoute := false
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, "#") {
			head := strings.TrimSpace(strings.TrimLeft(t, "#"))
			if title == "" {
				title = head
				continue
			}
			// 进入或离开「路线/目标」小节。
			inRoute = strings.Contains(head, "路线") || strings.Contains(head, "目标")
			continue
		}
		if title == "" {
			title = t
			continue
		}
		if inRoute {
			body = append(body, t)
		} else if len(body) == 0 && len(title) < 40 {
			// 无「路线」小节时退化为前几行正文，够模型认出方向即可。
			body = append(body, t)
		}
	}
	out := title
	if len(body) > 0 {
		out += "：" + strings.Join(body, " ")
	}
	if max > 0 && utf8.RuneCountInString(out) > max {
		cut := []rune(out)
		cut = cut[:max]
		for _, sep := range []string{"。", "；", "，", " ", "、"} {
			if i := strings.LastIndex(string(cut), sep); i > len(string(cut))/2 {
				cut = []rune(string(cut)[:i])
				break
			}
		}
		out = string(cut) + "…"
	}
	return out
}

// referencePathRe 匹配用户消息/答复中的参考产物路径（视频/图片/网页）。
var referencePathRe = regexp.MustCompile(`(?:[A-Za-z]:[\\/]|/)[^\s"'<>|]+\.(?:mp4|webm|mov|mkv|png|jpe?g|gif|webp|svg|html)`)

// SetDecisionStore 注入会话决策账本存储（bootstrap 装配）。nil 时账本仅内存生效。
func (s *ReactService) SetDecisionStore(st *store.SessionDecisionStore) {
	s.decisionStore = st
}

// recordDecision 追加一条账本记录（best-effort：落库失败只告警，不影响主流程）。
// 语义：
//   - 路线级指令（isRouteDirective）→ 旧 active route 全部 superseded；
//   - 用户否定（isNegativeFeedback）→ 写 rejection 并把 active route 标 rejected；
//   - 内容里出现参考产物路径 → 额外写 reference（验收基准）。
func (s *ReactService) recordDecision(ctx context.Context, session *reactInternalSession, kind, content, source string) {
	if session == nil {
		return
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return
	}
	content = ledgerEllipsis(content, decisionContentMaxRunes)
	if kind == "" {
		kind = store.DecisionKindDirective
	}
	if isNegativeFeedback(content) {
		kind = store.DecisionKindRejection
	} else if kind == store.DecisionKindDirective && isRouteDirective(content) {
		kind = store.DecisionKindRoute
	}

	dc := sessionDecisionCacheOf(session)
	dc.mu.Lock()
	// 去重：最近 8 条里同 kind 同内容不重复写（重启/重跑/多入口重复调用）。
	for i := len(dc.items) - 1; i >= 0 && i >= len(dc.items)-8; i-- {
		if dc.items[i].Kind == kind && dc.items[i].Content == content {
			dc.mu.Unlock()
			return
		}
	}
	d := store.SessionDecision{
		SessionID: session.ID,
		Kind:      kind,
		Content:   content,
		Source:    source,
		Status:    store.DecisionStatusActive,
		CreatedAt: time.Now(),
	}
	// 先落库拿 ID（失败降级为纯内存；2s 超时避免拖慢主循环）。
	if s.decisionStore != nil {
		ictx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		if id, err := s.decisionStore.Append(ictx, d); err == nil {
			d.ID = id
		} else {
			log.Printf("[decision] append failed session=%s kind=%s err=%v", session.ID, kind, err)
		}
		cancel()
	}
	dc.items = append(dc.items, d)
	// 状态翻转只改视图标记，不物理删行（底账 append-only 纪律）。
	switch kind {
	case store.DecisionKindRoute:
		// 按切片下标排除刚追加的这一条：纯内存模式（store=nil）所有行 ID 都是 0，
		// 用 ID 比较会把新路线也一起标成 superseded。
		self := len(dc.items) - 1
		for i := range dc.items {
			if i == self {
				continue
			}
			if dc.items[i].Kind == store.DecisionKindRoute &&
				dc.items[i].Status == store.DecisionStatusActive {
				dc.items[i].Status = store.DecisionStatusSuperseded
			}
		}
		if s.decisionStore != nil {
			ictx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			_, _ = s.decisionStore.Supersede(ictx, session.ID, []string{store.DecisionKindRoute}, d.ID)
			cancel()
		}
	case store.DecisionKindRejection:
		for i := range dc.items {
			if dc.items[i].Kind == store.DecisionKindRoute &&
				dc.items[i].Status == store.DecisionStatusActive {
				dc.items[i].Status = store.DecisionStatusRejected
			}
		}
		if s.decisionStore != nil {
			ictx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			_, _ = s.decisionStore.MarkRejected(ictx, session.ID, []string{store.DecisionKindRoute})
			cancel()
		}
	}
	// 参考产物 → 验收基准（独立行，供 decisionBlock 单独置顶）。
	if kind == store.DecisionKindDirective || kind == store.DecisionKindClarify ||
		kind == store.DecisionKindRoute || kind == store.DecisionKindRejection {
		for _, p := range extractReferencePaths(content) {
			ref := store.SessionDecision{
				SessionID: session.ID, Kind: store.DecisionKindReference, Content: p,
				Source: source, Status: store.DecisionStatusActive, CreatedAt: time.Now(),
			}
			if s.decisionStore != nil {
				ictx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
				if id, err := s.decisionStore.Append(ictx, ref); err == nil {
					ref.ID = id
				}
				cancel()
			}
			dc.items = append(dc.items, ref)
		}
	}
	if len(dc.items) > decisionCacheMax {
		dc.items = append([]store.SessionDecision(nil), dc.items[len(dc.items)-decisionCacheMax:]...)
	}
	dc.mu.Unlock()
}

// hydrateDecisions 首次使用时从 PG 懒加载账本；无记录时把会话目标播种为普通指令
// （不标 route——老会话的目标可能早已被后续消息修订，避免把一个陈旧目标再度扶正）。
func (s *ReactService) hydrateDecisions(ctx context.Context, session *reactInternalSession) {
	if session == nil {
		return
	}
	dc := sessionDecisionCacheOf(session)
	dc.mu.Lock()
	loaded := dc.loaded
	dc.loaded = true
	dc.mu.Unlock()
	if loaded {
		return
	}
	if s.decisionStore == nil {
		if strings.TrimSpace(session.Goal) != "" {
			s.recordDecision(ctx, session, store.DecisionKindDirective, session.Goal, "goal")
		}
		return
	}
	ictx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	list, err := s.decisionStore.List(ictx, session.ID, decisionCacheMax)
	cancel()
	if err != nil {
		log.Printf("[decision] hydrate failed session=%s err=%v", session.ID, err)
	}
	if len(list) > 0 {
		dc.mu.Lock()
		dc.items = list
		dc.mu.Unlock()
		return
	}
	if strings.TrimSpace(session.Goal) != "" {
		s.recordDecision(ctx, session, store.DecisionKindDirective, session.Goal, "goal")
	}
}

// decisionBlock 渲染每轮注入的【用户指令账本】。空账本返回空串（零注入）。
func (s *ReactService) decisionBlock(session *reactInternalSession) string {
	if session == nil {
		return ""
	}
	dc := sessionDecisionCacheOf(session)
	dc.mu.RLock()
	decisions := append([]store.SessionDecision(nil), dc.items...)
	dc.mu.RUnlock()
	if len(decisions) == 0 {
		return ""
	}
	var current *store.SessionDecision
	var firstDirective *store.SessionDecision
	var superseded, rejected []store.SessionDecision
	var refs []store.SessionDecision
	for i := range decisions {
		d := &decisions[i]
		switch d.Kind {
		case store.DecisionKindRoute:
			switch d.Status {
			case store.DecisionStatusActive:
				current = d
			case store.DecisionStatusSuperseded:
				superseded = append(superseded, *d)
			case store.DecisionStatusRejected:
				rejected = append(rejected, *d)
			}
		case store.DecisionKindDirective:
			if firstDirective == nil {
				firstDirective = d
			}
		case store.DecisionKindReference:
			if d.Status == store.DecisionStatusActive {
				refs = append(refs, *d)
			}
		}
	}
	var b strings.Builder
	b.WriteString("【用户指令账本】（服务端维护·不参与压缩·最高优先级）\n")
	b.WriteString("规则：本块与任何压缩摘要/项目偏好/用户画像/项目经验冲突时，一律以本块为准。")
	b.WriteString("若当前工作方向与本块【现行路线】不一致，必须停下来说明冲突并 ask_user 确认，禁止直接按旧方向继续。\n")
	if current != nil {
		b.WriteString("【现行路线】" + routeHeadline(current.Content, 420) + "\n")
		// 任务书全文由首条 user 消息承载；只在"这条路线就是任务书"或摘录确实短于原文时提示，
		// 避免模型以为账本被截断而去文件系统/临时目录里找"完整账本"。
		if current.Source == "goal" || utf8.RuneCountInString(current.Content) > 420 {
			b.WriteString("（任务书全文见会话首条用户消息，始终有效；本行只摘录路线要点）\n")
		}
	} else if firstDirective != nil {
		b.WriteString("【最初目标·可能已被后续指令修订】" + routeHeadline(firstDirective.Content, 300) + "\n")
	}
	for _, d := range rejected {
		b.WriteString("【已否决·禁止继续】" + ledgerEllipsis(d.Content, 120) + "（" + d.CreatedAt.Format("01-02 15:04") + "）\n")
	}
	if len(rejected) > 0 {
		b.WriteString("【行动要求】当前路线已被用户否决：停止在旧方案上继续修补；先向用户确认新方向，或严格按【验收基准】重做。\n")
	}
	// 已废止路线保留最近 2 条（每条 100 runes）：让模型知道"这条路被用户改掉了"，
	// 又不把整段旧指令灌回上下文（2026-10-22 用户反馈：已否决内容本身也是污染）。
	if len(superseded) > 2 {
		superseded = superseded[len(superseded)-2:]
	}
	for _, d := range superseded {
		b.WriteString("【已废止·禁止再采用】" + truncateRunes(d.Content, 100) + "（" + d.CreatedAt.Format("01-02 15:04") + "）\n")
	}
	if len(refs) > 0 {
		// 保留最近 3 条；每条整行输出（路径本身就是短串），不做"半句截断"。
		if len(refs) > 3 {
			refs = refs[len(refs)-3:]
		}
		b.WriteString("【验收基准·用户给的参考产物】\n")
		seen := map[string]bool{}
		for _, r := range refs {
			if seen[r.Content] {
				continue
			}
			seen[r.Content] = true
			b.WriteString("- " + ledgerEllipsis(r.Content, 200) + "\n")
		}
	}
	// 最近指令（旧→新）：三类都列，最后一条最新。跳过 reference（上面已单列）。
	type row struct {
		ts   time.Time
		tag  string
		text string
	}
	var rows []row
	for _, d := range decisions {
		var tag string
		switch d.Kind {
		case store.DecisionKindDirective:
			tag = "指令"
		case store.DecisionKindRoute:
			tag = "路线"
		case store.DecisionKindClarify:
			tag = "澄清答复"
		case store.DecisionKindPlan:
			tag = "已批准计划"
		case store.DecisionKindRejection:
			tag = "否决"
		default:
			continue
		}
		rows = append(rows, row{d.CreatedAt, tag, d.Content})
	}
	// 同文本只留最新一条（连续"继续/渲染给我看"不重复占额度）。
	seenRow := map[string]bool{}
	var deduped []row
	for i := len(rows) - 1; i >= 0; i-- {
		if seenRow[rows[i].text] {
			continue
		}
		seenRow[rows[i].text] = true
		deduped = append([]row{rows[i]}, deduped...)
	}
	rows = deduped
	if len(rows) > 5 {
		rows = rows[len(rows)-5:]
	}
	if len(rows) > 0 {
		b.WriteString("【最近用户指令（旧→新，最后一条最新）】\n")
		for _, r := range rows {
			b.WriteString("- " + r.ts.Format("01-02 15:04") + " [" + r.tag + "] " + ledgerEllipsis(r.text, 120) + "\n")
		}
	}
	out := strings.TrimRight(b.String(), "\n")
	return ledgerEllipsis(out, decisionBlockMaxRunes)
}

// planConflictWarning 在计划审批卡生成前比对"计划路线 vs 现行路线"，命中互斥主题
// 或明显"替换/替代"语句时返回警示文本（拼在计划详情顶部，用户批准前可见）。
// 2026-10-22 事故：14:23 的计划写着"替代此前的 16 帧整图序列"，审批卡没有任何提示，
// 用户秒点"批准开工"，整条错误路线由此合法化。
func (s *ReactService) planConflictWarning(session *reactInternalSession, planText, question string) string {
	if session == nil {
		return ""
	}
	text := strings.ToLower(question + "\n" + planText)
	// 现行路线 = 最新 active route；没有则退回最早 directive（老会话兼容）。
	dc := sessionDecisionCacheOf(session)
	dc.mu.RLock()
	decisions := append([]store.SessionDecision(nil), dc.items...)
	dc.mu.RUnlock()
	var current *store.SessionDecision
	var first *store.SessionDecision
	for i := range decisions {
		d := &decisions[i]
		if d.Kind == store.DecisionKindRoute && d.Status == store.DecisionStatusActive {
			current = d
		}
		if d.Kind == store.DecisionKindDirective && first == nil {
			first = d
		}
	}
	route := current
	if route == nil {
		route = first
	}
	if route == nil {
		return ""
	}
	planPos := positiveTopicText(text)
	routeGroup := topicGroupOf(route.Content)
	planGroup := topicGroupOf(text)
	// 只有"计划正面临摹了另一组、且完全没提现行路线那组"才算互斥。
	// 任务书里常有「明确排除：骨架拼接」这类否定句——已由 positiveTopicText 剔除，
	// 否则会把 16 帧图集计划误报成"属于另一条互斥路线"（2026-10-22 23:06 实证）。
	if routeGroup >= 0 && planGroup >= 0 && routeGroup != planGroup && !containsGroupWord(planPos, routeGroup) {
		return "⚠【决策冲突·审批前请确认】现行路线是「" + routeHeadline(route.Content, 160) +
			"」，本计划属于另一条互斥路线。批准即表示推翻现行路线；如非本意请选「需要修改」。\n\n"
	}
	if strings.Contains(text, "替代此前") || strings.Contains(text, "替换此前") ||
		strings.Contains(text, "取代此前") || strings.Contains(text, "替代之前") {
		return "⚠【决策冲突·审批前请确认】本计划声明要替代此前方案。现行路线：「" +
			routeHeadline(route.Content, 160) + "」。批准前请确认要让哪一条作废。\n\n"
	}
	return ""
}

// topicGroupOf 返回文本命中的路线主题组下标；未命中返回 -1。
// 先剔除否定分句并归一空白（「16 帧图集」≡「16帧图集」），再取命中词最多的组，
// 避免"明确排除：骨架拼接"或"禁拆部位"污染分组。
func topicGroupOf(text string) int {
	text = positiveTopicText(text)
	best, bestHits := -1, 0
	for gi, group := range routeTopicGroups {
		hits := 0
		for _, kw := range group {
			// 按出现次数计权：任务书常在标题+路线段各写一次"图集序列帧"，
			// 只数"命中几个不同关键词"会让只提一次的"骨架拼接"打平甚至胜出。
			hits += strings.Count(text, kw)
		}
		if hits > bestHits {
			best, bestHits = gi, hits
		}
	}
	return best
}

// isRouteDirective 判断一条用户文本是否属于路线级改向。
func isRouteDirective(s string) bool {
	s = strings.ToLower(s)
	for _, m := range routeDirectiveMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// isNegativeFeedback 判断用户是否在否定当前成果/路线。
func isNegativeFeedback(s string) bool {
	s = strings.ToLower(s)
	for _, m := range negativeFeedbackMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// extractReferencePaths 从文本中提取参考产物路径（去重保序，最多 3 条）。
func extractReferencePaths(s string) []string {
	matches := referencePathRe.FindAllString(s, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range matches {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
		if len(out) >= 3 {
			break
		}
	}
	return out
}

// clarifyAnswerApproved 判断一次澄清答复是否为"批准"。优先看选项 ID
// （approve/confirm），自由文本按关键词兜底。用于计划确认答复入账。
func clarifyAnswerApproved(pc *ClarifyRequest, answer string) bool {
	if pc != nil {
		for _, id := range pc.AnswerOptionIDs {
			if id == "approve" || id == "confirm" {
				return true
			}
		}
	}
	a := strings.ToLower(strings.TrimSpace(answer))
	return strings.Contains(a, "批准") || strings.Contains(a, "同意") || strings.Contains(a, "approve")
}

// planSummaryFromClarify 从计划确认卡里提取一句话计划摘要（任务简介 + 计划首行），
// 写入账本 kind=plan，供后续轮次回溯"用户批准过什么"。
func planSummaryFromClarify(pc *ClarifyRequest) string {
	if pc == nil {
		return ""
	}
	summary := ""
	for _, line := range strings.Split(pc.Question, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "任务简介:") {
			summary = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "任务简介:"))
			break
		}
	}
	first := ""
	for _, line := range strings.Split(pc.Detail, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			first = t
			break
		}
	}
	switch {
	case summary != "" && first != "":
		return truncateRunes(summary, 200) + " | " + truncateRunes(first, 200)
	case summary != "":
		return truncateRunes(summary, 300)
	default:
		return truncateRunes(first, 300)
	}
}
