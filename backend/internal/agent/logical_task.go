package agent

// logical_task.go 顶层会话与逻辑任务合同绑定（Harness 根治计划 WP1 §5.3）。
//
// 三个入口统一绑定同一套合同：
//   - runSession   ：新会话首跑（创建根任务）
//   - resumeSession：续跑 / 运行中用户纠正（refine 或新建任务）
//   - sendMessageFull：运行中消息注入（经邮箱到达主循环前即刷新合同）
//
// 注入方式（I-STATE-1：合同永不参与有损摘要）：
//   - 上下文：合同块经 directiveBlock 每轮尾部注入（与决策账本同构，不进压缩段）；
//   - 执行授权：合同经 tool.WithLogicalTask 注入 runCtx，Action pipeline 据此判越界。
//
// 灰度：taskManager 为 nil 或 mode=off 时整个文件零行为（与改造前逐字节一致）；
// mode=shadow 只创建/记录不注入、不授权（对拍期）；enforce 才注入 + 授权。

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/server/eventkind"
	"github.com/blockmemory/agent/backend/internal/team"
)

// SetTaskManager 注入逻辑任务管理器（bootstrap 装配；nil=关闭该能力）。
func (s *ReactService) SetTaskManager(m *team.TaskManager) {
	if s == nil {
		return
	}
	s.taskManager = m
}

// TaskManager 返回已注入的管理器（HTTP 查询端点/测试用）。
func (s *ReactService) TaskManager() *team.TaskManager {
	if s == nil {
		return nil
	}
	return s.taskManager
}

// logicalTaskActive 判定是否需要走合同写路径。
func (s *ReactService) logicalTaskActive() bool {
	return s != nil && s.taskManager != nil && s.taskManager.Mode() != team.LogicalTaskModeOff
}

// ensureLogicalTask 首跑/续跑/纠正统一入口：解析或创建当前逻辑任务合同。
//
// input 是"本轮的原始用户输入"（不含召回/技能前缀），actor 标记来源
// （goal/user_message/mailbox/clarify）；workDir 用于解析资源根。
// 返回 nil 表示未启用/无可绑定（legacy fallback，绝不伪造任务）。
func (s *ReactService) ensureLogicalTask(ctx context.Context, session *reactInternalSession,
	input, actor, workDir string) *team.LogicalTaskContract {
	if !s.logicalTaskActive() || session == nil {
		return nil
	}
	mode := s.taskManager.Mode()
	s.store.mu.RLock()
	goal := session.Goal
	s.store.mu.RUnlock()
	taskInput := team.TaskInput{
		SessionID: session.ID,
		Goal:      strings.TrimSpace(goal),
		Actor:     actor,
	}
	// 运行中纠正/续跑：以本轮输入作为当前任务描述（首跑用 goal）。
	if actor != "goal" {
		if txt := strings.TrimSpace(input); txt != "" {
			taskInput.Goal = txt
		}
	}
	// 不变量提取（确定性规则，不调 LLM）：只在用户消息轮做，且只在 enforce 下真生效
	// （shadow 阶段同样提取并记录，用于对拍"会不会漏约束"）。
	extraction := team.ExtractInvariants(input, nil, 0, "", 0)
	if len(extraction.Invariants) > 0 {
		taskInput.Invariants = extraction.Invariants
	}
	// 资源解析（2026-11-19 验收 P0-2）：合同必须真的带写范围，否则 enforce 下
	// 一写入就被拒（真实编码任务无法完成），而若是信任模式自动批准又退化成无限写权。
	scopeText := taskInput.Goal
	if actor != "goal" && strings.TrimSpace(input) != "" {
		scopeText = input
	}
	resolution := team.ResolveScope(scopeText, workDir, nil)
	if len(resolution.WriteSet) > 0 {
		taskInput.WriteSet = resolution.WriteSet
		taskInput.Targets = resolution.Targets
		// 用户纠正时收回旧写范围（只扩权不收口 = 渐进失控）。
		taskInput.ReplaceWriteSet = actor != "goal" && team.IsCorrection(scopeText)
	}
	if len(resolution.ExcludedSet) > 0 {
		taskInput.ExcludedSet = resolution.ExcludedSet
	}
	task, err := s.taskManager.EnsureTask(ctx, taskInput)
	if err != nil {
		// 合同系统故障不得当 legacy 放行（fail closed）：返回 nil 后，enforce 下的
		// Action 管线会因 RequireContract 拒绝未限定范围的写入，并给出可恢复错误。
		log.Printf("[logical-task] ensure failed: session=%s actor=%s err=%v", session.ID, actor, err)
		return nil
	}
	if task == nil {
		return nil
	}
	// 观测落事件流（shadow 也能看到任务划分/继承），用 System 事件避免污染对话正文档。
	if mode != team.LogicalTaskModeEnforce {
		s.store.addEvent(session, eventkind.System, "System",
			fmt.Sprintf("逻辑任务（shadow）：id=%s rev=%d 状态=%s 写范围=%d 禁止=%d 目标=%s",
				task.LogicalTaskID, task.Revision, task.Status,
				len(task.WriteSet), len(task.ExcludedSet), runeEllipsis(task.Goal, 60)),
			"", "", "", "", "", true)
	}
	if mode != team.LogicalTaskModeEnforce {
		// shadow：只记录，不注入上下文、不参与授权（与改造前行为一致）。
		return nil
	}
	s.attachLogicalTask(session, task)
	return task
}

