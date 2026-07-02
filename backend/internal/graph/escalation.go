package graph

// 升级处理节点：当 DomainAgent / SubDomainAgent 无法自行决策时，
// 把问题上抛到 EscalationHandlerNode，由它生成仲裁摘要并写回当前 SessionBlock，
// 随后把控制权交还 MetaAgent 继续循环。

import (
	"context"
	"fmt"
	"time"

	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// stepKeyType context key 类型（避免与其他包冲突）。
type stepKeyType struct{}

// stepKey 用于在 ctx 中存取当前升级步数等元信息。
var stepKey = stepKeyType{}

// EscalationHandlerNode 升级处理节点（3层架构兼容）。
// 实现 ThreeLayerNode 接口，被图调度循环在 NextAction==Escalate 时拉起。
type EscalationHandlerNode struct {
	name string // 节点名，固定 "EscalationHandler"
}

// NewEscalationHandlerNode 创建升级处理节点。
// 返回：节点实例，name 字段固定为 "EscalationHandler"（路由表用此名查找）。
func NewEscalationHandlerNode() *EscalationHandlerNode {
	return &EscalationHandlerNode{name: "EscalationHandler"}
}

// Name 返回节点名称。
// 实现 ThreeLayerNode 接口；路由表通过此名匹配。
func (n *EscalationHandlerNode) Name() string {
	return n.name
}

// Invoke 执行升级处理（ThreeLayerNode 接口）。
// 流程：
//  1. 打印升级日志（便于本地调试观察）。
//  2. 生成仲裁摘要字符串。
//  3. 把仲裁事件追加到当前 SessionBlock 的 Events 列表。
//  4. 清空 Reason，把 NextAction 置为 Continue，让图回到 MetaAgent 继续。
//
// 参数：
//   - ctx：上下文（当前未使用，预留扩展）。
//   - state：图状态，含 SessionID / Reason / CurrentBlockID 等。
//
// 返回：更新后的 state；不会返回 error。
// 副作用：向 state.ActiveBlocks[CurrentBlockID].Events 追加一条 EventEscalation。
func (n *EscalationHandlerNode) Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	// 调试日志：会话 + 升级原因
	fmt.Printf("[ESCALATION] Session: %s, Reason: %s\n", state.SessionID, state.Reason)

	// 生成仲裁摘要
	// 当前实现极简：直接拼接 Reason；后续可扩展为 LLM 仲裁
	arbitration := fmt.Sprintf("Escalation: %s", state.Reason)

	// 添加事件到当前会话块
	// SessionBlock 是 DomainAgent 的上下文隔离单元，升级事件写回这里便于回溯。
	// 注意：必须标记为 EventDone（H7 修复）——若标 Pending，下一 tick handleBlockEvents
	// 会再次触发 EventEscalation 分支 → 再次进入 EscalationHandler → 再追加 Pending 事件，
	// 形成无限升级循环。这里作为审计记录，直接置 Done。
	if block := state.ActiveBlocks[state.CurrentBlockID]; block != nil {
		block.Events = append(block.Events, &types.Event{
			ID:          fmt.Sprintf("esc_%d", time.Now().UnixNano()), // 纳秒时间戳保证唯一
			Type:        enums.EventEscalation,
			SourceAgent: state.CurrentDomain, // 记录升级来源
			Payload:     map[string]any{"arbitration": arbitration},
			Priority:    10,              // 升级事件高优先级
			Status:      enums.EventDone, // 审计记录直接置完成，避免触发循环
		})
	}

	// 清理升级状态，回到 Continue 让 MetaAgent 继续循环
	state.NextAction = enums.ActionContinue
	state.Reason = ""

	return state, nil
}
