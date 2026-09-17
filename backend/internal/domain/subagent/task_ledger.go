// task_ledger.go 实现任务台账（TaskLedger）：本会话所有派发任务的权威状态流水。
//
// 动机（2026-08-28 事故）：同一长会话中，MetaAgent 处理新一轮用户消息时把上一轮
// 的旧需求（"删除彩色圆球"）误称"用户消息中还有一项未钉进在途任务"，投邮箱让
// 已收口的领域 Agent 返工——上下文中没有任何机器可读的"任务完成状态"，旧需求是否
// 做过全凭模型对原始历史的记忆，压缩后更不可靠。
//
// 机制：派发即登记"进行中"（dispatchOne/dispatchHotDomain/dispatchToIdleSlot 三入口），
// notify 是唯一终态回传咽喉（完成/失败/墙钟/被杀均经它），在此一处登记终态+修改文件；
// 渲染时每轮用权威树（orchestrator.Tree）兜底协调 notify 未覆盖的状态（取消/挂起），
// 台账为空且树有节点时从树播种（进程重启恢复）。渲染结果经 agent 包
// task_ledger_context.go 包装器注入 MetaAgent 每轮上下文末尾。
package subagent

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
)

// 台账容量与渲染参数（包级常量，测试可直接引用）。
const (
	ledgerMaxEntries  = 64 // 每会话保留的最大条目数，超出优先淘汰最旧终态条目
	ledgerRenderMax   = 15 // 每轮注入最多渲染的条目数（防上下文膨胀）
	ledgerBriefRunes  = 60 // 任务摘要截断
	ledgerTextRunes   = 120 // 终态摘要/失败原因截断
	ledgerMaxFiles    = 5   // 修改文件最多列出个数
	ledgerFileRunes   = 48  // 单个文件路径截断
)

// ledgerStatus 台账条目状态（渲染为中文标签）。
type ledgerStatus int

const (
	ledgerRunning ledgerStatus = iota
	ledgerDone
	ledgerFailed
	ledgerUnverified // 交付未验证（三态黄态，非失败语义）
	ledgerCancelled
	ledgerPaused
)

// ledgerEntry 一条派发任务记录（任务粒度：热驻续建每次派发新开一条，不占旧条）。
type ledgerEntry struct {
	Seq      int          // 会话内递增序号（#N 展示用）
	ChildID  string       // 子 Agent ID（domain-N / 角色-N）
	Domain   string       // 领域名（可空，叶子助手无领域）
	Brief    string       // 任务一句话摘要（截断）
	Status   ledgerStatus // 状态
	Started  time.Time    // 派发时间
	Finished time.Time    // 终态时间（进行中为零值）
	Text     string       // 终态摘要（完成）或失败原因（失败），截断
	Files    []string     // 修改文件清单（返工定位用）
	FailKind string       // 失败类型（timeout/killed/...，仅失败/未验证）
	Note     string       // 备注：续建#N / 入队 / 重启恢复 等
}

// ledgerTreeView 渲染期状态协调与重启播种的抽象，*orchestrator.Tree 天然满足。
type ledgerTreeView interface {
	Get(id string) (orchestrator.Node, bool)
	Snapshot() []orchestrator.Node
}

// TaskLedger 会话级任务台账（派发/终态并发到达，全部走 mu）。
type TaskLedger struct {
	mu     sync.Mutex
	bySess map[string][]*ledgerEntry
	seq    map[string]int
	seeded map[string]bool // 每会话只从树播种一次（重启恢复场景）
}

// newTaskLedger 构造空台账。
func newTaskLedger() *TaskLedger {
	return &TaskLedger{
		bySess: make(map[string][]*ledgerEntry),
		seq:    make(map[string]int),
		seeded: make(map[string]bool),
	}
}

// failureMarkerRe 解析 notify 失败消息头部的机读标记 [failure kind=X retryable=Y]。
var failureMarkerRe = regexp.MustCompile(`^\[failure kind=(\S+) retryable=(?:true|false)\]\s*`)

// Purge 删除会话的全部台账状态（会话硬删除路径：条目/序号/播种标记一并清）。
// 幂等；l 为 nil 或 sessionID 空时 no-op。
func (l *TaskLedger) Purge(sessionID string) {
	if l == nil || sessionID == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.bySess, sessionID)
	delete(l.seq, sessionID)
	delete(l.seeded, sessionID)
}

