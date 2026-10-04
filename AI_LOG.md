# AI usage log

The tool is Claude Code (Claude Opus). Each entry records what I directed, what the AI produced, and what I decided or changed.

## 2026-10-04: Planning

- **Directed:** I gave the AI the assignment spec and talked through the plan: Go + Postgres, Railway, and a phased build (skeleton/deploy → schema + atomic reserve → idempotency → observability → burst).
- **AI produced:** suggestions for the phase breakdown and file layout.
- **Decided by me:** the stack, the platform, the phase order, and the repo layout (scaffolded by hand).

## 2026-10-04: Phase 1 skeleton

- **Directed:** I asked for the Phase 1 files: config loader, a `main.go` that serves only `/livez`, Dockerfile, compose, Makefile `up`/`down`, and `railway.json`.
- **AI produced:** those files. Standard library only, slog JSON logs, graceful shutdown on SIGTERM, a multi-stage distroless image, and Postgres in compose with a healthcheck.
- **Decided / reviewed by me:** _fill in_

## 2026-10-04 — Phase 2: plumbing and simple endpoints

- **Directed:** asked for pool with connect-retry, /readyz, embedded migrations under an advisory lock, logging + middleware, JWT/admin auth, and show create/get.
- **AI produced:** the above, plus unit tests (token tampering/alg-none/expiry, seat labels). Notable choices it proposed: HTTP listens before the DB is up so /livez works on cold start while /readyz and the API stay 503 until migrated; empty migration files are skipped and not recorded; a DB CHECK constraint encodes the seat state machine; GET /shows reads in one REPEATABLE READ snapshot and derives counts from the seat rows; unknown body fields are ignored (not 400) so a spoofed `user_id` is inert; open POST /tokens as an IdP stand-in.
- **Decided / reviewed by me:** _fill in_

## 2026-10-04 — Phase 3: correctness core

- **Directed:** asked for the booking rules + unit tests, the reserve SQL function, integration harness and the four concurrency tests one at a time, then cancel + identity tests, and the atomic-decision/idempotency write-up sections.
- **AI produced:** `reserve_seats` / `cancel_reservation` plpgsql functions with a global lock order (idempotency key → per-(show,user) advisory lock → seats `FOR UPDATE ORDER BY seat_no`); key stored on the reservation row with a fingerprint; declined attempts delete their row so keys aren't burned; lazy expiry; retry-on-40P01/40001 with a counter the tests assert stays 0; an invariant oracle (`checkConsistency`) used by every test; a negative control (random lock order → test fails).
- **Bugs found while running tests:** three were in the tests (seat labels beyond a row's width); none in the SQL. Also found a local Postgres on :5432 shadowing compose — moved compose DB to host port 55432.
- **Decided / reviewed by me:** _fill in — e.g. outcome→status choices (409 vs 422), expired-cancel returns 409, open /tokens_

## 2026-10-04 — Phase 4: observability

- **Directed:** asked for Prometheus metrics with exactly one outcome counter per response, DB-derived seat gauges + pool stats, an auditor exporting a mismatch gauge, and graceful shutdown (readiness 503 → drain → pool close).
- **AI produced:** all outcome counting in the single observe middleware (reason from the booking outcome table, or the error code for pre-decision failures); seat gauges as a scrape-time collector sharing one SQL expiry expression with GET /shows; auditor with 5 invariant checks in one REPEATABLE READ snapshot; split `migrated` vs `draining` so /readyz goes 503 during drain while requests are still served; tests for metric/API reconciliation and audit-detects-corruption, plus a final whole-DB audit after every integration run.
- **Verified by hand:** gauges matched GET /shows after a storm; corrupting a seat's owner in the local DB flipped `orphan_occupied_seat` and `reservation_missing_seats` to 1 with the seat/reservation logged, back to 0 after repair; `docker compose stop app` with a request blocked on a row lock logged shutdown 1/4 → 4/4 and the request completed with 201.
- **Bugs found:** test seat-layout mistakes again (my test fixtures, not the service); /readyz 503 during drain was logged at ERROR (would have paged) → now WARN.
- **Decided / reviewed by me:** _fill in_

## 2026-10-04 — Phase 5: burst tool and hardening

- **Directed:** asked for cmd/burst + burst.sh + make burst, built in layers (hot-seat storm → mixed workload → mid-burst poller → final reconciliation with non-zero exit), then ops checks.
- **AI produced:** the tool (public API + /metrics only, so it runs unchanged against local or live), a 6-kind workload, 13 checks, `make watch` for recording.
- **What the burst caught:** the metric reason for same-key-different-body was `idempotency-conflict` while the error code was `idempotency_key_reused` (same drift for two cancel outcomes) — the counter-vs-client reconciliation failed on it. Fixed by making every decline's reason equal its code, with a unit test enforcing it.
- **Ops checks found:** with the DB down the API answered `500 internal` — indistinguishable from a bug. Now `503 database_unavailable` + `Retry-After`, counted under its own reason.
- **Live:** first live run failed one check only because Railway still ran pre-fix code (reason rename not pushed) — pushed, redeployed, then 3 full 20k live runs passed (two consecutive). One run aborted in *setup* when a single token-mint request was lost at the edge (server metrics showed every mint served in ~8 ms, no restart); setup calls now retry, measured requests never do. /metrics showed latency is pool wait (20 conns × ~20 ms/decision ≈ 1k req/s), not the app.
- **Decided / reviewed by me:** _fill in — e.g. whether to raise DB_MAX_CONNS on Railway_
