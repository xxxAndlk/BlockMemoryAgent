.PHONY: backend-test test-test test-compile web-build lint up down migrate run bma-plugin plugins-up plugins-down plugins-build help

## Run all backend Go tests (unit + package tests).
backend-test:
	cd backend && GOTOOLCHAIN=local go test ./... -count=1

## Run integration tests under the test module (requires Postgres/Redis/MockLLM env).
test-test:
	GOTOOLCHAIN=local go test -tags=integration ./test/... -count=1

## Compile integration test modules without running them.
test-compile:
	GOTOOLCHAIN=local go test -tags=integration ./test/... -run=^$$ -count=1

## Install web dependencies and build the Vue SPA.
web-build:
	cd web && npm install && npm run build

## Run go vet on backend and test modules.
lint:
	GOTOOLCHAIN=local go vet ./backend/... ./test/...

## Start PostgreSQL + Redis via docker compose.
up:
	docker compose -f docker/docker-compose.yml up -d

## Stop PostgreSQL + Redis via docker compose.
down:
	docker compose -f docker/docker-compose.yml down

## Apply SQL migrations in order using psql. Requires POSTGRES_DSN.
migrate:
	@if [ -z "$(POSTGRES_DSN)" ]; then \
		echo "Error: POSTGRES_DSN is not set"; \
		exit 1; \
	fi
	@for f in migrations/*.sql; do \
		echo "Applying $$f..."; \
		psql "$(POSTGRES_DSN)" -f "$$f"; \
	done

## Build the web UI and run the backend HTTP server.
run: web-build
	go run ./backend

## One command to ready all plugin docker deps: build/pull images + Firecrawl stack up.
bma-plugin:
	bash docker/bma-plugin.sh

## Start the self-hosted Firecrawl stack (web_search plugin data plane, :3002).
plugins-up:
	docker compose -f docker/docker-compose.firecrawl.yml up -d

## Stop the self-hosted Firecrawl stack.
plugins-down:
	docker compose -f docker/docker-compose.firecrawl.yml down

## Build/pull plugin images: 4x local MCP bridges (bma/*:local) + open_design (ghcr pull).
plugins-build:
	docker build -t bma/firecrawl-mcp:local docker/firecrawl-mcp
	docker build -t bma/computer-use-mcp:local docker/computer-use
	docker build -t bma/open-design-mcp:local docker/open-design-mcp
	docker build -t bma/ui-preview-mcp:local docker/ui-preview-mcp
	docker pull ghcr.io/nexu-io/od:latest

## Show available targets.
help:
	@echo "Available targets:"
	@echo "  backend-test  - run backend Go tests"
	@echo "  test-test     - run integration tests (requires Postgres/Redis/MockLLM env)"
	@echo "  test-compile  - compile integration tests without running"
	@echo "  web-build     - install deps and build the Vue SPA"
	@echo "  lint          - run go vet on backend and test modules"
	@echo "  up            - start docker compose services"
	@echo "  down          - stop docker compose services"
	@echo "  migrate       - apply SQL migrations (requires POSTGRES_DSN)"
	@echo "  run           - build web UI and run backend server"
	@echo "  bma-plugin    - one command: build/pull all plugin images + Firecrawl stack up"
	@echo "  plugins-up    - start self-hosted Firecrawl stack (web_search data plane)"
	@echo "  plugins-down  - stop self-hosted Firecrawl stack"
	@echo "  plugins-build - build/pull plugin images (web_search / computer_use / open_design / ui_design / ui_preview)"
	@echo "  note: TUI 视频附件默认 native 直传（mp4/avi/mov ≤ native_max_mb 无需 ffmpeg）；webm/mkv/超限回落抽帧需宿主机 ffmpeg（winget install Gyan.FFmpeg），缺失时降级为仅元数据"