// RecordDispatch 登记一次派发（进行中）。仅记录 Meta 直派的任务——domain 派叶子
// 助手属领域内部实现细节，不进入 Meta 面向的台账（防噪声）。sessionID 空或
// 父非 meta 时 no-op。
func (l *TaskLedger) RecordDispatch(sessionID, parentID, childID, domain, brief, note string) {
	if l == nil || sessionID == "" || roleIDFromAgentID(parentID) != "meta" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seeded[sessionID] = true // 有内存派发记录的会话无需再播种
	l.seq[sessionID]++
	entries := l.bySess[sessionID]
	// 容量淘汰：优先挤掉最旧的终态条目，保留进行中；全是进行中（理论不至）挤最旧。
	if len(entries) >= ledgerMaxEntries {
		idx := 0
		for i, e := range entries {
			if e.Status != ledgerRunning {
				idx = i
				break
			}
		}
		entries = append(entries[:idx], entries[idx+1:]...)
	}
	l.bySess[sessionID] = append(entries, &ledgerEntry{
		Seq:     l.seq[sessionID],
		ChildID: childID,
		Domain:  strings.TrimSpace(domain),
		Brief:   truncateRunes(strings.TrimSpace(brief), ledgerBriefRunes),
		Status:  ledgerRunning,
		Started: time.Now(),
		Note:    note,
	})
}

// LastEntryByChild 返回该子 Agent 最新台账条目副本（整合纪要合成取 Files 用）。
func (l *TaskLedger) LastEntryByChild(sessionID, childID string) (ledgerEntry, bool) {
	if l == nil || sessionID == "" || childID == "" {
		return ledgerEntry{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	entries := l.bySess[sessionID]
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].ChildID == childID {
			return *entries[i], true
		}
	}
	return ledgerEntry{}, false
}

// RecordTerminal 登记终态（notify 咽喉点调用）：按失败机读标记区分 失败/未验证/完成，
// 记录终态摘要与修改文件。匹配该 childID 最后一条进行中条目就地收口；找不到
//（条目被容量淘汰或台账晚于派发启用）则补记一条终态记录。过滤规则同 RecordDispatch。
func (l *TaskLedger) RecordTerminal(sessionID, parentID, childID, summary string, files []string) {
	if l == nil || sessionID == "" || roleIDFromAgentID(parentID) != "meta" {
		return
	}
	status := ledgerDone
	failKind := ""
	text := strings.TrimSpace(summary)
	if m := failureMarkerRe.FindStringSubmatch(text); m != nil {
		failKind = m[1]
		text = strings.TrimSpace(failureMarkerRe.ReplaceAllString(text, ""))
		if failKind == string(FailureKindUnverified) || failKind == string(FailureKindVerifyMissing) {
			// 三态黄态（TODO #60）：产出已回传但缺验证证据，非失败语义。
			status = ledgerUnverified
		} else {
			status = ledgerFailed
		}
	}
	// 失败原因取正文首行（人读文案的第一句），完成摘要保留开头即可。
	if idx := strings.IndexByte(text, '\n'); idx > 0 && status != ledgerDone {
		text = text[:idx]
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	entries := l.bySess[sessionID]
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].ChildID == childID && entries[i].Status == ledgerRunning {
			entries[i].Status = status
			entries[i].Finished = time.Now()
			entries[i].Text = truncateRunes(text, ledgerTextRunes)
			entries[i].Files = capFiles(files)
			entries[i].FailKind = failKind
			return
		}
	}
	// 补记：无进行中条目（容量淘汰/台账启用晚于派发）。
	l.seq[sessionID]++
	l.bySess[sessionID] = append(entries, &ledgerEntry{
		Seq:      l.seq[sessionID],
		ChildID:  childID,
		Status:   status,
		Started:  time.Now(),
		Finished: time.Now(),
		Text:     truncateRunes(text, ledgerTextRunes),
		Files:    capFiles(files),
		FailKind: failKind,
		Note:     "补记",
	})
}

// capFiles 截断修改文件清单（个数与单路径长度），nil 安全。
func capFiles(files []string) []string {
	if len(files) == 0 {
		return nil
	}
	out := make([]string, 0, ledgerMaxFiles)
	for _, f := range files {
		if len(out) >= ledgerMaxFiles {
			break
		}
		out = append(out, truncateRunes(strings.TrimSpace(f), ledgerFileRunes))
	}
	return out
}

