package agent

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/graph"
	"github.com/blockmemory/agent/backend/internal/server/eventkind"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

func (st *sessionStore) runSession(ctx context.Context, session *internalSession) {
	sessionTimeout := 10 * time.Minute
	if rt := st.graph.Runtime(); rt != nil && rt.AgentCfg != nil && rt.AgentCfg.SessionTimeoutMin > 0 {
		sessionTimeout = time.Duration(rt.AgentCfg.SessionTimeoutMin) * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, sessionTimeout)
	defer cancel()

	defer func() {
		st.mu.Lock()
		status := session.Status
		tempDir := session.TempDir
		session.cancelFn = nil
		st.mu.Unlock()
		if status == enums.SessionStatusCompleted || status == enums.SessionStatusError {
			st.cleanupSessionTempDir(session.ID, tempDir)
		}
	}()

	state := types.NewThreeLayerState(session.ID)
	state.DomainGoal = session.Goal

	st.addEvent(session, eventkind.System, "MetaAgent", "会话启动，目标: "+session.Goal, "", "", "", "", "", false)

	result, err := st.graph.Invoke(ctx, state)
	if err != nil {
		st.mu.Lock()
		if session.Status == enums.SessionStatusRunning {
			session.Status = enums.SessionStatusError
			session.Result = err.Error()
			now := time.Now()
			session.EndedAt = &now
		}
		st.mu.Unlock()
		st.addEvent(session, eventkind.Error, "System", "执行失败: "+err.Error(), "", "", "", "", "", false)
		return
	}

	if result.NextAction == enums.ActionWait && result.PendingClarify != nil {
		st.mu.Lock()
		session.Status = enums.SessionStatusAwaitingClarify
		session.State = result
		st.mu.Unlock()
		st.addEvent(session, eventkind.Clarify, "MetaAgent",
			"请求用户澄清: "+result.PendingClarify.Question,
			"", "", "", "", "", false)
		return
	}

	st.mu.Lock()
	session.State = result
	session.Result = result.SessionSummary
	st.mu.Unlock()

	if rt := st.graph.Runtime(); rt != nil && rt.CmdQueue != nil && rt.CmdQueue.HasPending(session.ID) {
		st.mu.Lock()
		session.Status = enums.SessionStatusRunning
		session.EndedAt = nil
		st.mu.Unlock()
		st.addEvent(session, eventkind.System, "MetaAgent", "检测到待处理用户指令，继续执行", "", "", "", "", "", false)
		go st.resumeSession(session)
		return
	}

	st.mu.Lock()
	session.Status = enums.SessionStatusCompleted
	now := time.Now()
	session.EndedAt = &now
	session.Messages = append(session.Messages, types.ChatMessage{
		Role:      enums.ChatRoleAssistant,
		Content:   result.SessionSummary,
		Timestamp: now,
	})
	st.mu.Unlock()

	for _, inst := range st.registry.GetInstancesBySession(session.ID) {
		roleDef := st.registry.GetRoleDef(inst.RoleDefID)
		name := "unknown"
		if roleDef != nil {
			name = roleDef.Name
		}
		st.addEvent(session, eventkind.AgentDone, name, fmt.Sprintf("类型: %s, 领域: %s, 状态: %s", inst.Type, inst.Domain, inst.Status), "", "", "", "", "", false)
	}

	st.addEvent(session, eventkind.System, "MetaAgent", "会话完成: "+result.SessionSummary, "", "", "", "", "", false)

	st.persistHistory(session)
	st.persistEvents(session)

	if metaNode, ok := st.graph.GetNode("MetaAgent"); ok {
		if ma, ok := metaNode.(*graph.MetaAgentNode); ok {
			calls, timeouts, avg, max := ma.TimeoutStats()
			inTotal, outTotal := ma.LLMTracker().TokenTotals()
			if calls > 0 {
				st.addEvent(session, eventkind.Stats, "System",
					fmt.Sprintf("LLM统计: 调用%d次, 超时%d次, 平均%v, 最长%v, 输入Token=%d, 输出Token=%d",
						calls, timeouts, avg.Round(time.Millisecond), max.Round(time.Millisecond), inTotal, outTotal),
					"", "", "", "", "", false)
			}
		}
	}

	st.evictCompletedSessions()
}

