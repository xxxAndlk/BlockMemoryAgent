package agent

// task_ledger_context.go 实现任务台账注入（2026-08-28 事故根治）：
// MetaAgent 每轮上下文末尾追加【任务台账】system 消息——由 Dispatcher 机器维护的
// 派发任务状态流水（进行中/完成/失败+原因+修改文件），是压缩不可达、重启可经权威树
// 播种恢复的状态锚点。根治"新一轮用户消息时 Meta 把上一轮已完成/已失败的旧需求
// 当作未完成重新派发返工"（旧需求是否做过原先全凭模型对原始历史的记忆）。
//
// 实现仿 board_context.go：包装 MemoryPipeline 的 Assemble——在流水线输出
// （含压缩视图 + 近期事件 + 看板 + 空闲领域清单）之后追加台账段。只对 MetaAgent
// 使用（runSession/resumeSession 构造 meta 时包装），子 Agent 不注入。
// ledger 回调返回空串时零变化（未接线/无派发记录）。

import (
	"strings"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// ledgerInjectingPipeline 包装 MemoryPipeline，在 Assemble 输出末尾追加任务台账段。
// ledger 每轮调用（读实时台账快照）；空串跳过注入。
type ledgerInjectingPipeline struct {
	inner  MemoryPipeline
	ledger func() string
}

// Assemble 委托内层流水线后追加台账段（在【空闲领域Agent】之后，属不可缓存尾部，
// 不破坏前缀缓存）。
func (l *ledgerInjectingPipeline) Assemble(role types.RoleDefinition, agentID string, history []ReactMessage) []ReactMessage {
	out := l.inner.Assemble(role, agentID, history)
	if l.ledger != nil {
		if txt := strings.TrimSpace(l.ledger()); txt != "" {
			out = append(out, ReactMessage{Role: "system", Content: txt})
		}
	}
	return out
}

// Write 委托内层流水线。
func (l *ledgerInjectingPipeline) Write(agentID string, ev MemoryEvent) error {
	return l.inner.Write(agentID, ev)
}

// wrapMetaMemoryWithLedger 为 MetaAgent 包装记忆流水线：ledgerFn 已接线时注入台账段。
// 返回包装后的流水线；未接线返回原流水线（零行为变化）。
// 每轮经闭包重取台账（首个派发在 meta 首轮之后才发生，构造期查询会漏）。
func wrapMetaMemoryWithLedger(inner MemoryPipeline, ledgerFn func(sessionID string) string, sessionID string) MemoryPipeline {
	if ledgerFn == nil {
		return inner
	}
	return &ledgerInjectingPipeline{
		inner:  inner,
		ledger: func() string { return ledgerFn(sessionID) },
	}
}
