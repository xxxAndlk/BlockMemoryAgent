package bootstrap

// flow.go 可视化编排（flow）引擎的装配（2026-10-18）。依赖注入：*tool.Registry、
// *model.ModelFactory、store（PG）、logger。bootstrap.go 只留接线点。

import (
	"time"

	"github.com/blockmemory/agent/backend/internal/config"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/flow"
	"github.com/blockmemory/agent/backend/internal/logger"
	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/store"
)

// buildFlowEngine 装配可视化编排引擎。
//   - 持久化 = PG（flow_runs / flow_node_runs，权威全在表，NewPGStore 幂等建表）；
//   - tool 节点 = toolRegistry.Dispatch（ctx 带 session/workdir）；
//   - llm 节点 = ModelFactory（model=角色 ID 走角色绑定；空=轻量模型）。
func buildFlowEngine(
	cfg *config.Config,
	pgStore *store.PostgresStore,
	toolRegistry *tool.Registry,
	modelFactory *model.ModelFactory,
	log *logger.Logger,
) *flow.Engine {
	flowStore, err := flow.NewPGStore(pgStore.DB())
	if err != nil {
		log.Error(nil, "[flow] 存储初始化失败，引擎不可用", err)
		return nil
	}
	eng := flow.NewEngine(flow.Config{
		MaxActiveRuns: cfg.Flow.MaxActiveRuns,
		NodeTimeout:   time.Duration(cfg.Flow.NodeTimeoutMS) * time.Millisecond,
	}, flowStore, toolRegistry, modelFactory)
	eng.SetLogger(log)
	return eng
}
