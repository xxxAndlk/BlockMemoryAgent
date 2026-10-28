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
// 2026-10-24：1400 → 2200——多轴账本需要同时容纳「各轴现行决策 + 最新指令 + 审批门 +
// 验收标准」，旧预算下这些必留段放不下，而旧实现是最后整体截尾，恰好砍掉最新指令。
// 现在由 assembleLedger 按优先级装配：必留段先占，可选段放不下就整段跳过。
const decisionBlockMaxRunes = 2200

// decisionContentMaxRunes 单条账本内容上限。取值放宽到 4000：任务书全文要能进账本
// （首条 user 消息被压缩掉时，账本是唯一还带路线原文的锚）；注入侧只渲染 routeHeadline
// 短摘录，不会把 4000 字灌回上下文。
const decisionContentMaxRunes = 4000

// routeDirectiveMarkers 路线级改向标记：命中即视为"用户在改方向"，会废止上一条路线。
// 保守取值：只认明确的改向/选型语气，避免"渲染给我看"这类动作句误判成新路线。
var routeDirectiveMarkers = []string{
	"改用", "换成", "改成", "不要用", "别用", "不再用", "不用", "不走", "排除", "取消", "作废",
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
//   - 路线级指令（isRouteDirective）→ **同轴**旧 active route superseded（2026-10-24 多轴账本）；
//   - 用户否定（isNegativeFeedback）→ 写 rejection 并把 active route 标 rejected；
//   - 内容里出现参考产物路径 → 额外写 reference（验收基准）。
func (s *ReactService) recordDecision(ctx context.Context, session *reactInternalSession, kind, content, source string) {
	s.recordDecisionAxis(ctx, session, kind, "", content, source)
}

// recordDecisionAxis 追加一条带决策轴的账本记录（2026-10-24 多轴账本）。
//
// axis 为空时按内容自动分类（classifyDecisionAxis）；显式传入用于审批门等无法从
// 文本推断的场景（approval_anchor / approval_keyframe）。
// 轴语义见 doc/计划_媒体动画与记忆质量修复.md §5.1：同轴互斥、跨轴同时有效。
func (s *ReactService) recordDecisionAxis(ctx context.Context, session *reactInternalSession, kind, axis, content, source string) {
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
	axis = strings.TrimSpace(axis)
	if axis == "" {
		axis = classifyDecisionAxis(content)
	}
	switch {
	case isNegativeFeedback(content):
		// 否决作用于"当前成果/做法"，不落具体轴——否则一句"这个动作不对"会误撤
		// 视角、审批门、验收标准等其他维度的约束。
		kind = store.DecisionKindRejection
		axis = store.AxisGeneral
	case kind == store.DecisionKindCriteria:
		axis = store.AxisAcceptance
	case kind == store.DecisionKindPlan || kind == store.DecisionKindReference:
		axis = store.AxisGeneral
	case (kind == store.DecisionKindDirective || kind == store.DecisionKindClarify) &&
		isConstraintAxis(axis):
		// 约束轴上的任何用户表述都算该轴的"现行选择"（不只是命中改向关键词的句子）：
		// "动画用 16 帧图集""视角就按正面 45 度"同样要成为该轴的现行约束，
		// 否则后一句"重新 AI 生图"就没东西可共存，多轴语义形同虚设。
		kind = store.DecisionKindRoute
	}

	dc := sessionDecisionCacheOf(session)
	dc.mu.Lock()
	// 去重：最近 8 条里同 kind 同轴同内容不重复写（重启/重跑/多入口重复调用）。
	for i := len(dc.items) - 1; i >= 0 && i >= len(dc.items)-8; i-- {
		if dc.items[i].Kind == kind && dc.items[i].Axis == axis && dc.items[i].Content == content {
			dc.mu.Unlock()
			return
		}
	}
	d := store.SessionDecision{
		SessionID: session.ID,
		Kind:      kind,
		Axis:      axis,
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
			log.Printf("[decision] append failed session=%s kind=%s axis=%s err=%v", session.ID, kind, axis, err)
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
			if dc.items[i].Kind == store.DecisionKindRoute && dc.items[i].Axis == axis &&
				dc.items[i].Status == store.DecisionStatusActive {
				dc.items[i].Status = store.DecisionStatusSuperseded
			}
		}
		if s.decisionStore != nil {
			ictx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			// 2026-10-24：只替换**同轴**的旧路线（SupersedeAxis），不再一刀切。
			_, _ = s.decisionStore.SupersedeAxis(ictx, session.ID, axis, d.ID, []string{store.DecisionKindRoute})
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
	case store.DecisionKindDirective:
		// 审批门同轴只留最新一条（重新审批 / 撤回批准）：旧审批标 superseded，
		// 不物理删，历史可查；否则同一轴会有两条 active 行、注入侧要猜以哪条为准。
		if axis != store.AxisApprovalAnchor && axis != store.AxisApprovalKeyframe {
			break
		}
		self := len(dc.items) - 1
		for i := range dc.items {
			if i == self {
				continue
			}
			if dc.items[i].Kind == store.DecisionKindDirective && dc.items[i].Axis == axis &&
				dc.items[i].Status == store.DecisionStatusActive {
				dc.items[i].Status = store.DecisionStatusSuperseded
			}
		}
		if s.decisionStore != nil {
			ictx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			_, _ = s.decisionStore.SupersedeAxis(ictx, session.ID, axis, d.ID, []string{store.DecisionKindDirective})
			cancel()
		}
	}
	// 参考产物 → 验收基准（独立行，供 decisionBlock 单独置顶）。
	if kind == store.DecisionKindDirective || kind == store.DecisionKindClarify ||
		kind == store.DecisionKindRoute || kind == store.DecisionKindRejection {
		refs := extractReferencePaths(content)
		for _, p := range refs {
			ref := store.SessionDecision{
				SessionID: session.ID, Kind: store.DecisionKindReference, Axis: store.AxisGeneral,
				Content: p, Source: source, Status: store.DecisionStatusActive, CreatedAt: time.Now(),
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
		// 换参考资产才使旧审批门失效（规则 6）：改路线/换动画方式不得隐式清掉批准。
		if kind == store.DecisionKindRoute && len(refs) > 0 &&
			(axis == store.AxisIdentity || axis == store.AxisAssetSource) {
			s.invalidateApprovalsLocked(ctx, session.ID, dc, refs)
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
//
// 2026-10-24 多轴版本（P1）：
//   - 按决策轴渲染"现行决策"：同轴只留最新，跨轴同时有效（视角/审批门/验收标准
//     不因"重新 AI 生图"之类指令被隐式清除）；
//   - 预算裁剪确定性：必留段（规则/各轴/最新指令/审批门/验收标准）先装配，
//     可选段（旧参考/已废止/最近指令）放不下就整段跳过——绝不最后整体尾巴一刀切
//     （那会恰好砍掉最新指令，是 10-22 事故的成因之一）。
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
	var firstDirective *store.SessionDecision
	var criteria, latestRoute *store.SessionDecision
	var superseded, rejected, refs []store.SessionDecision
	// axisCurrent：轴 → 最新 active 路线行（同轴互斥后理论上唯一，兜底取最后一条）。
	axisCurrent := map[string]*store.SessionDecision{}
	axisOrder := make([]string, 0, 4)
	for i := range decisions {
		d := &decisions[i]
		switch d.Kind {
		case store.DecisionKindRoute:
			switch d.Status {
			case store.DecisionStatusActive:
				latestRoute = d
				if _, ok := axisCurrent[d.Axis]; !ok {
					axisOrder = append(axisOrder, d.Axis)
				}
				axisCurrent[d.Axis] = d
			case store.DecisionStatusSuperseded:
				superseded = append(superseded, *d)
			case store.DecisionStatusRejected:
				rejected = append(rejected, *d)
			}
		case store.DecisionKindDirective:
			// 审批门以普通指令行承载（content=approved/rejected ref=...）。
			if d.Axis == store.AxisApprovalAnchor || d.Axis == store.AxisApprovalKeyframe {
				continue
			}
			if firstDirective == nil {
				firstDirective = d
			}
		case store.DecisionKindCriteria:
			criteria = d // 列表按时间序，最后一条即最新批准计划的验收标准
		case store.DecisionKindReference:
			if d.Status == store.DecisionStatusActive {
				refs = append(refs, *d)
			}
		}
	}

	var b strings.Builder
	b.WriteString("【用户指令账本】（服务端维护·不参与压缩·最高优先级）\n")
	b.WriteString("规则：本块与任何压缩摘要/项目偏好/用户画像/项目经验冲突时，一律以本块为准。")
	b.WriteString("【现行决策】按轴独立：同一轴内以最新为准，不同轴同时有效；")
	b.WriteString("除非用户明确撤销，不得自行清除某轴的约束（视角/审批门/验收标准）。")
	b.WriteString("若当前工作方向与本块【现行路线】不一致，必须停下来说明冲突并 ask_user 确认。\n")
	header := b.String()

	var sections []ledgerSection
	sections = append(sections, ledgerSection{required: true, text: header})

	// 现行路线（最新 active route 的短摘录，兼容既有注入语义）。
	var routeText string
	if latestRoute != nil {
		routeText = "【现行路线】" + routeHeadline(latestRoute.Content, 420) + "\n"
		// 任务书全文由首条 user 消息承载；只在"这条路线就是任务书"或摘录确实短于原文时提示，
		// 避免模型以为账本被截断而去文件系统/临时目录里找"完整账本"。
		if latestRoute.Source == "goal" || utf8.RuneCountInString(latestRoute.Content) > 420 {
			routeText += "（任务书全文见会话首条用户消息，始终有效；本行只摘录路线要点）\n"
		}
	} else if firstDirective != nil {
		routeText = "【最初目标·可能已被后续指令修订】" + routeHeadline(firstDirective.Content, 300) + "\n"
	}
	sections = append(sections, ledgerSection{text: routeText})

	// 各轴现行约束（必留）。
	if len(axisOrder) > 0 {
		var ab strings.Builder
		ab.WriteString("【现行决策·按轴】（同轴只留最新，跨轴同时有效）\n")
		for _, axis := range axisOrder {
			d := axisCurrent[axis]
			if d == nil {
				continue
			}
			label := axisLabel(axis)
			if label == "" {
				// 无轴路线（isRouteDirective 命中但内容不属任何约束轴，如"就要用这个"）：
				// 退化为"其他约束"而不是空标签，避免输出 "- ：…"。
				label = "其他约束"
			}
			ab.WriteString("- " + label + "：" + routeHeadline(d.Content, 120) + "\n")
		}
		sections = append(sections, ledgerSection{required: true, text: ab.String()})
	}

	// 审批门（必留）：只要有审批轴记录就必须出现——未批准不得进入下一阶段。
	var gateText string
	for _, d := range decisions {
		if d.Kind != store.DecisionKindDirective ||
			(d.Axis != store.AxisApprovalAnchor && d.Axis != store.AxisApprovalKeyframe) {
			continue
		}
		if d.Status != store.DecisionStatusActive {
			continue
		}
		label := axisLabel(d.Axis)
		state := approvalStateOf(d.Content)
		ref := approvalRef(d.Content)
		line := "【审批门】" + label + " = " + state
		if ref != "" {
			line += "（参考：" + ref + "）"
		}
		if state != "approved" {
			line += " —— 未批准禁止进入下一阶段（keyframe/inbetween/运动视频本地拒绝）"
		}
		gateText += line + "\n"
	}
	sections = append(sections, ledgerSection{required: true, text: gateText})

	// 验收标准锚（必留）。
	var criteriaText string
	if criteria != nil {
		criteriaText = "【验收标准·已批准计划】" + ledgerEllipsis(criteria.Content, 400) + "\n" +
			"（交付前逐条对照；缺项不得以文字说明代替）\n"
	}
	sections = append(sections, ledgerSection{required: true, text: criteriaText})

	// 已否决（可选）。
	var rejText string
	for _, d := range rejected {
		rejText += "【已否决·禁止继续】" + ledgerEllipsis(d.Content, 120) + "（" + d.CreatedAt.Format("01-02 15:04") + "）\n"
	}
	if rejText != "" {
		rejText += "【行动要求】当前路线已被用户否决：停止在旧方案上继续修补；先向用户确认新方向，或严格按【验收基准】重做。\n"
	}
	sections = append(sections, ledgerSection{text: rejText})

	// 已废止（可选）：最近 2 条，让模型知道"这条路被用户改掉了"。
	if len(superseded) > 2 {
		superseded = superseded[len(superseded)-2:]
	}
	var supText string
	for _, d := range superseded {
		tag := ""
		if label := axisLabel(d.Axis); label != "" {
			tag = "[" + label + "]"
		}
		supText += "【已废止·禁止再采用】" + tag + truncateRunes(d.Content, 100) + "（" + d.CreatedAt.Format("01-02 15:04") + "）\n"
	}
	sections = append(sections, ledgerSection{text: supText})

	// 验收基准·参考产物（可选）。
	var refText string
	if len(refs) > 0 {
		if len(refs) > 3 {
			refs = refs[len(refs)-3:]
		}
		refText = "【验收基准·用户给的参考产物】\n"
		seen := map[string]bool{}
		for _, r := range refs {
			if seen[r.Content] {
				continue
			}
			seen[r.Content] = true
			refText += "- " + ledgerEllipsis(r.Content, 200) + "\n"
		}
	}
	sections = append(sections, ledgerSection{text: refText})

	// 最近用户指令（可选）：三类都列，最后一条最新。跳过 reference（上面已单列）。
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
			if d.Axis == store.AxisApprovalAnchor || d.Axis == store.AxisApprovalKeyframe {
				continue
			}
			tag = "指令"
			if label := axisLabel(d.Axis); label != "" {
				tag = label
			}
		case store.DecisionKindRoute:
			tag = "路线"
		case store.DecisionKindClarify:
			tag = "澄清答复"
		case store.DecisionKindPlan:
			tag = "已批准计划"
		case store.DecisionKindCriteria:
			tag = "验收标准"
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
	var recentText string
	if len(rows) > 0 {
		recentText = "【最近用户指令（旧→新，最后一条最新）】\n"
		for _, r := range rows {
			recentText += "- " + r.ts.Format("01-02 15:04") + " [" + r.tag + "] " + ledgerEllipsis(r.text, 120) + "\n"
		}
	}
	sections = append(sections, ledgerSection{text: recentText})

	return assembleLedger(sections, decisionBlockMaxRunes)
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

// planCriteriaFromClarify 从计划确认卡里提取「验收标准」（2026-10-23）。
// 计划正文是自由 markdown、没有结构化验收字段；取含"验收"的行最可靠——
// submit_plan 契约要求计划写验收标准（实证形态："7) 验收：16帧无缺无重、邻帧SSIM …"）。
// 无验收行返回空串（不硬造）。
func planCriteriaFromClarify(pc *ClarifyRequest) string {
	if pc == nil {
		return ""
	}
	var picked []string
	for _, line := range strings.Split(pc.Detail+"\n"+pc.Question, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || !strings.Contains(t, "验收") {
			continue
		}
		t = planListPrefixRe.ReplaceAllString(t, "")
		t = strings.TrimSpace(t)
		if t == "" || strings.HasSuffix(t, "验收标准") {
			continue // 纯标题行（如 "## 验收标准"）本身无信息
		}
		picked = append(picked, t)
		if len(picked) >= 3 {
			break
		}
	}
	if len(picked) == 0 {
		return ""
	}
	return ledgerEllipsis(strings.Join(picked, "；"), 600)
}

// planListPrefixRe 去掉计划行的列表序号/标题符号（"7) "、"## "、"- "、"• "）。
var planListPrefixRe = regexp.MustCompile(`^([0-9]{1,2}[.、)）]\s*|[-*•]\s*|#+\s*)+`)

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
