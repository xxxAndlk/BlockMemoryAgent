package subagent

// failure_prune.go 失败分支整支剪枝（TODO #20④，对齐 CC rewind 菜单 + /branch 语义）。
//
// 两件套：
//  1. domain 会话**逻辑检查点**——派发时 + 里程碑时快照任务账本，落 agent_events
//     （type=checkpoint）+ 内存回读点。账本快照可回滚、SQL 可查（"剪枝不误伤"双兜底之一）；
//  2. 失败处置三选：剪枝重派（默认，零用户决策）/ 同支续跑（现状语义）/ 分叉重派。
//
// 纪律（与 #20① 联动）：剪枝只动活跃上下文（新 run 以检查点种子起步 = 截断点落在
// run 边界），失败轨迹经 ArchiveMessages 留底账、永不物理删除；请求装配层
// sanitizeToolPairing 继续兜底工具调用残对。

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/mailbox"
)

// ReviveMode 失败分支处置三选（TODO #20④）。
type ReviveMode string

const (
	// ReviveAuto 按节点状态自动选档：Failed→prune，其余终态→continue。
	ReviveAuto ReviveMode = ""
	// RevivePrune 剪枝重派（对齐 CC restore both）：种子=原任务+检查点账本+用户消息，
	// 上轮失败轨迹不进活跃上下文（留底账）。
	RevivePrune ReviveMode = "prune"
	// ReviveContinue 同支续跑（现状语义保留）：种子带上轮结果/上轮错误，错误轻微时合适。
	ReviveContinue ReviveMode = "continue"
	// ReviveFork 分叉重派（对齐 CC /branch）：新支（新 ID）以剪枝种子重跑，旧支保留可考古。
	ReviveFork ReviveMode = "fork"
)

// checkpointEventType 逻辑检查点事件类型（agent_events type 自由文本，零 DDL）。
// 与 compaction 同待遇：SQL 取证数据，不渲染进【近期事件】（memory.isContextHiddenEvent）。
const checkpointEventType = "checkpoint"

// checkpointPhases 检查点相位（Content 字段）。
const (
	checkpointPhaseDispatch   = "dispatch"
	checkpointPhaseMilestone  = "milestone"
	checkpointPhaseReuse      = "reuse"
	checkpointPhaseUserDirect = "user_direct"
)

// writeCheckpoint 落逻辑检查点：任务账本快照进 agent_events + 内存回读点。
// brief 为空（会话无台账）时仍落事件（相位留痕），Input 留空。
// best-effort：memory 未接线/写失败仅跳过，不影响派发主流程。
func (d *Dispatcher) writeCheckpoint(agentID, phase string) {
	if d == nil || agentID == "" {
		return
	}
	brief := d.TaskLedgerBrief(sessionIDFromAgentID(agentID))
	d.lastCheckpoints.Store(agentID, checkpointRec{brief: brief, phase: phase, at: time.Now()})
	if d.memory == nil {
		return
	}
	_ = d.memory.Write(agentID, agent.MemoryEvent{
		Type:     checkpointEventType,
		AgentID:  agentID,
		Content:  phase,
		Input:    brief,
		Occurred: time.Now(),
	})
}

// checkpointRec 内存回读点（剪枝种子取最近检查点账本）。
type checkpointRec struct {
	brief string
	phase string
	at    time.Time
}

// lastCheckpointBrief 返回该 Agent 最近一次检查点的账本快照（无则空串）。
func (d *Dispatcher) lastCheckpointBrief(agentID string) string {
	if d == nil {
		return ""
	}
	v, ok := d.lastCheckpoints.Load(agentID)
	if !ok {
		return ""
	}
	rec, _ := v.(checkpointRec)
	return rec.brief
}

// resolveReviveMode 归一化处置档：空=auto。auto 映射（TODO #24 顺手修②）：
// Failed / Cancelled / 带错误 → 剪枝重派（回滚到检查点；Cancelled 此前落 continue，
// 会把 kill 消息/中断片段当"上一轮结果"带进种子，语义上更该 prune）；
// Done / delivered-unverified → 同支续跑（用户直连跟进语义，保留上轮结果）。
func resolveReviveMode(node orchestrator.Node, mode ReviveMode) ReviveMode {
	switch mode {
	case RevivePrune, ReviveContinue, ReviveFork:
		return mode
	}
	if node.Status == orchestrator.StatusFailed || node.Status == orchestrator.StatusCancelled || node.Err != "" {
		return RevivePrune
	}
	return ReviveContinue
}

// buildReviveSeed 按处置档拼装重跑种子。
//   - prune：原任务 + 【检查点账本（回滚点）】+ 用户消息——上轮结果/错误不进活跃上下文；
//   - continue：原任务 + 上轮结果 + 上轮错误 + 用户消息（现状语义）。
//
// fork 复用 prune 种子（新支干净重跑，旧支留档）。
func (d *Dispatcher) buildReviveSeed(node orchestrator.Node, userMsg string, mode ReviveMode) string {
	mode = resolveReviveMode(node, mode)
	if mode == ReviveContinue {
		seed := node.Task + "\n\n【上一轮结果】\n" + node.Summary
		if node.Err != "" {
			seed += "\n【上轮错误】\n" + node.Err
		}
		return seed + "\n\n【用户直连消息】\n" + userMsg
	}
	seed := node.Task
	if brief := d.lastCheckpointBrief(node.ID); brief != "" {
		seed += "\n\n【检查点账本（回滚点）】\n" + brief
	}
	return seed + "\n\n【用户直连消息】\n" + userMsg
}