// Render 渲染会话台账为【任务台账】文本块；无条目且无可播种节点时返回空串（注入层跳过）。
// tree 非 nil 时：台账为空先从树播种（进程重启恢复），进行中条目用树节点状态兜底
// 协调（notify 未覆盖的取消/挂起路径）。
func (l *TaskLedger) Render(sessionID string, tree ledgerTreeView) string {
	if l == nil || sessionID == "" {
		return ""
	}
	l.mu.Lock()
	if !l.seeded[sessionID] && len(l.bySess[sessionID]) == 0 && tree != nil {
		l.seedLocked(sessionID, tree.Snapshot())
	}
	entries := append([]*ledgerEntry(nil), l.bySess[sessionID]...)
	l.mu.Unlock()
	if len(entries) == 0 {
		return ""
	}
	// 渲染窗口：最近 ledgerRenderMax 条；更早的仅统计省略数。
	skipped := 0
	if len(entries) > ledgerRenderMax {
		skipped = len(entries) - ledgerRenderMax
		entries = entries[skipped:]
	}
	var b strings.Builder
	b.WriteString("【任务台账】本会话已派发任务的权威状态（机器维护，不受历史压缩影响，与你对旧对话的记忆冲突时以本表为准）\n")
	b.WriteString("- [完成]：只作档案与返工定位——禁止重新派发/重新验收/重新查询执行；用户明确点名返工时按「修改」列文件快速定位，只改用户点名的部分\n")
	b.WriteString("- [失败]/[交付未验证]：附原因——确需重做时新 spec 必须引用上次失败原因并给出规避措施\n")
	b.WriteString("- [进行中]/[挂起]：正在执行——禁止重复派发同领域任务（追加需求走 send_message 邮箱或 reuse_agent_id 续建）\n")
	b.WriteString("- 用户新消息只含其字面需求；禁止把台账/历史中的旧需求当作新消息的一部分再次派发\n")
	if skipped > 0 {
		fmt.Fprintf(&b, "…（更早 %d 条已完成任务略）\n", skipped)
	}
	for _, e := range entries {
		st := e.Status
		text, failKind, files, note := e.Text, e.FailKind, e.Files, e.Note
		finished := e.Finished
		if st == ledgerRunning && tree != nil {
			st, text, failKind, note, finished = reconcileRunning(e, tree)
		}
		b.WriteString(formatLedgerEntry(e, st, text, failKind, files, note, finished))
	}
	return strings.TrimRight(b.String(), "\n")
}

// seedLocked 从权威树播种台账（进程重启后内存台账为空、树经 PG 恢复的场景）。
// 重启时仍 Running 的节点实际已失联（热驻 goroutine 随进程消亡），记为取消+中断备注。
func (l *TaskLedger) seedLocked(sessionID string, nodes []orchestrator.Node) {
	l.seeded[sessionID] = true
	for _, n := range nodes {
		if roleIDFromAgentID(n.ParentID) != "meta" {
			continue // 只记 Meta 直派任务（与 RecordDispatch 过滤一致）
		}
		l.seq[sessionID]++
		e := &ledgerEntry{
			Seq:      l.seq[sessionID],
			ChildID:  n.ID,
			Domain:   n.Domain,
			Brief:    truncateRunes(n.Task, ledgerBriefRunes),
			Started:  n.Started,
			Finished: n.Finished,
			Text:     truncateRunes(n.Summary, ledgerTextRunes),
			Note:     "重启恢复",
		}
		switch n.Status {
		case orchestrator.StatusDone, orchestrator.StatusIdle:
			e.Status = ledgerDone
		case orchestrator.StatusFailed:
			e.Status = ledgerFailed
			if n.Err != "" {
				e.Text = truncateRunes(n.Err, ledgerTextRunes)
			}
		case orchestrator.StatusUnverified:
			e.Status = ledgerUnverified
		case orchestrator.StatusPaused:
			e.Status = ledgerPaused
		case orchestrator.StatusCancelled:
			e.Status = ledgerCancelled
		default: // Running：进程重启失联
			e.Status = ledgerCancelled
			e.Note = "进程重启时仍在运行，视为中断"
			if e.Finished.IsZero() {
				e.Finished = n.Started
			}
		}
		l.bySess[sessionID] = append(l.bySess[sessionID], e)
	}
}

