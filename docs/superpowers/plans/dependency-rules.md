# Package Dependency Rules

This document defines the allowed dependency directions for the backend
packages after the ReAct-loop architecture refactor. The goal is to keep the
codebase layered: shared primitives at the bottom, infrastructure in the
middle, domain services above it, and adapters/entry points at the top.

## Layer Overview

```
┌─────────────────────────────────────────────┐
│  Entry / Adapters                           │
│  backend/main.go, backend/cmd/*,            │
│  internal/server, internal/tui,             │
│  internal/testserver, test/*                 │
├─────────────────────────────────────────────┤
│  Agent Facade                               │
│  internal/agent                              │
├─────────────────────────────────────────────┤
│  Domain Services                            │
│  internal/domain/tool, internal/domain/role,│
│  internal/domain/memory,                    │
│  internal/domain/subagent                   │
├─────────────────────────────────────────────┤
│  Runtime Components                         │
│  internal/runtime, internal/dag,            │
│  internal/skill, internal/soul,             │
│  internal/watchdog, internal/board,         │
│  internal/mailbox, internal/cmdqueue        │
├─────────────────────────────────────────────┤
│  Infrastructure                             │
│  internal/store, internal/memory (legacy),  │
│  internal/model, internal/logger,           │
│  internal/embed, internal/config            │
├─────────────────────────────────────────────┤
│  Shared Primitives                          │
│  pkg/types, pkg/config, pkg/enums,          │
│  pkg/jsonutil, pkg/textutil, pkg/httputil   │
└─────────────────────────────────────────────┘
```

## Rules

1. **`pkg/*` is the bottom layer.**
   - Packages under `pkg/` may only import other `pkg/*` packages or the
     standard library.
   - They must not import any `internal/*` package.

2. **`internal/store`, `internal/memory`, `internal/model`, `internal/logger`,
   `internal/embed`, `internal/config` are infrastructure.**
   - They may depend on `pkg/*` and each other as needed for wiring.
   - They must not depend on `internal/domain/*`, `internal/runtime`,
     `internal/server`, `internal/tui`, `internal/agent`, or other upper-layer
     packages.

3. **`internal/domain/*` are domain services used by the ReAct engine.**
   - They may depend on `pkg/*`, infrastructure, and lower runtime component
     packages (e.g. `mailbox`).
   - They must not depend on `internal/server`, `internal/tui`, `internal/agent`,
     or entry-point packages.

4. **`internal/dag`, `internal/skill`, `internal/soul`, `internal/watchdog`,
   `internal/board`, `internal/mailbox`, `internal/cmdqueue` are runtime
   components.**
   - They may depend on `pkg/*` and infrastructure packages.
   - They must not depend on `internal/domain/*`, `internal/server`,
     `internal/tui`, `internal/agent`, or `internal/runtime` (runtime is the
     aggregator, not a dependency of its parts).

5. **`internal/runtime` aggregates runtime components.**
   - It may depend on the runtime component packages and infrastructure.
   - It must not depend on `internal/domain/*`, `internal/server`,
     `internal/tui`, or `internal/agent`.

6. **`internal/agent` is the facade.**
   - It encapsulates the ReAct loop, domain services, and runtime.
   - Adapters (`internal/server`, `internal/tui`, `internal/testserver`,
     `test/*`, `backend/main.go`, `backend/cmd/*`) should consume the agent
     through the `agent.Agent` interface rather than directly importing
     `internal/domain/*` or `internal/runtime` internals.

7. **Entry points own wiring.**
   - `backend/main.go`, `backend/cmd/*`, and integration tests wire the system
     via `internal/bootstrap`.
   - Bootstrap may import any package needed to construct the app, but it
     should keep upper-layer packages from leaking into lower layers.

## Verified Checks

The following commands confirm the critical dependency directions are clean:

```bash
cd backend
# domain/memory must not depend on agent
GOTOOLCHAIN=local go list -deps ./internal/domain/memory/... | grep internal/agent

# store must not depend on dag
GOTOOLCHAIN=local go list -deps ./internal/store/... | grep internal/dag
```

Both must produce no output. If a command ever prints a path, move the shared
(type or interface) to `pkg/types` or invert the dependency with an interface.

## Practical Guidance

- When two infrastructure packages need to share a type, prefer placing it in
  `pkg/types` (e.g. `types.DAG`, `types.Episode`).
- When an upper layer needs a lower-layer behavior, define a small interface in
  the upper layer and inject the implementation at wiring time (e.g.
  `dag.Store`, `dag.SessionLauncher`).
- Keep business rules (thresholds, routing keywords) in configuration files
  (`config/`) rather than embedding them in reusable packages.
- Exception — prompts: all role system prompts / dynamic templates live as Go
  constants in `pkg/prompts` (compiled into the binary, not user-editable in
  the config dir). `config.LoadRoleConfig` fills the `SystemPrompt` /
  `PromptTemplate` fields from that package at startup; unknown role IDs fail
  fast. Thresholds and routing keywords remain in `config/`.
