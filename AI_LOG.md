# AI usage log

**Tools:** Claude (claude.ai chat) for implementation planning and discussion; Claude Code (Claude Opus) for accelerating implementation.

**Split, in short:** The core concept, architecture, correctness requirements, and overall implementation approach were my plan. I used Claude to help structure the work into phases, validate the approach, and accelerate implementation. Claude Code drafted a significant portion of the code, tests, and documentation based on the requirements and design I provided. I reviewed the implementation, made the key engineering decisions, deployed the service, ran it locally and live, debugged failures, and verified that the system met the intended correctness and operational requirements.

## What I directed and decided

- **Core concept and architecture:** The overall concept and system design were mine. I defined the requirements around reservations, concurrency, idempotency, database-level atomicity, observability, and deployment. I used AI to help turn this design into an actionable implementation plan and to move faster during development.

- **Plan as acceptance gates:** I adopted a five-phase implementation structure with explicit "done when" criteria. I did not consider a phase complete until its acceptance checks passed. For example: readiness must fail closed when the database stops, concurrency tests must pass repeatedly under the race detector, gauges must match `GET /shows`, and two consecutive live bursts must have zero 5xx responses.

- **Stack and platform:** I selected Go, Postgres, and Railway based on my existing experience with Go and Postgres and the need to be able to maintain and extend the service myself. I chose a single replica with Postgres in the same region.

- **Correctness approach:** The core correctness model was my design. The atomic decision lives in the database through a conditional or locked update rather than a read-then-write flow. Multi-seat requests lock seats in a deterministic order to prevent deadlocks. Idempotency is enforced as part of the reservation flow rather than being handled only at the application layer.

- **Implementation direction:** I reviewed the AI-generated implementation against the design and requirements, traced the request flow, and corrected issues where the implementation did not match the intended behavior.

- **Deployment:** I created the Railway project and Postgres instance, configured the environment variables, diagnosed the crash loop caused by `DATABASE_URL is required`, fixed it using a service reference, and generated the public domain.

- **Testing and verification:** I manually tested the service using curl and Postman, both locally and against the live deployment. I reviewed the concurrency and burst results and investigated failures rather than treating AI-generated test output as sufficient.

- **Repository:** I set up the GitHub repository and committed Phases 1–4 incrementally.

- **Debugging and engineering decisions:** I investigated issues encountered during development and deployment, including local database conflicts, incorrect test fixtures, deadlock behavior, metric mismatches, incorrect HTTP status codes, stale deployed code, and deployment-edge failures.

## What the AI accelerated, phase by phase

| Phase | AI-assisted implementation | Found or fixed while running it |
|---|---|---|
| 1. Skeleton | Config, `/livez` server, Dockerfile, compose, Makefile, `railway.json` | — |
| 2. Plumbing | DB pool with retry, `/readyz`, migrations under an advisory lock, JSON logging and middleware, JWT/admin auth, shows endpoints, unit tests | My local `.env` overrode the compose secrets |
| 3. Correctness | Implemented the reservation/cancellation SQL functions, booking rules, integration and concurrency tests, and documentation based on my correctness design | A local Postgres on `:5432` shadowed compose, so I moved compose to `55432`. Fixed mistakes in test fixtures. A random-lock-order control proved the deadlock test catches deadlocks. |
| 4. Observability | Prometheus metrics, DB-backed seat gauges, auditor, graceful shutdown | `/readyz` returning 503 during drain was logged as `ERROR`; changed it to `WARN` |
| 5. Burst | `cmd/burst` workload and checks, `burst.sh`, `make watch` | A metric reason did not match its error code. A DB outage returned 500; changed it to 503. The first live run hit stale deployed code. A token-mint request was lost at the edge during setup. |

## Where AI was not used

- Defining the **core product concept and system architecture**.
- Making the key correctness decisions around database atomicity, concurrency, lock ordering, and idempotency.
- Railway and GitHub setup, environment configuration, and diagnosing the deployment crash loop.
- Manual testing with curl and Postman.
- Running and judging the live concurrency/burst tests.
- Reviewing the AI-drafted code and tracing the request flow before committing.
- Debugging issues in the local and deployed environments and deciding how they should be fixed.

## Summary

AI was primarily used as an **implementation accelerator**. I provided the core concept, requirements, architecture, and correctness model, then used Claude and Claude Code to reduce the time required to translate those decisions into code, tests, and documentation.

The resulting implementation was not accepted purely based on AI output. I reviewed the code, ran the system, identified and fixed issues, validated concurrency behavior, deployed it, and verified the final behavior locally and in the live environment.