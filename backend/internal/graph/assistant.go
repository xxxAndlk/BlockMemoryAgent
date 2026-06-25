package graph

import (
	"context"
	"fmt"
	"time"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// AssistantNode Layer 3: 助手角色 / 具体任务执行者。
//
// 职责：
//   - 接收上层（Domain/SubDomain）派发的单一任务并执行
//   - 把执行结果回写到对应 SessionBlock 的 TaskResults
//   - 通过 PopCallStack 返回控制权给调用者
//
// 并发安全：节点本身无共享可变状态；实例状态由 registry 内部锁保护。
type AssistantNode struct {
	name      string          // 节点名（固定 "Assistant"），实现 ThreeLayerNode.Name
	instID    string          // 本实例ID，对应 registry 中的 RoleInstance.ID
	registry  *RoleRegistry   // 角色注册表，查询实例/角色定义、更新状态
	workspace WorkspaceWriter // 工作区写入器，持久化 AgentOutput（可能为 nil）
}

// NewAssistantNode 创建助手节点。
//
// 参数：
//   - instID：实例ID（由上层 factory.CreateInstance 生成）
//   - registry：角色注册表
//   - workspace：工作区写入器（可为 nil，仅退化跳过持久化）
//
// 返回：装配好的 *AssistantNode，待图调度循环拉起。
func NewAssistantNode(instID string, registry *RoleRegistry, workspace WorkspaceWriter) *AssistantNode {
	return &AssistantNode{
		name:      "Assistant", // 节点名固定，便于路由表查找
		instID:    instID,      // 绑定本节点处理的具体实例
		registry:  registry,    // 注入注册表以便 GetInstance/GetRoleDef
		workspace: workspace,   // 注入工作区写入器
	}
}

// Name 返回节点名称。
// 实现 ThreeLayerNode 接口；图调度循环按此名做路由。
func (n *AssistantNode) Name() string {
	return n.name
}

// InstanceID 返回实例ID。
// 用于图调度循环在路由表里查找对应的动态节点。
func (n *AssistantNode) InstanceID() string {
	return n.instID
}

// Invoke 执行助手逻辑。
//
// 职责：
//   - 取出本实例对应的角色定义
//   - 从调用栈顶部读取调用请求（含任务与上下文）
//   - 调用 executeTask 执行任务（当前为模拟实现）
//   - 将结果回写到对应 SessionBlock 的 TaskResults
//   - 持久化 AgentOutput 到工作区
//   - 弹出调用栈，把控制权切回调用者（或返回 MetaAgent）
//
// 参数：
//   - ctx：请求上下文
//   - state：图的全局状态（会被原地修改）
//
// 返回：更新后的 state；若实例/角色定义缺失则返回 error。
//
// 副作用：更新 registry 中本实例与调用者的状态；修改 state.CallStack 与 ActiveBlocks。
//
// 并发安全：单次 Invoke 由图调度循环串行调用，无需加锁。
func (n *AssistantNode) Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	// 1. 取出本实例信息；实例不存在说明注册表已被清理或路由错误
	inst := n.registry.GetInstance(n.instID)
	if inst == nil {
		// 返回错误，图调度循环会终止本次会话
		return nil, fmt.Errorf("assistant instance %s not found", n.instID)
	}

	// 2. 取出角色定义；角色定义缺失说明配置不一致
	roleDef := n.registry.GetRoleDef(inst.RoleDefID)
	if roleDef == nil {
		// 返回错误，避免后续用空角色定义触发 panic
		return nil, fmt.Errorf("role def %s not found", inst.RoleDefID)
	}

	// 3. 标记实例为活跃，便于 UI/看板观察当前执行体
	n.registry.UpdateInstanceStatus(n.instID, types.RoleStatusActive)

	// 4. 从调用栈顶部读取调用请求（Domain/SubDomain 通过 PushCallStack 派发任务）
	var callReq *types.CallRequest
	if len(state.CallStack) > 0 {
		// 取栈顶元素（ peek，不弹出；弹出在步骤 10 完成）
		callReq = state.CallStack[len(state.CallStack)-1]
	}

	// 5. 确定任务文本：优先用调用请求里的 Task，否则退化到角色描述
	task := roleDef.Description
	if callReq != nil {
		// 调用请求中的 Task 是上层派发时传入的具体任务
		task = callReq.Task
	}

	// 6. 执行任务（当前为模拟实现，真实场景应调用 ChatModel + 工具循环）
	result := n.executeTask(ctx, roleDef, task, callReq)

	// 7. 把结果回写到调用请求所属的 SessionBlock
	if callReq != nil {
		// 从上下文取出 block_id，定位目标会话块
		if blockID, ok := callReq.Context["block_id"].(string); ok {
			if block := state.ActiveBlocks[blockID]; block != nil {
				// 懒初始化 TaskResults map
				if block.TaskResults == nil {
					block.TaskResults = make(map[string]string)
				}
				// 以任务文本为 key 存结果，便于上层去重/汇总
				block.TaskResults[callReq.Task] = result
			}
		}
	}

	// 8. 构造 AgentOutput 并持久化到工作区
	output := &types.AgentOutput{
		AgentID:   n.instID,   // 产出者实例ID
		Version:   1,          // 版本号，预留多版本输出
		Summary:   result,     // 执行结果摘要
		Timestamp: time.Now(), // 产出时间戳
		Validated: true,       // 助手输出直接标记为有效（无独立校验环节）
	}

	// 工作区写入器可能为 nil（测试桩），忽略错误以保证主流程不中断
	if n.workspace != nil {
		_ = n.workspace.SaveAgentOutput(ctx, state.SessionID, output)
	}

	// 9. 标记实例为完成
	n.registry.UpdateInstanceStatus(n.instID, types.RoleStatusDone)

	// 10. 弹出调用栈，释放本层占用的"调用槽"
	state.PopCallStack()

	// 11. 决定下一步路由：有调用者则切回调用者，否则继续图循环（由 MetaAgent 决策）
	if callReq != nil && callReq.CallerID != "" {
		// 恢复调用者为活跃状态，准备接收返回结果
		n.registry.UpdateInstanceStatus(callReq.CallerID, types.RoleStatusActive)
		state.NextAction = types.ActionSwitch // 切换到调用者节点
		state.TargetRoleID = callReq.CallerID // 路由目标
	} else {
		// 没有调用者（顶层派发），交给 MetaAgent 继续调度
		state.NextAction = types.ActionContinue
	}

	return state, nil
}

