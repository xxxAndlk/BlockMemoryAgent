package main

// cmd/tui 是 bubbletea 终端 UI 入口：在进程内直接持有 Runtime / SessionManager / Graph，
// 同时启动一个本地 HTTP 端口供 TUI 输入栏调用 /api/sessions/* /api/dag/*。
// 基础设施（Postgres/Redis/API key）缺失时降级运行，保证无外部依赖也能启动。

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/blockmemory/agent/backend/internal/config"
	"github.com/blockmemory/agent/backend/internal/dag"
	"github.com/blockmemory/agent/backend/internal/embed"
	"github.com/blockmemory/agent/backend/internal/graph"
	"github.com/blockmemory/agent/backend/internal/logging"
	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/runtime"
	"github.com/blockmemory/agent/backend/internal/server"
	"github.com/blockmemory/agent/backend/internal/skill"
	"github.com/blockmemory/agent/backend/internal/store"
	"github.com/blockmemory/agent/backend/internal/tui"
	pkgconfig "github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/types"
)

func main() {
	configPath := flag.String("config", "config/config.yaml", "基础设施配置路径")
	rolePath := flag.String("roles", "config/roles.yaml", "角色配置路径")
	envPath := flag.String("env", ".env", "环境变量文件路径")
	soulPath := flag.String("soul", "config/soul.md", "人格定义文件路径")
	skillPath := flag.String("skills", "config/skills.yaml", "Skill 池 YAML 路径（可选）")
	flag.Parse()

	if _, err := os.Stat(*envPath); err == nil {
		if err := config.LoadEnvFile(*envPath); err != nil {
			log.Printf("warning: load .env: %v", err)
		}
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Printf("warning: load config: %v; using defaults", err)
		cfg = &config.Config{}
	}

	// 日志文件输出（TUI 入口）：按天分割到 logs/tui-YYYY-MM-DD.log
	// 与浏览器入口分文件，便于按入口排查问题
	if cfg.Logging.Enabled {
		if _, err := logging.Init(logging.EntryTUI, cfg.Logging.Dir); err != nil {
			log.Printf("warning: init file logging: %v (stderr-only)", err)
		} else {
			defer logging.Close()
		}
	}
	log.Printf("BlockMemoryAgent TUI entry starting, log dir=%s", cfg.Logging.Dir)

	ctx := context.Background()

	var pgStore *store.PostgresStore
	if cfg.Postgres.DSN != "" {
		var err error
		pgStore, err = store.NewPostgresStore(cfg.Postgres.DSN)
		if err != nil {
			log.Printf("warning: postgres unavailable: %v", err)
		} else {
			defer pgStore.Close()
			if err := store.EnsureSessionHistorySchema(ctx, pgStore.DB()); err != nil {
				log.Printf("warning: session_history schema: %v", err)
			}
			if err := store.EnsureDAGSchema(ctx, pgStore.DB()); err != nil {
				log.Printf("warning: dag_jobs schema: %v", err)
			}
		}
	}

	roleCfg, err := pkgconfig.LoadRoleConfig(*rolePath)
	if err != nil {
		log.Printf("warning: load role config: %v; using defaults", err)
		roleCfg = defaultRoleConfig()
	}

	modelFactory := model.NewModelFactory(roleCfg)
	if err := modelFactory.WarmUp(ctx); err != nil {
		log.Printf("warning: model warmup: %v; mock fallback may be used", err)
	}

	registry := graph.NewRoleRegistry(roleCfg)
	factory := graph.NewRoleFactory(registry, modelFactory, roleCfg)

	var skillPool *skill.Pool
	if _, err := os.Stat(*skillPath); err == nil {
		pool, err := skill.LoadFromYAML(*skillPath)
		if err != nil {
			log.Printf("warning: load skills: %v; using builtin", err)
			skillPool = skill.BuiltinPool()
		} else {
			skillPool = pool
		}
	} else {
		skillPool = skill.BuiltinPool()
	}

	rt := runtime.New(*soulPath, skillPool)
	if cfg.Agent.MaxSteps > 0 {
		rt.SetAgentConfig(&cfg.Agent)
	}

	metaAgent := graph.NewMetaAgentNode(registry, factory, roleCfg.MetaAgent.MaxBlocks, roleCfg.MetaAgent.SummaryInterval)
	metaAgent.SetModelFactory(modelFactory)
	metaAgent.SetRuntime(rt)
	if pgStore != nil {
		metaAgent.SetHistoryStore(&pgHistoryAdapter{pg: pgStore})
	}
	escalation := graph.NewEscalationHandlerNode()
	sinker := &sinkerNode{}

	builder := graph.NewThreeLayerGraphBuilder(registry, factory)
	builder.SetModelFactory(modelFactory)
	builder.SetRuntime(rt)
	builder.AddNode(metaAgent)
	builder.AddNode(escalation)
	builder.AddNode(sinker)
	threeLayerGraph := builder.Build()

	sessionMgr := server.NewSessionManager(threeLayerGraph, registry)
	sessionMgr.SetPostgresStore(pgStore)

	if pgStore != nil {
		threeLayerGraph.SetBlockMemoryStore(&pgBlockMemoryAdapter{pg: pgStore, dim: cfg.PgVector.Dimensions})
		threeLayerGraph.SetArchiveStore(pgStore)
	}

	dagScheduler := dag.NewScheduler(pgStore, sessionMgr, 10*time.Second)
	dagScheduler.Start(ctx)

	// Start a local HTTP server so the TUI input bar can POST to /api/sessions/* and /api/dag/*.
	// 这里不直接复用 server.api.go 是因为 TUI 进程内已持有 SessionManager/Graph 实例，
	// 直接走本地 HTTP 比进程内调用更解耦：输入栏只关心 HTTP，便于后续替换为远程后端。
	mux := http.NewServeMux()
	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			sessionMgr.HandleListSessions(w, r)
		case http.MethodPost:
			sessionMgr.HandleCreateSession(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/sessions/", sessionRouter(sessionMgr))

	dagHandler := server.NewDAGHandler(pgStore, dagScheduler)
	mux.Handle("/api/dag", dagHandler)
	mux.Handle("/api/dag/", dagHandler)

	// 监听 127.0.0.1:0 让内核分配空闲端口，避免与其他进程冲突；端口通过 ln.Addr() 回传给 TUI
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	go func() {
		// 关闭连接时的 "use of closed network connection" 是正常退出，不当作错误
		if err := http.Serve(ln, mux); err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
			log.Printf("http server: %v", err)
		}
	}()
	httpAddr := "http://" + ln.Addr().String()
	log.Printf("TUI backend listening at %s", httpAddr)

	modelName := roleCfg.MetaAgent.ModelConfig.Model
	model := tui.NewModel(sessionMgr, registry, rt, dagHandler, pgStore, httpAddr, modelName)
	p := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseAllMotion())
	if _, err := p.Run(); err != nil {
		log.Fatalf("TUI error: %v", err)
	}
}

