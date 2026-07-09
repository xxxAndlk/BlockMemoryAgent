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

	// 注入 ModelFactory / Runtime / ToolCallback / Progress / Logger
	injectToNode[ModelFactoryReceiver](node, func(n ModelFactoryReceiver) {
		n.SetModelFactory(g.modelFactory)
	})
	injectToNode[RuntimeReceiver](node, func(n RuntimeReceiver) {
		n.SetRuntime(g.rt)
	})
	if g.toolCallback != nil {
		injectToNode[ToolCallbackReceiver](node, func(n ToolCallbackReceiver) {
			n.SetToolCallback(g.toolCallback)
		})
	}
	if g.progress != nil {
		injectToNode[ProgressReceiver](node, func(n ProgressReceiver) {
			n.SetProgressCallback(g.progress)
		})
	}
	if g.logger != nil {
		injectToNode[LoggerReceiver](node, func(n LoggerReceiver) {
			n.SetLogger(g.logger)
		})
	}

	// 注入块记忆存储（特性3）
	g.mu.RLock()
	bm := g.blockMemory
	g.mu.RUnlock()
	if bm != nil {
		injectToNode[BlockMemoryReceiver](node, func(n BlockMemoryReceiver) {
			n.SetBlockMemoryStore(bm)
		})
	}
	// 注入记忆回调处理器
	g.mu.RLock()
	memCb := g.memCallback
	g.mu.RUnlock()
	if memCb != nil {
		injectToNode[MemoryCallbackReceiver](node, func(n MemoryCallbackReceiver) {
			n.SetMemoryCallbackHandler(memCb)
		})
	}
	// 注入上下文组装器
	g.mu.RLock()
	asm := g.assembler
	g.mu.RUnlock()
	if asm != nil {
		injectToNode[ContextAssemblerReceiver](node, func(n ContextAssemblerReceiver) {
			n.SetContextAssembler(asm)
		})
	}
	// 注入快照管理器
	g.mu.RLock()
	sm := g.snapshotMgr
	g.mu.RUnlock()
	if sm != nil {
		injectToNode[SnapshotManagerReceiver](node, func(n SnapshotManagerReceiver) {
			n.SetAgentSnapshotManager(sm)
		})
	}

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
