# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

BlockMemoryAgent — Go multi-agent orchestration system built on go-kratos Blades. ReAct-loop core (single MetaAgent → `call_sub_agent` recursive dispatch) with event-stream memory, three-tier storage (PostgreSQL + Redis + pgvector), and a bubbletea TUI / HTTP web UI.

Go module: `github.com/blockmemory/agent/backend` (source under `backend/`, root `go.work` points at `./backend`). Go 1.25 (requires `GOTOOLCHAIN=local` on machines whose default Go is older — repo tests use this flag).

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

# Apply DB schema (apply all migrations/*.sql in order). 005 session_logs &
# 006 meta_memory column are NOT covered by BuildHandler's idempotent Ensure*
# auto-migration. 004_memory_write_failures.sql is a legacy dead-letter table
# whose step_count/index already live in 001_init.sql — apply all files in order.
for f in migrations/*.sql; do psql "$POSTGRES_DSN" -f "$f"; done
```

Flags for `main.go`: `-config config/config.yaml -roles config/roles.yaml -env .env -soul config/soul.md -skills config/skills.yaml`. Stores and Skill pool degrade gracefully if unavailable (warnings logged, nil'd) — server still boots without Postgres/Redis/API keys.

## Architecture

### Entry flow
`main.go` only does flag parsing, file logging, and HTTP listen/serve. All wiring is delegated to `bootstrap.Build` (`internal/bootstrap/bootstrap.go`) — the same path used by integration tests, so the production binary and tests share one init. Inside `Build`: `config.Load` → `store.NewPostgresStore`/`NewRedisStore` → idempotent `Ensure*Schema` auto-migration (session_history, session_events, 001 memory tables — but NOT 005 `session_logs` nor 006 `meta_memory`; apply those via migration files) → `pkgconfig.LoadRoleConfig` → `model.NewModelFactory(roleCfg).WarmUp` → `domain/role.NewRegistry` → `domain/tool.NewBuiltinRegistry` + `domain/subagent.NewDispatcher` → `domain/memory.NewPipeline` → `agent.NewReactService` → `server.NewSessionManager(agentSvc)` → HTTP `mux` with `/api/sessions`, `/api/sessions/{id}`, `/api/sessions/{id}/stream`, `/static/`, `/`.

### ReAct execution (`internal/agent/`)
`ReActAgent.RunWithHistory()` drives the main loop (max 50 iterations by default): prepend system prompt → `model.Generate` with tool schema → if the response contains `ToolPart`s, execute each tool via `ToolRegistry.Dispatch` and feed results back → repeat until a plain-text answer is returned. `call_sub_agent` spawns a child `ReActAgent` in a goroutine; the parent polls the shared `Mailbox` for summaries.

Key files: `react_agent.go` (loop), `react_types.go` (DTOs), `react_memory.go` (memory interface), `tool_adapter.go` (domain/tool wrapper), `service_react.go` + `session_react.go` (session lifecycle), `agent.go` (`Agent` facade interface).

### Runtime aggregation (`internal/runtime/runtime.go`)
Single `Runtime` struct holds `board.Manager`, `mailbox.Mailbox`, `skill.Registry`, `soul.Loader`, `watchdog.Watchdog`. Built once in `testserver.BuildHandler`, injected via `ThreeLayerGraphBuilder.SetRuntime`, then transparently propagated to dynamically constructed DomainAgent nodes. Avoids scattering per-component pointers across agent structs. See `doc/项目说明.md` §4.5 for the rationale and the v3 capability gaps this closes (Watchdog main-path activation, Skill library, soul.md + temperature, task board, mailbox).

- **Skill pipeline** (`internal/skill/`): `config/skills.yaml` → `skill.Pool` → `FilterByDomain(domain)` → LLM decides (`Pool.AssembleSet`) → `types.SkillSet` (≤8) → `Registry.Bind(agent)` → only those skill briefs go into the assistant system prompt; tool calls flow through `ToolExecutor`.
- **Task board / mailbox** (`internal/board/`, `internal/mailbox/`): MetaAgent creates a `TaskBoard` in `handleInitial`, top-level subtasks = inferred domain names. Agents `MarkDone`/`MarkFailed`. Cross-agent events go to `Mailbox`; MetaAgent's `processMailbox` fans out per-domain and converts to `EventEscalation` back into the SessionBlock.
- **Watchdog** (`internal/watchdog/`): `MetaAgentNode.Invoke` runs `runWatchdog` at the top of each loop tick — context-length monitoring.

### Memory
`internal/domain/memory/` implements a two-stage event stream: `Write` appends `MemoryEvent`s per agent, `Assemble` injects recent events as a system-context message before each LLM call. The legacy four-stage pipeline in `internal/memory/` (`compress.go`, `search.go`, `assembler.go`, `snapshot.go`) has been removed; only `write.go` remains for Episode persistence and is no longer on the hot path of the ReAct loop.

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

## Package Dependency Rules

The backend is layered from the bottom up:

- `pkg/*` — shared primitives (`types`, `enums`, `jsonutil`, `textutil`, `httputil`).
- `internal/store`, `internal/memory` (legacy), `internal/model`, `internal/logger`, `internal/embed`, `internal/config` — infrastructure.
- `internal/dag`, `internal/skill`, `internal/soul`, `internal/watchdog`, `internal/board`, `internal/mailbox`, `internal/cmdqueue` — runtime components.
- `internal/runtime` — runtime aggregator.
- `internal/domain/tool`, `internal/domain/role`, `internal/domain/memory`, `internal/domain/subagent` — domain services.
- `internal/agent` — facade exposing `agent.Agent`.
- `internal/server`, `internal/tui`, `internal/testserver`, `cmd/*`, `main.go`, `test/*` — adapters and entry points.

Key rules:

- `pkg/*` must not import `internal/*`.
- Infrastructure packages must not import `internal/domain/*`, `internal/runtime`, `internal/server`, `internal/tui`, `internal/agent`, or entry points.
- `internal/domain/*` must not import `internal/server`, `internal/tui`, `internal/agent`, or entry points.
- Adapters should consume the system through `agent.Agent` and `internal/bootstrap` rather than directly importing `internal/domain/*` / `internal/runtime` internals.

When a lower-layer package needs a type from an upper layer, move the type to `pkg/types` or invert the dependency with an interface. See `docs/superpowers/plans/dependency-rules.md` for the full rules and verification commands.

## Conventions

- `doc/项目说明.md` is the authoritative implementation guide (directory layout, code reading order, key design decisions). `doc/设计文档_v3.md` is a vision doc whose top carries a status note acknowledging divergence from code — defer to `项目说明.md` on conflict. `doc/TODO.md` tracks progress; `doc/TUI设计文档.md` covers TUI design. The codebase migrated from CloudWeGo Eino to go-kratos Blades (pre-v3 Eino docs are no longer in the tree).
- Integration tests live under `test/` (separate module, `test/go.mod`): `test/api/*` (HTTP API assertions), `test/coding/*` (snake-game / CSS-refactor / bug-fix end-to-end driven by a mock LLM through the full graph + memory stack), `test/tui/*` (simulated keystream), `test/fixtures/*` (shared fixtures incl. mock LLM with `RegisterSequence`/`RequestPrompts`).
- Per-package `_test.go` files exist for `agent` (react_agent, service_react), `board`, `mailbox`, `skill`, `soul`, `watchdog`, `config`, `model`, `logger`, `domain/tool`, `domain/role`, `domain/memory`, `domain/subagent`, `server` (session), `tui` (helpers) — run them when touching those packages.
