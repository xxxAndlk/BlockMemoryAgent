package server

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/runtime"
	"github.com/blockmemory/agent/backend/internal/store"
	pkgconfig "github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// APIHandler TUI / Web API 处理器
type APIHandler struct {
	broadcaster *TUIBroadcaster
	snapshotMgr interface {
		Load(ctx context.Context, agentID, topicID string) (*types.AgentSnapshot, error)
	}
	// graphControl 用于暂停/恢复 Graph
	paused map[string]bool

	rt         *runtime.Runtime
	sessionMgr *SessionManager
	pgStore    *store.PostgresStore
	redisStore *store.RedisStore
	roleCfg    *pkgconfig.RoleConfigFile
	modelFactory *model.ModelFactory
}

// NewAPIHandler 创建 API 处理器
func NewAPIHandler(broadcaster *TUIBroadcaster) *APIHandler {
	return &APIHandler{
		broadcaster: broadcaster,
		paused:      make(map[string]bool),
	}
}

// SetSnapshotManager 设置快照管理器
func (h *APIHandler) SetSnapshotManager(mgr interface {
	Load(ctx context.Context, agentID, topicID string) (*types.AgentSnapshot, error)
}) {
	h.snapshotMgr = mgr
}

// SetRuntime 注入 Runtime
func (h *APIHandler) SetRuntime(rt *runtime.Runtime) {
	h.rt = rt
}

// SetSessionManager 注入会话管理器
func (h *APIHandler) SetSessionManager(mgr *SessionManager) {
	h.sessionMgr = mgr
}

// SetStores 注入存储层
func (h *APIHandler) SetStores(pg *store.PostgresStore, redis *store.RedisStore) {
	h.pgStore = pg
	h.redisStore = redis
}

// SetRoleConfig 注入角色配置
func (h *APIHandler) SetRoleConfig(cfg *pkgconfig.RoleConfigFile) {
	h.roleCfg = cfg
}

// SetModelFactory 注入模型工厂
func (h *APIHandler) SetModelFactory(mf *model.ModelFactory) {
	h.modelFactory = mf
}

// RetrieveHandler 手动检索记忆
func (h *APIHandler) RetrieveHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		TopicID string `json:"topic_id"`
		AgentID string `json:"agent_id"`
		Query   string `json:"query"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
func (h *APIHandler) EventResolveHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		TopicID string `json:"topic_id"`
		EventID string `json:"event_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	h.broadcaster.Broadcast(req.TopicID, types.UIEvent{
		Type: "workspace.event",
		Payload: types.EventPayload{
			ID:     req.EventID,
			Status: "Done",
		},
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "resolved"})
}

// SnapshotInspectHandler 查看快照详情
func (h *APIHandler) SnapshotInspectHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		TopicID string `json:"topic_id"`
		AgentID string `json:"agent_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var snapshot *types.AgentSnapshot
	if h.snapshotMgr != nil {
		var err error
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
func (h *APIHandler) GraphPauseHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		TopicID string `json:"topic_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	h.paused[req.TopicID] = true
	h.broadcaster.Broadcast(req.TopicID, types.UIEvent{
		Type:    "graph.control",
		Payload: map[string]string{"action": "pause"},
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "paused"})
}

