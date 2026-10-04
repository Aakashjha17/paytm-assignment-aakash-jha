# seat-reservation

A seat-reservation API for high-contention on-sales, written in Go with Postgres.

> Work in progress. Right now only `GET /livez` is served.

## Run locally

```sh
make up     # Postgres + app via docker compose, waits until /livez responds
make down   # stop everything and drop the DB volume
```

Without Docker: `cp .env.example .env`, then `make run`. The app listens on `PORT` (default 8080).

## Deploy

Railway builds from the `Dockerfile` (see `railway.json`) and runs one replica, with Postgres in the same region.

Live URL: _TBD_

## Burst test

`./burst.sh <BASE_URL>`: _TBD_
