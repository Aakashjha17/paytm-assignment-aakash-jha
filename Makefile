.PHONY: up down run build

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
