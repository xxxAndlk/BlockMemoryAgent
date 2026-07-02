package graph

import (
	"github.com/blockmemory/agent/backend/internal/mailbox"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)


// processMailbox 把广播邮件按目标 domain 转给具体 DomainAgent 实例。
//
// 职责：
//   - 从邮箱拉取所有广播桶消息
//   - 按 payload.target_domain 在活跃块中匹配领域
//   - 调 Mailbox.Forward 转交目标实例
//   - 升级类消息（MsgEscalate）同时注入块事件
//
// 参数：
//   - state：图全局状态（原地修改块的 Events）
//
// 副作用：可能向 block.Events 追加 EventEscalation。
func (n *MetaAgentNode) processMailbox(state *types.ThreeLayerState) {
	// Runtime 或邮箱缺失则跳过
	if n.rt == nil || n.rt.Mailbox == nil {
		return
	}
	// 拉取并清空广播桶
	bcasts := n.rt.Mailbox.DrainBroadcast()
	if len(bcasts) == 0 {
		return
	}
	for _, msg := range bcasts {
		// 取目标领域提示（可选）
		var domainHint string
		if v, ok := msg.Payload["target_domain"].(string); ok {
			domainHint = v
		}
		// 在活跃块里寻找匹配领域
		for _, b := range state.ActiveBlocks {
			// 无 hint 或领域匹配
			if domainHint == "" || b.Domain == domainHint {
				// 块内有 Agent 则转发
				if len(b.Agents) > 0 {
					_ = msg.From // 显式忽略 From（保留语义占位）
					n.rt.Mailbox.Forward(msg.ID, b.Agents[0])
					// 升级类消息：注入块事件，由 handleBlockEvents 处理
					if msg.Type == mailbox.MsgEscalate {
						b.Events = append(b.Events, &types.Event{
							ID:        msg.ID,                  // 事件ID
							Type:      enums.EventEscalation,   // 升级事件
							Payload:   map[string]any{"reason": msg.Subject}, // 载荷含原因
							Priority:  msg.Priority,            // 优先级
							CreatedAt: msg.CreatedAt,           // 创建时间
							Status:    enums.EventPending,      // 待处理
						})
					}
					break // 一个目标只转发一次
				}
			}
		}
	}
}