# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

BlockMemoryAgent — Go multi-agent orchestration system built on go-kratos Blades. ReAct-loop core (single MetaAgent → `call_sub_agent` recursive dispatch) with event-stream memory, three-tier storage (PostgreSQL + Redis + pgvector), and a bubbletea TUI / HTTP web UI.

Go module: `github.com/blockmemory/agent/backend` (source under `backend/`; root `go.work` includes both `./backend` and `./test`). Go 1.25 (requires `GOTOOLCHAIN=local` on machines whose default Go is older — repo tests use this flag).

## Common commands

Makefile targets wrap the most common workflows:

```bash
make run            # build web UI and run the HTTP server
make backend-test   # run all backend tests
make test-test      # run integration tests
make lint           # run go vet on backend and test modules
make up             # start PostgreSQL + Redis via docker compose
make down           # stop docker compose services
make migrate        # apply SQL migrations (requires POSTGRES_DSN)
make help           # list all targets
```

Raw equivalents:

```bash
# Run HTTP server (main entrypoint — delegates all wiring to bootstrap.Build)
go run ./backend

# Run bubbletea TUI
go run ./backend/cmd/tui/main.go

# Memory inspection console
go run ./backend/cmd/memory-console/main.go

# All backend tests
GOTOOLCHAIN=local go test ./backend/... -count=1

# Single package / test
go test ./backend/internal/domain/tool/...

# Postgres + Redis via docker (pgvector image)
docker compose -f docker/docker-compose.yml up -d

# Apply DB schema (apply all migrations/*.sql in order). bootstrap.Build's
# idempotent Ensure* auto-migration already covers 002 session_history (+006
# meta_memory column), 003 session_events, 005 session_logs, 001 memory tables
# and dag_jobs, so a fresh database can boot without manual migration; applying
# the files is still recommended for index/extras parity. 004_memory_write_failures.sql
# is a legacy dead-letter table whose step_count/index already live in 001_init.sql.
for f in migrations/*.sql; do psql "$POSTGRES_DSN" -f "$f"; done
```

Flags for `main.go`: `-config config/config.yaml -roles config/roles.yaml -env .env -soul config/soul.md -skills config/skills.yaml`. **Strict startup**: `bootstrap.Build` fails fast — missing config files, unreachable PostgreSQL/Redis, or failed LLM warmup/connectivity check all abort boot with an explicit error. There is no degraded no-DB/no-key mode for the server binary.

## Architecture

### Entry flow
`main.go` only does flag parsing, file logging, and HTTP listen/serve. All wiring is delegated to `bootstrap.Build` (`internal/bootstrap/bootstrap.go`) — the same path used by the TUI and integration tests, so the production binary and tests share one init. Inside `Build`: `config.Load` → `pkgconfig.LoadRoleConfig` → `store.NewPostgresStore` (+ embedding dim / block-memory token cap) → `embed.NewEmbedder` → idempotent `Ensure*Schema` auto-migration (session_history + meta_memory, session_events, session_logs, dag_jobs, 001 memory tables) → `store.NewRedisStore` → `logger.NewWithConfig` (session logger, also injected into stores/agent/server/dag/cmdqueue via `SetLogger`) → `model.NewModelFactory(roleCfg).WarmUp` + `VerifyConnectivity` → skill pool → shared mailbox → `runtime.New` → `domain/role` + `domain/tool` registries → `domain/memory.NewPipeline(NewInMemoryStore())` → `domain/subagent.NewDispatcher` → `agent.NewReactService` (fresh session list by default; `RestoreSessions(50)` only when `agent.restore_sessions: true`) → `server.NewSessionManager(agentSvc)` → optional `dag.NewScheduler` (only when `agent.dag_enabled`) → HTTP `mux` (`bootstrap.NewDefaultMux`) with `/api/sessions/*`, `/api/dag*`, `/api/memory/*`, `/api/skills`, `/api/files*`, `/api/snapshot`, `/api/health|status|metrics`; `main.go` then adds `/assets/`, `/favicon.svg` and the SPA fallback at `/`.

### ReAct execution (`internal/agent/`)
`ReActAgent.RunWithHistory()` drives the main loop (max 50 iterations by default): prepend system prompt → `model.Generate` with tool schema → if the response contains `ToolPart`s, execute each tool via `ToolRegistry.Dispatch` and feed results back → repeat until a plain-text answer is returned. `call_sub_agent` spawns a child `ReActAgent` in a goroutine; the parent polls the shared `Mailbox` for summaries.

Key files: `react_agent.go` (loop), `react_types.go` (DTOs), `react_memory.go` (memory interface), `tool_adapter.go` (domain/tool wrapper), `service_react.go` + `session_react.go` (session lifecycle), `agent.go` (`Agent` facade interface).

### Runtime aggregation (`internal/runtime/runtime.go`)
Single `Runtime` struct holds `board.Manager`, `mailbox.Mailbox`, `skill.Registry`, `soul.Loader`, `watchdog.Watchdog`, `cmdqueue.Manager`. Built once in `bootstrap.Build`. Current wiring reality (post-ReAct): the **Mailbox is live** — it is shared with `agent.NewReactService` and the sub-agent dispatcher, and the ReAct loop drains it each iteration for child-agent summaries. The other components are currently consumed by HTTP/TUI adapter code only: Board → TUI panels + `/api/sessions/{id}/board`; Skill pool → `/api/skills` (pool is loaded, but `AssembleSet`/`Bind` prompt assembly is NOT wired into the ReAct path); Soul → loaded, name exposed via API, not injected into prompts; Watchdog → thresholds exposed via `agent.Query(QueryKindWatchdog)`, not run inside the loop; CmdQueue → enqueue/interrupt HTTP endpoints. See `doc/TODO.md` for the re-integration items.

