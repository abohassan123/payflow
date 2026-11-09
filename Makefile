# PayFlow developer commands. Run `make help` to list them.
# Requires: docker (with compose), go, make. Run from the repo root inside WSL.

SHELL := /bin/bash
-include .env   # the leading dash: don't fail if .env doesn't exist yet
export

SIM_DIR := services/simulator
TEST_PG := payflow-pg-test
TEST_PG_PORT := 55432

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

# ---------------------------------------------------------------------------
# Stack
# ---------------------------------------------------------------------------
.PHONY: up down reset ps logs
up: ## Build images and start the stack in the background
	docker compose up -d --build

down: ## Stop the stack (keeps data)
	docker compose down

reset: ## Stop the stack AND delete all data volumes (fresh start)
	docker compose down -v

ps: ## Show container status and health
	docker compose ps

logs: ## Follow logs of all services (Ctrl+C to stop)
	docker compose logs -f --tail=100

# ---------------------------------------------------------------------------
# Database
# ---------------------------------------------------------------------------
.PHONY: psql psql-app validate
psql: ## Open psql as the admin user
	docker compose exec postgres psql -U $(POSTGRES_USER) -d $(POSTGRES_DB)

psql-app: ## Open psql as the least-privilege app user
	docker compose exec -e PGPASSWORD=$(APP_DB_PASSWORD) postgres psql -h 127.0.0.1 -U $(APP_DB_USER) -d $(POSTGRES_DB)

validate: ## Run the Phase 1 validation queries
	docker compose exec postgres psql -U $(POSTGRES_USER) -d $(POSTGRES_DB) -f /queries/validate_phase1.sql

# ---------------------------------------------------------------------------
# Simulator (Go)
# ---------------------------------------------------------------------------
.PHONY: sim-tidy sim-fmt sim-vet sim-test sim-test-integration sim-metrics
sim-tidy: ## Download Go dependencies and write go.sum
	cd $(SIM_DIR) && go mod tidy

sim-fmt: ## Format Go code
	cd $(SIM_DIR) && gofmt -w .

sim-vet: ## Static checks
	cd $(SIM_DIR) && go vet ./... && go vet -tags integration ./...

sim-test: ## Unit tests (no database needed), with race detector
	cd $(SIM_DIR) && go test -race -count=1 -cover ./...

sim-test-integration: ## Integration tests against a throwaway Postgres container
	@docker rm -f $(TEST_PG) >/dev/null 2>&1 || true
	docker run -d --rm --name $(TEST_PG) -p $(TEST_PG_PORT):5432 \
	  -e POSTGRES_DB=payflow -e POSTGRES_USER=payflow_admin -e POSTGRES_PASSWORD=test \
	  -e APP_DB_USER=payflow_app -e APP_DB_PASSWORD=test \
	  -e READONLY_DB_USER=payflow_readonly -e READONLY_DB_PASSWORD=test \
	  -v $(CURDIR)/infra/postgres/init:/docker-entrypoint-initdb.d:ro postgres:17 >/dev/null
	@echo "waiting for test database..."
	@until docker exec $(TEST_PG) pg_isready -h 127.0.0.1 -U payflow_admin -d payflow >/dev/null 2>&1; do sleep 1; done
	cd $(SIM_DIR) && TEST_DATABASE_URL="postgres://payflow_app:test@localhost:$(TEST_PG_PORT)/payflow?sslmode=disable" \
	  go test -tags integration -race -count=1 -v ./internal/store/; \
	  status=$$?; docker rm -f $(TEST_PG) >/dev/null; exit $$status

sim-metrics: ## Show the simulator's business metrics
	@curl -s localhost:2112/metrics | grep '^payflow_sim' | grep -v _bucket
