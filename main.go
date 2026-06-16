package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/blockmemory/agent/internal/config"
	"github.com/blockmemory/agent/internal/graph"
	"github.com/blockmemory/agent/internal/model"
	"github.com/blockmemory/agent/internal/store"
	pkgconfig "github.com/blockmemory/agent/pkg/config"
	"github.com/blockmemory/agent/pkg/types"
)

func main() {
	configPath := flag.String("config", "config/config.yaml", "基础设施配置路径")
	rolePath := flag.String("roles", "config/roles.yaml", "角色配置路径")
	flag.Parse()

	// 加载基础设施配置
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 初始化存储层
	pgStore, err := store.NewPostgresStore(cfg.Postgres.DSN)
	if err != nil {
		log.Printf("Warning: postgres not available: %v", err)
		pgStore = nil
	}
	if pgStore != nil {
		defer pgStore.Close()
	}

	redisStore, err := store.NewRedisStore(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB)
	if err != nil {
		log.Printf("Warning: redis not available: %v", err)
		redisStore = nil
	}
	if redisStore != nil {
		defer redisStore.Close()
	}

	// 加载角色配置
	roleCfg, err := pkgconfig.LoadRoleConfig(*rolePath)
	if err != nil {
		log.Fatalf("load role config: %v", err)
	}

	// 初始化模型工厂
	modelFactory := model.NewModelFactory(roleCfg)
	if err := modelFactory.WarmUp(ctx); err != nil {
		log.Printf("Warning: model warmup failed: %v", err)
	}

	// 初始化注册表和角色工厂
	registry := graph.NewRoleRegistry(roleCfg)
	factory := graph.NewRoleFactory(registry, modelFactory, roleCfg)

	// 构建三层图
	metaAgent := graph.NewMetaAgentNode(registry, factory, roleCfg.MetaAgent.MaxBlocks, roleCfg.MetaAgent.SummaryInterval)
	metaAgent.SetModelFactory(modelFactory)
	escalation := graph.NewEscalationHandlerNode()
	sinker := &sinkerNode{}
	builder := graph.NewThreeLayerGraphBuilder(registry, factory)
	builder.SetModelFactory(modelFactory)
	builder.AddNode(metaAgent)
	builder.AddNode(escalation)
	builder.AddNode(sinker)
	threeLayerGraph := builder.Build()

	// 启动示例会话
	go func() {
		sessionID := "session-001"
		state := types.NewThreeLayerState(sessionID)
		state.DomainGoal = "修复商城主页穿模问题"

		log.Printf("Starting session: %s, goal: %s", sessionID, state.DomainGoal)

		result, err := threeLayerGraph.Invoke(ctx, state)
		if err != nil {
			log.Printf("Session error: %v", err)
			return
		}

		log.Printf("Session completed: action=%s, summary=%s", result.NextAction, result.SessionSummary)

		for _, inst := range registry.GetInstancesBySession(sessionID) {
			roleDef := registry.GetRoleDef(inst.RoleDefID)
			name := "unknown"
			if roleDef != nil {
				name = roleDef.Name
			}
			log.Printf("  - [%s] %s (type: %s, domain: %s, status: %s)", inst.ID, name, inst.Type, inst.Domain, inst.Status)
		}
	}()

	// 等待中断信号
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("Shutting down...")
	cancel()
}

// sinkerNode 终止节点
type sinkerNode struct{}

func (n *sinkerNode) Name() string { return "Sinker" }
func (n *sinkerNode) Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	state.NextAction = types.ActionFinish
	return state, nil
}
