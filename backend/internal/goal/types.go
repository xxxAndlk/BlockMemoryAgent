// Package goal 实现会话"指令模式"（goal 目标模式 / loop 循环模式，2026-10-18）。
//
// 语义对照 Kimi CLI 的 /goal 与循环执行：
//   - goal：会话挂一个目标（objective + 完成条件 + 预算），每轮终答经轻量模型评估，
//     未达成则自动注入"继续推进"消息续跑；达成则切回普通模式。Agent 可用
//     schedule_followup 工具把"回查"排到未来（如开店审核需数天，Agent 自己规划
//     首次回查时间，未通过再排下一次），到点由调度器注入提醒消息并恢复推进。
//   - loop：按固定间隔重复执行指令（每轮结束排下一次，调度器到点唤醒），
//     直到手动切普通模式 / 用户停止，或可选的停止条件(loop_until)被评估满足。
//
// 设计要点：
//   - 权威全在 PG（session_directive / goal_reminders），重启恢复 = 扫表，无内存权威；
//   - 与 ReAct 主循环零侵入：agent 包只在"回合成功结束"处分流（DirectiveHook.OnTurnEnd），
//     唤醒复用 ReactService.Send（运行中投邮箱 / 终态懒恢复续跑同一条路径）；
//   - 评估器 = ModelFactory.CallLightweightWithRetry（照 workflow/judge.go 模式）。
package goal

import "time"

// 指令模式枚举（Directive.Mode / DirectiveSpec.Mode 共用）。
const (
	ModeNormal = "normal"
	ModeGoal   = "goal"
	ModeLoop   = "loop"
)

// DirectiveStatus 枚举（session_directive.status）。
const (
	StatusActive   = "active"   // 生效中：回合结束会被评估/续排
	StatusComplete = "complete" // 目标达成 / 循环停止条件满足（正常终态）
	StatusFailed   = "failed"   // 预算耗尽（最大轮数/截止时间）停止
	StatusStopped  = "stopped"  // 手动停止（切普通模式 / 用户 Stop / 切换指令）
)

// Reminder 相关常量。
const (
	ReminderKindOnce     = "once"     // 一次性回查（schedule_followup 排的）
	ReminderKindInterval = "interval" // loop 的下一轮
	ReminderStatusPending   = "pending"
	ReminderStatusFired     = "fired"
	ReminderStatusCancelled = "cancelled"
)

// Directive 是 session_directive 的行模型（一会话一行，upsert）。
type Directive struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	// Mode 指令模式：normal|goal|loop（normal = 无生效指令）。
	Mode string `json:"mode"`
	// Objective 目标/循环指令文本（goal/loop 必填，空时由调用方回落为会话消息）。
	Objective string `json:"objective"`
	// CompletionCriterion goal 完成条件（可空 = 按目标语义判定）。
	CompletionCriterion string `json:"completion_criterion"`
	// MaxTurns 预算轮数（0=不限；goal 超限 failed，loop 超限 stopped）。
	MaxTurns int `json:"max_turns"`
	// Deadline 预算截止时间（可空）。
	Deadline *time.Time `json:"deadline"`
	// LoopIntervalSec loop 间隔秒（SetMode 时已按最小间隔收敛）。
	LoopIntervalSec int `json:"loop_interval_sec"`
	// LoopUntil loop 停止条件描述（可空 = 无限循环直到手动停）。
	LoopUntil string `json:"loop_until"`
	// Status 指令状态（active/complete/failed/stopped）。
	Status string `json:"status"`
	// TurnCount 已评估回合数（每轮成功结束 +1）。
	TurnCount int `json:"turn_count"`
	// LastEval 评估器最近一次的结论文本（展示用）。
	LastEval string `json:"last_eval"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Reminder 是 goal_reminders 的行模型（到点注入会话的系统消息）。
type Reminder struct {
	ID          string     `json:"id"`
	SessionID   string     `json:"session_id"`
	DirectiveID string     `json:"directive_id"`
	// Kind once=一次性回查 / interval=loop 下一轮。
	Kind string `json:"kind"`
	// Text 到点注入会话的消息正文。
	Text string `json:"text"`
	// RunAt 到点时间（调度器扫描 status=pending AND run_at<=now）。
	RunAt time.Time `json:"run_at"`
	// Status pending|fired|cancelled。
	Status string `json:"status"`
	// CreatedBy agent=Agent 自排（schedule_followup）/ user=用户 / system=系统（loop 续排）。
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	FiredAt   *time.Time `json:"fired_at,omitempty"`
}

// DirectiveView 是 GET/PUT 端点的线型视图：当前指令 + 未触发的提醒。
type DirectiveView struct {
	Directive        *Directive  `json:"directive"`
	PendingReminders []*Reminder `json:"pending_reminders"`
}

// ValidMode 校验指令模式枚举。
func ValidMode(m string) bool {
	return m == ModeNormal || m == ModeGoal || m == ModeLoop
}