func (st *sessionStore) resumeSession(session *internalSession) {
	ctx, rootCancel := context.WithCancel(context.Background())
	st.mu.Lock()
	session.cancelFn = rootCancel
	st.mu.Unlock()
	ctx, timeoutCancel := context.WithTimeout(ctx, 10*time.Minute)
	defer timeoutCancel()

	defer func() {
		st.mu.Lock()
		status := session.Status
		tempDir := session.TempDir
		session.cancelFn = nil
		st.mu.Unlock()
		if status == enums.SessionStatusCompleted || status == enums.SessionStatusError {
			st.cleanupSessionTempDir(session.ID, tempDir)
		}
	}()

	var history strings.Builder
	start := 0
	if len(session.Messages) > 20 {
		start = len(session.Messages) - 20
	}
	for _, msg := range session.Messages[start:] {
		fmt.Fprintf(&history, "%s: %s\n", msg.Role, msg.Content)
	}

	st.addEvent(session, eventkind.LLM, "LightweightModel",
		fmt.Sprintf("续话：调用轻量模型总结历史对话 (%d 字符)", history.Len()), "", "", "", "", "", false)
	goal, err := st.summarizeHistoryForGoal(ctx, session.ID, history.String(), session.Result)
	if err != nil {
		st.mu.Lock()
		session.Status = enums.SessionStatusError
		session.Result = err.Error()
		now := time.Now()
		session.EndedAt = &now
		st.mu.Unlock()
		st.addEvent(session, eventkind.Error, "System", "续话失败（轻量模型不可用）: "+err.Error(), "", "", "", "", "", false)
		log.Printf("[%s] 续话失败: %v", session.ID, err)
		return
	}
	st.addEvent(session, eventkind.Think, "LightweightModel",
		"续话：历史对话已总结为目标: "+goal, "", "", "", "", "", false)

	state := types.NewThreeLayerState(session.ID)
	state.DomainGoal = goal
	state.SessionSummary = session.Result
	state.Messages = session.Messages

	st.addEvent(session, eventkind.System, "MetaAgent", "继续会话，新消息已纳入上下文", "", "", "", "", "", false)

	result, err := st.graph.Invoke(ctx, state)
	if err != nil {
		st.mu.Lock()
		if session.Status == enums.SessionStatusRunning {
			session.Status = enums.SessionStatusError
			session.Result = err.Error()
			now := time.Now()
			session.EndedAt = &now
		}
		st.mu.Unlock()
		st.addEvent(session, eventkind.Error, "System", "执行失败: "+err.Error(), "", "", "", "", "", false)
		return
	}

	if result.NextAction == enums.ActionWait && result.PendingClarify != nil {
		st.mu.Lock()
		session.Status = enums.SessionStatusAwaitingClarify
		session.State = result
		st.mu.Unlock()
		st.addEvent(session, eventkind.Clarify, "MetaAgent",
			"请求用户澄清: "+result.PendingClarify.Question,
			"", "", "", "", "", false)
		return
	}

	st.mu.Lock()
	session.State = result
	session.Result = result.SessionSummary
	st.mu.Unlock()

	if rt := st.graph.Runtime(); rt != nil && rt.CmdQueue != nil && rt.CmdQueue.HasPending(session.ID) {
		st.mu.Lock()
		session.Status = enums.SessionStatusRunning
		session.EndedAt = nil
		st.mu.Unlock()
		st.addEvent(session, eventkind.System, "MetaAgent", "检测到待处理用户指令，继续执行", "", "", "", "", "", false)
		go st.resumeSession(session)
		return
	}

	st.mu.Lock()
	session.Status = enums.SessionStatusCompleted
	now := time.Now()
	session.EndedAt = &now
	session.Messages = append(session.Messages, types.ChatMessage{
		Role:      enums.ChatRoleAssistant,
		Content:   result.SessionSummary,
		Timestamp: now,
	})
	st.mu.Unlock()

	for _, inst := range st.registry.GetInstancesBySession(session.ID) {
		roleDef := st.registry.GetRoleDef(inst.RoleDefID)
		name := "unknown"
		if roleDef != nil {
			name = roleDef.Name
		}
		st.addEvent(session, eventkind.AgentDone, name, fmt.Sprintf("类型: %s, 领域: %s, 状态: %s", inst.Type, inst.Domain, inst.Status), "", "", "", "", "", false)
	}

	st.addEvent(session, eventkind.System, "MetaAgent", "会话完成: "+result.SessionSummary, "", "", "", "", "", false)
	st.persistHistory(session)
	st.persistEvents(session)
	st.evictCompletedSessions()
}
