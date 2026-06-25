# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

BlockMemoryAgent — Go multi-agent orchestration system built on go-kratos Blades. Four-layer agent hierarchy (MetaAgent → DomainAgent → SubDomainAgent → Assistant) with block-scoped private memory, three-tier storage (PostgreSQL + Redis + pgvector), and a bubbletea TUI / HTTP web UI.

Go module: `github.com/blockmemory/agent/backend` (source under `backend/`, root `go.work` points at `./backend`). Go 1.25 (requires `GOTOOLCHAIN=local` on machines whose default Go is older — repo tests use this flag).

## Common commands

```bash
# Run HTTP server (main entrypoint — wires graph, runtime, session manager, /api/sessions + /static)
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

# Apply DB schema
psql "$POSTGRES_DSN" -f migrations/001_init.sql
```

Flags for `main.go`: `-config config/config.yaml -roles config/roles.yaml -env .env -soul config/soul.md -skills config/skills.yaml`. Stores and Skill pool degrade gracefully if unavailable (warnings logged, nil'd) — server still boots without Postgres/Redis/API keys.

## Architecture

### Entry flow (main.go)
`config.Load` → `store.NewPostgresStore`/`NewRedisStore` → `pkgconfig.LoadRoleConfig` → `model.NewModelFactory(roleCfg).WarmUp` → `graph.NewRoleRegistry`+`NewRoleFactory` → `skill.LoadFromYAML` (fallback `skill.BuiltinPool`) → `runtime.New(soulPath, skillPool)` (boards + mailbox + skills + soul + watchdog) → `graph.NewThreeLayerGraphBuilder` injects `ModelFactory` + `Runtime` → `server.NewSessionManager(graph, registry)` → HTTP `mux` with `/api/sessions`, `/api/sessions/{id}`, `/api/sessions/{id}/stream`, `/static/`, `/`.

### Graph execution (`internal/graph/`)
`ThreeLayerGraph.Invoke()` drives a state-machine loop (max 200 steps). Each `ThreeLayerNode` returns `*types.ThreeLayerState` with `state.NextAction` ∈ {Continue, Switch, Escalate, Finish} controlling flow. `CallStack` implements nested DomainAgent→SubDomainAgent→Assistant calls. `SessionBlock` isolates per-domain context. Terminal nodes: `EscalationHandlerNode` (escalation arbitration) and `sinkerNode` (forces Finish).

Node files: `meta_agent.go`, `domain_agent.go`, `subdomain_agent.go`, `assistant.go`, `escalation.go`, `three_layer_graph.go`, `role_registry.go`, `role_factory.go`, `llm_tools.go`+`tool_executor.go` (tool-calling loop), `util.go`.

### Runtime aggregation (`internal/runtime/runtime.go`)
Single `Runtime` struct holds `board.Manager`, `mailbox.Mailbox`, `skill.Registry`, `soul.Loader`, `watchdog.Watchdog`. Built once in `main.go`, injected via `ThreeLayerGraphBuilder.SetRuntime`, then transparently propagated to dynamically constructed DomainAgent nodes. Avoids scattering per-component pointers across agent structs. See `doc/DEVELOPMENT_LOG_v3.md` §2 for the rationale and the v3 capability gaps this closes (Watchdog main-path activation, Skill library, soul.md + temperature, task board, mailbox, AIOps tests).

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

- v3 design doc is the source of truth for in-progress capabilities: `doc/设计文档_v3.md`, with progress log in `doc/DEVELOPMENT_LOG_v3.md` and test results in `doc/RESULT_v3.md`. Older `doc/设计文档.md` and `doc/设计文档_v2_Eino版.md` document the pre-v3 (Eino-based) architecture; the codebase has since migrated to go-kratos Blades.
- AIOps scenarios (`test/aiopstest/scenario_test.go`) are integration tests covering alert storm root-cause, playbook approval, chaos drill rollback, postmortem + knowledge — they exercise the full graph + memory stack. `test/aiopsmock/` holds mock fixtures.
- Per-package `_test.go` files exist for `board`, `mailbox`, `skill`, `soul`, `watchdog`, `graph/runtime_wiring_test.go` — run them when touching those packages.
