package server

import (
	"context"            // 超时上下文
	"encoding/json"      // JSON 编解码
	"fmt"                // 格式化字符串
	"math"               // 时间衰减用 math.Exp
	"net/http"           // HTTP 处理器
	"os"                 // 文件读取 / Stat
	"path/filepath"      // filepath.Base
	stdruntime "runtime" // 进程运行时指标
	"sort"               // 排序响应列表
	"strconv"            // Atoi 等
	"strings"            // 字符串处理
	"time"               // 超时与时间戳

	"github.com/blockmemory/agent/backend/internal/memory"           // BlockMemory 检索
	"github.com/blockmemory/agent/backend/internal/model"            // ModelFactory
	"github.com/blockmemory/agent/backend/internal/runtime"          // Runtime
	"github.com/blockmemory/agent/backend/internal/server/eventkind" // 事件类型常量
	"github.com/blockmemory/agent/backend/internal/store"            // Postgres / Redis
	pkgconfig "github.com/blockmemory/agent/backend/pkg/config"      // RoleConfigFile
	"github.com/blockmemory/agent/backend/pkg/types"                 // 共享类型
)

// APIHandler TUI / Web API 处理器
// 职责：聚合 broadcaster / snapshotMgr / 各存储与配置，统一暴露 HTTP 接口给前端。
//
// 字段说明：
//   - broadcaster: SSE 广播器
//   - snapshotMgr: 快照管理器（鸭子类型，避免循环依赖）
//   - paused: topic_id -> 是否暂停（用于 graphControl）
//   - rt: 聚合运行时（boards / mailbox / skills / soul / watchdog）
//   - sessionMgr: 会话管理器
//   - pgStore: Postgres 存储
//   - redisStore: Redis 存储
//   - roleCfg: 角色配置
//   - modelFactory: 模型工厂
type APIHandler struct {
	broadcaster *TUIBroadcaster // SSE 广播器
	snapshotMgr interface {     // 快照管理器（鸭子类型，避免循环依赖）
		Load(ctx context.Context, agentID, topicID string) (*types.AgentSnapshot, error)
	}
	// graphControl 用于暂停/恢复 Graph
	paused map[string]bool // topic_id -> 是否暂停

	rt           *runtime.Runtime          // 聚合运行时
	sessionMgr   *SessionManager           // 会话管理器
	pgStore      *store.PostgresStore      // Postgres 存储
	redisStore   *store.RedisStore         // Redis 存储
	roleCfg      *pkgconfig.RoleConfigFile // 角色配置
	modelFactory *model.ModelFactory       // 模型工厂
}

// NewAPIHandler 创建 API 处理器
// 参数：broadcaster - SSE 广播器。
// 返回值：*APIHandler（其余字段需通过 Set* 方法注入）。
func NewAPIHandler(broadcaster *TUIBroadcaster) *APIHandler {
	return &APIHandler{
		broadcaster: broadcaster,
		paused:      make(map[string]bool), // 初始化暂停表
	}
}

// SetSnapshotManager 设置快照管理器
// 参数：mgr - 实现了 Load(ctx, agentID, topicID) 的对象。
func (h *APIHandler) SetSnapshotManager(mgr interface {
	Load(ctx context.Context, agentID, topicID string) (*types.AgentSnapshot, error)
}) {
	h.snapshotMgr = mgr
}

// SetRuntime 注入 Runtime
// 参数：rt - 聚合运行时。
func (h *APIHandler) SetRuntime(rt *runtime.Runtime) {
	h.rt = rt
}

// SetSessionManager 注入会话管理器
// 参数：mgr - 会话管理器。
func (h *APIHandler) SetSessionManager(mgr *SessionManager) {
	h.sessionMgr = mgr
}

// SetStores 注入存储层
// 参数：pg - Postgres；redis - Redis。
func (h *APIHandler) SetStores(pg *store.PostgresStore, redis *store.RedisStore) {
	h.pgStore = pg
	h.redisStore = redis
}

// SetRoleConfig 注入角色配置
// 参数：cfg - 角色配置文件。
func (h *APIHandler) SetRoleConfig(cfg *pkgconfig.RoleConfigFile) {
	h.roleCfg = cfg
}

// SetModelFactory 注入模型工厂
// 参数：mf - 模型工厂。
func (h *APIHandler) SetModelFactory(mf *model.ModelFactory) {
	h.modelFactory = mf
}