### Memory
`internal/domain/memory/` implements a two-stage event stream: `Write` appends `MemoryEvent`s per agent, `Assemble` injects recent events as a system-context message before each LLM call. The legacy four-stage pipeline in `internal/memory/` (`compress.go`, `search.go`, `assembler.go`, `snapshot.go`) has been removed; only `write.go` remains for Episode persistence and is no longer on the hot path of the ReAct loop.

### Storage (`internal/store/`)
`postgres_store.go` — composite entry with per-domain sub-stores (Episode/Snapshot/Knowledge/Topic/AgentRegistry/Session), private memory, snapshots, topic metadata, knowledge base CRUD (relies on pgvector for vector search). `redis.go` — workspace Hash / SortedSet / Stream + TTL (snapshot TTL configured via `redis.snapshot_ttl_days`). Bad-row unmarshal failures log at `[ERRO]` via the injected `store.Logger` (narrow interface; `*logger.Logger` satisfies it — `store` cannot import `logger` due to the import cycle, `logger` already depends on `store`).

### Model factory (`internal/model/`)
`factory.go` caches blades `ModelProvider` instances per role. `blades_client.go` wraps `Generate`/`GenerateWithSystem` and exposes `Provider()` for the tool-calling path. Falls back to Mock when no API key — server runs without real LLM for testing.

### Config
- `config/config.yaml` — infra (Postgres DSN, pgvector, Redis, HTTP addr `:10010`, memory intervals). `${VAR:default}` env interpolation.
- `config/roles.yaml` — `meta_agent` (lightweight model recommended), `domain_agent` (shared by Domain/SubDomain), `fixed_roles[]` (per-role model + `can_be_called` + `skills`), `dynamic_templates` (LLM prompts for dynamic role creation).
- `config/skills.yaml` — Skill pool (optional; `skill.BuiltinPool` fallback).
- `config/soul.md` — persona definition (optional; missing file is non-fatal).
- `.env` — `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `POSTGRES_DSN`, `REDIS_ADDR`, `REDIS_PASSWORD`, `HTTP_ADDR`.

### Server / UI (`internal/server/`, `web/`)
`api.go` + `session.go` + `session_http.go` + `stream_http.go` — session CRUD and SSE stream at `/api/sessions/{id}/stream`. `tui_broadcaster.go` mirrors graph events to TUI subscribers. `web/` build output is served by `main.go` at `/assets/` and `/favicon.svg`, with all unmatched paths falling back to `index.html` (SPA history routing).

## Package Dependency Rules

The backend is layered from the bottom up:

- `pkg/*` — shared primitives (`types`, `enums`, `jsonutil`, `textutil`, `httputil`).
- `internal/store`, `internal/memory` (legacy), `internal/model`, `internal/logger`, `internal/logging`, `internal/logoutput`, `internal/embed`, `internal/config`, `internal/plugins`, `internal/computeruse` (placeholder) — infrastructure.
- `internal/dag`, `internal/skill`, `internal/soul`, `internal/watchdog`, `internal/board`, `internal/mailbox`, `internal/cmdqueue` — runtime components.
- `internal/runtime` — runtime aggregator.
- `internal/domain/tool`, `internal/domain/role`, `internal/domain/memory`, `internal/domain/subagent` — domain services.
- `internal/agent` — facade exposing `agent.Agent`.
- `internal/bootstrap` — shared wiring (builds `App` from config paths; used by every entry point).
- `internal/server`, `internal/tui`, `internal/testserver`, `cmd/*`, `main.go`, `test/*` — adapters and entry points.

Key rules:

- `pkg/*` must not import `internal/*`.
- Infrastructure packages must not import `internal/domain/*`, `internal/runtime`, `internal/server`, `internal/tui`, `internal/agent`, or entry points.
- `internal/domain/*` must not import `internal/server`, `internal/tui`, `internal/agent`, or entry points.
- Adapters should consume the system through `agent.Agent` and `internal/bootstrap` rather than directly importing `internal/domain/*` / `internal/runtime` internals.

When a lower-layer package needs a type from an upper layer, move the type to `pkg/types` or invert the dependency with an interface. See `docs/superpowers/plans/dependency-rules.md` for the full rules and verification commands.

## Conventions

- `doc/项目说明.md` is the authoritative implementation guide (directory layout, code reading order, key design decisions). `doc/设计文档_v3.md` is a vision doc whose top carries a status note acknowledging divergence from code — defer to `项目说明.md` on conflict. `doc/TODO.md` tracks progress; `doc/扩展设计_Agent工作流平台.md` is a future-vision doc (workflow platform), not a description of current code. The codebase migrated from CloudWeGo Eino to go-kratos Blades (pre-v3 Eino docs are no longer in the tree).
- Integration tests live under `test/` (separate module, `test/go.mod`): `test/api/*` (HTTP API assertions), `test/coding/*` (snake-game / CSS-refactor / bug-fix end-to-end driven by a mock LLM through the full graph + memory stack), `test/tui/*` (simulated keystream), `test/fixtures/*` (shared fixtures incl. mock LLM with `RegisterSequence`/`RequestPrompts`).
- Per-package `_test.go` files exist for `agent` (react_agent, service_react), `board`, `mailbox`, `skill`, `soul`, `watchdog`, `config`, `model`, `logger`, `domain/tool`, `domain/role`, `domain/memory`, `domain/subagent`, `server` (session), `tui` (helpers) — run them when touching those packages.
