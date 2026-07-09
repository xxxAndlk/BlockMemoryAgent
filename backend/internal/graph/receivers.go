package graph

import (
	"github.com/blockmemory/agent/backend/internal/logger"
	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/runtime"
)

// Receiver interfaces 统一图节点依赖注入方式。
// ThreeLayerGraph 不再对每个依赖写类型分支，而是按“能力接口”批量注入，
// 新增节点类型只要实现对应接口即可自动获得依赖，减少 three_layer_graph.go 的修改点。

type ModelFactoryReceiver interface {
	SetModelFactory(mf *model.ModelFactory)
}

type ProgressReceiver interface {
	SetProgressCallback(cb ProgressCallback)
}

type LoggerReceiver interface {
	SetLogger(l *logger.Logger)
}

type RuntimeReceiver interface {
	SetRuntime(rt *runtime.Runtime)
}

type ToolCallbackReceiver interface {
	SetToolCallback(cb ToolCallback)
}

type BlockMemoryReceiver interface {
	SetBlockMemoryStore(s BlockMemoryStore)
}

type MemoryCallbackReceiver interface {
	SetMemoryCallbackHandler(h MemoryCallbackHandler)
}

type SnapshotManagerReceiver interface {
	SetAgentSnapshotManager(s AgentSnapshotManager)
}

type ContextAssemblerReceiver interface {
	SetContextAssembler(a ContextAssembler)
}

type EpisodeCompressorReceiver interface {
	SetEpisodeCompressor(c EpisodeCompressor)
}

// injectTo 把 setter 应用到所有实现 R 的节点。
// 调用方在 setter 内自行判断依赖是否为 nil，保持简洁。
func injectTo[R any](nodes map[string]ThreeLayerNode, setter func(R)) {
	for _, node := range nodes {
		if r, ok := node.(R); ok {
			setter(r)
		}
	}
}

// injectToNode 把 setter 应用到单个节点（若其实现 R）。
// 用于动态节点 resolveInstanceNode 的按需注入。
func injectToNode[R any](node ThreeLayerNode, setter func(R)) {
	if r, ok := node.(R); ok {
		setter(r)
	}
}