// ReviveFork 分叉重派（TODO #20④ 第三选）：新支（新 ID）以剪枝种子重跑；
// 旧支节点与消息原样保留（编排页可 resume 考古），worktree 不动。
// 父感知：邮件告知新旧支 ID，防重复派发同领域。
func (d *Dispatcher) ReviveFork(ctx context.Context, node orchestrator.Node, userMsg string) error {
	roleDef := d.registry.Get(node.Role)
	if roleDef == nil {
		return fmt.Errorf("角色 %s 未注册，无法分叉", node.Role)
	}
	parentID := node.ParentID
	// ID 分配防撞（进程重启后 seq 归零，旧支节点经 PG 恢复树仍在）：
	// 逐个试号，跳过树中已存在的 ID——旧支必须原样保留，Register 撞名会覆盖考古入口。
	newID := ""
	for {
		candidate := fmt.Sprintf("%s/%s-%d", parentID, node.Role, d.seq.Add(1))
		if d.treeFn == nil {
			newID = candidate
			break
		}
		sidProbe := tool.SessionIDFromContext(ctx)
		if t := d.treeFn(sidProbe); t == nil {
			newID = candidate
			break
		} else if _, exists := t.Get(candidate); !exists {
			newID = candidate
			break
		}
	}

	subAgentCtx := tool.StopContextFrom(ctx)
	if subAgentCtx == nil {
		subAgentCtx = context.Background()
	}
	if sid := tool.SessionIDFromContext(ctx); sid != "" {
		subAgentCtx = tool.WithSessionID(subAgentCtx, sid)
	}
	subAgentCtx = tool.WithWorkDir(subAgentCtx, d.subAgentWorkDirFor(ctx))
	// 墙钟与复活路径同口径：domain 无显式预算时用侦察墙钟兜底。
	effectiveTimeout := d.effectiveWallClock(roleDef.ID, 0)
	baseCtx, baseCancel := context.WithCancel(subAgentCtx)

	if d.treeFn == nil {
		baseCancel()
		return fmt.Errorf("权威树未接线，无法分叉 %s", node.ID)
	}
	sid := tool.SessionIDFromContext(subAgentCtx)
	t := d.treeFn(sid)
	if t == nil {
		baseCancel()
		return fmt.Errorf("会话 %s 无权威树，无法分叉", sid)
	}
	t.Register(orchestrator.Node{
		ID:       newID,
		ParentID: parentID,
		Role:     node.Role,
		Domain:   node.Domain,
		Task:     node.Task,
		Started:  time.Now(),
		Status:   orchestrator.StatusRunning,
	})
	t.SetCancel(newID, baseCancel)

	seed := d.buildReviveSeed(node, userMsg, RevivePrune)
	if parentID != "" {
		d.trackChildStart(parentID)
	}
	if d.mailbox != nil {
		d.mailbox.Reopen(newID)
	}
	if parentID != "" {
		d.boardAssign(ctx, parentID, node.Domain, newID)
	}
	meta := &subAgentMeta{cancel: baseCancel, parentID: parentID, sessionID: sid, wallClock: effectiveTimeout}
	d.subMeta.Store(newID, meta)
	ev := newEvidence()
	if roleDef.ID != "meta" {
		d.activity.Store(newID, ev)
	}
	d.ensurePatrol()
	started := time.Now()
	go func() {
		defer baseCancel()
		defer d.subMeta.CompareAndDelete(newID, meta)
		defer d.activity.CompareAndDelete(newID, ev)
		defer d.lastWrites.Delete(newID)
		defer d.heldSkills.Delete(newID)
		paused := false
		runCtx, runCancel, release, gateErr := d.enterExecGate(baseCtx, ev, newID, effectiveTimeout)
		if gateErr != nil {
			log.Printf("[subagent] fork gate-abort: sub=%s err=%v", newID, gateErr)
			// 会话软停止的 gate-abort 由 settleGateAbort 代收口（见 dispatchOne 同名注释）。
			paused = d.settleGateAbort(baseCtx, parentID, newID, roleDef.ID)
		} else {
			defer runCancel()
			defer release()
			paused = d.runSubAgent(runCtx, parentID, newID, *roleDef, seed, node.Domain, "", agent.ModeReact, "", started)
		}
		if !paused && parentID != "" {
			meta.doneOnce.Do(func() { d.trackChildDone(parentID) })
		}
	}()
	if parentID != "" && d.mailbox != nil {
		_, _ = d.mailbox.Send(&mailbox.Message{
			From: "dispatcher", To: parentID, Type: mailbox.MsgInfo,
			Subject: "子 Agent 分叉重派",
			Body:    fmt.Sprintf("子 Agent %s 已分叉重派为新支 %s（剪枝种子重跑，旧支保留可考古），等待新支回传，勿重复派发同领域任务。", node.ID, newID),
		})
		d.pokeParent(parentID)
	}
	if sid != "" {
		d.ledger.RecordDispatch(sid, parentID, newID, node.Domain, truncateRunes(node.Task, 80), "分叉自 "+node.ID)
		d.writeCheckpoint(newID, checkpointPhaseDispatch)
	}
	log.Printf("[subagent] FORK-REVIVE: old=%s new=%s domain=%s", node.ID, newID, node.Domain)
	return nil
}
