// deadletter.go 实现 Purge 死信通知（2026-09-28 P0）：子 Agent 终结/被杀/销毁时
// 收件箱里未读的 request/escalate 不再静默消失——给发送方回投"未送达"通知并把
// 待应答注册销账，消除"发送时成功、投递后被清掉"的假投递。
package subagent

import (
	"fmt" // 通知正文格式化
	"log" // 死信投递失败仅记日志（best-effort）

	"github.com/blockmemory/agent/backend/internal/mailbox"
)

// purgeMailboxWithNotice 清空调用方指定 Agent 的收件箱，并对被丢弃的未读
// request/escalate 逐条给发送方回投 MsgInfo 死信通知（info 单向通知不回投——
// 它没有"等回复"语义）。reason 说明终结原因（进通知正文，供发送方决策改派/收口）。
// 会话级硬删除（PurgeSession/finalizeSession）不走这里：收件方同会话陪葬，通知无意义。
func (d *Dispatcher) purgeMailboxWithNotice(agentID, reason string) {
	if d.mailbox == nil {
		return
	}
	dropped := d.mailbox.Purge(agentID)
	for _, m := range dropped {
		if m.Type != mailbox.MsgRequest && m.Type != mailbox.MsgEscalate {
			continue
		}
		// 系统侧/自发消息不回投（user 直连有 UI 反馈，dispatcher/system 无消费方）。
		if m.From == "" || m.From == "user" || m.From == "dispatcher" || m.From == "system" || m.From == agentID {
			continue
		}
		// 死信等价于"永远不会有回复"：pending 注册销账，防超时升级误报。
		if d.pendingReqs != nil {
			d.pendingReqs.complete(m.ID)
		}
		if _, err := d.mailbox.Send(&mailbox.Message{
			From:    "dispatcher",
			To:      m.From,
			Type:    mailbox.MsgInfo,
			Subject: "消息未送达: " + truncateRunes(m.Subject, 60),
			Body: fmt.Sprintf("【系统】你发给 %s 的 %s（消息 %s：%s）未能送达：对方%s。"+
				"请据现状决定改派他人/自行处置/在回传中说明。",
				agentID, m.Type, m.ID, truncateRunes(m.Subject, 80), reason),
		}); err != nil {
			log.Printf("[subagent] dead-letter notice failed: to=%s msg=%s err=%v", m.From, m.ID, err)
		}
	}
}
