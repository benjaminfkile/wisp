# Wisp

Wisp leases you an authenticated, root-access, throwaway container with a shell,
for a bounded time. Then it vanishes. You bring your own tools.

This repository contains the Wisp broker daemon (`wispd`): the contract API
(create, list, status, release), sync and streaming exec, an interactive PTY
shell over WebSocket, an in-process event bus, a TTL reaper with container-death
detection, a restart reconcile driven by container labels, ordered isolation
tiers, aggregate capacity budgets, and whole-device GPU leasing. The design is
in `docs/DESIGN.md`.

## Layout

```
cmd/wispd/          daemon entrypoint
internal/bus/       in-process pub/sub event bus
internal/capacity/  aggregate host capacity allocator (contracts / cpus / memory)
internal/config/    environment-driven configuration
internal/contract/  contract model, lifecycle state machine, in-memory store
internal/gpu/       exclusive whole-device GPU allocator
internal/policy/    image allow-list + limits + isolation/GPU capability (operator config)
internal/reaper/    TTL reaper (expiring/expired transitions, container-death sweep)
internal/runtime/   container backend (Docker SDK) + in-memory fake for tests
internal/server/    HTTP routing and handlers, startup reconcile
```

## Build & run

```sh
go build ./...
go run ./cmd/wispd
```

`scripts/run-local.sh` builds the example `wisp-base` image and starts `wispd`
on `127.0.0.1:8090` with `examples/wisp.config.json`.

Configuration (environment):

| Variable                      | Default          | Meaning                          |
|-------------------------------|------------------|----------------------------------|
| `WISP_ADDR`                   | `127.0.0.1:8080` | Full listen address (host:port). Takes precedence over `WISP_PORT`. |
| `WISP_PORT`                   | (unset)          | Port only; bound on `127.0.0.1`. Used when `WISP_ADDR` is unset. |
| `WISP_CONFIG`                 | (unset)          | Path to the image allow-list + limits JSON config. Unset means built-in defaults. |
| `WISP_APP_TOKEN`              | (unset)          | App-level bearer token gating contract creation, the contract list, and the event bus. Unset means open (localhost default). |
| `WISP_REAP_INTERVAL_SECONDS`  | `1`              | How often the TTL reaper sweeps (positive integer). |
| `WISP_EXPIRING_LEAD_SECONDS`  | `60`             | How long before the TTL a ready contract moves to `expiring` (positive integer). |
| `WISP_RELEASE_GRACE_SECONDS`  | `30`             | How long the reaper skips a contract sitting in `releasing` before expiring it as a stuck release (positive integer). |
| `WISP_KILL_TIMEOUT_SECONDS`   | `30`             | How long a single reaper `Kill` may run before it is bounded out so the sweep can move on (positive integer). |

`wispd` needs a reachable Docker daemon (ambient `DOCKER_HOST` etc.) and exits at
startup if the client cannot be built or the config fails to load or validate.

## Contracts, images & limits

Wisp is domain-blind: it ships **no** opinionated, tool-aware presets. The
operator owns a small, data-driven config: an image **allow-list**, a
**default image**, and optional **limits**. The client picks an allowed
image and shapes network / resources per request. Userdata owns everything
*inside* the container (see [`docs/DESIGN.md` §7](docs/DESIGN.md)).

The config is a JSON file whose path comes from `WISP_CONFIG`; when unset, Wisp
uses safe built-in defaults: an allow-list of the two bare base images
(`wisp-base` and `wisp-base-windows`, so the same default serves either daemon
OS) with `wisp-base` as the default image, networks `none` + `open`, isolation
`shared` only, and conservative resource/TTL ceilings (TTL 3600 s, 4 CPUs,
4096 MB, 512 pids). An example lives at
[`examples/wisp.config.json`](examples/wisp.config.json):

```json
{
  "images": { "allow": ["wisp-base"], "default": "wisp-base" },
  "limits": {
    "max_ttl_seconds": 0, "max_cpus": 0, "max_memory_mb": 0, "pids_limit": 0,
    "max_contracts": 0, "total_cpus": 0, "total_memory_mb": 0,
    "max_gpus": 0, "gpus_disabled": false,
    "networks": ["none", "open"], "isolations": ["shared"], "default_isolation": "shared"
  }
}
```