// RetrieveHandler 手动检索记忆
// 职责：解析 POST body，广播 retrieve.request 事件给订阅者。
// 参数：w / r - HTTP 标准参数。
// 副作用：广播 UIEvent；不做实际检索，只触发前端展示。
func (h *APIHandler) RetrieveHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	req, err := DecodeBody[struct {
		TopicID string `json:"topic_id"`
		AgentID string `json:"agent_id"`
		Query   string `json:"query"`
	}](r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// 广播检索请求事件
	h.broadcaster.Broadcast(req.TopicID, types.UIEvent{
		Type:    "retrieve.request",
		Payload: map[string]string{"agent_id": req.AgentID, "query": req.Query},
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// EventResolveHandler 标记 Event 已处理
// 职责：解析 POST body，广播 workspace.event 事件（status=Done），通知前端关闭事件。
func (h *APIHandler) EventResolveHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	req, err := DecodeBody[struct {
		TopicID string `json:"topic_id"`
		EventID string `json:"event_id"`
	}](r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	h.broadcaster.Broadcast(req.TopicID, types.UIEvent{
		Type: "workspace.event",
		Payload: types.EventPayload{
			ID:     req.EventID,
			Status: "Done", // 标记为已解决
		},
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "resolved"})
}

// SnapshotInspectHandler 查看快照详情
// 职责：调用 snapshotMgr.Load 取 Agent 快照，返回 JSON。
func (h *APIHandler) SnapshotInspectHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	req, err := DecodeBody[struct {
		TopicID string `json:"topic_id"`
		AgentID string `json:"agent_id"`
	}](r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var snapshot *types.AgentSnapshot
	if h.snapshotMgr != nil {
		snapshot, err = h.snapshotMgr.Load(r.Context(), req.AgentID, req.TopicID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"agent_id": req.AgentID,
		"snapshot": snapshot,
	})
}

// GraphPauseHandler 暂停 Graph
// 职责：把 topic 标记为暂停，广播 graph.control(action=pause) 事件。
func (h *APIHandler) GraphPauseHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	req, err := DecodeBody[struct {
		TopicID string `json:"topic_id"`
	}](r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	h.paused[req.TopicID] = true // 标记暂停
	h.broadcaster.Broadcast(req.TopicID, types.UIEvent{
		Type:    "graph.control",
		Payload: map[string]string{"action": "pause"},
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "paused"})
}

// GraphResumeHandler 恢复 Graph
// 职责：从 paused 表删除 topic，广播 graph.control(action=resume) 事件。
func (h *APIHandler) GraphResumeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	req, err := DecodeBody[struct {
		TopicID string `json:"topic_id"`
	}](r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	delete(h.paused, req.TopicID) // 取消暂停
	h.broadcaster.Broadcast(req.TopicID, types.UIEvent{
		Type:    "graph.control",
		Payload: map[string]string{"action": "resume"},
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "resumed"})
}

// IsPaused 检查话题是否已暂停
// 参数：topicID - 话题 ID。
// 返回值：bool - true 表示已暂停。
func (h *APIHandler) IsPaused(topicID string) bool {
	return h.paused[topicID]
}

// MetricsHandler GET /api/metrics — 返回 Prometheus 格式运行时指标
// 指标：goroutine 数、内存分配、内存中会话数、LLM 调用/超时次数。
func (h *APIHandler) MetricsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var ms stdruntime.MemStats
	stdruntime.ReadMemStats(&ms)

	sessionCount := 0
	callCount, timeoutCount := 0, 0
	if h.sessionMgr != nil {
		sessionCount = h.sessionMgr.SessionCount()
		callCount, timeoutCount, _, _ = h.sessionMgr.LLMStats()
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintf(w, "# HELP go_goroutines Number of goroutines\n# TYPE go_goroutines gauge\ngo_goroutines %d\n\n", stdruntime.NumGoroutine())
	fmt.Fprintf(w, "# HELP go_memory_alloc_bytes Allocated memory in bytes\n# TYPE go_memory_alloc_bytes gauge\ngo_memory_alloc_bytes %d\n\n", ms.Alloc)
	fmt.Fprintf(w, "# HELP bma_sessions_total Total sessions in memory\n# TYPE bma_sessions_total gauge\nbma_sessions_total %d\n\n", sessionCount)
	fmt.Fprintf(w, "# HELP bma_llm_calls_total Total LLM calls\n# TYPE bma_llm_calls_total counter\nbma_llm_calls_total %d\n\n", callCount)
	fmt.Fprintf(w, "# HELP bma_llm_timeouts_total Total LLM timeouts\n# TYPE bma_llm_timeouts_total counter\nbma_llm_timeouts_total %d\n", timeoutCount)
}

// HealthHandler GET /api/health — 返回 Postgres / Redis / LLM 连接状态
// 职责：分别 ping Postgres、Redis，统计 LLM 调用健康度，返回 JSON 报告。
func (h *APIHandler) HealthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second) // 健康检查总超时 2 秒
	defer cancel()

	pgStatus := checkPostgres(ctx, h.pgStore)    // Postgres 探活
	redisStatus := checkRedis(ctx, h.redisStore) // Redis 探活
	llmStatus := checkLLM(h.sessionMgr)          // LLM 统计

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"postgres": pgStatus,
		"redis":    redisStatus,
		"llm":      llmStatus,
	})
}

