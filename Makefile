.PHONY: backend-test test-test web-build lint up down migrate run help

## Run all backend Go tests (unit + package tests).
backend-test:
	cd backend && GOTOOLCHAIN=local go test ./... -count=1

## Run integration tests under the test module.
test-test:
	GOTOOLCHAIN=local go test -tags=integration ./test/... -count=1

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

## Show available targets.
help:
	@echo "Available targets:"
	@echo "  backend-test  - run backend Go tests"
	@echo "  test-test     - run integration tests"
	@echo "  web-build     - install deps and build the Vue SPA"
	@echo "  lint          - run go vet on backend and test modules"
	@echo "  up            - start docker compose services"
	@echo "  down          - stop docker compose services"
	@echo "  migrate       - apply SQL migrations (requires POSTGRES_DSN)"
	@echo "  run           - build web UI and run backend server"