// sessionRouter 把 /api/sessions/{id}/{suffix} 路径分发到对应 handler。
// 路径解析：剥离前缀 → 按 / 切两段 → 第一段为 sessionID，第二段为操作后缀。
// 未识别后缀走 HandleGetSession（取单会话详情），保持 RESTful 路径风格。
func sessionRouter(mgr *server.SessionManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
		parts := strings.SplitN(path, "/", 2)
		id := parts[0]
		suffix := ""
		if len(parts) == 2 {
			suffix = parts[1]
		}
		if id == "" {
			http.Error(w, "session id required", http.StatusBadRequest)
			return
		}
		switch suffix {
		case "stream":
			mgr.HandleSessionStream(w, r) // SSE 流：实时推送 graph 事件到 TUI
		case "message":
			mgr.HandleSessionMessage(w, r)
		case "clarify":
			mgr.HandleSessionClarify(w, r) // 特性5：人机对话答复
		case "interrupt":
			mgr.HandleSessionInterrupt(w, r) // 特性6：抢占中断
		case "enqueue":
			mgr.HandleSessionEnqueue(w, r) // 特性6：队列注入
		case "board":
			mgr.HandleSessionBoard(w, r)
		case "agents":
			mgr.HandleSessionAgents(w, r)
		case "metrics":
			mgr.HandleSessionMetrics(w, r)
		case "watchdog":
			mgr.HandleSessionWatchdog(w, r)
		default:
			mgr.HandleGetSession(w, r) // 无后缀：单会话详情
		}
	}
}

type sinkerNode struct{}

func (s *sinkerNode) Name() string { return "Sinker" }

func (s *sinkerNode) Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	state.NextAction = types.ActionFinish
	return state, nil
}

type pgBlockMemoryAdapter struct {
	pg  *store.PostgresStore
	dim int
}

func (a *pgBlockMemoryAdapter) SaveBlockMemory(ctx context.Context, sessionID, domain, goal, summary string) error {
	content := fmt.Sprintf("领域:%s\n目标:%s\n摘要:%s", domain, goal, summary)
	emb := embed.PseudoEmbed(content, a.dim)
	return a.pg.SaveKnowledge(ctx, &types.KnowledgeRecord{
		KnowledgeType: "block_memory",
		TopicID:       sessionID,
		Content:       content,
		Embedding:     emb,
		CreatedAt:     time.Now(),
	})
}

func (a *pgBlockMemoryAdapter) SearchBlockMemory(ctx context.Context, query string, topK int) (string, error) {
	emb := embed.PseudoEmbed(query, a.dim)
	recs, err := a.pg.SearchKnowledgeByType(ctx, "block_memory", emb, topK)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, rec := range recs {
		sb.WriteString(rec.Content)
		sb.WriteString("\n---\n")
	}
	return sb.String(), nil
}

type pgHistoryAdapter struct {
	pg *store.PostgresStore
}

func (a *pgHistoryAdapter) RecentSessionHistories(ctx context.Context, limit int) ([]graph.HistoryEntry, error) {
	recs, err := a.pg.RecentSessionHistories(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]graph.HistoryEntry, 0, len(recs))
	for _, rec := range recs {
		out = append(out, graph.HistoryEntry{
			SessionID:   rec.SessionID,
			Goal:        rec.Goal,
			Summary:     rec.Summary,
			ToolResults: rec.ToolResults,
			CreatedAt:   rec.CreatedAt,
		})
	}
	return out, nil
}

func defaultRoleConfig() *pkgconfig.RoleConfigFile {
	return &pkgconfig.RoleConfigFile{
		MetaAgent:   pkgconfig.MetaAgentConfig{MaxBlocks: 5, SummaryInterval: 3},
		DomainAgent: pkgconfig.DomainAgentConfig{},
		FixedRoles: []types.RoleDefinition{
			{ID: "code_assistant", Name: "代码助手", Type: types.RoleTypeFixed, Lifecycle: types.RoleLifecyclePermanent, Description: "代码编写与审查", Skills: []string{"代码编写", "代码审查"}, Keywords: []string{"代码", "bug"}, CanBeCalled: true},
			{ID: "ui_assistant", Name: "UI助手", Type: types.RoleTypeFixed, Lifecycle: types.RoleLifecyclePermanent, Description: "前端UI实现", Skills: []string{"UI修复", "组件开发"}, Keywords: []string{"UI", "样式"}, CanBeCalled: true},
		},
	}
}
