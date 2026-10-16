package workflow

// events.go unit/run 状态迁移的事件账本写入（开发文档 §4.2）：
// 每次状态迁移写 session_events（kind=workflow_unit，detail_json 带 run_id/unit_id/from/to），
// 监控页可见。事件经 Emitter 回调送入会话事件流（bootstrap 适配 agent.LiveEvent →
// ReactService.ForwardLiveEvent → handleLiveEvent 落 session_events）。

import (
	"encoding/json"
	"fmt"
)

// UnitEvent 是一次 unit 状态迁移事件。
type UnitEvent struct {
	RunID     string `json:"run_id"`
	UnitID    string `json:"unit_id"`
	ItemID    string `json:"item_id"`
	ItemName  string `json:"item_name,omitempty"`
	Stage     string `json:"stage"`
	AttemptNo int    `json:"attempt_no"`
	From      string `json:"from"`
	To        string `json:"to"`
	Reason    string `json:"reason,omitempty"`
}

// DetailJSON 组装事件 detail_json（监控页按这些字段过滤/聚合）。
func (ev UnitEvent) DetailJSON() string {
	extra := map[string]any{"run_id": ev.RunID, "unit_id": ev.UnitID, "item_id": ev.ItemID, "stage": ev.Stage}
	if ev.ItemName != "" {
		extra["item_name"] = ev.ItemName
	}
	if ev.AttemptNo > 0 {
		extra["attempt_no"] = ev.AttemptNo
	}
	if ev.From != "" {
		extra["from"] = ev.From
	}
	if ev.To != "" {
		extra["to"] = ev.To
	}
	if ev.Reason != "" {
		extra["reason"] = ev.Reason
	}
	b, err := json.Marshal(extra)
	if err != nil {
		return ""
	}
	return string(b)
}

// Text 组装事件正文（对话/监控页一行可读）。
func (ev UnitEvent) Text() string {
	name := ev.ItemName
	if name == "" {
		name = ev.ItemID
	}
	msg := fmt.Sprintf("工作流 %s：%s/%s", ev.RunID, name, ev.Stage)
	if ev.AttemptNo > 1 {
		msg += fmt.Sprintf("（第 %d 次尝试）", ev.AttemptNo)
	}
	switch {
	case ev.From == "" && ev.To == UnitStatusPending:
		msg += " 已排队重摇"
	case ev.From != "" && ev.To != "":
		msg += fmt.Sprintf(" %s → %s", ev.From, ev.To)
	default:
		msg += " " + ev.To
	}
	if ev.Reason != "" {
		msg += "（" + ev.Reason + "）"
	}
	return msg
}

// RunEvent 是 run 级事件（创建/终态/暂停），detail_json 只带 run_id 与状态。
type RunEvent struct {
	RunID  string `json:"run_id"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	Note   string `json:"note,omitempty"`
}

// DetailJSON 组装 detail_json。
func (ev RunEvent) DetailJSON() string {
	extra := map[string]any{"run_id": ev.RunID, "status": ev.Status}
	if ev.Reason != "" {
		extra["reason"] = ev.Reason
	}
	b, err := json.Marshal(extra)
	if err != nil {
		return ""
	}
	return string(b)
}

// Text 组装正文。
func (ev RunEvent) Text() string {
	msg := fmt.Sprintf("工作流运行 %s：%s", ev.RunID, ev.Status)
	if ev.Reason != "" {
		msg += "（" + ev.Reason + "）"
	}
	if ev.Note != "" {
		msg += " " + ev.Note
	}
	return msg
}
