package bootstrap

import (
	"net/http"

	"github.com/blockmemory/agent/backend/internal/server"
)

// NewDefaultMux builds the production HTTP mux from an assembled App.
// It applies the configured auth token middleware and wires all API routes.
func NewDefaultMux(app *App) *http.ServeMux {
	cfg := app.Config
	mux := http.NewServeMux()

	authToken := ""
	publicPaths := []string{"/api/health"}
	if cfg.HTTP.AuthEnabled {
		authToken = cfg.HTTP.AuthToken
	}
	wrap := func(h http.HandlerFunc) http.HandlerFunc {
		return server.AuthMiddleware(authToken, publicPaths, h)
	}

	sessionMgr := app.Server

	mux.HandleFunc("/api/sessions", wrap(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			sessionMgr.HandleListSessions(w, r)
		case http.MethodPost:
			sessionMgr.HandleCreateSession(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))

	// Session subresources use Go 1.22 path variables so handlers can read r.PathValue("id").
	mux.HandleFunc("/api/sessions/{id}/stream", wrap(sessionMgr.HandleSessionStream))
	mux.HandleFunc("/api/sessions/{id}/message", wrap(sessionMgr.HandleSessionMessage))
	mux.HandleFunc("/api/sessions/{id}/board", wrap(sessionMgr.HandleSessionBoard))
	mux.HandleFunc("/api/sessions/{id}/agents", wrap(sessionMgr.HandleSessionAgents))
	mux.HandleFunc("/api/sessions/{id}/metrics", wrap(sessionMgr.HandleSessionMetrics))
	mux.HandleFunc("/api/sessions/{id}/logs", wrap(sessionMgr.HandleSessionLogs))
	mux.HandleFunc("/api/sessions/{id}/token-metrics", wrap(sessionMgr.HandleSessionTokenMetrics))
	mux.HandleFunc("/api/sessions/{id}/watchdog", wrap(sessionMgr.HandleSessionWatchdog))
	mux.HandleFunc("/api/sessions/{id}/mailbox", wrap(sessionMgr.HandleSessionMailbox))
	mux.HandleFunc("/api/sessions/{id}/clarify", wrap(sessionMgr.HandleSessionClarify))
	mux.HandleFunc("/api/sessions/{id}/interrupt", wrap(sessionMgr.HandleSessionInterrupt))
	mux.HandleFunc("/api/sessions/{id}/enqueue", wrap(sessionMgr.HandleSessionEnqueue))
	mux.HandleFunc("/api/sessions/{id}/cancel", wrap(sessionMgr.HandleSessionCancel))
	mux.HandleFunc("/api/sessions/{id}/topic", wrap(sessionMgr.HandleSessionTopic))
	mux.HandleFunc("/api/sessions/{id}", wrap(sessionMgr.HandleGetSession))

	apiHandler := server.NewAPIHandler(nil)
	apiHandler.SetSessionManager(sessionMgr)
	apiHandler.SetRuntime(app.Runtime)
	apiHandler.SetStores(app.Postgres, app.Redis)
	apiHandler.SetRoleConfig(app.RoleConfig)
	apiHandler.SetModelFactory(app.ModelFactory)
	apiHandler.SetSnapshotManager(app.SnapshotManager)

	mux.HandleFunc("/api/health", apiHandler.HealthHandler)
	mux.HandleFunc("/api/metrics", wrap(apiHandler.MetricsHandler))
	mux.HandleFunc("/api/status", wrap(apiHandler.StatusHandler))
	mux.HandleFunc("/api/metrics/timeline", wrap(apiHandler.TimelineHandler))
	mux.HandleFunc("/api/activity", wrap(apiHandler.ActivityHandler))

	dagHandler := app.DAGHandler
	mux.Handle("/api/dag", wrap(func(w http.ResponseWriter, r *http.Request) {
		dagHandler.ServeHTTP(w, r)
	}))
	mux.Handle("/api/dag/", wrap(func(w http.ResponseWriter, r *http.Request) {
		dagHandler.ServeHTTP(w, r)
	}))

	mux.HandleFunc("/api/snapshot", wrap(apiHandler.SnapshotHandler))
	mux.HandleFunc("/api/memory/search", wrap(apiHandler.MemorySearchHandler))
	mux.HandleFunc("/api/memory/levels", wrap(apiHandler.MemoryLevelsHandler))
	mux.HandleFunc("/api/memory/eval", wrap(apiHandler.MemoryEvalHandler))
	mux.HandleFunc("/api/skills", wrap(apiHandler.SkillsHandler))
	mux.HandleFunc("/api/agents/{id}/skills", wrap(apiHandler.AgentSkillsHandler))
	mux.HandleFunc("/api/files", wrap(apiHandler.FilesHandler))
	mux.HandleFunc("/api/files/content", wrap(apiHandler.FileContentHandler))

	return mux
}
