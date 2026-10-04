# Write-up

## The atomic decision

**Mechanism.** One reserve request is one call to the Postgres function `reserve_seats` (`migrations/0002_reserve_function.sql`), so it is one statement and one transaction. The function never does a read-then-write on seat state. It **locks the rows it is about to decide on, then reads them**:

```sql
SELECT label, status, held_until FROM seats
 WHERE show_id = $show AND label = ANY($seats)
 ORDER BY seat_no
   FOR UPDATE;
```

Only after every requested seat is locked does it check that all of them are `available`, or held with an expired `held_until`. If they are, it writes all of them. If any one is taken, it writes none.

**Why it's race-free.** `FOR UPDATE` gives the transaction an exclusive row lock on each seat until commit. Under a hot-seat storm, the first transaction to lock seat A12 wins. Every other transaction queues on that row lock. When the winner commits, Postgres (READ COMMITTED) hands each waiter the *committed new version* of the row, not the version it saw before waiting. Each waiter therefore sees `status = 'held'` and declines with `409 seat_taken`. No second transaction can ever read A12 as free while the first one is deciding, because reading it for a decision requires the lock. That is why the test fires 400 users at one seat and gets exactly one `201`, every time.

The database also enforces the state machine as a backstop: a `CHECK` constraint on `seats` makes a held seat without an owner, or an available seat with one, impossible to write. `available + held + confirmed == total_seats` holds by construction. Every seat is exactly one row with exactly one status, created up front with the show. `GET /shows/{id}` tallies counts from the seat rows themselves inside one REPEATABLE READ snapshot, so a response can't mix two moments.

**Multi-seat requests and deadlock.** Locks are taken in **ascending `seat_no`**, whatever order the client listed the seats in. Two overlapping requests therefore always contend first on their lowest shared seat. One of them waits there while holding nothing the other needs, so no cycle can form. This is the standard fix: one global lock order. Every write path uses the same overall order:

1. the idempotency key (unique index insert on `reservations(user_id, idempotency_key)`)
2. the per-(show, user) advisory lock
3. seat rows, ascending `seat_no`

`cancel_reservation` takes 2 and then 3 (plus its own reservation row) and never takes 1. A reserve holds at most one key, and only its own. Since every transaction moves forward through the same order, deadlock isn't merely unlikely, it can't happen.

**How it's tested.** `TestMultiSeatRingNoDeadlock` builds the textbook deadlock: 300 users each request two adjacent seats around a ring, listed in reverse order. The store counts every transaction it had to retry after a deadlock (`40P01`) or serialization failure, and the test asserts that count stays **zero**. A count of zero also proves that no retry is silently hiding a problem. As a control, I swapped `ORDER BY seat_no` for `ORDER BY random()`. The same test then failed immediately with deadlock retries.

**Per-user limit.** "At most N seats per user" is a check across rows: count this user's live seats, then add. Done naively, that is a read-then-write: ten parallel requests could all count 0 and all succeed. The function first takes `pg_advisory_xact_lock(hash(show, user))`, which serializes one user's reserves on one show while leaving different users fully parallel. Under that lock, only this user's transactions can add seats for this user, and they are all queued behind the same lock, so the count can't go stale. The advisory lock is released automatically at commit or rollback. The test fires 10 parallel single-seat reserves at a limit-4 show and gets exactly 4 × `201` and 6 × `409 per_user_limit`.

## Idempotency

**Where the key lives.** In the `reservations` table itself, under `UNIQUE (user_id, idempotency_key)`. There's no separate idempotency store, and no cache that could disagree with the seats. The key is scoped per user, so two users can't collide or probe each other's keys. The reservation row is created **in the same transaction** that takes the seats, so it can never record success for seats that weren't taken, or the reverse.

**How exactly-once is enforced.** The first step of `reserve_seats` is:

```sql
INSERT INTO reservations (..., user_id, idempotency_key, fingerprint, ...)
ON CONFLICT (user_id, idempotency_key) DO NOTHING RETURNING id;
```

- **First request:** the insert succeeds, the transaction goes on to take the seats, and it returns `201`.
- **A retry that arrives later:** the insert hits the committed row. The function reads that row and returns it with `200` and an `Idempotent-Replayed: true` header, without touching any seats.
- **A retry that arrives concurrently**, while the first is still in flight: Postgres makes the second insert *wait* on the first transaction's uncommitted index entry. If the first commits, the second sees a conflict and replays. If the first was declined, it deleted its own row before committing, so the second's insert goes through and it runs as a normal request. Either way, exactly one reservation exists per key. `TestIdempotentParallelRetries` fires 50 identical requests at once and gets one `201`, 49 `200`s, one row and two held seats.

