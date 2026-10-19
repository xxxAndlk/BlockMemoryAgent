package agent

// directive.go 会话"指令模式"（goal 目标模式 / loop 循环模式）契约。
//
// 语义（2026-10-18，对标 Kimi CLI /goal + 循环执行）：
//   - goal：会话挂 objective + 完成条件 + 预算，每轮终答由外部评估器判定，
//     未达成自动注入"继续推进"续跑；受外部阻塞（如审核数日）时 Agent 可用
//     schedule_followup 工具自排回查，到点系统注入提醒恢复推进。
//   - loop：按 loop_interval_sec 间隔重复执行 objective，直到手动停止/切普通，
//     或 loop_until 停止条件被评估满足。
//
// 实现挂在 goal.Manager（internal/goal），经 DirectiveHook 注入；nil 时全部零行为。
// agent 包不依赖 goal 包（钩子接口 + DTO 自定义，避免反向依赖）。

import (
	"context"
	"fmt"
)

// DirectiveSpec 是创建会话/发消息/切换模式时携带的指令规格（JSON 线型同名）。
type DirectiveSpec struct {
	// Mode 指令模式：normal|goal|loop（空/normal = 不启用，等同无指令）。
	Mode string `json:"mode"`
	// Objective 目标/循环指令文本；goal/loop 必填（调用方已回落为消息内容时可为空串）。
	Objective string `json:"objective,omitempty"`
	// CompletionCriterion goal 完成条件（可空 = 按目标语义判定）。
	CompletionCriterion string `json:"completion_criterion,omitempty"`
	// MaxTurns 预算轮数（0=不限；超限停止）。
	MaxTurns int `json:"max_turns,omitempty"`
	// Deadline 预算截止时间（RFC3339，可空）。
	Deadline string `json:"deadline,omitempty"`
	// LoopIntervalSec loop 间隔秒（loop 必填；Manager 侧按最小间隔收敛）。
	LoopIntervalSec int `json:"loop_interval_sec,omitempty"`
	// LoopUntil loop 停止条件描述（可空 = 无限循环直到手动停）。
	LoopUntil string `json:"loop_until,omitempty"`
}

// DirectiveHook 是 ReactService 对指令模式的回调面（goal.Manager 实现，bootstrap 注入）。
// 全部方法 best-effort：失败只记事件/日志，不影响会话本身流转。
type DirectiveHook interface {
	// ApplyDirective 应用/切换会话指令（创建会话与 HTTP 切换共用，upsert 语义）。
	// objective 为空时实现方按会话消息回落由调用方保证。
	ApplyDirective(ctx context.Context, sessionID string, spec *DirectiveSpec) error
	// OnTurnEnd 回合结束分流：status=completed 时由实现方评估并决定续跑/终局。
	// 由 ReactService 以 goroutine 异步调用（评估可能跑轻量模型，耗时秒级）。
	OnTurnEnd(sessionID, status, finalText string)
	// Deactivate 停止会话指令（切 normal / 用户 Stop/Interrupt/Cancel 时调用）。
	Deactivate(sessionID, reason string)
}

// SetDirectiveHook 注入指令模式钩子（bootstrap 在 goal.Manager 就绪后调用）。
// nil（默认）时 goal/loop 全部零行为。
func (s *ReactService) SetDirectiveHook(h DirectiveHook) {
	s.directiveHook = h
}

// ApplyDirective 应用/切换会话指令模式（server 层消息路径的鸭子类型入口）。
// objective 为空时调用方应先回落为消息内容；未接线返回错误。
func (s *ReactService) ApplyDirective(ctx context.Context, sessionID string, spec *DirectiveSpec) error {
	if s.directiveHook == nil {
		return fmt.Errorf("指令模式未接线（goal 管理器未注入）")
	}
	return s.directiveHook.ApplyDirective(ctx, sessionID, spec)
}

// notifyDirectiveTurnEnd 异步通知指令钩子：本轮终态 + 终答文本。
// status 只传回合终态（completed/paused/awaiting_child/...），由钩子自行分流。
func (s *ReactService) notifyDirectiveTurnEnd(sessionID, status, finalText string) {
	if s.directiveHook == nil {
		return
	}
	go s.directiveHook.OnTurnEnd(sessionID, status, finalText)
}

// stopDirective 用户显式停止类操作时同步停止指令（best-effort，不阻断主流程）。
func (s *ReactService) stopDirective(sessionID, reason string) {
	if s.directiveHook == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			// 钩子 panic 不得影响会话停止主流程。
		}
	}()
	s.directiveHook.Deactivate(sessionID, reason)
}