A zero/empty numeric limit means **no cap** (the built-in defaults above are
non-zero ceilings; the example file lifts them all). The file is parsed strictly
(unknown fields are rejected). On load Wisp validates that the allow-list is
non-empty, the default image is in it (an omitted default falls back to the
first allowed image), every network is one of `none` / `open` / `egress`, and
every isolation level and the default are valid (`shared` / `sandboxed` / `vm`;
`confidential` is rejected). An omitted `networks` list falls back to
`none` + `open`; omitted `isolations` / `default_isolation` fall back to `shared`.

`max_cpus` / `max_memory_mb` / `pids_limit` are **per-lease** caps. Alongside
them the operator can declare **host capacity budgets**: caps on aggregate load
across *all* concurrent contracts:

| Budget            | Bounds                                                | `0`/omitted |
| ----------------- | ----------------------------------------------------- | ----------- |
| `max_contracts`   | number of concurrent (non-terminal) contracts         | unlimited   |
| `total_cpus`      | aggregate CPU (fraction of host cores) across leases   | unlimited   |
| `total_memory_mb` | aggregate memory (MB) across leases                    | unlimited   |

Budgets must not be negative, and when both a total and its matching per-lease
max are set the total must be `>=` the per-lease max (a budget smaller than one
lease could never admit anything). These budgets are **advertised** (the
`GET /images` `capacity` block below) and **enforced** at create time: a create
reserves its post-clamp cpus / memory plus one contract slot against all three
budgets atomically, and an exhausted budget is a `409` with the message
`host at capacity: contract, cpu, or memory budget exhausted`. The reservation
is returned on release, TTL expiry, out-of-band container death, or a
provisioning failure, and it survives a `wispd` restart (it is rebuilt from the
`wisp.cpus` / `wisp.memory_mb` container labels on startup).

`POST /contracts` accepts:

```json
{
  "ttl_seconds": 3600,
  "image": "wisp-base",
  "network": "open",
  "isolation": "shared",
  "resources": { "cpus": 2, "memory_mb": 4096, "pids": 1024, "gpus": 0 },
  "userdata": "#!/bin/sh\n...",
  "env": { "KEY": "value" },
  "external_id": "upstream-lease-id",
  "meta": { "job": "build-42" }
}
```

`ttl_seconds` is required (> 0). `image` defaults to the config default for the
daemon's OS and must be allow-listed (else `400`); `network` defaults to `open`
when allowed (else the first configured network) and must be one of the
configured networks (else `400`); `isolation` defaults to the host's effective
default and must be one of the host's advertised `isolation.supported` levels,
ordered `shared` < `sandboxed` < `vm`, with `confidential` reserved and rejected
(else `400`). Each of `resources.cpus` / `memory_mb` / `pids` and the TTL is
clamped down to any configured maximum, and an omitted (zero) value inherits the
maximum when one is set. `resources.gpus` is a whole-device count that is
rejected, never clamped (see below). `env` is an opaque `KEY -> VALUE` map
injected as the container environment (write-only: never echoed on reads; at
most 128 entries and 256 KiB total; keys must be non-empty and free of `=` /
NUL). `external_id` is an opaque caller identifier (at most 128 bytes) persisted
on the container and echoed on reads. `meta` is opaque and echoed back on status
reads.

The call is synchronous: it boots the container, runs `userdata` to completion,
and returns `201 {"contract_id","token","status":"ready"}`. A userdata script
that exits non-zero (or any other provisioning failure) destroys the container,
marks the contract `expired`, and returns `500`; an image the daemon rejects for
this host's OS/platform returns `400`.

Any consumer can discover what it may request via the **unauthenticated**
`GET /images` (like `/healthz`):

```sh
curl -s localhost:8080/images
# {"os":"linux","images":["wisp-base","wisp-base-windows"],"default":"wisp-base","limits":{"max_ttl_seconds":3600,"max_cpus":4,"max_memory_mb":4096,"pids_limit":512,"networks":["none","open"]},"isolation":{"supported":["shared"],"default":"shared"},"gpu":{"supported":false,"devices":[],"max_gpus":0,"isolations":[]},"capacity":{"max_contracts":0,"active_contracts":0,"total_cpus":0,"used_cpus":0,"total_memory_mb":0,"used_memory_mb":0}}
```

The `os` field reports the Docker daemon's current container mode (`linux` or
`windows`), which Wisp **detects** from `docker info` (`OSType`). Wisp serves
whichever mode the host is in: on a `windows`-mode daemon it defaults to the
Windows base image ([`examples/wisp-base-windows`](examples/wisp-base-windows/Dockerfile))
when that image is allow-listed, and drives containers with the Windows
keep-alive and `cmd.exe` shell. The **operator** owns the mode (the Docker
Desktop engine switch / daemon config); **Wisp never switches it**. One daemon
runs one mode at a time, Windows containers require a Windows host, and the
container/host OS build plus isolation (process vs Hyper-V) must be compatible.
See [`docs/DESIGN.md` §12](docs/DESIGN.md).

