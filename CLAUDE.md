# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

BlockMemoryAgent — Go multi-agent orchestration system built on go-kratos Blades. Four-layer agent hierarchy (MetaAgent → DomainAgent → SubDomainAgent → Assistant) with block-scoped private memory, three-tier storage (PostgreSQL + Redis + pgvector), and a bubbletea TUI / HTTP web UI.

Go module: `github.com/blockmemory/agent/backend` (source under `backend/`, root `go.work` points at `./backend`). Go 1.25 (requires `GOTOOLCHAIN=local` on machines whose default Go is older — repo tests use this flag).

## Common commands

```bash
# Run HTTP server (main entrypoint — delegates all wiring to testserver.BuildHandler)
go run ./backend

# Run CLI demo (three-layer flow, prints role instances created)
go run ./backend/cmd/demo/main.go

# Run bubbletea TUI
go run ./backend/cmd/tui/main.go

# Memory inspection console
go run ./backend/cmd/memory-console/main.go

# All tests
GOTOOLCHAIN=local go test ./backend/... -count=1

# Single package / test
go test ./backend/internal/board/...

# Postgres + Redis via docker (pgvector image)
docker compose -f docker/docker-compose.yml up -d

# Apply DB schema (migrations 001-006). 005 session_logs & 006 meta_memory column
# are NOT covered by BuildHandler's idempotent Ensure* auto-migration — apply all files.
for f in migrations/*.sql; do psql "$POSTGRES_DSN" -f "$f"; done
```

