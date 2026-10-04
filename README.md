# seat-reservation

A seat-reservation API for high-contention on-sales, written in Go with Postgres.

> Work in progress: health, tokens and shows are in; reservations come next.

## API so far

| Method | Path | Auth | Notes |
|---|---|---|---|
| GET | `/livez` | none | process is up |
| GET | `/readyz` | none | 200 only when DB reachable **and** migrations done, else 503 |
| POST | `/tokens` | none | `{"user_id":"alice"}` → signed JWT (stand-in for an IdP) |
| POST | `/shows` | `X-Admin-Key` | `{"name","total_seats","price_paise", "seats_per_row"?, "per_user_limit"?, "hold_ttl_seconds"?}` |
| GET | `/shows/{id}` | none | every seat's status + `counts` (available + held + confirmed == total_seats) |

Every response carries `X-Request-ID` (an incoming one is reused); each request logs exactly one JSON line with the same ID.

## Run locally

```sh
make up     # Postgres + app via docker compose, waits until /livez responds
make down   # stop everything and drop the DB volume
```

Without Docker: `cp .env.example .env`, export it, then `make run`. `JWT_SECRET` and `ADMIN_API_KEY` are required (>= 16 chars) — the server refuses to start without them. The app listens on `PORT` (default 8080).

## Deploy

Railway builds from the `Dockerfile` (see `railway.json`) and runs one replica, with Postgres in the same region.

Live URL: _TBD_

## Burst test

`./burst.sh <BASE_URL>`: _TBD_
