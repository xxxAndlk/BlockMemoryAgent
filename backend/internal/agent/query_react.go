// query_react.go 实现 ReactService.Query 的其余只读查询分支
// （QueryKindBoard / Metrics / Mailbox / Watchdog / TokenMetrics）。
//
// 背景：ReactService.Query 原先只处理 SessionCount / LLMStats / Logs 三类，
// 上述五个 Kind 全部落入 default 返回空 Result——HTTP 层 /api/sessions/{id}/board、
// /metrics、/mailbox、/watchdog、/token-metrics 五个端点自 ReAct 重构以来一直
// 返回空数据（web 监控面板空转）。本文件补齐各分支，数据口径对齐 TUI 读路径：
//   - Board：write_plan 权威看板优先（s.Board），无计划回退权威 Agent 树合成
//     （状态映射与 tui/helpers.go boardSnapshot 同构，含 Idle 翻绿/Unverified 黄/Paused 阻塞）。
//   - Metrics：pgStore session_logs 按 llm 前缀 phase 聚合调用数/token/耗时，
//     超时数从会话 error 事件做超时关键词匹配计数（logs 无超时标记字段）。
//   - TokenMetrics：pgStore.AggregateSessionTokens 按 agent|model 聚合，转 HTTP 线型。
//   - Mailbox：权威树全部节点实例 ID 的未取走邮箱消息（Peek 非破坏性）。
//   - Watchdog：watchdog 组件已随 runtime 步骤 6 退役（无决策产生方），
//     返回空决策列表保持端点兼容。
package agent

import (
	"context"
	"encoding/json"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/board"
	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
	"github.com/blockmemory/agent/backend/internal/mailbox"
	"github.com/blockmemory/agent/backend/internal/server/eventkind"
	"github.com/blockmemory/agent/backend/internal/store"
	"github.com/blockmemory/agent/backend/pkg/enums"
)

