# seat-reservation

A seat-reservation API for high-contention on-sales, written in Go with Postgres.

> Design and reasoning: see `WRITEUP.md`.

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
| `reservations_declined_total{reason}` | counter | every other reserve response: `seat-taken`, `per-user-limit`, `idempotent-replay`, `idempotency-key-reused`, `seat-not-found`, `unauthenticated`, `missing-idempotency-key`, … Exactly one of these two counters moves per reserve response. |
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

## Burst test (one command)

```sh
./burst.sh http://localhost:8080                                  # local (admin key read from .env)
ADMIN_API_KEY=<live key> ./burst.sh https://<app>.up.railway.app  # live
make burst BASE_URL=... BURST_FLAGS="-requests 5000 -concurrency 300"
```

Needs Go 1.24+. It creates a fresh show (5,000 seats, limit 4), mints ~11k user tokens, then fires 20,000 reserve requests at concurrency 1,000:

| share | kind | what it proves |
|---|---|---|
| 40% | hot-seat storm: one request per user at 5 hot seats | exactly one 201 per hot seat |
| 10% | exact retries of hot requests (same user/key/seats), concurrent with the original | idempotent replay moves nothing |
| 37% | cold traffic, 1–3 seats, 20% of winners cancel | multi-seat all-or-nothing, cancel under load |
| 5% | users firing 10 parallel reserves at limit 4 | per-user limit under concurrency |
| 4% | same key, two different seat sets, concurrently | 409 on key reuse |
| 4% | body claims `user_id` of another user | identity comes from the token |

While it runs it polls `GET /shows` (every 250 ms) and `/metrics` (every 1 s) for the invariant. Afterwards it reconciles the final API counts against the clients' own wins − cancels, the seat gauges against the API, the outcome counters against what clients received (reason by reason), checks `db_tx_retries_total` didn't move, and waits for a post-burst audit with zero mismatches. **Exit code 0 only if every check passes.** Metric reconciliation assumes one replica and no other traffic during the run.

`make watch BASE_URL=...` prints the key metrics every second (handy to record next to the logs).

### A passing run (local compose, Apple Silicon laptop)

```
== burst against http://localhost:8080 — show 7 (5000 seats, limit 4), run c4b0c57c, seed 14803961708237827859
20000 reserve + 378 cancel requests in 6.806s (2994 req/s), concurrency 1000
latency: p50 335ms  p95 374ms  p99 400ms  max 433ms

outcomes (reserve), by what the client received:
  confirmed                       3052
  idempotency-key-reused           192
  idempotent-replay                  4
  per-user-limit                   923
  seat-taken                     15829
  5xx                                0

by workload kind:
  cold       confirmed=2047  per-user-limit=616  seat-taken=4737
  conflict   confirmed=250  idempotency-key-reused=192  seat-taken=358
  greedy     confirmed=371  per-user-limit=307  seat-taken=322
  hot        confirmed=4  idempotent-replay=1  seat-taken=7995
  hot-retry  confirmed=1  idempotent-replay=3  seat-taken=1996
  spoof      confirmed=379  seat-taken=421

hot seats (201s / requests):
  A1    1 / 1988
  A2    1 / 2016
  A3    1 / 1989
  A4    1 / 2008
  A5    1 / 1999

checks:
  PASS  zero 5xx                                     0
  PASS  zero transport errors / timeouts             0
  PASS  each hot seat: exactly one 201, rest 409     5 seats 
  PASS  same key → one reservation                   3052 keys with 2xx, 0 violating
  PASS  per-user limit holds                         0 users over 4
  PASS  identity comes from the token                0 responses for the wrong user
  PASS  during burst: invariant held on every poll   14 /shows polls, 6 /metrics polls, 0 poll errors all match
  PASS  final: available + held + confirmed == total 1234 + 3766 + 0 = 5000 of 5000
  PASS  final: held == client wins − cancels         API held 3766, clients hold 3766
  PASS  metrics: seat gauges == API                  gauges 1234/3766/0 of 5000, API 1234/3766/0
  PASS  metrics: outcome counters == client tally    all match
  PASS  metrics: no deadlock/serialization retries   +0
  PASS  audit after burst: zero mismatches           audit_mismatches total 0

RESULT: PASS
```

### Live run

_TODO: paste a passing run against the Railway URL._
