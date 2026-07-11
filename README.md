# Wisp

Wisp leases you an authenticated, root-access, throwaway container with a shell,
for a bounded time — then it vanishes. You bring your own tools.

This repository contains the Wisp broker daemon (`wispd`). The current scaffold
stands up the HTTP server, structured logging, and environment-driven config
with a `GET /healthz` liveness probe; the broker surface is filled in per the
design doc (`docs/DESIGN.md`).

## Layout

```
cmd/wispd/          daemon entrypoint
internal/config/    environment-driven configuration
internal/server/    HTTP routing and handlers
```

## Build & run

```sh
go build ./...
go run ./cmd/wispd
```

Configuration (environment):

| Variable    | Default            | Meaning                          |
|-------------|--------------------|----------------------------------|
| `WISP_ADDR` | `127.0.0.1:8080`   | Full listen address (host:port). |
| `WISP_PORT` | —                  | Port only; used when `WISP_ADDR` is unset. |

Check liveness:

```sh
curl -s localhost:8080/healthz   # {"status":"ok"}
```

## Test

```sh
go test ./...
```