Flags for `main.go`: `-config config/config.yaml -roles config/roles.yaml -env .env -soul config/soul.md -skills config/skills.yaml`. Stores and Skill pool degrade gracefully if unavailable (warnings logged, nil'd) — server still boots without Postgres/Redis/API keys.

## Architecture

### Entry flow
`main.go` only does flag parsing, file logging, and HTTP listen/serve. All wiring is delegated to `testserver.BuildHandler` (`internal/testserver/testserver.go`) — the same path used by integration tests, so the production binary and tests share one init. Inside `BuildHandler`: `config.Load` → `store.NewPostgresStore`/`NewRedisStore` → idempotent `Ensure*Schema` auto-migration (session_history, session_events, 001 memory tables — but NOT 005 `session_logs` nor 006 `meta_memory`; apply those via migration files) → `pkgconfig.LoadRoleConfig` → `model.NewModelFactory(roleCfg).WarmUp` → `graph.NewRoleRegistry`+`NewRoleFactory` → `skill.LoadFromYAML` (fallback `skill.BuiltinPool`) → `runtime.New(soulPath, skillPool)` (boards + mailbox + skills + soul + watchdog) → `graph.NewThreeLayerGraphBuilder` injects `ModelFactory` + `Runtime` → `server.NewSessionManager(graph, registry)` → HTTP `mux` with `/api/sessions`, `/api/sessions/{id}`, `/api/sessions/{id}/stream`, `/static/`, `/`.

### Graph execution (`internal/graph/`)
`ThreeLayerGraph.Invoke()` drives a state-machine loop (max 200 steps). Each `ThreeLayerNode` returns `*types.ThreeLayerState` with `state.NextAction` ∈ {Continue, Switch, Escalate, Finish} controlling flow. `CallStack` implements nested DomainAgent→SubDomainAgent→Assistant calls. `SessionBlock` isolates per-domain context. Terminal nodes: `EscalationHandlerNode` (escalation arbitration) and `sinkerNode` (forces Finish).

Node files: `meta_agent.go`, `domain_agent.go`, `subdomain_agent.go`, `assistant.go`, `escalation.go`, `three_layer_graph.go`, `role_registry.go`, `role_factory.go`, `llm_tools.go`+`tool_executor.go` (tool-calling loop), `util.go`.

### Runtime aggregation (`internal/runtime/runtime.go`)
Single `Runtime` struct holds `board.Manager`, `mailbox.Mailbox`, `skill.Registry`, `soul.Loader`, `watchdog.Watchdog`. Built once in `testserver.BuildHandler`, injected via `ThreeLayerGraphBuilder.SetRuntime`, then transparently propagated to dynamically constructed DomainAgent nodes. Avoids scattering per-component pointers across agent structs. See `doc/项目说明.md` §4.5 for the rationale and the v3 capability gaps this closes (Watchdog main-path activation, Skill library, soul.md + temperature, task board, mailbox).

- **Skill pipeline** (`internal/skill/`): `config/skills.yaml` → `skill.Pool` → `FilterByDomain(domain)` → LLM decides (`Pool.AssembleSet`) → `types.SkillSet` (≤8) → `Registry.Bind(agent)` → only those skill briefs go into the assistant system prompt; tool calls flow through `ToolExecutor`.
- **Task board / mailbox** (`internal/board/`, `internal/mailbox/`): MetaAgent creates a `TaskBoard` in `handleInitial`, top-level subtasks = inferred domain names. Agents `MarkDone`/`MarkFailed`. Cross-agent events go to `Mailbox`; MetaAgent's `processMailbox` fans out per-domain and converts to `EventEscalation` back into the SessionBlock.
- **Watchdog** (`internal/watchdog/`): `MetaAgentNode.Invoke` runs `runWatchdog` at the top of each loop tick — context-length monitoring.

### Memory (`internal/memory/`)
Four-stage pipeline: `write.go` (importance scoring + topic binding) → `compress.go` (4 levels: Raw → Standard → Compact → Marker) → `search.go` (multi-signal relevance: pgvector semantic + entity overlap + causal chain + time decay) → `assembler.go` (4-segment context assembly with TokenBudget: System/TopicGlobal/SharedState/PrivateMemory). `snapshot.go` does Redis hot-load + Postgres persistence.

### Storage (`internal/store/`)
`postgres.go` — private memory, snapshots, topic metadata, knowledge base CRUD (relies on pgvector for vector search). `redis.go` — workspace Hash / SortedSet / Stream + TTL (snapshot TTL configured via `memory.snapshot_ttl_days`).

### Model factory (`internal/model/`)
`factory.go` caches blades `ModelProvider` instances per role. `blades_client.go` wraps `Generate`/`GenerateWithSystem` and exposes `Provider()` for the tool-calling path. Falls back to Mock when no API key — server runs without real LLM for testing.

### Config
- `config/config.yaml` — infra (Postgres DSN, pgvector, Redis, HTTP addr `:10010`, memory intervals). `${VAR:default}` env interpolation.
- `config/roles.yaml` — `meta_agent` (lightweight model recommended), `domain_agent` (shared by Domain/SubDomain), `fixed_roles[]` (per-role model + `can_be_called` + `skills`), `dynamic_templates` (LLM prompts for dynamic role creation).
- `config/skills.yaml` — Skill pool (optional; `skill.BuiltinPool` fallback).
- `config/soul.md` — persona definition (optional; missing file is non-fatal).
- `.env` — `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `POSTGRES_DSN`, `REDIS_ADDR`, `REDIS_PASSWORD`, `HTTP_ADDR`.

### Server / UI (`internal/server/`, `web/`)
`api.go` + `session.go` — session CRUD and SSE stream at `/api/sessions/{id}/stream`. `tui_broadcaster.go` mirrors graph events to TUI subscribers. `web/` static files served at `/static/`, `index.html` at `/`.

## Conventions

- `doc/项目说明.md` is the authoritative implementation guide (directory layout, code reading order, key design decisions). `doc/设计文档_v3.md` is a vision doc whose top carries a status note acknowledging divergence from code — defer to `项目说明.md` on conflict. `doc/TODO.md` tracks progress; `doc/TUI设计文档.md` covers TUI design. The codebase migrated from CloudWeGo Eino to go-kratos Blades (pre-v3 Eino docs are no longer in the tree).
- Integration tests live under `test/` (separate module, `test/go.mod`): `test/api/*` (HTTP API assertions), `test/coding/*` (snake-game / CSS-refactor / bug-fix end-to-end driven by a mock LLM through the full graph + memory stack), `test/tui/*` (simulated keystream), `test/fixtures/*` (shared fixtures incl. mock LLM with `RegisterSequence`/`RequestPrompts`).
- Per-package `_test.go` files exist for `board`, `mailbox`, `skill`, `soul`, `watchdog`, `config`, `model`, `logger`, `memory` (block_vector, callback), `server` (session_logs), `tui` (helpers), `graph` (agent_common, llm_tools, meta_watchdog, plan, router, runtime_wiring) — run them when touching those packages.
