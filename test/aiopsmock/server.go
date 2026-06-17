// Package aiopsmock 实现 doc/test/测试项目设计与接口文档.md 中的
// "智能运维事件管理平台 (AIOps Incident Manager)" Mock Server。
//
// 它在内存中模拟告警接入、事件聚合、根因分析、Playbook 执行、人工
// 审批、复盘等接口，输出严格遵循文档约定。Agent 在测试用例中
// 通过 HTTPGet/HTTPPost 工具与之交互。
package aiopsmock

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Server AIOps Mock Server
type Server struct {
	mu sync.Mutex

	httpServer *httptest.Server
	mux        *http.ServeMux

	alertSeq    atomic.Int64
	incidentSeq atomic.Int64
	execSeq     atomic.Int64

	incidents     map[string]*Incident
	executions    map[string]*Execution
	knowledgeHits []KnowledgeRecord
}

// Incident 告警聚合事件
type Incident struct {
	ID                  string             `json:"id"`
	Title               string             `json:"title"`
	Status              string             `json:"status"`
	Severity            string             `json:"severity"`
	CreatedAt           string             `json:"created_at"`
	Assignee            string             `json:"assignee,omitempty"`
	AlertCount          int                `json:"alert_count"`
	SuggestedRootCauses []SuggestedRC      `json:"suggested_root_causes"`
	RootCauseTask       string             `json:"-"`
	RootCauseStatus     string             `json:"-"`
	RootCauseResult     *RootCauseResult   `json:"-"`
	Postmortem          map[string]any     `json:"-"`
}

// SuggestedRC 简化根因建议
type SuggestedRC struct {
	Node       string  `json:"node"`
	Cause      string  `json:"cause"`
	Confidence float64 `json:"confidence"`
}

// RootCauseResult 根因分析结果
type RootCauseResult struct {
	Status     string                   `json:"status"`
	RootCauses []map[string]interface{} `json:"root_causes"`
}

// Execution Playbook 执行实例
type Execution struct {
	ID     string                 `json:"execution_id"`
	Status string                 `json:"status"`
	Steps  []*ExecutionStep       `json:"steps"`
	Params map[string]any         `json:"-"`
}

// ExecutionStep 一个 Playbook 步骤
type ExecutionStep struct {
	ID     string `json:"id"`
	Action string `json:"action"`
	Status string `json:"status"`
	Output any    `json:"output,omitempty"`
}

// KnowledgeRecord 简化知识库记录
type KnowledgeRecord struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Body  string `json:"body"`
	Tags  []string `json:"tags"`
}

// NewServer 启动 mock，返回带 URL 的 Server
func NewServer() *Server {
	s := &Server{
		mux:        http.NewServeMux(),
		incidents:  make(map[string]*Incident),
		executions: make(map[string]*Execution),
		knowledgeHits: []KnowledgeRecord{
			{ID: "kb-001", Title: "MySQL 慢查询导致 CPU 飙高的处置", Body: "排查慢日志、KILL 慢 SQL、添加索引、扩容只读副本。", Tags: []string{"mysql", "cpu"}},
			{ID: "kb-002", Title: "订单服务故障转移演练注意事项", Body: "演练前先校验预发健康度，先摘流量再注入故障；失败回滚优先恢复 DB。", Tags: []string{"chaos", "order"}},
		},
	}
	s.routes()
	s.httpServer = httptest.NewServer(s.mux)
	s.seed()
	return s
}

// URL 暴露 Mock URL
func (s *Server) URL() string { return s.httpServer.URL }

// Close 关闭 Mock
func (s *Server) Close() { s.httpServer.Close() }

// seed 预置一个事件，用于场景 B / C / D 的入口
func (s *Server) seed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := "INC-20260616-0018"
	s.incidents[id] = &Incident{
		ID:        id,
		Title:     "订单服务延迟飙升关联DB异常",
		Status:    "acknowledged",
		Severity:  "critical",
		CreatedAt: "2026-06-16T09:58:00Z",
		Assignee:  "zhangsan",
		AlertCount: 15,
		SuggestedRootCauses: []SuggestedRC{
			{Node: "mysql-primary", Cause: "CPU利用率98%，慢查询堆积", Confidence: 0.89},
		},
	}
}

// routes 注册路由
func (s *Server) routes() {
	s.mux.HandleFunc("/api/v1/alerts", s.handleAlerts)
	s.mux.HandleFunc("/api/v1/incidents", s.handleIncidents)
	s.mux.HandleFunc("/api/v1/incidents/", s.handleIncidentSub)
	s.mux.HandleFunc("/api/v1/topology", s.handleTopology)
	s.mux.HandleFunc("/api/v1/playbooks", s.handlePlaybookList)
	s.mux.HandleFunc("/api/v1/executions/", s.handleExecutionSub)
	s.mux.HandleFunc("/api/v1/knowledge/search", s.handleKnowledgeSearch)
}

// ----- alerts -----

