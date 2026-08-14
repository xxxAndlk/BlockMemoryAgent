# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

BlockMemoryAgent - Go multi-agent orchestration system built on go-kratos Blades. ReAct-loop core (single MetaAgent -> `call_sub_agent` recursive dispatch) with event-stream memory, three-tier storage (PostgreSQL + Redis + pgvector), and a bubbletea TUI / HTTP web UI.

Go module: `github.com/blockmemory/agent/backend` (source under `backend/`; root `go.work` includes both `./backend` and `./test`). Go 1.25.

## Hard Conventions

- **`GOTOOLCHAIN=local` required** on machines whose default Go is older. All `go test` / `go run` commands in this repo use this flag. Do not omit.
- **Windows bash shell**: use Unix syntax (`/dev/null` not `NUL`, forward slashes in paths).
- **Strict startup**: `bootstrap.Build` fails fast - missing config files, unreachable PostgreSQL/Redis, or failed LLM warmup/connectivity check all abort boot. No degraded no-DB/no-key mode for the server binary.
- **Flags for `main.go`**: `-config config/config.yaml -roles config/roles.yaml -env .env -soul config/soul.md -skills config/skills.yaml`.

## Commands

See `Makefile` (`make help` lists all targets). Key targets: `make run` (build web + run server), `make backend-test`, `make test-test`, `make lint`, `make up`/`down` (docker compose), `make migrate`.

## Documentation Map

- **`doc/项目说明.md`** - authoritative implementation guide (directory layout, code reading order, key design decisions, architecture). Defer to this on conflict.
- **`doc/TODO.md`** - progress tracking and open items.
- **`doc/设计文档_v3.md`** - vision doc, top carries status note acknowledging divergence from code.
- **`doc/扩展设计_Agent工作流平台.md`** - future-vision doc (workflow platform), not current code.
- **`docs/superpowers/plans/dependency-rules.md`** - full package dependency rules and verification commands.
- **`评测文档真伪核查报告.md`** + **`上下文工程改进点.md`** - prompt engineering audit reports.

## Package Layering (quick reference)

Bottom up: `pkg/*` -> infrastructure (`store`/`memory`/`model`/`logger`/`embed`/`config`/...) -> runtime components (`dag`/`skill`/`soul`/`watchdog`/`board`/`mailbox`/`cmdqueue`) -> `runtime` aggregator -> `domain/*` (`tool`/`role`/`memory`/`subagent`/`verifyloop`/`assembly`) -> `agent` facade -> `bootstrap` -> adapters (`server`/`tui`/`testserver`/`cmd/*`/`main.go`/`test/*`).

Key rules: `pkg/*` must not import `internal/*`; infrastructure must not import `domain/*`/`runtime`/`server`/`tui`/`agent`; `domain/*` must not import `server`/`tui`/`agent`; adapters consume via `agent.Agent` + `bootstrap`. Full rules in `docs/superpowers/plans/dependency-rules.md`.

## Tests

- Per-package `_test.go` exist for `agent`, `board`, `mailbox`, `skill`, `soul`, `watchdog`, `config`, `model`, `logger`, `domain/tool`, `domain/role`, `domain/memory`, `domain/subagent`, `server`, `tui` - run when touching those packages.
- Integration tests under `test/` (separate module): `test/api/*`, `test/coding/*` (e2e via mock LLM), `test/tui/*`, `test/fixtures/*`. Fixtures use `docker/docker-compose.test.yml` (isolated ports PG 55432 / Redis 56380) — never point tests at the dev instance (5432/6380, VECTOR(1024)).
- Eval suite under `test/eval/` (build tag `eval`, real LLM, costs API credits): scenario YAMLs + deterministic checkpoint judges + metrics, reports to `test/eval/runs/<ts>/report.{json,md}`. Dry-run: `cd test && EVAL_DRY_RUN=1 GOTOOLCHAIN=local go test -tags=eval ./eval/ -run TestEval -v`. Knobs: `EVAL_FILTER`, `EVAL_RUNS`, `JUDGE_*` for llm_judge checks.

## History

Codebase migrated from CloudWeGo Eino to go-kratos Blades (pre-v3 Eino docs no longer in tree).
