# Wisp — Design Doc

**Status:** Draft / v0 · **Language:** Go · **Author:** Ben
**One-liner:** Wisp leases you an authenticated, root-access, throwaway container with a shell, for a bounded time — then it vanishes. You bring your own tools.

---

## 1. The idea

Wisp is a small, domain-blind **broker**. A client application asks Wisp for a **contract**: a
short-lived container it can drive live for an agreed duration (e.g. "give me a container for
1 hour"). Wisp boots a fresh container from a **bare base image** (a shell + a package manager,
nothing domain-specific), runs the client's provisioning script, and hands back a handle. The
client installs whatever it needs — a language runtime, git, a build tool — itself. For the life
of the contract the client has **root inside that container** and drives it two ways:

- an **interactive shell** (a real PTY over a WebSocket — stateful: `cd`, env, `vim`, `top` all work), and
- **one-shot exec** calls (`POST .../exec` — fresh process, clean stdout/stderr/exit).

When the client releases the contract, or its TTL expires, Wisp **destroys the container**.
Nothing survives. Clients coordinate with each other over a separate **event bus**.

That's the whole product. Everything that looks like a feature — running coding tasks, watching
ADO/GitHub/Datadog, ordering work, self-healing failed runs — is a **satellite application** that
talks to Wisp over HTTP / WebSocket / the bus. Wisp itself never knows what any of it means.

## 2. Core principles

1. **Domain-blind.** Wisp knows nothing about tasks, repos, git, PRs, work items, or reviews. It
   leases containers and brokers access. All intent lives in satellites.
2. **Ephemeral by construction.** Containers are cattle, not pets. Destroyed at contract end. This
   buys isolation for free: no state drift, no cross-contract bleed, and any credentials a client
   installs die with the container.
3. **Tool-agnostic.** The base image ships nothing domain-specific — no runtime, no tool, no concept
   of a task; just a shell and a package manager. The client installs whatever it needs (a runtime,
   git, build tools) via userdata or the shell. Wisp has no concept of a "workload" or a "command"
   with meaning at all; running any job is just a command the client happens to run in its container.
   Wisp could host any workload.
4. **The client is in the loop.** Because a contract is a *live* lease (not a fire-and-forget job),
   the client supervises its own work — watches output stream in, probes the filesystem, reacts.
   This is what lets all the "is this healthy?" intelligence live in satellites.
5. **The bus and the session are different layers.** The event bus is async coordination *between
   apps* (control plane). The contract is a live held connection into one container (data plane).
   The session is never built on top of the bus.

## 3. What Wisp is NOT

- Not a task queue, scheduler, or hierarchy/phase engine.
- Not a git client and does not manage credentials — the client installs and uses those inside its
  own contract.
- Not an ADO / GitHub / Datadog integration.
- Not tool-aware at all: the base image ships no tool, no git, no runtime — just a shell +
  package manager. Clients bring their own tools.
- Not a place where domain logic lives. If a feature needs to know *what* the work is, it's a satellite.

## 4. The contract model

A contract is the unit of everything. Requesting one:

```
POST /contracts
{
  "ttl_seconds": 3600,            // required, > 0; clamped to limits.max_ttl_seconds (§7)
  "image": "wisp-base",           // optional; defaults to images.default, must be allow-listed (§7)
  "network": "open",              // optional; one of limits.networks (§7)
  "isolation": "shared",          // optional; one of limits.isolations; shared<sandboxed<vm (§7)
  "resources": { "cpus": 2, "memory_mb": 4096, "pids": 1024 },  // optional; clamped to limits (§7)
  "userdata": "#!/bin/sh\napt-get install -y git ...",   // provisioning, runs at boot
  "meta": { ... }                 // opaque client tags, echoed back on status reads
}
→ 201 { "contract_id": "...", "token": "...", "status": "provisioning" }
```

### Lifecycle (state machine)

```
requested → provisioning → ready → (in use) → expiring → released | expired → destroyed
                 │                                              ▲
          (userdata runs)                                (DELETE or TTL)
```

