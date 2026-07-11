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

| Variable         | Default            | Meaning                          |
|------------------|--------------------|----------------------------------|
| `WISP_ADDR`      | `127.0.0.1:8080`   | Full listen address (host:port). |
| `WISP_PORT`      | —                  | Port only; used when `WISP_ADDR` is unset. |
| `WISP_APP_TOKEN` | —                  | App-level bearer token gating contract creation. Unset ⇒ open (localhost default). |

## Auth

Two tiers of bearer token (see [`docs/DESIGN.md` §8](docs/DESIGN.md)):

- **App-level token** — gates contract creation (`POST /contracts`). Set it with
  `WISP_APP_TOKEN` and send `Authorization: Bearer <app-token>`. When unset the
  gate is disabled: any caller may create contracts. That is the intended
  localhost default — Wisp binds `127.0.0.1` and the OS user boundary is the
  outer defense. Set a token when exposing Wisp beyond the loopback interface.
- **Per-contract token** — returned at creation and required on every
  contract-scoped call: `POST /contracts/:id/exec` (`Authorization: Bearer
  <contract-token>`) and the `WS /contracts/:id/shell` handshake (`?token=` or a
  `bearer.<token>` subprotocol). Missing or bad credentials return `401`. The app
  token does **not** authorize these calls, and vice versa.

Check liveness (unauthenticated):

```sh
curl -s localhost:8080/healthz   # {"status":"ok"}
```

## Test

```sh
go test ./...
```