// checkPostgres 检查 Postgres 连接。
// 返回值：map - 含 name / online / detail / latency_ms。
func checkPostgres(ctx context.Context, pg *store.PostgresStore) map[string]any {
	status := map[string]any{"name": "Postgres", "online": false, "detail": "not configured"}
	if pg == nil {
		return status // 未配置直接返回
	}
	start := time.Now()
	if err := pg.DB().PingContext(ctx); err != nil {
		status["detail"] = err.Error() // ping 失败
		return status
	}
	status["online"] = true
	status["latency_ms"] = time.Since(start).Milliseconds() // 延迟毫秒
	status["detail"] = "connected"
	return status
}

// checkRedis 检查 Redis 连接。
// 返回值：map - 含 name / online / detail / latency_ms。
func checkRedis(ctx context.Context, redis *store.RedisStore) map[string]any {
	status := map[string]any{"name": "Redis", "online": false, "detail": "not configured"}
	if redis == nil {
		return status // 未配置直接返回
	}
	start := time.Now()
	if err := redis.Ping(ctx); err != nil {
		status["detail"] = err.Error()
		return status
	}
	status["online"] = true
	status["latency_ms"] = time.Since(start).Milliseconds()
	status["detail"] = "connected"
	return status
}

// checkLLM 通过 SessionManager 的 LLM 调用统计判断 LLM 健康。
// 健康判定：calls>0 且 timeouts<calls。
// 返回值：map - 含 name / online / detail。
func checkLLM(mgr *SessionManager) map[string]any {
	status := map[string]any{"name": "LLM API", "online": false, "detail": "not configured"}
	if mgr == nil {
		return status
	}
	calls, timeouts, avg, max := mgr.LLMStats() // 聚合统计
	online := calls > 0 && timeouts < calls     // 调用次数 > 0 且超时未占满
	status["online"] = online
	status["detail"] = fmt.Sprintf("calls=%d timeouts=%d avg=%v max=%v", calls, timeouts, avg.Round(time.Millisecond), max.Round(time.Millisecond))
	if !online {
		status["detail"] = "no recent calls" // 无近期调用视为不健康
	}
	return status
}

// StatusHandler GET /api/status — 返回程序、模式、人格、LLM 配置概览
// 职责：聚合 Soul 名称与角色配置中的 LLM provider / model，返回静态信息。
func (h *APIHandler) StatusHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	soulName := "default"
	if h.rt != nil && h.rt.Soul != nil {
		soulName = h.rt.Soul.Name() // 取人格名称
	}

	llmProvider := "mock"
	llmModel := "mock"
	if h.roleCfg != nil {
		llmProvider = h.roleCfg.MetaAgent.ModelConfig.Provider // 供应商
		llmModel = h.roleCfg.MetaAgent.ModelConfig.Model       // 模型名
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"program":      "BlockMemoryAgent",
		"mode":         "multi-agent",
		"soul":         soulName,
		"llm_provider": llmProvider,
		"llm_model":    llmModel,
		"version":      "v3",
	})
}

