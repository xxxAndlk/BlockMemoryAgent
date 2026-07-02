package graph

// 本文件承载三层图的路由表：determineNext 入口分发 + 各节点的 *Next 路由策略。
// 从 three_layer_graph.go 拆出（P0-3）。

import (
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// determineNext 三层调度逻辑。
// 入口分发：按 current 的名字路由到对应的 *Next 子函数。
// 当 current 是动态实例 ID 时，通过 registry 查类型再分发。
// 返回空串表示无下一跳（Invoke 会终止循环）。
func (g *ThreeLayerGraph) determineNext(current string, state *types.ThreeLayerState) string {
	switch current {
	case "MetaAgent":
		return g.metaAgentNext(state)
	case "DomainAgent":
		return g.domainAgentNext(state)
	case "Assistant":
		return g.assistantNext(state)
	default:
		// 动态实例 ID：查类型再分发
		if inst := g.registry.GetInstance(current); inst != nil {
			switch inst.Type {
			case enums.RoleTypeDomain:
				return g.domainAgentNext(state)
			case enums.RoleTypeSubDomain:
				return g.subDomainAgentNext(state)
			case enums.RoleTypeFixed, enums.RoleTypeDynamic:
				return g.assistantNext(state)
			}
		}
	}
	return ""
}

// metaAgentNext MetaAgent 的路由策略。
//   - Switch：切到目标角色；无目标则回退到第一个活跃 block 的第一个 Agent。
//   - Escalate：交给 EscalationHandler 仲裁。
//   - Finish：交给 Sinker 收尾（强制结束）。
//   - Continue：继续在 MetaAgent 循环。
func (g *ThreeLayerGraph) metaAgentNext(state *types.ThreeLayerState) string {
	switch state.NextAction {
	case enums.ActionSwitch:
		// 显式目标优先
		if state.TargetRoleID != "" {
			return state.TargetRoleID
		}
		// 无显式目标：从活跃 block 中挑第一个 Agent
		for blockID := range state.ActiveBlocks {
			block := state.ActiveBlocks[blockID]
			if len(block.Agents) > 0 {
				return block.Agents[0]
			}
		}
	case enums.ActionEscalate:
		return "EscalationHandler" // 上抛到升级处理节点
	case enums.ActionFinish:
		return "Sinker" // 走收尾节点
	case enums.ActionContinue:
		return "MetaAgent" // 自循环
	}
	return "MetaAgent" // 默认回到 MetaAgent
}

// domainAgentNext DomainAgent 的路由策略。
//   - Switch：切到显式目标角色。
//   - Continue：无调用栈则回 MetaAgent；否则继续当前 Assistant。
//
// 其余情况默认回 MetaAgent 汇报。
func (g *ThreeLayerGraph) domainAgentNext(state *types.ThreeLayerState) string {
	switch state.NextAction {
	case enums.ActionSwitch:
		if state.TargetRoleID != "" {
			return state.TargetRoleID
		}
	case enums.ActionContinue:
		// 没有挂起的子调用 → 回 MetaAgent 汇报
		if !state.IsCalling() {
			return "MetaAgent"
		}
		// 有挂起的子调用 → 继续执行当前 Assistant
		if state.CurrentAssistantID != "" {
			return state.CurrentAssistantID
		}
	}
	return "MetaAgent"
}

// subDomainAgentNext SubDomainAgent 的路由策略，与 domainAgentNext 同构。
// SubDomainAgent 完成后同样回 DomainAgent（经由 MetaAgent 调度）。
func (g *ThreeLayerGraph) subDomainAgentNext(state *types.ThreeLayerState) string {
	switch state.NextAction {
	case enums.ActionSwitch:
		if state.TargetRoleID != "" {
			return state.TargetRoleID
		}
	case enums.ActionContinue:
		if !state.IsCalling() {
			return "MetaAgent" // 子任务做完，回上层
		}
		if state.CurrentAssistantID != "" {
			return state.CurrentAssistantID // 继续当前 Assistant
		}
	}
	return "MetaAgent"
}

// assistantNext Assistant 的路由策略。
// Assistant 完成后无脑回 MetaAgent（由 MetaAgent 决定是否继续 DomainAgent 流程）。
func (g *ThreeLayerGraph) assistantNext(state *types.ThreeLayerState) string {
	switch state.NextAction {
	case enums.ActionSwitch:
		if state.TargetRoleID != "" {
			return state.TargetRoleID
		}
	case enums.ActionContinue:
		return "MetaAgent"
	}
	return "MetaAgent"
}

// DetermineNext 确定下一个节点。
// 导出版本，供外部调试 / 单测验证路由表。
func (g *ThreeLayerGraph) DetermineNext(current string, state *types.ThreeLayerState) string {
	return g.determineNext(current, state)
}
