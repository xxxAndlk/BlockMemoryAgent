package graph

// 本文件承载三层图的动态节点解析：resolveInstanceNode + 导出版本。
// 从 three_layer_graph.go 拆出（P0-3）。

import (
	"github.com/blockmemory/agent/backend/pkg/enums"
)

// resolveInstanceNode 根据实例ID解析节点（动态创建）。
// 静态节点表查不到时调用：从 registry 取实例元信息，按类型 new 一个对应节点，
// 注入依赖后缓存回 nodes 表（避免下次再 new）。
// 参数：
//   - instID：角色实例 ID（形如 domain_xxx_1 / assistant_2）。
//
// 返回：构造好的节点；实例不存在则返回 nil。
// 副作用：成功时会把新节点写入 g.nodes，后续命中走快路径。
// 并发安全：读实例无锁（registry 内部自锁），写 nodes 持写锁。
func (g *ThreeLayerGraph) resolveInstanceNode(instID string) ThreeLayerNode {
	inst := g.registry.GetInstance(instID)
	if inst == nil {
		return nil // 实例已被清理或不存在
	}

	var node ThreeLayerNode

	// 按实例类型构造对应节点
	switch inst.Type {
	case enums.RoleTypeDomain:
		node = NewDomainAgentNode(instID, g.registry, g.factory)
	case enums.RoleTypeSubDomain:
		node = NewSubDomainAgentNode(instID, g.registry, g.factory)
	case enums.RoleTypeFixed, enums.RoleTypeDynamic:
		node = NewAssistantNode(instID, g.registry, nil)
	default:
		return nil // 未知类型，无法构造
	}

	// 注入 ModelFactory（内含模型/工具/Runtime 依赖）
	g.injectModelFactory(node)
	// 注入 Progress / ToolCallback
	g.injectProgress(node)
	if g.toolCallback != nil {
		switch n := node.(type) {
		case *DomainAgentNode:
			n.SetToolCallback(g.toolCallback)
		case *SubDomainAgentNode:
			n.SetToolCallback(g.toolCallback)
		case *AssistantNode:
			n.SetToolCallback(g.toolCallback) // 修复 SubDomain→Assistant 路径无工具的问题
		}
	}
	// 注入块记忆存储（特性3）
	g.mu.RLock()
	bm := g.blockMemory
	g.mu.RUnlock()
	if bm != nil {
		if d, ok := node.(*DomainAgentNode); ok {
			d.SetBlockMemoryStore(bm)
		}
	}
	// 注入记忆回调处理器
	g.mu.RLock()
	memCb := g.memCallback
	g.mu.RUnlock()
	if memCb != nil {
		if d, ok := node.(*DomainAgentNode); ok {
			d.SetMemoryCallbackHandler(memCb)
		}
		if sd, ok := node.(*SubDomainAgentNode); ok {
			sd.SetMemoryCallbackHandler(memCb)
		}
	}
	// 注入上下文组装器
	g.mu.RLock()
	asm := g.assembler
	g.mu.RUnlock()
	if asm != nil {
		if aNode, ok := node.(*AssistantNode); ok {
			aNode.SetContextAssembler(asm)
		}
	}
	// 注入快照管理器
	g.mu.RLock()
	sm := g.snapshotMgr
	g.mu.RUnlock()
	if sm != nil {
		if d, ok := node.(*DomainAgentNode); ok {
			d.SetAgentSnapshotManager(sm)
		}
		if sd, ok := node.(*SubDomainAgentNode); ok {
			sd.SetAgentSnapshotManager(sm)
		}
	}
	// 注入结构化日志器
	g.injectLogger(node)

	// 缓存到静态表：下次同名 ID 直接命中，避免重复构造
	g.mu.Lock()
	g.nodes[instID] = node
	g.mu.Unlock()
	return node
}

// ResolveInstanceNode 根据实例ID动态解析节点。
// 导出版本，供 server / 测试代码显式触发动态节点构造。
func (g *ThreeLayerGraph) ResolveInstanceNode(instID string) ThreeLayerNode {
	return g.resolveInstanceNode(instID)
}