- **provisioning** — the container is up and the `userdata` script is running. Execs against the
  contract in this state are rejected (or block) until it's `ready`. If userdata exits non-zero the
  contract fails and the container is destroyed.
- **ready** — the client may exec / open shells freely.
- **expiring** — emitted as a warning event a configurable lead time before the TTL, so the client
  can exfiltrate work (push a branch, POST results out) before the hard kill.
- **released** — client called `DELETE /contracts/:id`.
- **expired** — TTL elapsed; Wisp kills in-flight execs and destroys the container.

Clients are responsible for getting artifacts **out** before the container dies (git push, upload,
POST to a satellite). Wisp returns command output but does not persist the filesystem.

## 5. Consumption modes

All three are `docker exec` under the hood with different flags. A running container accepts
**unlimited concurrent execs**, so Wisp never has to multiplex sessions itself — the client just
opens as many as it wants.

| Mode | Endpoint | Shape | Use |
|---|---|---|---|
| **Interactive shell** | `WS /contracts/:id/shell` | PTY, stateful, bidirectional | The live workload session; humans; anything interactive |
| **Sync exec** | `POST /contracts/:id/exec` | one process → `{stdout,stderr,exit_code}` | Machine-readable probes: `git diff`, `ls`, health checks |
| **Streaming exec** | `POST /contracts/:id/exec?stream=1` (SSE/chunked) | output flows live | Long-runners where you watch latency (the coding session) |

**How the shell works (the only non-trivial bit):** Docker's Engine API already gives an
interactive PTY over a *hijacked* connection (`exec create` with `Tty:true` + `exec start`). Wisp
bridges that duplex byte stream to a WebSocket — client bytes → container stdin, container stdout →
client. This is the same pattern every web terminal uses (Portainer, k8s dashboard, ttyd). In Go
it's a small read/write-loop bridge over the Docker SDK's hijacked stream and `gorilla/websocket`.

*Framing & resize.* Raw terminal bytes ride **binary** WebSocket messages in both directions —
that is the whole protocol for a client that never resizes, so pre-resize clients are unchanged.
Terminal resize is layered on as an optional out-of-band control channel: the client sends a
**text** message carrying a small JSON control frame `{"type":"resize","rows":<n>,"cols":<n>}`,
and Wisp forwards the new window size to the exec's TTY via Docker's `ContainerExecResize` instead
of writing it to stdin. A text message that is not a recognized control frame is still written to
stdin verbatim, so the addition is fully backward compatible (no resize sent ⇒ current behavior).

### Concurrency example (the motivating use case)

A satellite runs a coding session on the **streaming** exec and watches how long the response text
takes. While that streams, it fires a **sync** exec `cd /repo && git diff --stat` in parallel —
same container, separate process, no blocking. Empty diff + long silence ⇒ the satellite decides
the run is stuck and acts. Wisp did nothing special; Docker multiplexed the two execs.

## 6. The event bus

A separate, dumb pub/sub layer for **coordination between satellites**. Wisp is a relay:

- Clients `POST /events` to publish; clients subscribe via `WS /events` (or SSE) with a filter.
- Wisp emits its own **contract lifecycle** events: `contract.created`, `contract.ready`,
  `contract.expiring`, `contract.released`, `contract.expired`.
- Payloads are opaque to Wisp. It does not interpret event `type` strings or bodies.
- Optional lightweight persistence (SQLite) for replay / at-least-once; may start in-memory.

The bus kicks work off and announces lifecycle. The actual work happens over a contract's live
session, never as bus round-trips.

## 7. Image allow-list + limits (operator config)

Wisp is **domain-blind** (§2, §3): it has no opinionated, tool-aware presets. Instead the operator
owns a small, data-driven **config** — an image **allow-list**, a **default image**, and optional
**limits** — and the client picks an allowed image and shapes network / resources **per request**.
Userdata owns everything *inside* the container.

