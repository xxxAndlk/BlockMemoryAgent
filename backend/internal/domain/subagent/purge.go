// purge.go 实现会话硬删除的运行时清扫（ReactService.DeleteSession 经接口断言调用）。
// PG 数据删除由 agent 层 store.DeleteSessionData 负责，本文件只负责 dispatcher
// 进程内按会话/节点键控的全部运行时状态，防止删除后残留影响同进程后续会话。
package subagent

import (
	"log"
	"strings"
)

// PurgeSession 清空指定会话在 dispatcher 进程内的全部运行时状态：
//   - 软停标记 / 手动暂停标记 / 挂起状态 / 分发计数 / 任务台账；
//   - 热驻槽（destroySlot 级联：树收尾、实例级模型覆盖回收、邮箱清理、父未决计数补偿）；
//   - 按 nodeIDs（会话内全部节点）键控的在跑表/活动证据/子 Agent 元信息/写盘清单/
//     最近活动/技能持有/聚合登记；
//   - 按 parentID 前缀扫描的 spec 缓存/契约违例去重表/计划确认状态。
//
// 幂等：对不存在的会话/节点全部 no-op。调用方（DeleteSession）已先行取消
// 会话 ctx 与树节点 cancelFn，本方法只管状态表清理，不再触发取消。
func (d *Dispatcher) PurgeSession(sessionID string, nodeIDs []string) {
	if d == nil || sessionID == "" {
		return
	}
	// 1. 会话级标记与计数。
	d.softStopMu.Lock()
	delete(d.softStops, sessionID)
	d.softStopMu.Unlock()
	d.sessionCounts.Delete(sessionID)
	if d.ledger != nil {
		d.ledger.Purge(sessionID)
	}
	d.suspendStates.Delete(sessionID)
	// 2. 节点级标记：手动暂停标记 + 各类 sync.Map。
	d.pauseRequestMu.Lock()
	for _, id := range nodeIDs {
		delete(d.pauseRequests, id)
	}
	d.pauseRequestMu.Unlock()
	for _, id := range nodeIDs {
		d.running.Delete(id)
		d.activity.Delete(id)
		d.lastWrites.Delete(id)
		d.recentActs.Delete(id)
		d.subMeta.Delete(id)
		d.heldSkills.Delete(id)
		d.aggByAgent.Delete(id)
	}
	// 3. 热驻槽销毁（内部完成池摘除/邮箱清理/树收尾/模型覆盖回收）。
	if d.pool != nil {
		for _, s := range d.pool.slots(sessionID) {
			d.destroySlot(s, "session deleted")
		}
	}
	// 3.5 节点邮箱清理（会话 ID 本身由 agent 层 DeleteSession 兜底清）。
	if d.mailbox != nil {
		for _, id := range nodeIDs {
			d.mailbox.Purge(id)
		}
	}
	// 4. 前缀扫描类表：键含节点 ID（可能不在 nodeIDs 快照中的迟到节点也一并清）。
	inSession := func(id string) bool {
		return id == sessionID || strings.HasPrefix(id, sessionID+"/")
	}
	d.parentSpecs.Range(func(k, _ any) bool {
		if key, ok := k.(specRecKey); ok && inSession(key.parentID) {
			d.parentSpecs.Delete(k)
		}
		return true
	})
	d.pushedViolations.Range(func(k, _ any) bool {
		if s, ok := k.(string); ok && hasViolationKeySession(s, sessionID) {
			d.pushedViolations.Delete(k)
			d.pushedViolationTimes.Delete(k)
		}
		return true
	})
	// 4.5 未决子计数表（F3①）：parentID 恒为会话 ID 或 "会话ID/节点ID" 前缀，此前
	// 无任何 Delete——每个派发过子 Agent 的节点条目永久驻留 sync.Map，长会话内存只增不减。
	// 会话已整体终结（前面已取消全部节点），不存在新派发与删除的竞态，前缀整批回收。
	d.pending.Range(func(k, _ any) bool {
		if key, ok := k.(string); ok && inSession(key) {
			d.pending.Delete(k)
		}
		return true
	})
	if d.planState != nil {
		d.planState.purgeSession(sessionID)
	}
	log.Printf("[subagent] SESSION PURGED: session=%s nodes=%d", sessionID, len(nodeIDs))
}

// hasViolationKeySession 报告违例去重键（"parentID\x00指纹"）归属指定会话。
func hasViolationKeySession(key, sessionID string) bool {
	parentID, _, _ := strings.Cut(key, "\x00")
	return parentID == sessionID || strings.HasPrefix(parentID, sessionID+"/")
}