// attachLogicalTask 把合同挂到会话缓存（下一轮 directiveBlock/runCtx 读取）。
func (s *ReactService) attachLogicalTask(session *reactInternalSession, task *team.LogicalTaskContract) {
	if session == nil || task == nil {
		return
	}
	session.logicalTask.Store(task)
}

// currentLogicalTask 读取会话当前合同（未启用/未绑定返回 nil）。
func (s *ReactService) currentLogicalTask(session *reactInternalSession) *team.LogicalTaskContract {
	if session == nil {
		return nil
	}
	if v := session.logicalTask.Load(); v != nil {
		if c, ok := v.(*team.LogicalTaskContract); ok {
			return c
		}
	}
	return nil
}

// logicalTaskBlock 渲染注入上下文的【当前逻辑任务】段（空串=不注入）。
// 与决策账本/长期记忆同槽位（directiveBlock 尾部），确保不被窗口裁剪与压缩吞掉。
func (s *ReactService) logicalTaskBlock(session *reactInternalSession) string {
	c := s.currentLogicalTask(session)
	if c == nil {
		return ""
	}
	maxRunes := s.runtimeCfg.LogicalTaskContractMaxRunes
	return team.RenderContractBlock(c, maxRunes)
}

// withLogicalTaskContext 把合同注入工具上下文（Action pipeline 授权依据）。
// 未绑定合同时原样返回：legacy 会话只做受保护目录校验（兼容旧行为）。
func withLogicalTaskContext(ctx context.Context, c *team.LogicalTaskContract) context.Context {
	if c == nil {
		return ctx
	}
	return tool.WithLogicalTask(ctx, c)
}

// AcceptResult 用户显式接受本次结果（WP6 最小闭环）：verified/delivered-unverified → accepted。
//
// 这是 Harness 唯一的"用户已接受"事实来源：Evolver 只在 accepted 之后允许产生正面技能
// 与长期记忆晋升（I-LEARN-1）。未接线时（mode=off）返回错误提示，不伪造状态。
func (s *ReactService) AcceptResult(ctx context.Context, sessionID string) (*team.LogicalTaskContract, error) {
	if s == nil || s.taskManager == nil || s.taskManager.Mode() == team.LogicalTaskModeOff {
		return nil, fmt.Errorf("%w: 逻辑任务未启用（agent.logical_task_mode=off）", ErrInvalidSessionState)
	}
	active, err := s.taskManager.ResolveActiveContract(ctx, sessionID)
	if err != nil || active == nil || !team.TaskAcceptsUserDecision(active.Status) {
		// 会话已终态（正常结算后 active 就不再存在）：由存储层按**交付事实**查最新
		// 可接受任务。不能用 ListTasks 数组末尾推断——列表按 revision 排序，旧任务被
		// 多次 refine 后 revision 反而更大，会误操作已废弃任务或拒掉真正待接受的结果
		//（2026-11-19 再次验收 P1-2）。
		acceptable, findErr := s.taskManager.Store().FindAcceptable(ctx, sessionID)
		if findErr != nil || acceptable == nil {
			return nil, fmt.Errorf("%w: 该会话没有可接受的逻辑任务（需先交付：verified 或 delivered-unverified）",
				ErrInvalidSessionState)
		}
		active = acceptable
	}
	return s.taskManager.Accept(ctx, active, "user")
}

