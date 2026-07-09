# Package Dependency Rules

This document defines the allowed dependency directions for the backend
packages after the architecture decoupling tracks. The goal is to keep the
codebase layered: shared primitives at the bottom, infrastructure in the
middle, orchestration above it, and adapters/entry points at the top.

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
│  Orchestration & Runtime                    │
│  internal/graph, internal/runtime,          │
│  internal/dag, internal/skill,              │
│  internal/soul, internal/watchdog,          │
│  internal/board, internal/mailbox           │
├─────────────────────────────────────────────┤
│  Infrastructure                             │
│  internal/store, internal/memory,           │
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
   - They must not depend on `internal/graph`, `internal/runtime`,
     `internal/server`, `internal/tui`, `internal/agent`, or other upper-layer
     orchestration/adapter packages.

3. **`internal/dag`, `internal/skill`, `internal/soul`, `internal/watchdog`,
   `internal/board`, `internal/mailbox` are runtime components.**
   - They may depend on `pkg/*` and infrastructure packages.
   - They must not depend on `internal/graph`, `internal/server`,
     `internal/tui`, `internal/agent`, or `internal/runtime` (runtime is the
     aggregator, not a dependency of its parts).

4. **`internal/runtime` aggregates runtime components.**
   - It may depend on the runtime component packages and infrastructure.
   - It must not depend on `internal/graph`, `internal/server`, `internal/tui`,
     or `internal/agent`.

5. **`internal/graph` is the orchestration layer.**
   - It may depend on `internal/runtime`, `internal/model`, `internal/memory`,
     `internal/store`, `internal/logger`, `pkg/*`, and lower runtime
     components.
   - It must not depend on `internal/server`, `internal/tui`, `internal/agent`,
     or entry-point packages.

6. **`internal/agent` is the facade.**
   - It encapsulates `internal/graph`, `internal/runtime`, and related
     orchestration packages.
   - Adapters (`internal/server`, `internal/tui`, `internal/testserver`,
     `test/*`, `backend/main.go`, `backend/cmd/*`) should consume the agent
     through the `agent.Agent` interface rather than directly importing
     `internal/graph` or `internal/runtime` internals.

7. **Entry points own wiring.**
   - `backend/main.go`, `backend/cmd/*`, and integration tests wire the system
     via `internal/bootstrap`.
   - Bootstrap may import any package needed to construct the app, but it
     should keep upper-layer packages from leaking into lower layers.

## Verified Checks

The following commands confirm the critical dependency directions are clean:

```bash
cd backend
# memory must not depend on graph
GOTOOLCHAIN=local go list -deps ./internal/memory/... | grep internal/graph

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
- Keep business rules (prompts, thresholds, routing keywords) in configuration
  files (`config/`) rather than embedding them in reusable packages.
