// activity.go 实现子 Agent 活动证据化（TODO 第10项②，对标 Codex stall 判定）：
// 盲时间戳（*atomic.Int64）升级为结构化证据 activityEvidence——巡检不再只看
// "多久没动静"，而是区分三种状态：
//  1. 在飞 LLM（llmInFlight）→ 一律豁免（慢思考单呼可达 25min，流式 chunk/流空闲
//     卡口/调用超时/会话墙钟另有兜底，不靠心跳杀）；
//  2. 工具在飞超阈值（toolStartTS）→ 杀（真挂死工具；keepalive 盲报不再刷新 lastTS
//     续命，RunCommand 自身 60s 超时兜底在前，MCP 长操作调大阈值）；
//  3. 步间静默超阈值（lastTS）→ 杀（步间死锁）。
//
// 活动经 activityReporter(kind) 语义化上报：llm_start/llm_end（ReAct 主循环 LLM 调用
// 首尾）、tool:<名>/tool_end（工具派发首尾）、stream（流式 chunk，真实步进）、
// keepalive（保活 tick，证明进程活着但不刷新 lastTS——盲报根除）、user_wait
//（审批/提问等待，PingActivity，刷新 lastTS——等用户不算静默）、descendant
//（后代活动冒泡，刷新 lastTS——合法等待）。
package subagent

import (
	"strings"
	"sync/atomic"
	"time"
)

// activityEvidence 是单个子 Agent 的活动证据（巡检只读，reporter 写入；分字段原子）。
type activityEvidence struct {
	// lastTS 最后一次"真实步进"活动时间（llm_start/llm_end/tool/stream/user_wait/
	// descendant）；keepalive 不刷新。UnixNano；0=从未活动（立即判静默）。
	lastTS atomic.Int64
	// lastKind lastTS 对应的活动种类（展示用："in <tool> · active Xs ago"）。
	lastKind atomic.Value // string
	// llmStartTS 当前 LLM 调用（含引擎辅助 LLM）开始时间；0=无在飞。
	llmStartTS atomic.Int64
	// llmInFlight LLM 调用进行中（ReAct 主循环与引擎 judge/plan 共用）。
	llmInFlight atomic.Bool
	// toolStartTS 当前工具派发开始时间；0=无在飞工具。
	toolStartTS atomic.Int64
	// toolName 在飞工具名（并行派发时为最近一次 tool:<名> 证据，仅展示用）。
	toolName atomic.Value // string
	// waitingChildren 是否正处于 waitForChildren 阻塞等待（编排页"等待下级返回"标识，
	// 兼作用户直连的发送闸门）。等子期间后代活动冒泡只续命（lastTS）不覆盖 lastKind，
	// 否则 child_wait 展示态会被 descendant 冒泡秒刷掉（实证：子 Agent 流式期间
	// child_wait 占空比≈0，前端恒显"执行中"、直连恒 409）；Agent 自身恢复活动即清除。
	waitingChildren atomic.Bool
	// queued 并发池排队标记（2026-09-28 P1 并发池）：Acquire 等待期间置位，巡检豁免——
	// 排队中的 Agent 还没开始执行，lastTS 停留在派发时刻，不豁免则排队超阈值被误杀。
	queued atomic.Bool
}

// newEvidence 创建并初始化一条活动证据（lastTS=now，kind=created）。
func newEvidence() *activityEvidence {
	e := &activityEvidence{}
	now := time.Now().UnixNano()
	e.lastTS.Store(now)
	e.lastKind.Store("created")
	return e
}

