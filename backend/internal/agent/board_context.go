package agent

// board_context.go 实现 TODO #35 Phase 0 任务看板注入：
// MetaAgent 每轮上下文末尾追加【任务看板】system 消息——看板状态（哪些领域完成/失败/
// 进行中、目标、依赖）是机器可读且压缩不可达的编排状态锚点，"重新执行"类无宾语短指令
// 得以消歧（2026-08-10 事故根因 2：board 只喂 TUI 面板不进 LLM 上下文）。
//
// 实现：包装 MemoryPipeline 的 Assemble——在流水线输出（含压缩视图 + 近期事件）之后
// 追加看板段。只对 MetaAgent 使用（runSession/resumeSession 构造 meta 时包装），
// 子 Agent 不注入（干活 Agent 无需看板全貌，防上下文膨胀）。
// board 回调返回空串时零变化（未接线/看板未创建）。

import (
	"strings"

	"github.com/blockmemory/agent/backend/internal/board"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// boardInjectingPipeline 包装 MemoryPipeline，在 Assemble 输出末尾追加任务看板段。
// board 每轮调用（读实时看板快照）；空串跳过注入。
type boardInjectingPipeline struct {
	inner MemoryPipeline
	board func() string
}

// Assemble 委托内层流水线后追加看板段（在【近期事件】之后，属不可缓存尾部，不破坏前缀缓存）。
func (b *boardInjectingPipeline) Assemble(role types.RoleDefinition, agentID string, history []ReactMessage) []ReactMessage {
	out := b.inner.Assemble(role, agentID, history)
	if b.board != nil {
		if txt := strings.TrimSpace(b.board()); txt != "" {
			out = append(out, ReactMessage{Role: "system", Content: txt})
		}
	}
	return out
}

// Write 委托内层流水线。
func (b *boardInjectingPipeline) Write(agentID string, ev MemoryEvent) error {
	return b.inner.Write(agentID, ev)
}

// wrapMetaMemory 为 MetaAgent 包装记忆流水线：boardFn 已接线且看板存在时注入看板段。
// 返回包装后的流水线；未接线/无看板返回原流水线（零行为变化）。
// 每轮经闭包重查看板（write_plan 在 meta 首轮之后才创建看板，构造期查询会漏）。
func wrapMetaMemory(inner MemoryPipeline, boardFn func(sessionID string) *board.TaskBoard, sessionID string) MemoryPipeline {
	if boardFn == nil {
		return inner
	}
	return &boardInjectingPipeline{
		inner: inner,
		board: func() string {
			b := boardFn(sessionID)
			if b == nil {
				return ""
			}
			return b.Brief(6)
		},
	}
}