The config is a JSON file whose path comes from `WISP_CONFIG`. When unset, Wisp uses safe built-in
defaults (allow-list of just the bare base image, networks `none` + `open`, isolation `shared` only,
and conservative resource/TTL ceilings: TTL 3600 s, 4 CPUs, 4096 MB, 512 pids).

```jsonc
{
  "images": { "allow": ["wisp-base"], "default": "wisp-base" },
  "limits": {
    "max_ttl_seconds": 3600,   // 0 / omitted ⇒ no cap; built-in default 3600
    "max_cpus": 4,             // fraction of host cores; 0 ⇒ no cap; built-in default 4
    "max_memory_mb": 4096,     // 0 ⇒ no cap; built-in default 4096
    "pids_limit": 512,         // 0 ⇒ no cap; built-in default 512
    "networks": ["none", "open"],        // which of none/open/egress a client may request
    "isolations": ["shared"],            // which of shared/sandboxed/vm a client may request
    "default_isolation": "shared"        // applied when a create omits isolation
  }
}
```

On load Wisp validates: the allow-list is non-empty, the default image is in it, every network is one
of `none` / `open` / `egress`, and every configured isolation level and the default are valid
(`shared` / `sandboxed` / `vm`). An example lives at `examples/wisp.config.json`.

At create time (§4) the client sends `image`, `network`, `isolation`, and `resources`:

- **image** — optional; defaults to `images.default`. Must be in the allow-list, else `400`.
- **network** — optional; defaults to `open` when allowed, else the first configured network. Must
  be one of `limits.networks`, else `400`. `none` disconnects the container from all networks;
  `open` boots on the runtime's default network; `egress` boots on a dedicated Wisp-managed bridge
  with inter-container communication disabled (`enable_icc=false`), so the container has outbound
  access but cannot reach other leases on the host.
- **isolation** — optional; one of `limits.isolations`, defaults to `default_isolation` (`shared`).
  Ordered `shared < sandboxed < vm` (`confidential` is reserved and rejected). The host maps the
  level to a container runtime at launch (`shared`→runc, `sandboxed`→gVisor/`runsc`, `vm`→Kata on
  Linux or Hyper-V on Windows) and rejects a level it cannot actually run.
- **resources** — optional `{cpus, memory_mb, pids}`; each value is **clamped down** to the matching
  configured maximum when that maximum is set.
- **ttl_seconds** — required; clamped down to `limits.max_ttl_seconds` when set.

Any consumer can discover what it may request via the unauthenticated `GET /images` (§10), which
returns `{ os, images, default, limits, isolation }`. `os` is the daemon's detected container OS mode
(`"linux"` or `"windows"`) so a consumer knows what this host serves; `default` is the
OS-appropriate base image (the Windows base on a windows-mode host, the Linux base otherwise);
`isolation` is `{ supported, default }`, the host's **effective** isolation posture — the operator
allow-list intersected with the levels the daemon can actually run, so a consumer only ever sees
(and requests) levels this host can launch. The host detects its runnable levels at startup from the
daemon's registered runtimes and OS (`shared` always; `sandboxed` when the gVisor runtime `runsc` is
registered; `vm` when a Kata runtime is registered on a Linux daemon or the daemon runs Windows
containers via Hyper-V), drops any policy-allowed level it cannot run with a startup warning, and
rejects a create requesting an unavailable level. A
Docker daemon is fixed in one mode by the operator — Wisp only detects it and never switches it —
so an image whose OS does not match is rejected. Wisp cannot know an arbitrary image's OS up front,
so it attempts the create and maps the daemon's OS/platform rejection to a clear
`this host is in <os> container mode; the requested image is not compatible` error (a `400`).

**Cold-start escape hatch.** Installing a toolchain on every contract costs provisioning time. To
skip it, a client builds its own image `FROM wisp-base` with its tools baked in and the operator
adds that tag to the allow-list. That image is just data — Wisp still never knows what's inside.
So: bare-and-generic by default, warm-and-ready by choice; the pre-baked image is configuration,
never part of Wisp.