// report 按语义化 kind 更新证据（activityReporter 闭包调用；kind 约定见文件头注释）。
// keepalive 等未知 kind 不刷新 lastTS（盲报根除：保活证明进程活着，不证明任务在推进）。
func (e *activityEvidence) report(kind string, now int64) {
	switch {
	case kind == "llm_start":
		e.llmInFlight.Store(true)
		e.llmStartTS.Store(now)
		e.stamp(kind, now)
	case kind == "llm_end":
		e.llmInFlight.Store(false)
		e.llmStartTS.Store(0)
		e.stamp(kind, now)
	case strings.HasPrefix(kind, "tool:"):
		e.toolStartTS.Store(now)
		e.toolName.Store(strings.TrimPrefix(kind, "tool:"))
		e.stamp(kind, now)
	case kind == "tool_end":
		e.toolStartTS.Store(0)
		e.toolName.Store("")
		e.stamp(kind, now)
	case kind == "stream" || kind == "user_wait" || kind == "descendant":
		e.stamp(kind, now)
	default:
		// keepalive 等：不刷新 lastTS。
	}
}

// stamp 刷新 lastTS 与 lastKind。
// 例外：等子展示态（waitingChildren）期间的后代冒泡只续命不换 kind——等子存活性仍由
// lastTS 保证（不误杀），展示面与发送闸门则稳定停在 child_wait，直到 Agent 自身恢复活动
// （llm_start/tool 等非 descendant kind 到达）清除该态。
func (e *activityEvidence) stamp(kind string, now int64) {
	e.lastTS.Store(now)
	if kind == "descendant" && e.waitingChildren.Load() {
		return
	}
	e.waitingChildren.Store(false)
	e.lastKind.Store(kind)
}

// markChildWait 标记进入 waitForChildren 等待（编排页展示态，不刷新 lastTS：
// 等子期间的存活判定仍由后代活动冒泡决定，展示态不续命、不掩盖"后代全灭"）。
func (e *activityEvidence) markChildWait() {
	e.lastKind.Store("child_wait")
	e.waitingChildren.Store(true)
}

// markQueued 标记进入并发池排队（巡检豁免 + 展示态 "queued"）。
func (e *activityEvidence) markQueued() {
	e.queued.Store(true)
	e.lastKind.Store("queued")
}

// clearQueued 标记出队开始执行：刷 lastTS（排队时长不计入静默）并落 "dequeued" 展示态。
func (e *activityEvidence) clearQueued(now int64) {
	e.queued.Store(false)
	e.lastTS.Store(now)
	e.lastKind.Store("dequeued")
}

// beginAuxLLM 标记引擎辅助 LLM（judge/plan_execute）开始：在飞 LLM 豁免巡检
//（长考 judge 不再靠 ticker 盲报续命，调用结束/超时/墙钟自然收口）。
func (e *activityEvidence) beginAuxLLM(now int64) {
	e.llmInFlight.Store(true)
	e.llmStartTS.Store(now)
}

// endAuxLLM 标记引擎辅助 LLM 结束。
func (e *activityEvidence) endAuxLLM(now int64) {
	e.llmInFlight.Store(false)
	e.llmStartTS.Store(0)
	e.stamp("llm_end", now)
}

// kindString 读 lastKind（空值安全）。
func (e *activityEvidence) kindString() string {
	if v, ok := e.lastKind.Load().(string); ok {
		return v
	}
	return ""
}

// toolNameString 读 toolName（空值安全）。
func (e *activityEvidence) toolNameString() string {
	if v, ok := e.toolName.Load().(string); ok {
		return v
	}
	return ""
}

// activityEvidenceFor 返回该 Agent 的活动证据；未注册（meta/已终结）返回 nil。
func (d *Dispatcher) activityEvidenceFor(agentID string) *activityEvidence {
	if agentID == "" {
		return nil
	}
	if v, ok := d.activity.Load(agentID); ok {
		return v.(*activityEvidence)
	}
	return nil
}

// ActivityEvidenceOf 查询子 Agent 活动证据（展示面：TUI/Web "in <tool> · active Xs ago"）。
// ok=false 表示该 Agent 无活动监控条目（meta/已终结/热驻 Idle）。
func (d *Dispatcher) ActivityEvidenceOf(agentID string) (kind string, lastAgo time.Duration, ok bool) {
	e := d.activityEvidenceFor(agentID)
	if e == nil {
		return "", 0, false
	}
	last := e.lastTS.Load()
	if last == 0 {
		return e.kindString(), 0, true
	}
	return e.kindString(), time.Since(time.Unix(0, last)), true
}