// reconcileRunning 用权威树协调"台账仍进行中但 notify 未收口"的条目（取消/挂起/
// 树侧先行终态）。返回协调后的展示状态与文本（树非 Running 时以其为准）。
func reconcileRunning(e *ledgerEntry, tree ledgerTreeView) (ledgerStatus, string, string, string, time.Time) {
	n, ok := tree.Get(e.ChildID)
	if !ok {
		return e.Status, e.Text, e.FailKind, e.Note, e.Finished
	}
	switch n.Status {
	case orchestrator.StatusDone:
		return ledgerDone, pickText(e.Text, n.Summary), e.FailKind, e.Note, n.Finished
	case orchestrator.StatusIdle:
		return ledgerDone, pickText(e.Text, n.Summary), e.FailKind, joinNote(e.Note, "完成且热驻可复用"), n.Finished
	case orchestrator.StatusFailed:
		return ledgerFailed, pickText(e.Text, n.Err, n.Summary), e.FailKind, e.Note, n.Finished
	case orchestrator.StatusCancelled:
		return ledgerCancelled, pickText(e.Text, n.Summary), e.FailKind, e.Note, n.Finished
	case orchestrator.StatusPaused:
		return ledgerPaused, pickText(e.Text, n.Summary), e.FailKind, e.Note, n.Finished
	case orchestrator.StatusUnverified:
		return ledgerUnverified, pickText(e.Text, n.Summary), e.FailKind, e.Note, n.Finished
	}
	return e.Status, e.Text, e.FailKind, e.Note, e.Finished
}

// pickText 返回首个非空文本。
func pickText(cands ...string) string {
	for _, c := range cands {
		if strings.TrimSpace(c) != "" {
			return truncateRunes(strings.TrimSpace(c), ledgerTextRunes)
		}
	}
	return ""
}

// joinNote 拼接备注（空跳过）。
func joinNote(base, extra string) string {
	if base == "" {
		return extra
	}
	if extra == "" {
		return base
	}
	return base + "；" + extra
}

// formatLedgerEntry 渲染单条台账行：
// #3 [完成] 游戏渲染逻辑 · 塔绘制三层拆分…（耗时 2h13m，续建#2）｜修改: js/game.js, js/ui.js ｜摘要: …
// #2 [失败·timeout] 游戏渲染逻辑 · 路径对齐等5项…（2h00m）｜原因: 墙钟预算耗尽… ｜已改文件: …
func formatLedgerEntry(e *ledgerEntry, st ledgerStatus, text, failKind string, files []string, note string, finished time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "#%d [%s]", e.Seq, ledgerStatusLabel(st, failKind))
	if e.Domain != "" {
		b.WriteString(" " + truncateRunes(e.Domain, 24))
	}
	if e.Brief != "" {
		b.WriteString(" · " + e.Brief)
	}
	// 耗时：终态用 Started→Finished；进行中用 Started→now。
	dur := ""
	if !e.Started.IsZero() {
		end := finished
		if st == ledgerRunning || end.IsZero() {
			end = time.Now()
			dur = "已 " + ledgerDur(end.Sub(e.Started))
		} else {
			dur = "耗时 " + ledgerDur(end.Sub(e.Started))
		}
	}
	if note != "" {
		if dur != "" {
			dur += "，"
		}
		dur += note
	}
	if dur != "" {
		fmt.Fprintf(&b, "（%s）", dur)
	}
	if len(files) > 0 {
		label := "｜修改: "
		if st == ledgerFailed || st == ledgerUnverified {
			label = "｜已改文件: "
		}
		b.WriteString(label + strings.Join(files, ", "))
	}
	if text != "" {
		if st == ledgerFailed || st == ledgerUnverified || st == ledgerCancelled {
			b.WriteString("｜原因: " + text)
		} else if st == ledgerDone {
			b.WriteString("｜摘要: " + text)
		}
	}
	b.WriteString("\n")
	return b.String()
}

// ledgerStatusLabel 状态中文标签；失败/未验证附失败类型。
func ledgerStatusLabel(st ledgerStatus, failKind string) string {
	switch st {
	case ledgerDone:
		return "完成"
	case ledgerFailed:
		if failKind != "" {
			return "失败·" + failKind
		}
		return "失败"
	case ledgerUnverified:
		if failKind != "" {
			return "交付未验证·" + failKind
		}
		return "交付未验证"
	case ledgerCancelled:
		return "已取消"
	case ledgerPaused:
		return "挂起"
	}
	return "进行中"
}

// ledgerDur 紧凑时长：45s / 35m / 2h13m。
func ledgerDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}