// TimelineHandler GET /api/metrics/timeline — 返回最近会话 / LLM 时间线
// 职责：按小时聚合所有会话的 token_usage 事件，返回 N 个时间点的 calls / tokens。
// 参数：?points=N - 时间点数（默认 12）。
func (h *APIHandler) TimelineHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	points := 12 // 默认 12 个点（12 小时）
	if q := r.URL.Query().Get("points"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 {
			points = n // 解析 query 参数
		}
	}

	data := make([]map[string]any, points)
	now := time.Now()
	for i := 0; i < points; i++ {
		data[i] = map[string]any{
			"time":   now.Add(-time.Duration(points-1-i) * time.Hour).Format("15:00"), // 第 i 个点的时间
			"calls":  0,
			"tokens": 0,
		}
	}

	if h.sessionMgr != nil {
		for _, s := range h.sessionMgr.ListSessions() {
			for _, ev := range s.Events {
				if ev.Kind != eventkind.TokenUsage {
					continue // 仅统计 token_usage
				}
				hourIdx := points - 1 - int(now.Sub(ev.Timestamp).Hours()) // 计算落在第几个时间点
				if hourIdx < 0 || hourIdx >= points {
					continue // 超出范围跳过
				}
				data[hourIdx]["calls"] = data[hourIdx]["calls"].(int) + 1
				data[hourIdx]["tokens"] = data[hourIdx]["tokens"].(int) + ev.InputTokens + ev.OutputTokens
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"points": data})
}

// ActivityHandler GET /api/activity — 返回最近活动流
// 职责：聚合所有会话事件，倒序取前 N 条作为活动流。
// 参数：?limit=N - 返回条数（默认 10）。
func (h *APIHandler) ActivityHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	limit := 10 // 默认 10 条
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 {
			limit = n
		}
	}

	var activities []map[string]any
	if h.sessionMgr != nil {
		for _, s := range h.sessionMgr.ListSessions() {
			// 从尾部倒序遍历，直到达到 limit
			for i := len(s.Events) - 1; i >= 0 && len(activities) < limit; i-- {
				ev := s.Events[i]
				if ev.Kind == "" {
					continue // 跳过无 kind 的事件
				}
				activities = append(activities, map[string]any{
					"session_id": s.ID,
					"agent":      ev.Agent,
					"kind":       ev.Kind,
					"content":    ev.Message,
					"time":       ev.Timestamp.Format("15:04:05"),
				})
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"activities": activities})
}

// SnapshotHandler POST /api/snapshot — 查看 Agent 快照
// 职责：与 SnapshotInspectHandler 类似，按 agent_id + topic_id 加载快照。
func (h *APIHandler) SnapshotHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	req, err := DecodeBody[struct {
		AgentID string `json:"agent_id"`
		TopicID string `json:"topic_id"`
	}](r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var snap *types.AgentSnapshot
	if h.snapshotMgr != nil {
		snap, err = h.snapshotMgr.Load(r.Context(), req.AgentID, req.TopicID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"agent_id": req.AgentID,
		"snapshot": snap,
	})
}

