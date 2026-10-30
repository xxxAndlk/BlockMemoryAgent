.PHONY: backend-test test-test test-compile web-build lint up down migrate run bma-plugin bma-plugin-clean plugins-build dist help

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

## Start PostgreSQL + Redis + open_design via docker compose.
up:
	docker compose -f docker/docker-compose.yml up -d

## Stop PostgreSQL + Redis + open_design via docker compose.
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

## One command to ready all plugin docker deps: build local MCP bridge images.
bma-plugin:
	bash docker/bma-plugin.sh

## Clean docker leftovers: stopped bma-plugin-* containers + dangling images (same as backend startup reap).
bma-plugin-clean:
	bash docker/bma-plugin.sh clean

## Build plugin images: local MCP bridges (bma/*:local). open_design daemon 由 compose 统一管理（up 时自动构建）。
plugins-build:
	docker build -t bma/computer-use-mcp:local docker/computer-use
	docker build -t bma/open-design-mcp:local docker/open-design-mcp
	docker build -t bma/ui-preview-mcp:local docker/ui-preview-mcp

## Build dist/ install layout: dist/{bin,config,web/dist,plugins.d}. Then run install.ps1 to install. PowerShell 无 make 时用 .\dist.ps1。
dist: web-build
	cd backend && GOTOOLCHAIN=local go build -o ../dist/bin/bma-server.exe . && GOTOOLCHAIN=local go build -o ../dist/bin/tui.exe ./cmd/tui
	mkdir -p dist/config
	cp config/*.yaml config/soul.md config/user_profile.md dist/config/
	rm -rf dist/config/skills_learned && cp -r config/skills_learned dist/config/
	rm -rf dist/web && mkdir -p dist/web
	cp -r web/dist dist/web/dist
	mkdir -p dist/plugins.d
	@echo "dist/ 布局完成,运行 install.ps1 安装"

## Show available targets.
help:
	@echo "Available targets:"
	@echo "  backend-test  - run backend Go tests"
	@echo "  test-test     - run integration tests (requires Postgres/Redis/MockLLM env)"
	@echo "  test-compile  - compile integration tests without running"
	@echo "  web-build     - install deps and build the Vue SPA"
	@echo "  lint          - run go vet on backend and test modules"
	@echo "  up            - start docker compose services (postgres / redis / open_design)"
	@echo "  down          - stop docker compose services"
	@echo "  migrate       - apply SQL migrations (requires POSTGRES_DSN)"
	@echo "  run           - build web UI and run backend server"
	@echo "  bma-plugin    - one command: build all plugin MCP bridge images"
	@echo "  bma-plugin-clean - clean leftover plugin containers + dangling images"
	@echo "  plugins-build - build plugin MCP bridge images (computer_use / ui_design / ui_preview)"
	@echo "  dist          - build dist/ install layout (bin/config/web/dist/plugins.d)"
	@echo "  note: TUI 视频附件默认 native 直传（mp4/avi/mov ≤ native_max_mb 无需 ffmpeg）；webm/mkv/超限回落抽帧需宿主机 ffmpeg（winget install Gyan.FFmpeg），缺失时降级为仅元数据"
