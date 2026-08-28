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