## 8. Auth & security

- **Root = root *in the container*, not the host.** The host is protected by container isolation.
  Ephemeral + TTL-bounded + isolated is what makes handing out root acceptable.
- **Every contract gets a bearer token** (returned at creation). It's required on every `/exec`
  call and on the shell WebSocket handshake — because exec-into-container is arbitrary code
  execution, this is non-optional.
- **Creating contracts / using the bus** requires an app-level credential (per-satellite token).
- Wisp binds `127.0.0.1` by default; the OS user remains the outer boundary. Tokens gate the
  inner, cross-app surface.
- The operator config (image allow-list + limits) caps blast radius (image, network, TTL,
  resources) per contract; `GET /images` is the one unauthenticated discovery endpoint.
- **Create-time `env` injection** — `POST /contracts` accepts an optional, opaque `KEY->VALUE`
  `env` map that becomes the container's `Config.Env` (inherited by every exec/shell). It is
  write-only: never returned on `GET /contracts/:id` and never logged.
  - **TODO (harden):** lease `env` is currently passed as plaintext container environment
    (visible via `docker inspect` and to anything that can read the container config). A future
    task should deliver secret env via a tmpfs-backed file fed on the create call's stdin —
    mirroring grunt's secret-injection pattern — so secret values never appear in `docker inspect`
    or logs. v1 is plaintext, intended for local/trusted use only.

## 9. Architecture & stack

Single Go binary (daemon). Suggested pieces:

- **HTTP:** `net/http` + a light router (`chi` or std `ServeMux`).
- **Docker control:** official Docker SDK for Go (`github.com/docker/docker/client`) — container
  create/start/kill, `ContainerExecCreate` / `ContainerExecAttach` (native hijacked stream for the
  PTY bridge).
- **WebSocket:** `gorilla/websocket` (or `coder/websocket`).
- **Event bus:** in-process channels + goroutines fan-out; HTTP ingest + WS/SSE egress.
- **Persistence:** SQLite (`modernc.org/sqlite` or `mattn/go-sqlite3`) for contracts, tokens, and
  (optionally) events. Can begin in-memory.
- **TTL reaper:** a ticker goroutine that transitions `expiring`/`expired` and destroys containers.

```
             ┌─────────── satellites (any language) ───────────┐
             │  task-runner   ado-watcher   github-watcher  ... │
             └──────┬───────────────┬──────────────┬───────────┘
             HTTP/WS │           bus │          bus │
                     ▼               ▼              ▼
        ┌────────────────────────── Wisp (Go) ─────────────────────────┐
        │  contracts API   exec/shell bridge   event bus   TTL reaper   │
        │        │               │                                       │
        │        └──────── Docker Engine (SDK) ────────┐                 │
        └──────────────────────────────────────────────┼─────────────────┘
                                                        ▼
                        ephemeral containers  [ bare base: shell + pkg mgr ]
                        (root, TTL-bounded, client installs its own tools,
                         destroyed on release/expiry)
```

## 10. API surface (v0 sketch)

```
POST   /contracts                     create + boot + run userdata
GET    /contracts/:id                 status, time remaining
DELETE /contracts/:id                 release now (destroy container)
POST   /contracts/:id/exec            run a command (sync)         { command }
POST   /contracts/:id/exec?stream=1   run a command (streamed output)
WS     /contracts/:id/shell           interactive PTY console

POST   /events                        publish an event to the bus
WS     /events                        subscribe (with filter)
GET    /images                        os + allow-list + default + limits + effective isolation (unauthenticated, §7)
GET    /healthz                       liveness

Auth: Authorization: Bearer <contract token> on contract-scoped calls;
      per-satellite app token to create contracts / use the bus.
```

## 11. End-to-end walkthrough — a "task runner" satellite

The old task-runner/scheduler/hierarchy stops being part of core and becomes one satellite:

