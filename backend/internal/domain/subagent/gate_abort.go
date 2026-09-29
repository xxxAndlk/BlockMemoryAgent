package subagent

// gate_abort.go 并发池排队期取消（gate-abort）的软停止收口补漏（TODO #37 配套）。
//
// 背景：dispatchOne/ReviveWithMessage/fork 的 dispatch goroutine 先 enterExecGate 再
// runSubAgent；排队期 baseCtx 被取消时 gate 出局、runSubAgent 不执行。cancel_agent
// 与心跳巡检硬取消由取消方收口树态与父通知；但会话软停止（ReactService.Stop）刻意
// 不改树态、委托 runSubAgent 的 context.Canceled 分支收口（domain→Paused 可续跑 /
// 叶子→部分回灌）——gate 出局时该分支永不执行，节点卡 Running（PG 常驻），
// 工作项无声消失。settleGateAbort 在 gate-abort 分支按软停止语义代收口。

import (
	"context"
	"log"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
)

// settleGateAbort 处理 gate-abort（排队期被取消）的会话软停止收口。
// 返回 true 表示按 paused 语义处理（调用方跳过 trackChildDone，PendingChildren
// 保持 >0）；返回 false 表示调用方走正常尾部（doneOnce → trackChildDone）。
//
//   - 非软停止（cancel_agent/巡检硬取消）：返回 false 零副作用——树态与父通知由
//     取消方收口，此处不越俎代庖。
//   - 软停止 + domain：与 runSubAgent 软停止分支同口径——tree.Pause（可续跑，
//     resume_agent 经 ResumePaused 重进 gate）+ pokeParent 唤醒父 wait loop，
//     不 notify、不递减 PendingChildren（父终结保护 → PausedOnChild，恢复路由生效）。
//   - 软停止 + 叶子：无 Pause 语义——终态落树（与 ResumePaused gate-abort 同口径：
//     treeFinish 带 context.Canceled）+ notify 父"排队中随会话停止被取消，未执行"，
//     返回 false 由调用方递减计数。
//
// ctx 为已取消的 baseCtx：仅用于读 sessionID（ctx 取消后 value 仍可读）；
// treeFinish 内部也只取 sessionID，mailbox 发送不依赖活跃 ctx，无需脱取消。
func (d *Dispatcher) settleGateAbort(ctx context.Context, parentID, subAgentID, roleID string) bool {
	sid := tool.SessionIDFromContext(ctx)
	if sid == "" || !d.isSoftStop(sid) {
		return false
	}
	if roleID == "domain" {
		if d.treeFn != nil {
			if t := d.treeFn(sid); t != nil {
				t.Pause(subAgentID, "user stop")
			}
		}
		if parentID != "" {
			d.pokeParent(parentID)
		}
		log.Printf("[subagent] SOFT-STOP QUEUED-PAUSED: sub=%s role=%s（排队期随会话停止，待续跑）", subAgentID, roleID)
		return true
	}
	d.treeFinish(ctx, subAgentID, "", context.Canceled)
	if parentID != "" {
		d.notify(parentID, subAgentID, failureMarker(FailureKindError, false)+"\n子 Agent 排队中随会话停止被取消，未执行；续跑后可按需重派。", nil)
	}
	log.Printf("[subagent] SOFT-STOP QUEUED-CANCELLED: sub=%s role=%s（排队期随会话停止，未执行）", subAgentID, roleID)
	return false
}