// MemorySearchHandler POST /api/memory/search — 检索 Agent 记忆
// 职责：优先按 domain 做块记忆向量检索（P0-1 领域过滤）；未提供 domain 时回退到
// Episode 关键词评分排序，返回 top N。
// 参数：?limit=N - 默认 10。
func (h *APIHandler) MemorySearchHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	req, err := DecodeBody[struct {
		AgentID string `json:"agent_id"`
		TopicID string `json:"topic_id"`
		Domain  string `json:"domain"`
		Query   string `json:"query"`
		Limit   int    `json:"limit"`
	}](r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Limit <= 0 {
		req.Limit = 10 // 默认 10 条
	}

	var results []map[string]any
	if h.pgStore != nil {
		if req.Domain != "" {
			// P0-1：按 domain 过滤的块记忆向量检索
			recs, err := memory.SearchBlockMemory(r.Context(), h.pgStore, req.Domain, req.Query, req.Limit)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			for _, rec := range recs {
				results = append(results, map[string]any{
					"step_id":    "",
					"summary":    rec.Summary,
					"action":     "block_memory",
					"importance": 0.0,
					"score":      0.0,
					"time":       rec.CreatedAt,
					"domain":     rec.Domain,
					"goal":       rec.Goal,
				})
			}
		} else {
			// 无 domain 时回退到 Episode 关键词评分
			eps, err := h.pgStore.GetEpisodes(r.Context(), req.AgentID, req.TopicID, 200) // 取最近 200 条
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			// 无 embedder，使用简单关键词评分
			scored := scoreEpisodesByKeywords(eps, req.Query)
			if len(scored) > req.Limit {
				scored = scored[:req.Limit] // 截断到 limit
			}
			for _, s := range scored {
				results = append(results, map[string]any{
					"step_id":    s.Episode.StepID,
					"summary":    s.Episode.ObservationSummary,
					"action":     s.Episode.Action,
					"importance": s.Episode.Importance,
					"score":      s.Score,
					"time":       s.Episode.Timestamp,
				})
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"agent_id": req.AgentID,
		"query":    req.Query,
		"domain":   req.Domain,
		"results":  results,
	})
}

// scoreEpisodesByKeywords 简化关键词评分（无 embedder 时兜底）
// 评分 = 关键词重叠 * 0.05 + 时间衰减 * 0.3 + 重要性 * 0.3（上限 1.0）
// 参数：eps - 候选 Episode 列表；query - 查询字符串。
// 返回值：按分数降序排列的 scoredEpisode 列表。
func scoreEpisodesByKeywords(eps []*types.Episode, query string) []*scoredEpisode {
	var out []*scoredEpisode
	queryRunes := []rune(strings.ToLower(query)) // 查询转小写 rune 列表
	for _, ep := range eps {
		score := 0.0
		summaryLower := strings.ToLower(ep.ObservationSummary) // 摘要转小写
		// 关键词重叠
		for _, r := range queryRunes {
			if strings.ContainsRune(summaryLower, r) {
				score += 0.05 // 每命中一个 rune 加 0.05
			}
		}
		// 时间衰减
		score += math.Exp(-0.01*time.Since(ep.Timestamp).Hours()) * 0.3 // 越新分数越高
		// 重要性
		score += ep.Importance * 0.3
		if score > 1.0 {
			score = 1.0 // 截顶 1.0
		}
		out = append(out, &scoredEpisode{Episode: ep, Score: score})
	}
	// 冒泡排序（数据量小，简单实现）
	for i := 0; i < len(out)-1; i++ {
		for j := 0; j < len(out)-1-i; j++ {
			if out[j].Score < out[j+1].Score {
				out[j], out[j+1] = out[j+1], out[j] // 降序交换
			}
		}
	}
	return out
}

// scoredEpisode 包装 Episode 及其关键词评分。
type scoredEpisode struct {
	Episode *types.Episode
	Score   float64
}

// MemoryLevelsHandler GET /api/memory/levels — 返回压缩层级分布
// 职责：调用 Postgres 统计两级压缩层级（Raw=0 / Standard=1）的 Episode 数，返回分布 JSON。
// 参数：?agent_id=...&topic_id=...
func (h *APIHandler) MemoryLevelsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	agentID := r.URL.Query().Get("agent_id")
	topicID := r.URL.Query().Get("topic_id")

	levels := map[string]int{"raw": 0, "standard": 0}
	total := 0
	if h.pgStore != nil {
		for lvl, key := range map[int]string{0: "raw", 1: "standard"} {
			var cnt int
			err := h.pgStore.DB().QueryRowContext(r.Context(), `
				SELECT COUNT(*) FROM agent_private_memory
				WHERE agent_id = $1 AND topic_id = $2 AND compression_level = $3
			`, agentID, topicID, lvl).Scan(&cnt)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			levels[key] = cnt
			total += cnt
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"agent_id": agentID,
		"topic_id": topicID,
		"total":    total,
		"levels":   levels,
	})
}

