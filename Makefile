.PHONY: up down run build test test-integration test-race

up:
	docker compose up -d --build
	@echo "waiting for /livez..."
	@for i in $$(seq 1 30); do curl -fsS localhost:8080/livez && echo && exit 0; sleep 1; done; \
		echo "app did not become live"; docker compose logs app; exit 1

down:
	docker compose down -v

run:
	go run ./cmd/server

build:
	go build -o bin/server ./cmd/server

# Unit tests: no database needed.
test:
	go test ./...

# Concurrency/integration suite against the compose Postgres (fresh DB per run).
test-integration:
	docker compose up -d --wait db
	go test -race -tags integration -count=1 ./test/integration/

# The bar for Phase 3: green under the race detector across many repeats.
test-race:
	docker compose up -d --wait db
	go test -race -tags integration -count=20 ./test/integration/