The `isolation` object reports the host's **effective** isolation posture.
`supported` is the operator allow-list intersected with the levels the daemon
can actually run (`shared` always; `sandboxed` when the gVisor runtime `runsc`
is registered; `vm` when a Kata runtime (`kata-runtime` or `kata`) is registered
on a Linux daemon, or on any Windows daemon via Hyper-V), and `default` is the
level applied when a create omits `isolation`. A consumer only ever sees, and
can only request, levels this host can launch. When a `vm` lease launches on a
Linux daemon Wisp names the Kata runtime the daemon actually registered (the
canonical `kata-runtime` when present, else the short alias `kata`), so a daemon
that registered Kata under either name both advertises `vm` and launches it
successfully (launch tracks detection). Wisp **fails loud, never downgrades**: a
policy-allowed level the host cannot run is dropped with a startup warning, and
if *none* of the configured levels are runnable (and the operator excluded
`shared`) the host logs an error, advertises an empty `supported` list, and
**refuses every `POST /contracts` with `409 no runnable isolation tier`**,
whether or not the request names an isolation, instead of silently falling back
to `shared`.

The `capacity` block advertises the host's aggregate capacity posture: the
operator budgets (`max_contracts`, `total_cpus`, `total_memory_mb`; `0` means
unlimited) alongside current usage (`active_contracts` is the live non-terminal
contract count; `used_cpus` / `used_memory_mb` are the capacity allocator's live
reserved totals). The block is authoritative across repos (an agent forwards it
verbatim), so its snake_case field names are a wire contract.

## GPU leasing

GPUs are an operator-gated, host-detected dimension a lease can request as a
**count** (`resources.gpus`); wisp assigns **whole devices exclusively**: no two
live leases share a device. What a host needs to lease GPUs in v1:

- **NVIDIA driver + [`nvidia-container-toolkit`](https://github.com/NVIDIA/nvidia-container-toolkit)**,
  configured so the Docker daemon registers the `nvidia` runtime. wisp detects
  GPU support from two signals: the daemon advertising that runtime **and**
  `nvidia-smi` enumerating at least one device. Missing either means GPU leasing
  is advertised as unsupported (a startup log line, never a fatal error).
- **Shared isolation only.** v1 attaches GPUs via `docker run --gpus`-style
  device requests under runc; no sandboxed/VM GPU backend exists yet, so GPU
  attach is available at `shared` isolation only.
- **Config knobs** (`WISP_CONFIG` `limits`, both optional):
  - `max_gpus`: per-lease GPU cap; `0`/omitted means no operator cap (all
    detected devices). Must not be negative.
  - `gpus_disabled`: `true` turns GPU leasing off entirely regardless of
    hardware; omitted means enabled.

Unlike `cpus`/`memory_mb`/`pids` (which are clamped), an over-large `gpus`
request is **rejected** (`400`: no GPU support, over the effective cap, or GPU
attach unavailable at the resolved isolation level), never silently reduced.
When the ask is within limits but every device is held by another live lease
the create is a `409` (`no GPU devices currently available`). Assigned device
IDs are echoed as `gpus` on status reads. `GET /images` carries a `gpu` block
advertising the effective posture: `{ supported, devices, max_gpus, isolations }`.
See [`docs/DESIGN.md` §7](docs/DESIGN.md) for the full model (detection,
allocator, restart reconcile, and the Kata + VFIO seam), and
[`docs/ISOLATION_TESTING.md`](docs/ISOLATION_TESTING.md) for the isolation
posture and CI limits.

## Auth

Two tiers of bearer token (see [`docs/DESIGN.md` §8](docs/DESIGN.md)):

- **App-level token**: gates contract creation (`POST /contracts`), the
  contract list (`GET /contracts`), and the event bus (`POST`/`WS /events`).
  Set it with `WISP_APP_TOKEN` and send `Authorization: Bearer <app-token>` (the
  bus WebSocket also accepts `?token=`). When unset the gate is disabled: any
  caller may create and list contracts and use the bus. That is the intended
  localhost default: Wisp binds `127.0.0.1` and the OS user boundary is the
  outer defense. Set a token when exposing Wisp beyond the loopback interface.
- **Per-contract token**: returned at creation and required on every
  contract-scoped call: `POST /contracts/:id/exec` (`Authorization: Bearer
  <contract-token>`) and the `WS /contracts/:id/shell` handshake (`?token=` or a
  `bearer.<token>` subprotocol). The app token does **not** authorize these
  calls.
- **Either token** authorizes `GET /contracts/:id` and `DELETE /contracts/:id`,
  so the local agent can read or release any lease it created while a satellite
  holding only a contract token can still read or release its own. When
  `WISP_APP_TOKEN` is unset these two calls are open.

Missing or bad credentials return `401`. All comparisons are constant-time.

Check liveness (unauthenticated):

```sh
curl -s localhost:8080/healthz   # {"status":"ok"}
```

## API

```
GET    /healthz                       liveness, {"status":"ok"}                       (open)
GET    /images                        discovery document (see above)                 (open)
POST   /contracts                     create + boot + run userdata; 201              (app token)
GET    /contracts                     list live contracts                            (app token)
GET    /contracts/:id                 status                                         (app or contract token)
DELETE /contracts/:id                 release now (destroy container); 200           (app or contract token)
POST   /contracts/:id/exec            run a command, sync    {"command":"..."}       (contract token)
POST   /contracts/:id/exec?stream=1   run a command, streamed as Server-Sent Events  (contract token)
WS     /contracts/:id/shell           interactive PTY                                (contract token)
POST   /events                        publish an opaque event; 202                   (app token)
WS     /events                        subscribe, optional ?type=a,b filter           (app token)
```

- `GET /contracts` returns `{"contracts":[{id, external_id, token, status,
  expires_at, ttl_seconds_remaining, reserved_cpus, reserved_memory_mb, gpus}]}`
  for every contract in `provisioning`, `ready`, or `expiring`. Terminal
  contracts (`released`, `expired`) are excluded; the transient pre-provision
  `requested` state and the transient (non-terminal) `releasing` fence are
  also excluded, so consumers may treat those three as an exhaustive `status`
  enum. `gpus` is always an array. The list includes each contract's current
  bearer token so a restarted local agent can rebuild its lease map, which is
  why it is app-token gated.
- `GET /contracts/:id` and `DELETE /contracts/:id` return
  `{contract_id, status, ttl_seconds_remaining, gpus?, meta?, external_id}`.
  `status` is one of `provisioning`, `ready`, `expiring`, `releasing`,
  `released`, or `expired`; only `released` and `expired` are terminal, and
  `releasing` is the transient (non-terminal) fence the release handler
  installs before killing the container. `DELETE` first transitions the
  contract to `releasing` to fence the reaper off it, then kills the
  container, then completes the transition to `released`. `DELETE` of an
  already `released` or `expired` contract is an **idempotent success** (200
  echoing the terminal status; nothing is freed twice); `DELETE` of a
  contract already in `releasing` (a concurrent release is in flight) is
  likewise 200 echoing the current `releasing` status without a second
  container kill or a double free. A `DELETE` whose fence-installing
  transition finds the contract already purged, or whose final mark-released
  finds the store has since purged it, also returns 200 with a `released`
  status. Unknown ids are `404`. Terminal contracts are purged from the
  store one reaper sweep after they become terminal, after which their id is
  `404`.
- `POST /contracts/:id/exec` runs the command through `/bin/sh -c` (or `cmd /c`
  on a Windows daemon) as a fresh process and returns
  `{stdout, stderr, exit_code}` (a non-zero exit is still `200`). It is `409`
  unless the contract is `ready` or `expiring` (execs stay usable through the
  lead window so the client can exfiltrate work before the hard kill; execs
  against a contract in `releasing`, `released`, or `expired` are `409`),
  `400` on an empty command. With `?stream=1` the response is
  `text/event-stream`: `chunk` events carrying
  `{"stream":"stdout"|"stderr","data":"..."}`, then one `exit` event with
  `{"exit_code":n}` (or an `error` event if the exec fails mid-stream).
- `WS /contracts/:id/shell` bridges a TTY exec of `/bin/sh` (`cmd.exe` on
  Windows). Raw bytes ride binary messages both ways; a text message
  `{"type":"resize","rows":n,"cols":n}` resizes the TTY and any other text is
  written to stdin verbatim. Pre-upgrade rejections are `404` / `401` / `409`
  (not `ready` or `expiring`; shells stay usable through the lead window too,
  and are `409` in `releasing` / `released` / `expired`).

Every response body other than the SSE stream is JSON; errors are
`{"error":"..."}`.

## Lifecycle, reaper & restart reconcile

A contract moves `requested -> provisioning -> ready -> expiring -> releasing -> released`,
or exits to `expired` from any active state. `released` (client `DELETE`) and
`expired` (TTL elapsed, provisioning failure, or container death) are terminal
and destroy the container. The `releasing` state is the transient,
**non-terminal** fence a `DELETE` installs BEFORE killing the container so the
reaper cannot expire (and then purge) the contract from under the handler: the
reaper skips `releasing` contracts for a short release grace
(`WISP_RELEASE_GRACE_SECONDS`, default 30 s), so within that window the DELETE
handler is the sole owner of the container kill and the terminal transition
to `released`. Past the grace the release is presumed stuck (the handler died
mid-release, or its request was cancelled before it reached the final
mark-released transition) and the reaper expires the contract like any other
non-terminal one, killing the container if needed and returning its capacity
and GPUs to the allocators rather than leaking them until restart. A hung
Docker daemon on the reaper's own `Kill` is bounded separately: each reaper
`Kill` runs under `WISP_KILL_TIMEOUT_SECONDS` (default 30 s) and runs off the
sweep in its own goroutine, so a wedged container adds no latency to the tick;
the state transition and capacity free apply in that goroutine's completion,
the tick skips any contract with a kill in flight so the next sweep never
double-kills it, and a timed-out contract is left in place for a later kill
attempt while the sweep proceeds with the remaining contracts. The reaper sweeps every
`WISP_REAP_INTERVAL_SECONDS`: a `ready` contract inside the
`WISP_EXPIRING_LEAD_SECONDS` window becomes `expiring`; a contract past its TTL
is killed and `expired`; and a `ready`/`expiring` contract whose container has
stopped or been removed out of band (`docker kill` / `docker rm` / OOM) is
expired on the same path within a sweep or two, so its capacity and GPUs return
to the allocators immediately instead of at TTL. A transport error from the
daemon is treated as inconclusive and retried next sweep. A `Kill` that races
another concurrent tear down (the daemon returns "removal already in progress")
is treated as success (the container is being torn down either way).

Every leased container carries labels so a `wispd` restart can rebuild state
from Docker alone: `wisp.contract` (id), `wisp.expires_at` (Unix seconds),
`wisp.cpus` / `wisp.memory_mb` (post-clamp reservation), `wisp.gpus`
(comma-joined device IDs, only when GPUs are held), and `wisp.external_id`
(only when supplied). On startup, before the reaper runs, Wisp lists every
container with `wisp.contract`: **running** ones with well-formed labels are
re-adopted in `ready` with a **fresh** contract token (the old one died with the
previous process; `meta` and `isolation` do not survive), and their cpus /
memory / GPUs are re-reserved; **non-running** ones and any container with a
missing or malformed `wisp.contract` / `wisp.expires_at` label are force-removed
rather than adopted.

## Event bus

A dumb in-process pub/sub layer for coordination between satellites (see
[`docs/DESIGN.md` §6](docs/DESIGN.md)). Payloads are opaque: Wisp routes on an
event's `type` string but never interprets it or the body. Delivery is
fire-and-forget: each subscriber has a 64-event buffer and a subscriber that
falls behind has events dropped (and logged) rather than stalling publishers.
There is no persistence or replay.

- `POST /events`: publish an opaque event: `{"type":"...","data":{...}}` returns
  `202 {"status":"published"}`; an empty `type` is `400`. Gated by the app-level token.
- `WS /events`: subscribe to the live fan-out (gated by the app-level token via
  `Authorization: Bearer` or `?token=`). An optional `?type=` query filters to
  one or more comma-separated types; with no filter every event is delivered.
  Events arrive as JSON text messages `{"type":"...","data":...}`.

Wisp publishes its own contract lifecycle events on the same bus:
`contract.created`, `contract.ready`, `contract.expiring`, `contract.released`,
`contract.expired`. Each carries `{"contract_id":"...","status":"..."}`.
`contract.expired` additionally carries a `reason` field so a subscriber can
distinguish a provisioning failure (`provisioning_failed`, published by the
create path when image pull, container create/start, or userdata fails) from a
natural TTL expiry (`ttl_expired`) or an out-of-band container death
(`container_died`, detected by the reaper's liveness sweep). The other
lifecycle events omit the field.

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
