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
