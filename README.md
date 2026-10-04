# seat-reservation

A seat-reservation API for high-contention on-sales, written in Go with Postgres.

> Work in progress: reserve/cancel and observability are in; the burst script comes next. Design: see `WRITEUP.md`.

## API so far

| Method | Path | Auth | Notes |
|---|---|---|---|
| GET | `/livez` | none | process is up |
| GET | `/readyz` | none | 200 only when DB reachable **and** migrations done, else 503 |
| POST | `/tokens` | none | `{"user_id":"alice"}` → signed JWT (stand-in for an IdP) |
| POST | `/shows` | `X-Admin-Key` | `{"name","total_seats","price_paise", "seats_per_row"?, "per_user_limit"?, "hold_ttl_seconds"?}` |
| GET | `/shows/{id}` | none | every seat's status + `counts` (available + held + confirmed == total_seats) |
| POST | `/shows/{id}/reservations` | Bearer | header `Idempotency-Key`, body `{"seats":["A1","A2"]}` → 201 new, 200 replay, 409 `seat_taken` / `per_user_limit` / `idempotency_key_reused`, 422 `seat_not_found` |
| DELETE | `/reservations/{id}` | Bearer | owner only (others get 404) → 200 `cancelled` (repeatable), 409 `reservation_expired` |

| GET | `/metrics` | none | Prometheus metrics (below) |

Every response carries `X-Request-ID` (an incoming one is reused); each request logs exactly one JSON line with the same ID.

## Run locally

```sh
make up     # Postgres + app via docker compose, waits until /livez responds
make down   # stop everything and drop the DB volume
```

Without Docker: `cp .env.example .env`, export it, then `make run`. `JWT_SECRET` and `ADMIN_API_KEY` are required (>= 16 chars) — the server refuses to start without them. The app listens on `PORT` (default 8080).

## Metrics (`GET /metrics`)

| Metric | Type | Meaning |
|---|---|---|
| `reservations_confirmed_total` | counter | reserve → 201 (new hold) |
| `reservations_declined_total{reason}` | counter | every other reserve response: `seat-taken`, `per-user-limit`, `idempotent-replay`, `idempotency-conflict`, `seat-not-found`, `unauthenticated`, `missing-idempotency-key`, … Exactly one of these two counters moves per reserve response. |
| `reservation_cancellations_total{outcome}` | counter | cancel responses |
| `seats_available` / `seats_held` / `seats_confirmed` / `seats_total` `{show_id}` | gauge | read from the DB at scrape time, same expiry rule as `GET /shows`, so they always match it |
| `audit_mismatches{check}` | gauge | rows breaking each invariant at the last audit (every `AUDIT_INTERVAL`); must be 0 |
| `db_tx_retries_total` | counter | transactions retried after deadlock/serialization failure; should stay 0 |
| `db_pool_*` | gauge/counter | connection pool: in use, idle, waits |
| `http_requests_total{route,code}`, `http_request_duration_seconds{route}`, `http_requests_in_flight` | | per-route traffic and latency |
| `app_ready` | gauge | 1 when migrated and not draining |

Reconciliation in PromQL: `seats_available + seats_held + seats_confirmed == seats_total`.

## Shutdown

On SIGTERM: `/readyz` → 503 (requests still served for `DRAIN_DELAY`), then the listener closes and in-flight requests finish (up to `SHUTDOWN_TIMEOUT`), then the DB pool closes. Each step is logged as `shutdown 1/4` … `4/4`.

## Tests

```sh
make test               # unit tests, no DB
make test-integration   # concurrency suite vs compose Postgres, race detector
make test-race          # same, 20 repeats
```

The compose Postgres is published on host port **55432** (so it doesn't clash with a local Postgres on 5432).

## Deploy

Railway builds from the `Dockerfile` (see `railway.json`) and runs one replica, with Postgres in the same region.

Live URL: _TBD_

## Burst test

`./burst.sh <BASE_URL>`: _TBD_