// runeEllipsis 按 rune 截断（事件展示用；与 react_agent.go 的同名逻辑不复用，
// 避免跨文件改名影响既有提示词渲染路径）。
func runeEllipsis(s string, max int) string {
	if max <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// settleLogicalTask 会话收尾时推进任务状态（WP3/WP6 生产状态迁移，2026-11-19 验收要求）。
//
// 旧实现只有 accepted 一个转换点，正常任务永远停在 active，导致：
//   - `POST /accept` 无法接受（只允许 verified/delivered-unverified）；
//   - Evolver 闸门永远阻断，真正的"用户接受后学习"链路走不到。
//
// 这里按确定性的保守规则落状态（不调用 LLM、不假造 verified）：
//   - 会话成功但无机器验收证据 → delivered-unverified（黄态，可被用户 accepted）；
//   - 会话出错/取消 → failed（不得直接 accepted，需重新激活）；
//   - 已是终态 → 不动（幂等）。
func (s *ReactService) settleLogicalTask(session *reactInternalSession, outcome string) {
	if s == nil || s.taskManager == nil || s.taskManager.Mode() != team.LogicalTaskModeEnforce || session == nil {
		return
	}
	c := s.currentLogicalTask(session)
	if c == nil || team.IsTerminalTaskStatus(c.Status) {
		return
	}
	ctx := context.Background()
	cur := c
	if cur.Status != team.TaskStatusAwaitingVerification {
		// 这里是**纯状态迁移**，必须用不带合同替换语义的事件：复用 refined 会写入
		// 空 payload 的快照事件，重放时 JSON 解码失败，事件底账与投影因此不一致
		//（2026-11-19 再次验收 P0-2）。
		transitioned, err := s.taskManager.Transition(ctx, cur, team.TaskStatusAwaitingVerification,
			team.LogicalEventAwaitingVerification, "session", "session turn settled")
		if err != nil {
			log.Printf("[logical-task] settle: awaiting_verification failed: session=%s task=%s err=%v",
				session.ID, cur.LogicalTaskID, err)
			return
		}
		cur = transitioned
	}
	next, kind := team.TaskStatusDeliveredUnverified, team.LogicalEventUnverified
	if outcome != "success" {
		next, kind = team.TaskStatusFailed, team.LogicalEventFailed
	}
	transitioned, err := s.taskManager.Transition(ctx, cur, next, kind, "session", "outcome="+outcome)
	if err != nil {
		log.Printf("[logical-task] settle: %s failed: session=%s task=%s err=%v",
			session.ID, next, cur.LogicalTaskID, err)
		return
	}
	s.attachLogicalTask(session, transitioned)
	s.store.addEvent(session, eventkind.System, "System",
		fmt.Sprintf("逻辑任务已结算：id=%s rev=%d 状态=%s（用户可在结果上「接受」，接受后才会产生经验沉淀）",
			transitioned.LogicalTaskID, transitioned.Revision, transitioned.Status),
		"", "", "", "", "", true)
}

// evolveGateBlocks 判定本次会话收尾是否必须跳过正面学习（I-LEARN-1）。
//
// 只在任务合同 enforce 模式下生效（shadow/off 保持既有行为，便于灰度回退）：
//   - 已 accepted → 放行；
//   - verified / delivered-unverified / failed / cancelled / 无任务 → 阻断；
//   - 任务仍 active（会话被中断/暂停）→ 阻断（未完成的任务不该沉淀经验）。
func (s *ReactService) evolveGateBlocks(session *reactInternalSession) bool {
	if s == nil || s.taskManager == nil || s.taskManager.Mode() != team.LogicalTaskModeEnforce {
		return false
	}
	if session == nil {
		return true
	}
	c := s.currentLogicalTask(session)
	if c == nil {
		// enforce 下未绑定合同（legacy 会话/合同解析失败）：不阻断既有行为，
		// 但绝不把它当 accepted——阻断只针对"新合同任务"，避免存量会话集体停学。
		return false
	}
	return c.Status != team.TaskStatusAccepted
}

// shortContentHash 事件展示用：合同 hash 只留前 12 位（可对照、不刷屏）。
func shortContentHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
