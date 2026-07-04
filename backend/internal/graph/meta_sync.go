package graph

import (
	"fmt"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/mailbox"
	"github.com/blockmemory/agent/backend/internal/runtime"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// mergeSessionMetaMemory 把 state.MetaMemory 中尚未出现在 block.MetaMemory 的条目合并进去。
// 用于 R8 修复：新创建的会话块自动继承当前会话级调度记忆，避免跨域失忆。
func mergeSessionMetaMemory(state *types.ThreeLayerState, block *types.SessionBlock) {
	if state == nil || block == nil || len(state.MetaMemory) == 0 {
		return
	}
	existing := make(map[string]bool, len(block.MetaMemory))
	for _, e := range block.MetaMemory {
		existing[e.Content] = true
	}
	for _, e := range state.MetaMemory {
		if existing[e.Content] {
			continue
		}
		block.MetaMemory = append(block.MetaMemory, e)
		existing[e.Content] = true
	}
}

// syncMetaMemoryToActiveBlocks 把当前 state.MetaMemory 同步到所有活跃块。
// 在 collectBlockResult 更新 state.MetaMemory 后调用，让其他活跃块也能看到最新决策。
func syncMetaMemoryToActiveBlocks(state *types.ThreeLayerState) {
	if state == nil || len(state.MetaMemory) == 0 {
		return
	}
	for _, block := range state.ActiveBlocks {
		mergeSessionMetaMemory(state, block)
	}
}

// notifyOtherActiveBlocks 当某个块完成时，通过 Mailbox 向其他活跃块发送信息同步。
// 用于 R7 修复：跨域修改/结果通过 Mailbox 自动通知相关 Agent，避免块间隔离导致的信息断层。
func notifyOtherActiveBlocks(state *types.ThreeLayerState, completed *types.SessionBlock, rt *runtime.Runtime) {
	if rt == nil || rt.Mailbox == nil || state == nil || completed == nil {
		return
	}
	if len(state.ActiveBlocks) == 0 {
		return
	}

	var bodyParts []string
	if completed.Result != nil {
		if completed.Result.SummaryForUser != "" {
			bodyParts = append(bodyParts, completed.Result.SummaryForUser)
		}
		if completed.Result.MemoryForMeta != "" {
			bodyParts = append(bodyParts, completed.Result.MemoryForMeta)
		}
		if len(completed.Result.Facts) > 0 {
			bodyParts = append(bodyParts, "facts: "+strings.Join(completed.Result.Facts, "; "))
		}
	}
	if len(bodyParts) == 0 {
		bodyParts = append(bodyParts, fmt.Sprintf("领域[%s]已完成目标: %s", completed.Domain, completed.Goal))
	}

	for _, b := range state.ActiveBlocks {
		// 不通知自己
		if b.ID == completed.ID || len(b.Agents) == 0 {
			continue
		}
		rt.Mailbox.Send(&mailbox.Message{
			From:     completed.Agents[0],
			To:       b.Agents[0],
			Type:     mailbox.MsgInfo,
			Subject:  fmt.Sprintf("[%s] 完成，可能影响 %s", completed.Domain, b.Domain),
			Body:     strings.Join(bodyParts, "\n"),
			Priority: 3,
		})
	}
}

// applyMailboxToBlock 把当前 Agent 邮箱中的未读消息抽取并注入 block.MetaMemory。
// DomainAgent / SubDomainAgent 在 Invoke 开始时调用，消费其他块发来的跨域通知。
func applyMailboxToBlock(state *types.ThreeLayerState, block *types.SessionBlock, rt *runtime.Runtime, agentID string) {
	if rt == nil || rt.Mailbox == nil || block == nil || agentID == "" {
		return
	}
	msgs := rt.Mailbox.Drain(agentID)
	if len(msgs) == 0 {
		return
	}
	for _, msg := range msgs {
		content := msg.Subject
		if msg.Body != "" {
			content += "\n" + msg.Body
		}
		block.MetaMemory = append(block.MetaMemory, types.MetaMemoryEntry{
			Timestamp: time.Now(),
			Source:    msg.From,
			Content:   content,
			Tags:      []string{"mailbox", string(msg.Type)},
		})
	}
}