1. Satellite gets a unit of work (however it decides — its own queue, a bus event, a human).
2. `POST /contracts { ttl_seconds: 3600, image: "wisp-base", network: "open", userdata: "install
   a language runtime + git + build tools, configure credentials, clone <repo>, checkout <branch>" }`
   — or request an allow-listed image that already has the toolchain baked in, and userdata just
   clones + checks out.
3. On `contract.ready`, it drives the job on a **streaming exec**:
   `cd /repo && make build` (or whatever command the workload is).
4. While that streams, it **watches latency** and periodically fires a **sync exec**
   `cd /repo && git diff --stat`. Long silence + empty diff ⇒ it treats the run as stuck and reacts
   (retry, escalate, abort). *This is the old "Doctor" logic — now client-side, because the client
   has a live window into the work.*
5. On success it runs `git push` inside the contract (its own credentials), maybe POSTs a result
   event to the bus.
6. It `DELETE`s the contract (or lets the TTL reap it). Container gone; credentials gone.

Wisp knew none of: git, the repo, the branch, "a task," or the tool that ran it. It leased a box
and brokered access.

## 12. Windows + Linux containers

The base image exists per platform (`examples/wisp-base`, `examples/wisp-base-windows`); Wisp selects
by the Docker daemon's current mode. Wisp **detects** that mode from the daemon (`docker info` →
`OSType`), **advertises** the result as `os` in `GET /images`, and — when the daemon is in `windows`
mode — boots the Windows base by default and drives the container with the Windows keep-alive
(`cmd /c ping -t localhost >NUL` as PID 1) and a `cmd.exe` shell for exec/interactive sessions.
The **operator** sets the mode (the Docker Desktop engine switch, or daemon config); **Wisp never
switches it** — it only serves whichever mode the host is in.

**Constraints:**

- **One daemon, one mode at a time** — a single Docker daemon cannot run Windows and Linux containers
  simultaneously. On a Windows host you switch the engine between modes; a given Wisp instance serves
  whichever mode its host is in.
- **Windows containers require a Windows host** — on a Linux host you can't run Windows containers at
  all, so a Linux-hosted Wisp only ever reports `os: "linux"`.
- **OS version + isolation must be compatible** — the container's base-image OS build and the host's
  OS build, plus the isolation mode (process vs Hyper-V), must line up per Windows-container rules;
  a mismatch fails at launch. Wisp surfaces such a rejection as a clear OS-mismatch error rather than
  guessing or switching modes.

## 13. Design constraints / gotchas (bake these in)

- **Each exec is a fresh process** — no shared cwd/env between calls. Clients send compound commands
  (`cd X && …`) or Wisp pins a per-contract working dir.
- **Bare ≠ empty** — the base must include a shell + package manager (e.g. `debian-slim`/`alpine`)
  or "install it yourself" is impossible; and the requested network must allow egress for installs.
- **Provisioning race** — reject/queue execs until `ready`; surface userdata failures.
- **TTL is a hard kill** — emit `contract.expiring` with enough lead time; clients must exfiltrate
  before then.
- **Output backpressure** — the shell/stream bridge must handle large/fast output without blowing
  memory (stream, don't buffer whole).
- **Reaping guarantees** — the reaper destroys containers even across Wisp restarts. Each container
  is labeled with its contract id and expiry (`wisp.contract`, `wisp.expires_at`); on boot Wisp
  reconciles its in-memory store from the live containers' labels before the reaper starts, so a
  restart cannot orphan a lease (`internal/server/reconcile.go`).

## 14. Open questions

- Does `/exec` also return a declared **output file** on release, or is "push it yourself" enough?
- Event bus delivery guarantees: fire-and-forget vs at-least-once with replay?
- Image allow-list + limits config: static file (`WISP_CONFIG`, today) vs a management API?
- Pooling: keep a warm base container ready to cut cold-start, or always cold?
- Multi-host later (one Wisp fanning across several Docker hosts), or strictly single-host?

---

*Wisp is deliberately small. The discipline is: if a decision requires knowing what the work
means, it belongs in a satellite, not in Wisp.*