// tokenStatsWire 是按 agent|model 聚合 token 统计的 HTTP 线型
// （store.AgentModelTokenStats 无 json tag，直出会是 Go 字段名，Web 侧对不上）。
type tokenStatsWire struct {
	Agent        string `json:"agent"`
	Model        string `json:"model"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	Calls        int    `json:"calls"`
}

// boardQueryResult 返回会话任务看板：write_plan 权威快照优先，
// 无计划（未 write_plan / 旧会话 / 看板未接线）回退从权威 Agent 树合成。
func (s *ReactService) boardQueryResult(ctx context.Context, sessionID string) Result {
	if snap, err := s.Board(ctx, sessionID); err == nil && snap != nil && len(snap.Tasks) > 0 {
		return Result{Data: snap}
	}
	nodes, err := s.Tree(ctx, sessionID)
	if err != nil {
		return Result{Data: board.Snapshot{}}
	}
	nodes = filterPrevRoundNodes(nodes, s.currentRoundStart(sessionID))
	snap := synthesizeBoardSnapshot(sessionID, nodes)
	return Result{Data: &snap}
}

// currentRoundStart 返回会话当前轮任务的开始时间：最后一条用户消息的时间；
// 无用户消息或消息时间缺失时回退到会话开始时间（与 tui/helpers.go currentRoundStart
// 同构）。会话不在内存（已驱逐/重启恢复）时返回零值，不过滤前轮节点。
func (s *ReactService) currentRoundStart(sessionID string) time.Time {
	sess := s.store.getSession(sessionID)
	if sess == nil {
		return time.Time{}
	}
	for i := len(sess.Messages) - 1; i >= 0; i-- {
		if sess.Messages[i].Role == string(enums.ChatRoleUser) {
			if !sess.Messages[i].Timestamp.IsZero() {
				return sess.Messages[i].Timestamp
			}
			break
		}
	}
	return sess.StartedAt
}

// filterPrevRoundNodes 过滤上一轮已终结的树节点（终态且启动早于本轮起点），
// 防止多轮会话中上一轮残留节点稀释看板进度（与 tui/agent_tree_panel.go 同构）。
func filterPrevRoundNodes(nodes []orchestrator.Node, roundStart time.Time) []orchestrator.Node {
	if roundStart.IsZero() {
		return nodes
	}
	out := make([]orchestrator.Node, 0, len(nodes))
	for _, n := range nodes {
		switch n.Status {
		case orchestrator.StatusDone, orchestrator.StatusFailed, orchestrator.StatusCancelled, orchestrator.StatusUnverified:
			if n.Started.Before(roundStart) {
				continue
			}
		}
		out = append(out, n)
	}
	return out
}

// synthesizeBoardSnapshot 把权威树节点映射为看板子任务。状态映射：
// Done/Cancelled/Idle→done（Idle=任务完结热驻留，Wake 复用翻回 Running）、
// Failed→failed、Unverified→delivered-unverified（标黄不标红）、Paused→blocked、
// 其余（Running 等）→in_progress。
func synthesizeBoardSnapshot(sessionID string, nodes []orchestrator.Node) board.Snapshot {
	_ = sessionID // 保留入参便于日志排查与后续扩展
	tasks := make([]board.SubTask, 0, len(nodes))
	for _, n := range nodes {
		st := board.TaskInProgress
		switch n.Status {
		case orchestrator.StatusDone, orchestrator.StatusCancelled, orchestrator.StatusIdle:
			st = board.TaskDone
		case orchestrator.StatusFailed:
			st = board.TaskFailed
		case orchestrator.StatusUnverified:
			st = board.TaskUnverified
		case orchestrator.StatusPaused:
			st = board.TaskBlocked
		}
		title := "派发 " + n.Domain
		if n.Domain == "" {
			title = "派发 " + n.ID
		}
		if n.Task != "" {
			title += ": " + n.Task
		}
		updated := n.Started
		if !n.Finished.IsZero() {
			updated = n.Finished
		}
		tasks = append(tasks, board.SubTask{
			ID:        n.ID,
			Title:     title,
			Status:    st,
			Result:    n.Summary,
			CreatedAt: n.Started,
			UpdatedAt: updated,
		})
	}
	return board.Snapshot{Tasks: tasks}
}

// metricsQueryResult 聚合会话级 LLM 指标：调用数 / 超时数 / 平均与最大耗时 /
// 输入输出 token。调用与 token、耗时来自 pgStore session_logs（phase 前缀 llm，
// 重启恢复的会话也可查）；超时数 logs 无标记字段，从会话 error 事件的超时
// 关键词（timeout / DeadlineExceeded / Client.Timeout）匹配计数。
func (s *ReactService) metricsQueryResult(ctx context.Context, sessionID string) Result {
	calls := 0
	timeouts := 0
	var inTok, outTok int
	var totalDur, maxDur time.Duration

	if pg := s.store.pgStore; pg != nil {
		recs, err := pg.QuerySessionLogs(ctx, sessionID, "", "", 5000, 0)
		if err == nil {
			for _, r := range recs {
				if !strings.HasPrefix(r.Phase, "llm") {
					continue
				}
				calls++
				lat := time.Duration(r.LatencyMs) * time.Millisecond
				if r.LatencyMs < 0 {
					// 历史约定：latency 负值表示超时（metricsCollector.record 语义）。
					timeouts++
					lat = -lat
				}
				totalDur += lat
				if lat > maxDur {
					maxDur = lat
				}
				inTok += r.InputTokens
				outTok += r.OutputTokens
			}
		}
	}
	if timeouts == 0 {
		timeouts = s.countTimeoutEvents(ctx, sessionID)
	}

	avg := "0s"
	if calls > 0 {
		avg = (totalDur / time.Duration(calls)).Round(time.Millisecond).String()
	}
	return Result{Data: map[string]any{
		"session_id":    sessionID,
		"calls":         calls,
		"timeouts":      timeouts,
		"avg_duration":  avg,
		"max_duration":  maxDur.Round(time.Millisecond).String(),
		"input_tokens":  inTok,
		"output_tokens": outTok,
		"total_tokens":  inTok + outTok,
	}}
}

// countTimeoutEvents 从会话事件（内存优先，已驱逐会话回退 PG 加载）统计
// 携带超时特征的 error 事件数。best-effort：pgStore 不可用或查询失败返回 0。
func (s *ReactService) countTimeoutEvents(ctx context.Context, sessionID string) int {
	var events []internalEvent
	if sess := s.store.getSession(sessionID); sess != nil {
		events = sess.Events
	} else if pg := s.store.pgStore; pg != nil {
		events = s.store.loadSessionEvents(ctx, sessionID)
	}
	count := 0
	for _, ev := range events {
		if ev.Type != eventkind.Error {
			continue
		}
		msg := strings.ToLower(ev.Message + " " + ev.ToolError)
		if strings.Contains(msg, "timeout") || strings.Contains(msg, "deadlineexceeded") {
			count++
		}
	}
	return count
}

// tokenMetricsQueryResult 返回按 agent|model 聚合的 token 消耗（HTTP 线型）。
func (s *ReactService) tokenMetricsQueryResult(ctx context.Context, sessionID string) Result {
	agg := &store.TokenAggregation{ByAgentModel: map[string]store.AgentModelTokenStats{}}
	if pg := s.store.pgStore; pg != nil {
		if a, err := pg.AggregateSessionTokens(ctx, sessionID); err == nil && a != nil {
			agg = a
		}
	}
	stats := make([]tokenStatsWire, 0, len(agg.ByAgentModel))
	for _, v := range agg.ByAgentModel {
		stats = append(stats, tokenStatsWire{
			Agent:        v.Agent,
			Model:        v.Model,
			InputTokens:  v.InputTokens,
			OutputTokens: v.OutputTokens,
			Calls:        v.Calls,
		})
	}
	sort.Slice(stats, func(i, j int) bool {
		return stats[i].InputTokens+stats[i].OutputTokens > stats[j].InputTokens+stats[j].OutputTokens
	})
	return Result{Data: map[string]any{
		"session_id":          sessionID,
		"total_input_tokens":  agg.TotalInputTokens,
		"total_output_tokens": agg.TotalOutputTokens,
		"total_calls":         agg.TotalCalls,
		"stats":               stats,
	}}
}

// mailboxQueryResult 返回会话权威树全部 Agent 实例的未取走邮箱消息。
// Peek 非破坏性（不标记已读不转移信件），重复轮询安全。
func (s *ReactService) mailboxQueryResult(ctx context.Context, sessionID string) Result {
	msgs := []*mailbox.Message{}
	if s.mailbox != nil {
		if nodes, err := s.Tree(ctx, sessionID); err == nil {
			seen := make(map[string]bool, len(nodes))
			for _, n := range nodes {
				if seen[n.ID] {
					continue
				}
				seen[n.ID] = true
				msgs = append(msgs, s.mailbox.Peek(n.ID)...)
			}
		}
	}
	return Result{Data: msgs}
}

// efficiencyBranchWire 支路成本表行（HTTP 线型）。
type efficiencyBranchWire struct {
	NodeID       string   `json:"node_id"`
	Role         string   `json:"role"`
	Domain       string   `json:"domain"`
	Task         string   `json:"task"`
	Status       string   `json:"status"`
	WallClockSec float64  `json:"wall_clock_sec"`
	ToolCalls    int      `json:"tool_calls"`
	Rounds       int      `json:"rounds"`
	FilesWritten []string `json:"files_written"`
}

// efficiencyRoleWire 角色级 token/延迟统计行（HTTP 线型）。
type efficiencyRoleWire struct {
	Role           string `json:"role"`
	Calls          int    `json:"calls"`
	InputTokens    int64  `json:"input_tokens"`
	OutputTokens   int64  `json:"output_tokens"`
	AvgLatencyMs   int64  `json:"avg_latency_ms"`
	P50InputTokens float64 `json:"p50_input_tokens"`
	P95InputTokens float64 `json:"p95_input_tokens"`
}

// efficiencyQueryResult 组装会话效率一等指标（TODO 第9项⑥/第10项③）。
// 五项指标：每交付文件 token 成本 / 每派发平均轮次 / 校验开销占比 / meta:domain token 比 /
// 单呼输入 P50/P95；附支路成本表（按树节点）与角色级 token 统计。
// 数据源纯聚合：agent_tree_nodes（墙钟/角色）+ agent_events（工具调用/交付文件）+
// session_logs（token/延迟分位数），无新采集管道。
func (s *ReactService) efficiencyQueryResult(ctx context.Context, sessionID string) Result {
	// 角色级 token/延迟统计（session_logs.agent = 角色 ID）。
	roleRows := []*store.RoleEfficiencyStats{}
	var totalIn, totalOut int64
	var verifyTok, domainTok, metaTok int64
	if pg := s.store.pgStore; pg != nil {
		if rs, err := pg.QuerySessionRoleEfficiency(ctx, sessionID); err == nil {
			roleRows = rs
			for _, r := range rs {
				totalIn += r.InputTokens
				totalOut += r.OutputTokens
				tok := r.InputTokens + r.OutputTokens
				switch {
				case isVerifyRole(r.Agent):
					verifyTok += tok
				case r.Agent == "domain":
					domainTok += tok
				case r.Agent == "meta":
					metaTok += tok
				}
			}
		}
	}
	// 会话级单呼输入 P50/P95。
	var p50, p95 float64
	if pg := s.store.pgStore; pg != nil {
		if a, b, err := pg.QuerySessionInputPercentiles(ctx, sessionID); err == nil {
			p50, p95 = a, b
		}
	}
	// 实例级事件统计（agent_events.agent_id = 实例 ID）。
	eventStats := map[string]*store.AgentEventStats{}
	var filesDelivered int
	if pg := s.store.pgStore; pg != nil {
		if es, err := pg.QueryAgentEventStats(ctx, sessionID); err == nil {
			for _, e := range es {
				eventStats[e.AgentID] = e
				filesDelivered += len(e.WrittenFiles)
			}
		}
	}

	// 支路成本表：树节点逐支路（墙钟/轮次/工具调用/交付文件）。
	branches := []efficiencyBranchWire{}
	totalRounds, dispatches := 0, 0
	if nodes, err := s.Tree(ctx, sessionID); err == nil {
		for _, n := range nodes {
			isMeta := n.Role == "meta"
			toolCalls, files := 0, []string{}
			if es := eventStats[n.ID]; es != nil {
				toolCalls = es.ToolCalls
				files = es.WrittenFiles
			}
			// 轮次口径：工具轮 + 1 个终答轮（ReAct 每轮要么派工具要么出终答）。
			rounds := toolCalls + 1
			if !isMeta {
				dispatches++
				totalRounds += rounds
			}
			wall := 0.0
			if !n.Finished.IsZero() && n.Finished.After(n.Started) {
				wall = n.Finished.Sub(n.Started).Seconds()
			}
			task := n.Task
			if len([]rune(task)) > 80 {
				task = string([]rune(task)[:80]) + "…"
			}
			branches = append(branches, efficiencyBranchWire{
				NodeID: n.ID, Role: n.Role, Domain: n.Domain, Task: task,
				Status: n.Status.String(), WallClockSec: wall,
				ToolCalls: toolCalls, Rounds: rounds, FilesWritten: files,
			})
		}
	}

	totalTok := totalIn + totalOut
	// 每派发平均轮次 = 非-meta 支路轮次和 / 派发数；无派发时 0。
	avgRounds := 0.0
	if dispatches > 0 {
		avgRounds = float64(totalRounds) / float64(dispatches)
	}
	// 校验开销占比：校验/评审类角色 token / 会话总 token。
	verifyShare := 0.0
	if totalTok > 0 {
		verifyShare = float64(verifyTok) / float64(totalTok)
	}
	// meta:domain token 比：domain 总 token 为分母（0 时报 0）。
	metaDomainRatio := 0.0
	if domainTok > 0 {
		metaDomainRatio = float64(metaTok) / float64(domainTok)
	}
	// 每交付文件 token 成本：总 token / 去重交付文件数。
	costPerFile := 0.0
	if filesDelivered > 0 {
		costPerFile = float64(totalTok) / float64(filesDelivered)
	}

	roleStats := make([]efficiencyRoleWire, 0, len(roleRows))
	for _, r := range roleRows {
		roleStats = append(roleStats, efficiencyRoleWire{
			Role: r.Agent, Calls: r.Calls,
			InputTokens: r.InputTokens, OutputTokens: r.OutputTokens,
			AvgLatencyMs: r.AvgLatency.Milliseconds(),
			P50InputTokens: r.P50InputTokens, P95InputTokens: r.P95InputTokens,
		})
	}

	return Result{Data: map[string]any{
		"session_id":           sessionID,
		"tokens_per_file":      costPerFile,
		"files_delivered":      filesDelivered,
		"avg_rounds_per_dispatch": avgRounds,
		"verify_token_share":   verifyShare,
		"meta_domain_ratio":    metaDomainRatio,
		"input_p50":            p50,
		"input_p95":            p95,
		"total_input_tokens":   totalIn,
		"total_output_tokens":  totalOut,
		"branches":             branches,
		"role_stats":           roleStats,
	}}
}

// isVerifyRole 判定角色是否校验/评审类（校验开销占比口径）：
// 角色名含 review/verif/judge/audit 子串即计入（verif 覆盖 verify/verifier/verification）。
func isVerifyRole(role string) bool {
	r := strings.ToLower(role)
	return strings.Contains(r, "review") || strings.Contains(r, "verif") ||
		strings.Contains(r, "judge") || strings.Contains(r, "audit")
}

// agentEventsQueryResult 子 Agent 审计下钻（TODO 第10项③）：按实例 ID 取逐轮事件。
// pgStore 不可用或查询失败返回空列表（审计面降级不报错）。
func (s *ReactService) agentEventsQueryResult(ctx context.Context, sessionID, agentID string, limit, offset int) Result {
	events := []map[string]any{}
	if agentID != "" && s.store.pgStore != nil {
		if evs, err := s.store.pgStore.QueryAgentEvents(ctx, sessionID, agentID, limit, offset); err == nil {
			events = evs
		}
	}
	return Result{Data: map[string]any{
		"session_id": sessionID,
		"agent_id":   agentID,
		"events":     events,
	}}
}

// agentMessagesQueryResult 编排页 Agent 对话视图数据源：Redis 热层优先（运行中最新），
// miss/不足回退 PG agent_messages 全量切片；附 mailbox 留痕（type=mailbox 事件，双方各一行）。
// agentID 传 "meta" 或空时映射为 sessionID（MetaAgent agentID==sessionID）。
func (s *ReactService) agentMessagesQueryResult(ctx context.Context, sessionID, agentID string, beforeSeq, afterSeq, limit int) Result {
	if agentID == "" || agentID == "meta" {
		agentID = sessionID
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	mails := []map[string]any{}
	if s.store.pgStore != nil {
		if rows, err := s.store.pgStore.QueryAgentMailboxTrace(ctx, sessionID, agentID, 100); err == nil {
			mails = rows
		}
	}
	return Result{Data: map[string]any{
		"session_id": sessionID,
		"agent_id":   agentID,
		"messages":   s.readAgentMessages(ctx, agentID, beforeSeq, afterSeq, limit),
		"mails":      mails,
	}}
}

// readAgentMessages 读消息窗口：热层优先，热层不足时与 PG 全量切片按 seq 合并
//（热层同 seq 覆盖 PG——热层是运行中最新版本，PG 是终态快照）。
//
// 只读热层不合并的场景：after_seq 增量窗口——PG 是上一轮终态快照，掺进来会把旧内容
// 当新消息回放；该窗口仅在热层整段丢失（Redis 重启/过期）时回退 PG。
func (s *ReactService) readAgentMessages(ctx context.Context, agentID string, beforeSeq, afterSeq, limit int) []map[string]any {
	var hot []store.AgentMsgEntry
	if s.agentMsgCache != nil {
		var err error
		switch {
		case afterSeq > 0:
			hot, err = s.agentMsgCache.AfterMsg(ctx, agentID, afterSeq, limit)
		case beforeSeq > 0:
			hot, err = s.agentMsgCache.BeforeMsg(ctx, agentID, beforeSeq, limit)
		default:
			hot, err = s.agentMsgCache.TailMsg(ctx, agentID, limit)
		}
		if err != nil {
			log.Printf("[query] agent msg hot-read failed, fallback PG: agent=%s err=%v", agentID, err)
			hot = nil
		}
	}
	if afterSeq > 0 && len(hot) > 0 {
		return entriesToWire(hot)
	}
	if len(hot) >= limit {
		return entriesToWire(hot)
	}

	// 热层不足（未接线/被 LTRIM/TTL 过期/Redis 重启后只补了少数条目）：并入 PG 切片，
	// 否则更早的历史在面板上不可达（"加载更早消息"永远取不到东西）。
	pgWire := []map[string]any{}
	if s.store.pgStore != nil {
		if all, err := NewPostgresMessagesStore(s.store.pgStore.DB()).LoadMessages(ctx, agentID); err == nil {
			pgWire = sliceMessagesWire(all, beforeSeq, afterSeq, limit)
		}
	}
	if len(hot) == 0 {
		return pgWire
	}
	merged := make(map[int]map[string]any, len(pgWire)+len(hot))
	for _, w := range pgWire {
		if seq, ok := w["seq"].(int); ok {
			merged[seq] = w
		}
	}
	for _, w := range entriesToWire(hot) {
		if seq, ok := w["seq"].(int); ok {
			merged[seq] = w // 热层优先
		}
	}
	out := make([]map[string]any, 0, len(merged))
	for seq, w := range merged {
		if beforeSeq > 0 && seq >= beforeSeq {
			continue // before 窗口只保留更早的
		}
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["seq"].(int) < out[j]["seq"].(int) })
	if len(out) > limit {
		out = out[len(out)-limit:] // 两窗口都取"最近 limit 条"
	}
	return out
}

// sliceMessagesWire PG 全量切片（seq=下标）：after_seq 优先，再次 before_seq，缺省取尾部。
func sliceMessagesWire(msgs []ReactMessage, beforeSeq, afterSeq, limit int) []map[string]any {
	start, end := 0, len(msgs)
	switch {
	case afterSeq > 0:
		start = afterSeq + 1
		if start > len(msgs) {
			start = len(msgs)
		}
		if start+limit < end {
			end = start + limit
		}
	case beforeSeq > 0:
		end = beforeSeq
		if end > len(msgs) {
			end = len(msgs)
		}
		start = end - limit
		if start < 0 {
			start = 0
		}
	default:
		start = len(msgs) - limit
		if start < 0 {
			start = 0
		}
	}
	out := make([]map[string]any, 0, end-start)
	for i := start; i < end; i++ {
		out = append(out, reactMessageWire(i, msgs[i]))
	}
	return out
}

// reactMessageWire 单条 ReactMessage 转线型（PG 路径无 at 时间戳，省略该键）。
func reactMessageWire(seq int, m ReactMessage) map[string]any {
	w := map[string]any{
		"seq":     seq,
		"role":    m.Role,
		"content": m.Content,
	}
	if m.ToolCallID != "" {
		w["tool_call_id"] = m.ToolCallID
	}
	if len(m.ToolCalls) > 0 {
		w["tool_calls"] = m.ToolCalls
	}
	if m.ReasoningContent != "" {
		w["reasoning"] = m.ReasoningContent
	}
	return w
}

// entriesToWire 热层条目转线型：tool_calls JSON 反序列化为数组（非法 JSON 降级省略）。
func entriesToWire(entries []store.AgentMsgEntry) []map[string]any {
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		w := map[string]any{
			"seq":     e.Seq,
			"at":      e.At,
			"role":    e.Role,
			"content": e.Content,
		}
		if e.ToolCallID != "" {
			w["tool_call_id"] = e.ToolCallID
		}
		if e.ToolCalls != "" {
			var calls []any
			if json.Unmarshal([]byte(e.ToolCalls), &calls) == nil {
				w["tool_calls"] = calls
			}
		}
		if e.Reasoning != "" {
			w["reasoning"] = e.Reasoning
		}
		out = append(out, w)
	}
	return out
}
