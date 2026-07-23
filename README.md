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
internal/policy/    image allow-list + limits (operator config)
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
| `WISP_CONFIG`    | —                  | Path to the image allow-list + limits JSON config. Unset ⇒ built-in defaults. |
| `WISP_APP_TOKEN` | —                  | App-level bearer token gating contract creation. Unset ⇒ open (localhost default). |

## Contracts, images & limits

Wisp is domain-blind: it ships **no** opinionated, tool-aware presets. The
operator owns a small, data-driven config — an image **allow-list**, a
**default image**, and optional **limits** — and the client picks an allowed
image and shapes network / resources per request. Userdata owns everything
*inside* the container (see [`docs/DESIGN.md` §7](docs/DESIGN.md)).

The config is a JSON file whose path comes from `WISP_CONFIG`; when unset, Wisp
uses safe built-in defaults (allow-list of just `wisp-base`, networks `none` +
`open`, isolation `shared` only, and conservative resource/TTL ceilings: TTL
3600 s, 4 CPUs, 4096 MB, 512 pids). An example lives at
[`examples/wisp.config.json`](examples/wisp.config.json):

```json
{
  "images": { "allow": ["wisp-base"], "default": "wisp-base" },
  "limits": { "max_ttl_seconds": 3600, "max_cpus": 4, "max_memory_mb": 4096, "pids_limit": 512, "networks": ["none", "open"], "isolations": ["shared"], "default_isolation": "shared" }
}
```

A zero/empty numeric limit means **no cap** (the built-in defaults above are
non-zero ceilings). On load Wisp validates that the allow-list is non-empty, the
default image is in it, every network is one of `none` / `open` / `egress`, and
every isolation level and the default are valid (`shared` / `sandboxed` / `vm`).

`POST /contracts` accepts:

```json
{
  "ttl_seconds": 3600,
  "image": "wisp-base",
  "network": "open",
  "isolation": "shared",
  "resources": { "cpus": 2, "memory_mb": 4096, "pids": 1024 },
  "userdata": "#!/bin/sh\n...",
  "meta": { "job": "build-42" }
}
```

`ttl_seconds` is required (> 0). `image` defaults to the config default and must
be allow-listed (else `400`); `network` defaults to `open` when allowed and must
be one of the configured networks (else `400`); `isolation` defaults to
`default_isolation` (`shared`) and must be one of the host's advertised
`isolation.supported` levels — ordered `shared` < `sandboxed` < `vm`, with
`confidential` reserved and rejected (else `400`); each `resources` value and the
TTL are clamped down to any configured maximum. `meta` is opaque and echoed back
on status reads.

Any consumer can discover what it may request via the **unauthenticated**
`GET /images` (like `/healthz`):

```sh
curl -s localhost:8080/images
# {"images":["wisp-base"],"default":"wisp-base","os":"linux","limits":{"max_ttl_seconds":3600,...,"networks":["none","open"]},"isolation":{"supported":["shared"],"default":"shared"}}
```

The `os` field reports the Docker daemon's current container mode (`linux` or
`windows`), which Wisp **detects** from `docker info` (`OSType`). Wisp serves
whichever mode the host is in: on a `windows`-mode daemon it defaults to the
Windows base image ([`examples/wisp-base-windows`](examples/wisp-base-windows/Dockerfile))
and drives containers with the Windows keep-alive and `cmd.exe` shell. The
**operator** owns the mode (the Docker Desktop engine switch / daemon config);
**Wisp never switches it**. One daemon runs one mode at a time, Windows
containers require a Windows host, and the container/host OS build plus isolation
(process vs Hyper-V) must be compatible — see
[`docs/DESIGN.md` §12](docs/DESIGN.md).

The `isolation` object reports the host's **effective** isolation posture —
`supported` is the operator allow-list intersected with the levels the daemon can
actually run (`shared` always; `sandboxed` when the gVisor runtime `runsc` is
registered; `vm` on a Kata-enabled Linux daemon or a Windows daemon via Hyper-V),
and `default` is the level applied when a create omits `isolation`. A consumer
only ever sees, and can only request, levels this host can launch.

## Auth

Two tiers of bearer token (see [`docs/DESIGN.md` §8](docs/DESIGN.md)):

- **App-level token** — gates contract creation (`POST /contracts`) and the
  event bus (`POST`/`WS /events`). Set it with `WISP_APP_TOKEN` and send
  `Authorization: Bearer <app-token>` (the bus WebSocket also accepts `?token=`).
  When unset the gate is disabled: any caller may create contracts and use the
  bus. That is the intended localhost default — Wisp binds `127.0.0.1` and the OS
  user boundary is the outer defense. Set a token when exposing Wisp beyond the
  loopback interface.
- **Per-contract token** — returned at creation and required on every
  contract-scoped call: `POST /contracts/:id/exec` (`Authorization: Bearer
  <contract-token>`) and the `WS /contracts/:id/shell` handshake (`?token=` or a
  `bearer.<token>` subprotocol). Missing or bad credentials return `401`. The app
  token does **not** authorize these calls, and vice versa.

Check liveness (unauthenticated):

```sh
curl -s localhost:8080/healthz   # {"status":"ok"}
```

## Event bus

A dumb in-process pub/sub layer for coordination between satellites (see
[`docs/DESIGN.md` §6](docs/DESIGN.md)). Payloads are opaque: Wisp routes on an
event's `type` string but never interprets it or the body.

- `POST /events` — publish an opaque event: `{"type":"...","data":{...}}` →
  `202`. Gated by the app-level token.
- `WS /events` — subscribe to the live fan-out. An optional `?type=` query
  filters to one or more comma-separated types; with no filter every event is
  delivered. Events arrive as JSON text messages.

Wisp publishes its own contract lifecycle events on the same bus:
`contract.created`, `contract.ready`, `contract.expiring`, `contract.released`,
`contract.expired`. Each carries `{"contract_id":"...","status":"..."}`.

```sh
# subscribe (blocks, prints events as JSON)
websocat "ws://localhost:8080/events?type=contract.ready"

# publish
curl -s localhost:8080/events -d '{"type":"task.start","data":{"repo":"acme"}}'
```

## Test

```sh
go test ./...
```