func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var alert struct {
		Source     string         `json:"source"`
		AlertID    string         `json:"alert_id"`
		Name       string         `json:"name"`
		Severity   string         `json:"severity"`
		Status     string         `json:"status"`
		Labels     map[string]any `json:"labels"`
		Annotations map[string]any `json:"annotations"`
	}
	if err := json.NewDecoder(r.Body).Decode(&alert); err != nil {
		writeErr(w, 1001, "invalid body")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// 简化：所有 critical 告警聚合到同一 Incident，否则新建
	target := "INC-20260616-0018"
	matched := target
	if alert.Severity == "critical" {
		s.incidents[target].AlertCount++
	} else {
		newID := fmt.Sprintf("INC-20260616-%04d", 100+s.incidentSeq.Add(1))
		matched = newID
		s.incidents[newID] = &Incident{
			ID:         newID,
			Title:      "新事件：" + alert.Name,
			Status:     "open",
			Severity:   alert.Severity,
			CreatedAt:  time.Now().UTC().Format(time.RFC3339),
			AlertCount: 1,
		}
	}

	writeOK(w, map[string]any{
		"incident_id":      matched,
		"is_new_incident":  matched != target,
		"matched_incident": target,
	})
}

// ----- incidents -----

func (s *Server) handleIncidents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make([]*Incident, 0, len(s.incidents))
	for _, in := range s.incidents {
		items = append(items, in)
	}
	writeOK(w, map[string]any{
		"items": items, "total": len(items), "page": 1, "size": len(items),
	})
}