// MemoryEvalHandler GET /api/memory/eval — 记忆层简化评测入口（P3-1）
// 职责：汇总所有 (agent_id, topic_id) 下的 Raw/Standard 分布，输出评测 JSON。
// 调用方应先跑 test/coding/ 与 test/api/ 集成测试，再请求本端点获取分布数据。
func (h *APIHandler) MemoryEvalHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.pgStore == nil {
		http.Error(w, "postgres store not available", http.StatusServiceUnavailable)
		return
	}

	rows, err := h.pgStore.DB().QueryContext(r.Context(), `
		SELECT agent_id, topic_id, compression_level, COUNT(*)
		FROM agent_private_memory
		GROUP BY agent_id, topic_id, compression_level
		ORDER BY topic_id, agent_id, compression_level
	`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type pairStat struct {
		AgentID string         `json:"agent_id"`
		TopicID string         `json:"topic_id"`
		Levels  map[string]int `json:"levels"`
		Total   int            `json:"total"`
	}

	pairs := make(map[string]*pairStat)
	grandTotal := 0
	grandRaw := 0
	grandStandard := 0

	for rows.Next() {
		var agentID, topicID string
		var level, count int
		if err := rows.Scan(&agentID, &topicID, &level, &count); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		key := topicID + "/" + agentID
		p, ok := pairs[key]
		if !ok {
			p = &pairStat{AgentID: agentID, TopicID: topicID, Levels: map[string]int{"raw": 0, "standard": 0}}
			pairs[key] = p
		}
		switch level {
		case 0:
			p.Levels["raw"] = count
			grandRaw += count
		case 1:
			p.Levels["standard"] = count
			grandStandard += count
		default:
			// 未预期 level：跳过，避免 raw+standard != total 的不一致
			continue
		}
		p.Total += count
		grandTotal += count
	}
	// 检查迭代错误（database/sql 契约：rows.Next() 退出可能因错误而非正常结束）
	if err := rows.Err(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// map 迭代顺序非确定，排序保证响应可复现（与 SQL ORDER BY 一致）
	var pairList []*pairStat
	for _, p := range pairs {
		pairList = append(pairList, p)
	}
	sort.Slice(pairList, func(i, j int) bool {
		if pairList[i].TopicID != pairList[j].TopicID {
			return pairList[i].TopicID < pairList[j].TopicID
		}
		return pairList[i].AgentID < pairList[j].AgentID
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"total_episodes": grandTotal,
		"raw":            grandRaw,
		"standard":       grandStandard,
		"pairs":          pairList,
		"note":           "Run test/coding/ and test/api/ integration tests, then call this endpoint to evaluate Raw/Standard distribution.",
	})
}

// SkillsHandler GET /api/skills — 返回 Skill 池全部技能
// 职责：从 Runtime.Skills.Pool 取所有技能，返回 JSON 列表。
func (h *APIHandler) SkillsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var skills []*types.Skill
	if h.rt != nil && h.rt.Skills != nil && h.rt.Skills.Pool() != nil {
		skills = h.rt.Skills.Pool().All() // 取所有技能
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"skills": skills})
}

// AgentSkillsHandler GET /api/agents/{id}/skills — 返回 Agent 已装配 SkillSet
// 职责：从 URL 解析 agent id，查询其 SkillSet。
func (h *APIHandler) AgentSkillsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "agent id required", http.StatusBadRequest)
		return
	}
	var set *types.SkillSet
	if h.rt != nil && h.rt.Skills != nil {
		set = h.rt.Skills.GetForAgent(id) // 查询 Agent 已装配技能
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"agent_id": id,
		"skillset": set,
	})
}

// FilesHandler GET /api/files — 返回某会话 WriteFile 输出文件列表
// 职责：扫描会话事件中的 WriteFile 工具调用，去重后返回文件路径 / 大小 / 名称。
// 参数：?session=session-N
func (h *APIHandler) FilesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sessionID := r.URL.Query().Get("session")
	if sessionID == "" {
		http.Error(w, "session required", http.StatusBadRequest)
		return
	}

	var files []map[string]any
	if h.sessionMgr != nil {
		s := h.sessionMgr.GetSession(sessionID)
		if s != nil {
			seen := make(map[string]bool) // 文件路径去重
			for _, ev := range s.Events {
				if ev.Type != eventkind.ToolExec || ev.Tool != "WriteFile" || !ev.Success || ev.ToolPath == "" {
					continue // 仅成功的 WriteFile 事件
				}
				if seen[ev.ToolPath] {
					continue // 去重
				}
				seen[ev.ToolPath] = true
				info, _ := os.Stat(ev.ToolPath) // 查询文件信息
				files = append(files, map[string]any{
					"path": ev.ToolPath,
					"size": fileSize(info),
					"name": filepath.Base(ev.ToolPath), // 仅文件名
				})
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"session_id": sessionID,
		"files":      files,
	})
}

// FileContentHandler GET /api/files/content — 读取文件内容
// 职责：按 ?path=... 读取文件全文，返回 JSON（content 为字符串）。
func (h *APIHandler) FileContentHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "path required", http.StatusBadRequest)
		return
	}
	data, err := os.ReadFile(path) // 读取文件
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"path":    path,
		"content": string(data),
	})
}

// fileSize 安全地获取文件大小，info 为 nil 时返回 0。
// 参数：info - os.FileInfo（可为 nil）。
// 返回值：int64 - 文件字节数。
func fileSize(info os.FileInfo) int64 {
	if info == nil {
		return 0
	}
	return info.Size()
}
