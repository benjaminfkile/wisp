# Wisp — Design Doc

**Status:** Draft / v0 · **Language:** Go · **Author:** Ben (with Claude)
**One-liner:** Wisp leases you an authenticated, root-access, throwaway container with a shell, for a bounded time — then it vanishes. You bring your own tools.

---

## 1. The idea

Wisp is a small, domain-blind **broker**. A client application asks Wisp for a **contract**: a
short-lived container it can drive live for an agreed duration (e.g. "give me a container for
1 hour"). Wisp boots a fresh container from a **bare base image** (a shell + a package manager,
nothing domain-specific), runs the client's provisioning script, and hands back a handle. The
client installs whatever it needs — Claude Code, git, a language runtime — itself. For the life
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
3. **Tool-agnostic (not even Claude).** The base image ships nothing domain-specific — just a shell
   and a package manager. The client installs its own toolchain (Claude Code, git, runtimes) via
   userdata or the shell. Wisp has no concept of "Claude" or a "prompt" at all; running an agent is
   just a command the client happens to run in its container. Wisp could host any workload.
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
- Not tool-aware at all: the base image ships no Claude Code, no git, no runtime — just a shell +
  package manager. Clients bring their own tools.
- Not a place where domain logic lives. If a feature needs to know *what* the work is, it's a satellite.

## 4. The contract model

A contract is the unit of everything. Requesting one:

```
POST /contracts
{
  "ttl_seconds": 3600,
  "preset": "coding",             // named launch config (§7)
  "userdata": "#!/bin/sh\napt-get install -y git ...",   // provisioning, runs at boot
  "meta": { ... }                 // opaque client tags, echoed back
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
| **Interactive shell** | `WS /contracts/:id/shell` | PTY, stateful, bidirectional | The Claude session; humans; anything interactive |
| **Sync exec** | `POST /contracts/:id/exec` | one process → `{stdout,stderr,exit_code}` | Machine-readable probes: `git diff`, `ls`, health checks |
| **Streaming exec** | `POST /contracts/:id/exec?stream=1` (SSE/chunked) | output flows live | Long-runners where you watch latency (the coding session) |

**How the shell works (the only non-trivial bit):** Docker's Engine API already gives an
interactive PTY over a *hijacked* connection (`exec create` with `Tty:true` + `exec start`). Wisp
bridges that duplex byte stream to a WebSocket — client bytes → container stdin, container stdout →
client. This is the same pattern every web terminal uses (Portainer, k8s dashboard, ttyd). In Go
it's a small read/write-loop bridge over the Docker SDK's hijacked stream and `gorilla/websocket`.

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

## 7. Permission presets

A **preset** is a named bundle of launch configuration, referenced by name when creating a
contract so clients don't pass raw Docker knobs:

- base image (the bare default, **or a custom pre-baked image** the client supplies — see below)
- resource limits (CPU, memory, pids)
- network policy (none / egress-only / open)
- which secrets/mounts are available
- max TTL allowed
- the flags the Claude session starts with (e.g. `--dangerously-skip-permissions`)

Example: a `coding` preset = read-write, network on, git-cred mount allowed; a `probe` preset =
no network, tighter limits, no secrets.

**Cold-start escape hatch.** Installing a toolchain (e.g. Node + Claude Code) on every contract
costs provisioning time. To skip it, a client builds its own image `FROM wisp-base` with its tools
baked in and points a preset at it. That image is just preset *configuration* — Wisp still never
knows what's inside. So: bare-and-generic by default, warm-and-ready by choice, and the
"Claude-ready" image is data, never part of Wisp.

## 8. Auth & security

- **Root = root *in the container*, not the host.** The host is protected by container isolation.
  Ephemeral + TTL-bounded + isolated is what makes handing out root acceptable.
- **Every contract gets a bearer token** (returned at creation). It's required on every `/exec`
  call and on the shell WebSocket handshake — because exec-into-container is arbitrary code
  execution, this is non-optional.
- **Creating contracts / using the bus** requires an app-level credential (per-satellite token).
- Wisp binds `127.0.0.1` by default; the OS user remains the outer boundary. Tokens gate the
  inner, cross-app surface.
- The preset caps blast radius (network, secrets, TTL) per contract.

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
GET    /healthz                       liveness

Auth: Authorization: Bearer <contract token> on contract-scoped calls;
      per-satellite app token to create contracts / use the bus.
```

## 11. End-to-end walkthrough — a "task runner" satellite

The old task-runner/scheduler/hierarchy stops being part of core and becomes one satellite:

1. Satellite gets a unit of work (however it decides — its own queue, a bus event, a human).
2. `POST /contracts { ttl_seconds: 3600, preset: "coding", userdata: "install node + claude code +
   git, configure credentials, clone <repo>, checkout <branch>" }` — or use a preset whose image
   already has the toolchain baked in, and userdata just clones + checks out.
3. On `contract.ready`, it drives the coding session on a **streaming exec**:
   `claude -p "<the task>" --dangerously-skip-permissions`.
4. While that streams, it **watches latency** and periodically fires a **sync exec**
   `cd /repo && git diff --stat`. Long silence + empty diff ⇒ it treats the run as stuck and reacts
   (retry, escalate, abort). *This is the old "Doctor" logic — now client-side, because the client
   has a live window into the work.*
5. On success it runs `git push` inside the contract (its own credentials), maybe POSTs a result
   event to the bus.
6. It `DELETE`s the contract (or lets the TTL reap it). Container gone; credentials gone.

Wisp knew none of: git, the repo, the branch, "a task," or Claude. It leased a box and brokered
access.

## 12. Windows + Linux containers (future / optional)

The base image exists per platform; Wisp selects by the Docker daemon's current mode. **Constraint:**
a single Docker daemon cannot run Windows and Linux containers simultaneously — on a Windows host
you switch modes; on a Linux host you can't run Windows containers at all. So a given Wisp instance
serves whichever mode its host is in. Treat Windows support as a later addition unless a concrete
use case needs it now.

## 13. Design constraints / gotchas (bake these in)

- **Each exec is a fresh process** — no shared cwd/env between calls. Clients send compound commands
  (`cd X && …`) or Wisp pins a per-contract working dir.
- **Bare ≠ empty** — the base must include a shell + package manager (e.g. `debian-slim`/`alpine`)
  or "install it yourself" is impossible; and the preset must allow network egress for installs.
- **Provisioning race** — reject/queue execs until `ready`; surface userdata failures.
- **TTL is a hard kill** — emit `contract.expiring` with enough lead time; clients must exfiltrate
  before then.
- **Output backpressure** — the shell/stream bridge must handle large/fast output without blowing
  memory (stream, don't buffer whole).
- **Reaping guarantees** — the reaper must destroy containers even across Wisp restarts (persist
  contract → container mapping; reconcile on boot).

## 14. Open questions

- Does `/exec` also return a declared **output file** on release, or is "push it yourself" enough?
- Event bus delivery guarantees: fire-and-forget vs at-least-once with replay?
- Preset definition: static config files vs a management API?
- Pooling: keep a warm base container ready to cut cold-start, or always cold?
- Multi-host later (one Wisp fanning across several Docker hosts), or strictly single-host?

---

*Wisp is deliberately small. The discipline is: if a decision requires knowing what the work
means, it belongs in a satellite, not in Wisp.*