// executeTask 执行具体任务。
//
// 职责：返回任务的执行结果文本（当前为模拟实现）。
//
// 参数：
//   - ctx：请求上下文
//   - roleDef：角色定义（含名称/描述）
//   - task：任务文本
//   - callReq：调用请求（可能含领域等上下文）
//
// 返回：结果文本。
//
// 注意：TODO 标记表明真实场景应调用配置中的 ChatModel + 工具循环
// （见 DomainAgent.executeAssistantTask）。
func (n *AssistantNode) executeTask(ctx context.Context, roleDef *types.RoleDefinition, task string, callReq *types.CallRequest) string {
	// TODO: 实际应调用配置中的 ChatModel
	// 这里返回模拟结果，仅用于无 LLM key 时的占位

	// 从调用请求中提取领域信息，拼到结果里便于调试
	contextInfo := ""
	if callReq != nil && callReq.Context != nil {
		// 尝试读取 Context 里的 "domain" 字段
		if domain, ok := callReq.Context["domain"]; ok {
			// 拼成 "[领域: xxx] " 前缀
			contextInfo = fmt.Sprintf("[领域: %v] ", domain)
		}
	}

	// 返回模拟结果，包含领域、角色名、任务、实例ID
	return fmt.Sprintf("%s助手[%s]完成任务: %s\n结果: 已处理%s",
		contextInfo, roleDef.Name, task, n.instID)
}

// CanHandle 检查是否能处理某类任务。
//
// 职责：判断本助手角色是否声明了能处理 taskType 技能。
//
// 参数：
//   - taskType：任务类型字符串（对应角色 Skills 列表中的某项）
//
// 返回：能处理返回 true；实例/角色定义缺失或技能不匹配返回 false。
//
// 用途：上层派发助手时可用此方法做粗粒度匹配
// （实际匹配逻辑在 DomainAgent.matchFixedAssistant）。
func (n *AssistantNode) CanHandle(taskType string) bool {
	// 取出本实例
	inst := n.registry.GetInstance(n.instID)
	if inst == nil {
		// 实例已被清理，无法判定
		return false
	}

	// 取出角色定义
	roleDef := n.registry.GetRoleDef(inst.RoleDefID)
	if roleDef == nil {
		// 角色定义缺失，无法判定
		return false
	}

	// 遍历角色声明的技能，命中即返回 true
	for _, skill := range roleDef.Skills {
		if skill == taskType {
			return true
		}
	}
	// 全部不匹配
	return false
}