**Same key, different body.** Each reservation stores a `fingerprint`: the SHA-256 of the canonical request, meaning the show id plus the seat labels upper-cased, de-duplicated and sorted. Seat order and case therefore don't matter: `["a2","A1"]` replays `["A1","A2"]`. A request whose key matches but whose fingerprint differs gets `409 idempotency_key_reused` and changes nothing. That covers different seats, a subset, a superset, or the same seats on another show. When different bodies race on one key, exactly one wins. Requests with the winner's body replay it, and the rest get 409 (`TestIdempotencyRacingDifferentBodies`).

**What a key remembers.** Only successes. A declined attempt (seat taken, over limit, unknown seat) deletes its row in the same transaction, so the key isn't used up. A client retrying a declined request with the same key gets a fresh decision. If the seat has been released in the meantime, the retry can succeed. That suits a client that retries on a timeout without knowing what happened. A replay after the reservation was cancelled returns the reservation's *current* state (`cancelled`). It does not silently re-hold the seats.

**Identity.** The user is always the `sub` of a verified HS256 token, never anything in the request. The body decoder reads only `seats`, so a body `user_id` is ignored rather than rejected. `cancel_reservation` matches on `id AND user_id`, so another user's reservation returns the same `404` as a missing one, and IDs can't be probed for existence.

## Holds & expiry

A successful reserve creates a **hold**, not a sale. The show defines the TTL (`hold_ttl_seconds`, default 300s, allowed 10–3600s). In the same transaction, the reservation gets `expires_at` and every one of its seats gets the identical `held_until`. The auditor checks the two never drift (`hold_expiry_mismatch`).

**Expiry is lazy: no job has to run for a hold to lapse.** Everywhere a seat's state matters, a hold past its time counts as available:
- the reserve decision (step 3 treats `held` with `held_until <= now()` as free)
- the per-user limit count (only live holds count)
- `GET /shows`, the seat gauges and the auditor

All of them use one SQL expression (`effectiveSeatStatus`), so they can't disagree. A sweeper would add a moving part that can lag or crash. Lazy expiry is exact at every read.