// /api/v1/incidents/{id}/...
func (s *Server) handleIncidentSub(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/incidents/")
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	id := parts[0]
	s.mu.Lock()
	inc := s.incidents[id]
	s.mu.Unlock()
	if inc == nil {
		writeErrCode(w, 1002, "incident not found", http.StatusNotFound)
		return
	}
	if len(parts) == 1 {
		writeOK(w, inc)
		return
	}
	switch parts[1] {
	case "actions":
		s.handleIncidentAction(w, r, inc)
	case "rootcause":
		if len(parts) >= 3 && parts[2] == "analyze" {
			s.handleRootCauseAnalyze(w, r, inc)
			return
		}
		if len(parts) >= 3 && parts[2] == "result" {
			s.handleRootCauseResult(w, r, inc)
			return
		}
		http.NotFound(w, r)
	case "playbooks":
		// /incidents/{id}/playbooks/{pid}/execute
		if len(parts) >= 4 && parts[3] == "execute" {
			s.handlePlaybookExecute(w, r, inc, parts[2])
			return
		}
		http.NotFound(w, r)
	case "postmortem":
		s.handlePostmortem(w, r, inc)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleIncidentAction(w http.ResponseWriter, r *http.Request, inc *Incident) {
	var body struct {
		Action string         `json:"action"`
		Params map[string]any `json:"params"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch body.Action {
	case "escalate":
		inc.Severity = "critical"
		if assignee, ok := body.Params["assignee"].(string); ok {
			inc.Assignee = assignee
		}
		inc.Status = "escalated"
	case "ack":
		inc.Status = "acknowledged"
	case "close":
		inc.Status = "closed"
	}
	writeOK(w, map[string]any{"incident_id": inc.ID, "status": inc.Status})
}

func (s *Server) handleRootCauseAnalyze(w http.ResponseWriter, r *http.Request, inc *Incident) {
	s.mu.Lock()
	defer s.mu.Unlock()
	taskID := fmt.Sprintf("rca-task-%d", time.Now().UnixNano()%100000)
	inc.RootCauseTask = taskID
	inc.RootCauseStatus = "running"
	// 模拟异步：稍后填充结果
	go func(target *Incident) {
		time.Sleep(50 * time.Millisecond)
		s.mu.Lock()
		defer s.mu.Unlock()
		target.RootCauseStatus = "completed"
		target.RootCauseResult = &RootCauseResult{
			Status: "completed",
			RootCauses: []map[string]interface{}{
				{
					"node":       "mysql-primary",
					"type":       "resource_saturation",
					"detail":     "慢SQL `SELECT * FROM orders WHERE create_time < '2024-01-01' ORDER BY id` 导致CPU飙高",
					"confidence": 0.92,
					"suggested_actions": []string{
						"playbook:kill-slow-query",
						"playbook:add-index-suggestion",
					},
				},
			},
		}
	}(inc)
	writeOK(w, map[string]any{"task_id": taskID, "status": "running"})
}

func (s *Server) handleRootCauseResult(w http.ResponseWriter, r *http.Request, inc *Incident) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if inc.RootCauseResult == nil {
		writeOK(w, map[string]any{"status": "running"})
		return
	}
	writeOK(w, inc.RootCauseResult)
}

// ----- playbook -----

func (s *Server) handlePlaybookList(w http.ResponseWriter, r *http.Request) {
	scope := r.URL.Query().Get("scope")
	tags := r.URL.Query().Get("tags")
	playbooks := []map[string]any{
		{"id": "kill-slow-query", "name": "Kill 慢查询", "scope": "incident", "tags": []string{"diagnosis", "mysql"}},
		{"id": "chaos-drill-order", "name": "订单服务容灾演练", "scope": "drill", "tags": []string{"chaos"}},
	}
	// 简易过滤
	if scope != "" || tags != "" {
		var filtered []map[string]any
		for _, p := range playbooks {
			if scope != "" && p["scope"] != scope {
				continue
			}
			if tags != "" {
				found := false
				for _, t := range p["tags"].([]string) {
					if t == tags {
						found = true
						break
					}
				}
				if !found {
					continue
				}
			}
			filtered = append(filtered, p)
		}
		playbooks = filtered
	}
	writeOK(w, map[string]any{"items": playbooks})
}

func (s *Server) handlePlaybookExecute(w http.ResponseWriter, r *http.Request, inc *Incident, pid string) {
	var body struct {
		Parameters      map[string]any `json:"parameters"`
		DryRun          bool           `json:"dry_run"`
		RequireApproval bool           `json:"require_approval"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	s.mu.Lock()
	defer s.mu.Unlock()
	execID := fmt.Sprintf("exec-%03d", s.execSeq.Add(1))
	steps := []*ExecutionStep{
		{ID: "step1", Action: "slow_query_analysis", Status: "completed", Output: map[string]string{"top_sql": "SELECT * FROM orders WHERE..."}},
		{ID: "step2", Action: "kill_slow_query", Status: "pending"},
	}
	if pid == "chaos-drill-order" {
		steps = []*ExecutionStep{
			{ID: "check_pre", Action: "http_check", Status: "completed", Output: map[string]any{"code": 200}},
			{ID: "drain_traffic", Action: "webhook", Status: "completed"},
			{ID: "chaos_db_fail", Action: "exec_chaos", Status: "completed"},
			{ID: "verify_failover", Action: "parallel", Status: "failed", Output: map[string]any{"check_order_availability": map[string]int{"code": 502}}},
			{ID: "rollback_traffic", Action: "webhook", Status: "completed"},
			{ID: "recover_db", Action: "exec_chaos", Status: "completed"},
			{ID: "generate_report", Action: "call_internal_api", Status: "completed", Output: "演练失败已自动回滚"},
		}
	}
	status := "running"
	if body.RequireApproval {
		status = "pending_approval"
	}
	exec := &Execution{
		ID: execID, Status: status, Steps: steps, Params: body.Parameters,
	}
	s.executions[execID] = exec
	writeOK(w, exec)
}

// /api/v1/executions/{id}/...
func (s *Server) handleExecutionSub(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/executions/")
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	id := parts[0]
	s.mu.Lock()
	exec := s.executions[id]
	s.mu.Unlock()
	if exec == nil {
		writeErrCode(w, 1002, "execution not found", http.StatusNotFound)
		return
	}
	if len(parts) == 1 {
		writeOK(w, exec)
		return
	}
	if len(parts) >= 4 && parts[1] == "steps" && parts[3] == "approve" {
		var body struct {
			Approved bool   `json:"approved"`
			Comment  string `json:"comment"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		stepID := parts[2]
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, st := range exec.Steps {
			if st.ID == stepID {
				if body.Approved {
					st.Status = "completed"
					st.Output = "approved & executed"
					exec.Status = "completed"
				} else {
					st.Status = "rejected"
					exec.Status = "aborted"
				}
				break
			}
		}
		writeOK(w, exec)
		return
	}
	http.NotFound(w, r)
}

// ----- topology / knowledge -----

func (s *Server) handleTopology(w http.ResponseWriter, r *http.Request) {
	writeOK(w, map[string]any{
		"nodes": []map[string]string{
			{"id": "order-service", "type": "service", "status": "degraded"},
			{"id": "payment-service", "type": "service", "status": "healthy"},
			{"id": "mysql-primary", "type": "database", "status": "critical"},
			{"id": "redis-cluster", "type": "cache", "status": "healthy"},
		},
		"edges": []map[string]string{
			{"from": "order-service", "to": "payment-service", "type": "rpc"},
			{"from": "order-service", "to": "mysql-primary", "type": "jdbc"},
			{"from": "order-service", "to": "redis-cluster", "type": "cache-rw"},
		},
	})
}

func (s *Server) handleKnowledgeSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(r.URL.Query().Get("q"))
	var out []KnowledgeRecord
	for _, kb := range s.knowledgeHits {
		if q == "" {
			out = append(out, kb)
			continue
		}
		if strings.Contains(strings.ToLower(kb.Title+kb.Body+strings.Join(kb.Tags, " ")), q) {
			out = append(out, kb)
		}
	}
	writeOK(w, map[string]any{"items": out, "total": len(out)})
}

func (s *Server) handlePostmortem(w http.ResponseWriter, r *http.Request, inc *Incident) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	inc.Postmortem = body
	inc.Status = "closed"
	s.mu.Unlock()
	writeOK(w, map[string]any{"incident_id": inc.ID, "postmortem_saved": true})
}

// ----- helpers -----

func writeOK(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code": 0, "message": "success", "data": data,
	})
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeErrCode(w, code, msg, http.StatusBadRequest)
}

func writeErrCode(w http.ResponseWriter, code int, msg string, http_code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http_code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code": code, "message": msg, "data": nil,
	})
}