// GraphResumeHandler 恢复 Graph
func (h *APIHandler) GraphResumeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		TopicID string `json:"topic_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	delete(h.paused, req.TopicID)
	h.broadcaster.Broadcast(req.TopicID, types.UIEvent{
		Type:    "graph.control",
		Payload: map[string]string{"action": "resume"},
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "resumed"})
}

// IsPaused 检查话题是否已暂停
func (h *APIHandler) IsPaused(topicID string) bool {
	return h.paused[topicID]
}

// HealthHandler GET /api/health — 返回 Postgres / Redis / LLM 连接状态
func (h *APIHandler) HealthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	pgStatus := checkPostgres(ctx, h.pgStore)
	redisStatus := checkRedis(ctx, h.redisStore)
	llmStatus := checkLLM(h.sessionMgr)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"postgres": pgStatus,
		"redis":    redisStatus,
		"llm":      llmStatus,
	})
}

func checkPostgres(ctx context.Context, pg *store.PostgresStore) map[string]any {
	status := map[string]any{"name": "Postgres", "online": false, "detail": "not configured"}
	if pg == nil {
		return status
	}
	start := time.Now()
	if err := pg.DB().PingContext(ctx); err != nil {
		status["detail"] = err.Error()
		return status
	}
	status["online"] = true
	status["latency_ms"] = time.Since(start).Milliseconds()
	status["detail"] = "connected"
	return status
}

func checkRedis(ctx context.Context, redis *store.RedisStore) map[string]any {
	status := map[string]any{"name": "Redis", "online": false, "detail": "not configured"}
	if redis == nil {
		return status
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

func checkLLM(mgr *SessionManager) map[string]any {
	status := map[string]any{"name": "LLM API", "online": false, "detail": "not configured"}
	if mgr == nil {
		return status
	}
	calls, timeouts, avg, max := mgr.LLMStats()
	online := calls > 0 && timeouts < calls
	status["online"] = online
	status["detail"] = fmt.Sprintf("calls=%d timeouts=%d avg=%v max=%v", calls, timeouts, avg.Round(time.Millisecond), max.Round(time.Millisecond))
	if !online {
		status["detail"] = "no recent calls"
	}
	return status
}

// StatusHandler GET /api/status — 返回程序、模式、人格、LLM 配置概览
func (h *APIHandler) StatusHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	soulName := "default"
	if h.rt != nil && h.rt.Soul != nil {
		soulName = h.rt.Soul.Name()
	}

	llmProvider := "mock"
	llmModel := "mock"
	if h.roleCfg != nil {
		llmProvider = h.roleCfg.MetaAgent.ModelConfig.Provider
		llmModel = h.roleCfg.MetaAgent.ModelConfig.Model
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

// TimelineHandler GET /api/metrics/timeline — 返回最近会话/LLM 时间线
func (h *APIHandler) TimelineHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	points := 12
	if q := r.URL.Query().Get("points"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 {
			points = n
		}
	}

	data := make([]map[string]any, points)
	now := time.Now()
	for i := 0; i < points; i++ {
		data[i] = map[string]any{
			"time":  now.Add(-time.Duration(points-1-i) * time.Hour).Format("15:00"),
			"calls": 0,
			"tokens": 0,
		}
	}

	if h.sessionMgr != nil {
		for _, s := range h.sessionMgr.ListSessions() {
			for _, ev := range s.Events {
				if ev.Kind != "token_usage" {
					continue
				}
				hourIdx := points - 1 - int(now.Sub(ev.Timestamp).Hours())
				if hourIdx < 0 || hourIdx >= points {
					continue
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
func (h *APIHandler) ActivityHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	limit := 10
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 {
			limit = n
		}
	}

	var activities []map[string]any
	if h.sessionMgr != nil {
		for _, s := range h.sessionMgr.ListSessions() {
			for i := len(s.Events) - 1; i >= 0 && len(activities) < limit; i-- {
				ev := s.Events[i]
				if ev.Kind == "" {
					continue
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
func (h *APIHandler) SnapshotHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		AgentID string `json:"agent_id"`
		TopicID string `json:"topic_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var snap *types.AgentSnapshot
	if h.snapshotMgr != nil {
		var err error
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

// MemorySearchHandler POST /api/memory/search — 关键词检索 Agent 记忆
func (h *APIHandler) MemorySearchHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		AgentID string `json:"agent_id"`
		TopicID string `json:"topic_id"`
		Query   string `json:"query"`
		Limit   int    `json:"limit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Limit <= 0 {
		req.Limit = 10
	}

	var results []map[string]any
	if h.pgStore != nil {
		eps, err := h.pgStore.GetEpisodes(r.Context(), req.AgentID, req.TopicID, 200)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// 无 embedder，使用简单关键词评分
		scored := scoreEpisodesByKeywords(eps, req.Query)
		if len(scored) > req.Limit {
			scored = scored[:req.Limit]
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

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"agent_id": req.AgentID,
		"query":    req.Query,
		"results":  results,
	})
}

// scoreEpisodesByKeywords 简化关键词评分（无 embedder 时兜底）
func scoreEpisodesByKeywords(eps []*types.Episode, query string) []*scoredEpisode {
	var out []*scoredEpisode
	queryRunes := []rune(strings.ToLower(query))
	for _, ep := range eps {
		score := 0.0
		summaryLower := strings.ToLower(ep.ObservationSummary)
		// 关键词重叠
		for _, r := range queryRunes {
			if strings.ContainsRune(summaryLower, r) {
				score += 0.05
			}
		}
		// 时间衰减
		score += math.Exp(-0.01 * time.Since(ep.Timestamp).Hours()) * 0.3
		// 重要性
		score += ep.Importance * 0.3
		if score > 1.0 {
			score = 1.0
		}
		out = append(out, &scoredEpisode{Episode: ep, Score: score})
	}
	for i := 0; i < len(out)-1; i++ {
		for j := 0; j < len(out)-1-i; j++ {
			if out[j].Score < out[j+1].Score {
				out[j], out[j+1] = out[j+1], out[j]
			}
		}
	}
	return out
}

type scoredEpisode struct {
	Episode *types.Episode
	Score   float64
}

// MemoryLevelsHandler GET /api/memory/levels — 返回压缩层级分布
func (h *APIHandler) MemoryLevelsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	agentID := r.URL.Query().Get("agent_id")
	topicID := r.URL.Query().Get("topic_id")

	levels := map[string]int{"0": 0, "1": 0, "2": 0, "3": 0}
	total := 0
	if h.pgStore != nil {
		counts, err := h.pgStore.CountEpisodesByLevel(r.Context(), agentID, topicID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for lvl, cnt := range counts {
			levels[strconv.Itoa(int(lvl))] = cnt
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

// SkillsHandler GET /api/skills — 返回 Skill 池全部技能
func (h *APIHandler) SkillsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var skills []*types.Skill
	if h.rt != nil && h.rt.Skills != nil && h.rt.Skills.Pool() != nil {
		skills = h.rt.Skills.Pool().All()
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"skills": skills})
}

// AgentSkillsHandler GET /api/agents/{id}/skills — 返回 Agent 已装配 SkillSet
func (h *APIHandler) AgentSkillsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/agents/")
	id = strings.TrimSuffix(id, "/skills")
	if id == "" {
		http.Error(w, "agent id required", http.StatusBadRequest)
		return
	}
	var set *types.SkillSet
	if h.rt != nil && h.rt.Skills != nil {
		set = h.rt.Skills.GetForAgent(id)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"agent_id": id,
		"skillset": set,
	})
}

// FilesHandler GET /api/files — 返回某会话 WriteFile 输出文件列表
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
			seen := make(map[string]bool)
			for _, ev := range s.Events {
				if ev.Type != "tool_exec" || ev.Tool != "WriteFile" || !ev.Success || ev.ToolPath == "" {
					continue
				}
				if seen[ev.ToolPath] {
					continue
				}
				seen[ev.ToolPath] = true
				info, _ := os.Stat(ev.ToolPath)
				files = append(files, map[string]any{
					"path": ev.ToolPath,
					"size": fileSize(info),
					"name": filepath.Base(ev.ToolPath),
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
	data, err := os.ReadFile(path)
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

func fileSize(info os.FileInfo) int64 {
	if info == nil {
		return 0
	}
	return info.Size()
}