**Consequences:**
- **One clock.** Every expiry decision uses the database's `now()`, never the app's clock, so app instances with skewed clocks can't disagree. Inside a transaction `now()` is the transaction's start time. A request that waited on a lock may see a hold as still live a moment after it expired, which only errs toward declining (`seat_taken`), never toward double-selling.
- **Re-selling an expired seat** overwrites the stale owner in the same locked update. Cancelling an expired hold returns `409 reservation_expired` and writes nothing, so a late cancel can't release a seat that has since gone to someone else (covered by `TestExpiredHold`).
- **Stale rows remain.** An expired reservation stays `status='held'` in its table and is *reported* as `expired`. The seat row keeps the old owner until someone takes it. This is harmless, because every reader applies the expiry rule, but it is untidy (see *What I'd do next*).
- **No confirm step yet.** `confirmed` exists in the schema, the state machine and the counts, but no endpoint moves a hold to confirmed, because payment isn't in scope. One would take the per-user lock, then the reservation row, then its seats in `seat_no` order (the same lock order as cancel), and refuse if `expires_at <= now()`.

## Consistency vs availability under a partition

**This system chooses consistency.** There is one source of truth, the Postgres primary, and every decision is a single transaction on it. When the app can't reach the database (a partition, a failover or an outage), it refuses rather than guesses:
- reserve and cancel return `503 database_unavailable` with `Retry-After`
- `GET /shows` also returns 503 rather than serving a possibly stale seat map
- `/readyz` fails closed, so a load balancer stops sending traffic
- `/livez` stays 200, so the orchestrator doesn't restart a healthy process for a database problem

There is no cache, local queue or "accept now, reconcile later" path that could sell a seat twice. For an on-sale, a double-sold seat is far worse than a few seconds of 503s.

**What makes this survivable for clients is idempotency.** The bad case in a partition is "I don't know if it worked": the commit succeeded but the response was lost. The client retries with the same `Idempotency-Key`. If the first attempt committed, the retry replays it (200). If it didn't, the retry runs it. Either way the result is exactly once, and the client never has to guess.

**If this had to scale out**, I'd keep the same choice and partition by show: all of a show's seats, reservations and per-user locks live on one database shard, so every decision is still local to one primary. Read replicas could serve browsing, as long as they're labelled as possibly stale and never used for a decision. Multi-primary or eventually consistent writes for seat inventory are exactly what this design avoids.

## Observability: what pages at 2am

Every request writes exactly one JSON log line, carrying `request_id` (also returned as `X-Request-ID`), `user_id`, the booking `outcome`, `error_code`, status and latency. Every reserve response moves exactly one outcome counter: `reservations_confirmed_total` or `reservations_declined_total{reason}`. That's guaranteed because one middleware does all the counting after the response is written, including for auth failures, bad input and panics. Seat gauges are read from the database at scrape time using the same expiry rule as `GET /shows`, so they reconcile with the API by construction rather than by careful bookkeeping.

An **auditor** re-checks the invariants every `AUDIT_INTERVAL` in one consistent snapshot. Its checks are written independently of the reserve code. It verifies that seat rows equal `total_seats`, that every live reservation owns all its seats, that every occupied seat is backed by its owner's live reservation, that hold expiries agree, and that no user is over the limit. The results go to `audit_mismatches{check}`. I verified it by corrupting a seat's owner in the database by hand: the next run reported `orphan_occupied_seat=1, reservation_missing_seats=1` and logged the exact seat and reservation. An integration test does the same on every run, and the suite ends with a whole-database audit that fails the run on any mismatch.

**Page (wake someone up):**
- `audit_mismatches > 0` for any check. That is a correctness bug or data corruption, possibly a double-sell. This is the one alert that matters most.
- 5xx rate above zero on the reserve route (`http_requests_total{code=~"5.."}`). Declines are designed to be 4xx, so any 5xx means something is broken.
- `/readyz` failing or `app_ready == 0` for more than a minute, or `seats_scrape_success == 0`. The database is unreachable.
- `db_tx_retries_total` increasing. The lock order is supposed to make deadlocks impossible, so a retry means that reasoning has been broken by a code change.

**Ticket (look in the morning):**
- `audit_errors_total` increasing, meaning the auditor itself can't run.
- Reserve p99 latency high together with `db_pool_empty_acquires_total` climbing. The pool is saturated, so add connections or capacity.
- An unusual mix in `reservations_declined_total`. For example, a jump in `unauthenticated` or `idempotency-key-reused` usually points to a broken client release.

## AI usage

The detailed log is in `AI_LOG.md`. In short:

- **Directed by me:** the requirements and the phased plan, with explicit "done when" acceptance checks per phase (planned with the help of Claude chat). Also the stack (Go, Postgres, Railway), and the correctness constraints: the decision lives in the database, no read-then-write, deterministic lock order for multi-seat requests.
- **Drafted by AI (Claude Code):** most of the code, tests and docs, to that plan. That includes the SQL functions and their lock order, the test harness and concurrency tests, the observability layer, and the burst tool.
- **Done or decided by me:** the Railway deployment and configuration, including diagnosing the `DATABASE_URL` crash loop. Manual testing locally and live with curl and Postman. Running and judging the live bursts. Reviewing the implementation and the outcome choices.
- **What running it caught:** several problems were found by executing, not by reading. A metric reason didn't match its error code; the burst's reconciliation caught it. A database outage surfaced as 500, now 503. The first live run hit stale deployed code. A test-fixture mistake. A random-lock-order control showed the deadlock test really catches deadlocks.

The rule I held to: nothing counted as done because code existed. It counted when its acceptance check passed, locally and then live.

## What I'd do next

1. **Confirm/payment step:** `POST /reservations/{id}/confirm` with its own idempotency key, using cancel's lock order and refusing expired holds. Money stays integer paise end to end.
2. **Expiry tidiness:** a low-priority sweeper that marks lapsed reservations `expired` and clears stale seat owners, plus an `holds_expired_total` counter. This isn't needed for correctness, but it keeps the tables and reports clean.
3. **Throughput:** live `/metrics` shows latency is almost entirely waiting for one of 20 DB connections (about 20ms per decision means about 1,000 req/s). Next steps would be to raise `DB_MAX_CONNS` or put PgBouncer in front, and to fail fast on obviously sold-out hot seats before queuing for the lock.
4. **Real identity and abuse controls:** replace the open `POST /tokens` (a stand-in for an identity provider) with real auth; add per-user and per-IP rate limits; rotate secrets.
5. **Ops as code:** a Prometheus + Grafana setup with the 2am alert rules above checked into the repo; logs shipped to a searchable store; CI running unit tests plus the race-detector integration suite on every PR.
6. **Retention:** expire old idempotency keys and reservations after a window (e.g. 24h past the show), and add `GET /reservations/{id}` for clients.
