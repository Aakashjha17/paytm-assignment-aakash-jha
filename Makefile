.PHONY: up down run build test test-integration test-race burst watch

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

# On-sale stampede + reconciliation. BASE_URL defaults to local compose.
#   make burst
#   make burst BASE_URL=https://<app>.up.railway.app ADMIN_API_KEY=<live key>
BASE_URL ?= http://localhost:8080
burst:
	./burst.sh $(BASE_URL) $(BURST_FLAGS)

# Live view of the key metrics, refreshed every second (for screen recording
# alongside the logs during a burst).  make watch BASE_URL=https://...
watch:
	@while true; do clear; date; curl -s $(BASE_URL)/metrics | grep -E '^(reservations_|reservation_cancellations|seats_(available|held|confirmed|total)|audit_mismatches|db_tx_retries|db_pool_(acquired|max|empty)|http_requests_in_flight|app_ready)'; sleep 1; done
