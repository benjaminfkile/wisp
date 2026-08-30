# Wisp: Design Doc

**Status:** Draft / v0 · **Language:** Go · **Author:** Ben
**One-liner:** Wisp leases you an authenticated, root-access, throwaway container with a shell, for a bounded time, then it vanishes. You bring your own tools.

---

## 1. The idea

Wisp is a small, domain-blind **broker**. A client application asks Wisp for a **contract**: a
short-lived container it can drive live for an agreed duration (e.g. "give me a container for
1 hour"). Wisp boots a fresh container from a **bare base image** (a shell + a package manager,
nothing domain-specific), runs the client's provisioning script, and hands back a handle. The
client installs whatever it needs, a language runtime, git, a build tool, itself. For the life
of the contract the client has **root inside that container** and drives it two ways:

- an **interactive shell** (a real PTY over a WebSocket, stateful: `cd`, env, `vim`, `top` all work), and
- **one-shot exec** calls (`POST .../exec`, fresh process, clean stdout/stderr/exit).

When the client releases the contract, or its TTL expires, Wisp **destroys the container**.
Nothing survives. Clients coordinate with each other over a separate **event bus**.

That's the whole product. Everything that looks like a feature, running coding tasks, watching
ADO/GitHub/Datadog, ordering work, self-healing failed runs, is a **satellite application** that
talks to Wisp over HTTP / WebSocket / the bus. Wisp itself never knows what any of it means.

## 2. Core principles

1. **Domain-blind.** Wisp knows nothing about tasks, repos, git, PRs, work items, or reviews. It
   leases containers and brokers access. All intent lives in satellites.
2. **Ephemeral by construction.** Containers are cattle, not pets. Destroyed at contract end. This
   buys isolation for free: no state drift, no cross-contract bleed, and any credentials a client
   installs die with the container.
3. **Tool-agnostic.** The base image ships nothing domain-specific, no runtime, no tool, no concept
   of a task; just a shell and a package manager. The client installs whatever it needs (a runtime,
   git, build tools) via userdata or the shell. Wisp has no concept of a "workload" or a "command"
   with meaning at all; running any job is just a command the client happens to run in its container.
   Wisp could host any workload.
4. **The client is in the loop.** Because a contract is a *live* lease (not a fire-and-forget job),
   the client supervises its own work, watches output stream in, probes the filesystem, reacts.
   This is what lets all the "is this healthy?" intelligence live in satellites.
5. **The bus and the session are different layers.** The event bus is async coordination *between
   apps* (control plane). The contract is a live held connection into one container (data plane).
   The session is never built on top of the bus.

## 3. What Wisp is NOT

- Not a task queue, scheduler, or hierarchy/phase engine.
- Not a git client and does not manage credentials, the client installs and uses those inside its
  own contract.
- Not an ADO / GitHub / Datadog integration.
- Not tool-aware at all: the base image ships no tool, no git, no runtime, just a shell +
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
  "resources": { "cpus": 2, "memory_mb": 4096, "pids": 1024, "gpus": 0 },  // optional; cpus/memory/pids clamped, gpus rejected-not-clamped (§7)
  "userdata": "#!/bin/sh\napt-get install -y git ...",   // provisioning, runs at boot
  "env": { "KEY": "value" },      // optional, opaque, write-only container environment (§8)
  "external_id": "...",           // optional opaque caller id (<= 128 bytes), persisted on the container, echoed on reads
  "meta": { ... }                 // opaque client tags, echoed back on status reads
}
→ 201 { "contract_id": "...", "token": "...", "status": "ready" }
```

The create call is synchronous: it boots the container, runs `userdata` to completion, and only
then returns, so a successful response always carries `status: "ready"`. A provisioning failure
(userdata exiting non-zero, image pull/create/start failing) destroys the container, marks the
contract `expired`, and returns `500`; an image the daemon rejects for this host's OS/platform
returns `400` (§7). Over-budget requests are `409` (§7).

### Lifecycle (state machine)

```
requested → provisioning → ready → (in use) → expiring → releasing → released
                 │                                              ↘
          (userdata runs)                                     expired → destroyed
                                                     (DELETE or TTL / death)
```

- **requested**: the contract exists but no container has been provisioned yet. A transient state a
  create passes through; it never appears in `GET /contracts`.
- **provisioning**: the container is up and the `userdata` script is running. Execs and shells
  against the contract in this state are rejected with `409` until it's `ready`. If userdata exits
  non-zero the contract is marked `expired` and the container is destroyed.
- **ready**: the client may exec / open shells freely.
- **expiring**: a warning state (and bus event) a configurable lead time before the TTL
  (`WISP_EXPIRING_LEAD_SECONDS`, default 60), so the client can exfiltrate work (push a branch,
  POST results out) before the hard kill. Exec and shell stay usable through the lead window
  (accepted in both `ready` and `expiring`) so a client can actually use the grace period; only
  once the contract reaches `releasing` or a terminal state do they return `409`.
- **releasing**: the transient, **non-terminal** fence a `DELETE /contracts/:id` installs BEFORE
  killing the container so the reaper's TTL / container-died sweeps cannot race the release. The
  reaper skips `releasing` contracts for a short release grace
  (`WISP_RELEASE_GRACE_SECONDS`, default 30 s), and inside that window the DELETE handler is the
  sole owner of the container kill and the terminal transition. Past the grace the release is
  presumed stuck (the handler died mid-release, or its request was cancelled before it reached
  the final mark-released transition) and the reaper expires the contract just like any other
  non-terminal one, so the lease's capacity and GPUs return to the allocators instead of leaking
  until restart; `expired` is therefore also a legal successor from `releasing`. A hung Docker
  daemon on the reaper's own `Kill` is bounded separately (see §13, "Release grace bounds the
  fence"), not by this grace. Exec and shell are `409` against a `releasing` contract
  (the container is being torn down). A `DELETE` against a contract already in `releasing` is an
  idempotent success (200 echoing the current `releasing` status) so a concurrent second DELETE
  does not double-kill or double-free. The state is internal to the release handler (it lives at
  most for one container-kill call under normal conditions) and is excluded from `GET /contracts`
  the same way `requested` is, so the list `status` enum stays `provisioning|ready|expiring`.
- **released**: client called `DELETE /contracts/:id`. A `DELETE` against a contract that is
  already `released` or `expired` is an idempotent success (200 echoing the terminal status) and
  never frees capacity or GPUs a second time. A `DELETE` whose fence-installing transition finds
  the contract already purged, or whose final `mark released` finds the store has since purged it,
  also returns 200 with a `released` status rather than 500.
- **expired**: TTL elapsed, provisioning failed, or the container died out of band (`docker kill` /
  `docker rm` / OOM, detected by the reaper's liveness sweep); Wisp destroys the container.

Legal transitions: `requested` may go to `provisioning`, `releasing`, or `expired`; `provisioning`
to `ready`, `releasing`, or `expired`; `ready` to `expiring`, `releasing`, or `expired`;
`expiring` to `releasing` or `expired`; `releasing` to `released` (the DELETE handler's success
path) or `expired` (the reaper's release-grace escape hatch: past the grace the reaper reclaims
a stuck release the same way it would any other non-terminal contract). The terminal states have
no outgoing transitions, and the store rejects anything else, which is what makes every terminal
side effect (freeing capacity and GPUs, publishing the lifecycle event) happen exactly once even
when a `DELETE` races the reaper. A terminal contract stays readable for one reaper sweep and is
then purged from the store, after which its id is `404`.

Clients are responsible for getting artifacts **out** before the container dies (git push, upload,
POST to a satellite). Wisp returns command output but does not persist the filesystem.

## 5. Consumption modes

All three are `docker exec` under the hood with different flags. A running container accepts
**unlimited concurrent execs**, so Wisp never has to multiplex sessions itself, the client just
opens as many as it wants.

| Mode | Endpoint | Shape | Use |
|---|---|---|---|
| **Interactive shell** | `WS /contracts/:id/shell` | PTY, stateful, bidirectional | The live workload session; humans; anything interactive |
| **Sync exec** | `POST /contracts/:id/exec` | one process → `{stdout,stderr,exit_code}` | Machine-readable probes: `git diff`, `ls`, health checks |
| **Streaming exec** | `POST /contracts/:id/exec?stream=1` (Server-Sent Events) | output flows live | Long-runners where you watch latency (the coding session) |

Both exec modes take `{ "command": "..." }`, run it through the container OS's shell (`/bin/sh -c`
on Linux, `cmd /c` on a Windows daemon) as a fresh process, and require the contract to be `ready`
or `expiring` (`409` otherwise). The interactive shell handshake follows the same rule: both
`ready` and `expiring` are accepted so a client can exfiltrate work during the lead window. Sync
exec returns `{stdout, stderr, exit_code}`; a non-zero exit code is still a `200`. Streaming exec
commits `200` with `Content-Type: text/event-stream` and then emits `chunk` events
(`{"stream":"stdout"|"stderr","data":"..."}`, JSON-escaped so newlines survive SSE framing), a
final `exit` event (`{"exit_code": n}`), or an `error` event if the exec fails mid-stream.

**How the shell works (the only non-trivial bit):** Docker's Engine API already gives an
interactive PTY over a *hijacked* connection (`exec create` with `Tty:true` + `exec start`). Wisp
bridges that duplex byte stream to a WebSocket, client bytes → container stdin, container stdout →
client. This is the same pattern every web terminal uses (Portainer, k8s dashboard, ttyd). In Go
it's a small read/write-loop bridge over the Docker SDK's hijacked stream and `gorilla/websocket`.

*Framing & resize.* Raw terminal bytes ride **binary** WebSocket messages in both directions, that is the whole protocol for a client that never resizes, so pre-resize clients are unchanged.
Terminal resize is layered on as an optional out-of-band control channel: the client sends a
**text** message carrying a small JSON control frame `{"type":"resize","rows":<n>,"cols":<n>}`,
and Wisp forwards the new window size to the exec's TTY via Docker's `ContainerExecResize` instead
of writing it to stdin. A text message that is not a recognized control frame is still written to
stdin verbatim, so the addition is fully backward compatible (no resize sent ⇒ current behavior).

### Concurrency example (the motivating use case)

A satellite runs a coding session on the **streaming** exec and watches how long the response text
takes. While that streams, it fires a **sync** exec `cd /repo && git diff --stat` in parallel, same container, separate process, no blocking. Empty diff + long silence ⇒ the satellite decides
the run is stuck and acts. Wisp did nothing special; Docker multiplexed the two execs.

## 6. The event bus

A separate, dumb pub/sub layer for **coordination between satellites**. Wisp is a relay:

- Clients `POST /events` (`{"type":"...","data":...}`, `202`; empty `type` is `400`) to publish;
  clients subscribe via `WS /events` with an optional `?type=a,b` filter and receive each event as
  a JSON text message. Both are gated by the app-level token (§8); the WebSocket handshake also
  accepts `?token=`.
- Wisp emits its own **contract lifecycle** events: `contract.created`, `contract.ready`,
  `contract.expiring`, `contract.released`, `contract.expired`, each carrying
  `{"contract_id":"...","status":"..."}`. `contract.expired` additionally carries a `reason`
  field so a subscriber can distinguish a provisioning failure (`provisioning_failed`, published
  by the create path when image pull, container create/start, or userdata fails) from a natural
  TTL expiry (`ttl_expired`) or an out-of-band container death (`container_died`, detected by the
  reaper's liveness sweep). The other lifecycle events omit the field.
- Payloads are opaque to Wisp. It does not interpret event `type` strings or bodies.
- Delivery is fire-and-forget, in-process fan-out: each subscriber has a 64-event buffer and a
  subscriber that falls behind has events dropped (and logged) rather than stalling publishers.
  There is no persistence or replay today (an SQLite-backed at-least-once option remains an open
  question, §14).

The bus kicks work off and announces lifecycle. The actual work happens over a contract's live
session, never as bus round-trips.

## 7. Image allow-list + limits (operator config)

Wisp is **domain-blind** (§2, §3): it has no opinionated, tool-aware presets. Instead the operator
owns a small, data-driven **config**, an image **allow-list**, a **default image**, and optional
**limits**, and the client picks an allowed image and shapes network / resources **per request**.
Userdata owns everything *inside* the container.

The config is a JSON file whose path comes from `WISP_CONFIG`. When unset, Wisp uses safe built-in
defaults (allow-list of the two bare base images, `wisp-base` and `wisp-base-windows`, so one
default serves either daemon OS, with `wisp-base` as the default image; networks `none` + `open`;
isolation `shared` only; and conservative resource/TTL ceilings: TTL 3600 s, 4 CPUs, 4096 MB,
512 pids).

```jsonc
{
  "images": { "allow": ["wisp-base"], "default": "wisp-base" },
  "limits": {
    "max_ttl_seconds": 3600,   // 0 / omitted ⇒ no cap; built-in default 3600
    "max_cpus": 4,             // fraction of host cores; 0 ⇒ no cap; built-in default 4
    "max_memory_mb": 4096,     // 0 ⇒ no cap; built-in default 4096
    "pids_limit": 512,         // 0 ⇒ no cap; built-in default 512
    "max_contracts": 0,        // host budget: concurrent contracts; 0 / omitted ⇒ unlimited
    "total_cpus": 0,           // host budget: aggregate CPU across leases; 0 / omitted ⇒ unlimited
    "total_memory_mb": 0,      // host budget: aggregate memory across leases; 0 / omitted ⇒ unlimited
    "max_gpus": 0,             // per-lease GPU cap; 0 / omitted ⇒ no cap (all detected)
    "gpus_disabled": false,    // true turns GPU leasing off entirely; omitted ⇒ enabled
    "networks": ["none", "open"],        // which of none/open/egress a client may request
    "isolations": ["shared"],            // which of shared/sandboxed/vm a client may request
    "default_isolation": "shared"        // applied when a create omits isolation
  }
}
```

The file is parsed strictly (unknown fields are a load error). On load Wisp validates: the
allow-list is non-empty, the default image is in it (an omitted default falls back to the first
allowed image), every network is one of `none` / `open` / `egress` (an omitted list falls back to
`none` + `open`), every configured isolation level and the default are valid (`shared` /
`sandboxed` / `vm`; omitted values fall back to `shared`), `max_gpus` is not negative, and the host
capacity budgets are not negative with each total `>=` its matching per-lease max when both are
set. An example lives at `examples/wisp.config.json` (it lifts every numeric cap to `0`).

**Host capacity budgets.** `max_cpus` / `max_memory_mb` / `pids_limit` cap a **single lease**; the GPU
allocator enforces per-device exclusivity, but nothing bounds **aggregate** load across contracts (N
leases × `max_memory_mb` each can exceed real RAM, and contract count is unbounded). `max_contracts`,
`total_cpus`, and `total_memory_mb` are operator-declared **host budgets** on that aggregate: concurrent
contract count, total reserved CPU, and total reserved memory across all live leases. Each follows the
`0`/omitted ⇒ unlimited convention, and when a total and its matching per-lease max are both set the
total must be `>=` the per-lease max (a budget below one lease could never admit anything). These
budgets are **advertised** on `GET /images` (the `capacity` block below) and **enforced** at create
time by a shared aggregate allocator: a create reserves its post-clamp cpus/memory and a contract slot
against all three budgets all-or-nothing, and an exhausted budget is a **`409`** carrying an `at capacity`
error (distinct from the GPU `409`). The reservation is the post-clamp resources actually applied to the
container, an omitted dimension reserves the per-lease max when one is configured, else `0`, so an
unbounded container on an unbudgeted dimension stays uncounted, and it is returned on release, TTL
expiry, out-of-band container death, or a provisioning failure. Reserved usage **survives** a wispd
restart: the post-clamp cpus / memory are written to the `wisp.cpus` / `wisp.memory_mb` container
labels at create time and the startup reconcile (§13) re-reserves them (unconditionally, so usage may
legitimately sit above a since-lowered budget until those leases drain). The capacity reservation is
taken before GPU allocation, and a GPU rejection or store failure frees it again, so a rejected
create strands nothing.

**GPUs.** GPU leasing is an operator-gated, host-detected dimension, computed the same data-driven way
as isolation. `max_gpus` caps the GPUs a single lease may request (`0`/omitted ⇒ no operator cap → all
detected devices, mirroring the other `max_*` limits); `gpus_disabled: true` turns GPU leasing off
entirely regardless of hardware (omitted ⇒ enabled, the default). At startup Wisp detects GPU support, the daemon must advertise the `nvidia` runtime **and** `nvidia-smi` must enumerate at least one device, intersects it with the operator config, and advertises the result on `GET /images`; a detection failure
degrades to unsupported (a startup log line, never a fatal error). The full v1 GPU model, detection,
the capability block's wire shape, the create-path semantics, the exclusive allocator, restart
reconcile, and the per-launch-mechanism attachment seam, is the **GPU leasing (v1)** subsection below.

At create time (§4) the client sends `image`, `network`, `isolation`, and `resources`:

- **image**, optional; defaults to `images.default`. Must be in the allow-list, else `400`.
- **network**, optional; defaults to `open` when allowed, else the first configured network. Must
  be one of `limits.networks`, else `400`. `none` disconnects the container from all networks;
  `open` boots on the runtime's default network; `egress` boots on a dedicated Wisp-managed bridge
  with inter-container communication disabled (`enable_icc=false`), so the container has outbound
  access but cannot reach other leases on the host.
- **isolation**, optional; one of `limits.isolations`, defaults to `default_isolation` (`shared`).
  Ordered `shared < sandboxed < vm` (`confidential` is reserved and rejected). The host maps the
  level to a container runtime at launch (`shared`→runc, `sandboxed`→gVisor/`runsc`, `vm`→Kata on
  Linux or Hyper-V on Windows) and rejects a level it cannot actually run.
- **resources**, optional `{cpus, memory_mb, pids, gpus}`; `cpus` / `memory_mb` / `pids` are each
  **clamped down** to the matching configured maximum when that maximum is set, and an omitted
  (zero) value **inherits** that maximum so a lease that omits a dimension is still bounded (with no
  maximum configured, zero means no cap). `gpus` is rejected rather than clamped (see GPU leasing).
- **ttl_seconds**, required and positive; clamped down to `limits.max_ttl_seconds` when set.

Any consumer can discover what it may request via the unauthenticated `GET /images` (§10), which
returns `{ os, images, default, limits, isolation, gpu, capacity }`. The `gpu` block is **always present** and
carries the host's effective GPU posture: `{ supported, devices, max_gpus, isolations }`, `supported`
is whether GPUs may be leased at all; `devices` is the enumerated GPUs (`[{ id, class, vram_mb }]`,
`class` a normalized product name like `nvidia-geforce-rtx-4090`); `max_gpus` is the effective per-lease
cap (`min(operator cap, detected count)`); and `isolations` lists the isolation levels at which GPU
attach is available (**at most `["shared"]`** in v1, computed as data so a future VM/Kata passthrough
backend needs only to flip that slot). An unsupported host reports `supported:false`, `devices:[]`,
`max_gpus:0`. `os` is the daemon's detected container OS mode
(`"linux"` or `"windows"`) so a consumer knows what this host serves; `default` is the
OS-appropriate base image (the Windows base on a windows-mode host, the Linux base otherwise);
`isolation` is `{ supported, default }`, the host's **effective** isolation posture, the operator
allow-list intersected with the levels the daemon can actually run, so a consumer only ever sees
(and requests) levels this host can launch. The host detects its runnable levels at startup from the
daemon's registered runtimes and OS (`shared` always; `sandboxed` when the gVisor runtime `runsc` is
registered; `vm` when a Kata runtime (`kata-runtime` or `kata`) is registered on a Linux daemon or
the daemon runs Windows containers via Hyper-V), drops any policy-allowed level it cannot run with a
startup warning, and rejects a create requesting an unavailable level. It **fails loud rather than
downgrading**: when the configured default is not runnable the effective default falls back to
`shared` only if the operator allow-listed `shared`, otherwise to the first runnable configured
level; and when *none* of the configured levels are runnable (and `shared` was excluded) the host
logs an error, advertises an **empty** `supported` list, and **rejects every `POST /contracts` with
`409 no runnable isolation tier`**, whether or not the request names an isolation, rather than
quietly serving the weakest tier the operator did not authorise. Omitting `isolation` on a degraded
host is refused for the same reason an explicit request is: defaulting to the empty string would
silently downgrade to `runc`. If the daemon info query itself fails, detection falls back to
the OS-derived set (`shared`, plus `vm` on a Windows daemon) with a warning. A
Docker daemon is fixed in one mode by the operator, Wisp only detects it and never switches it, so an image whose OS does not match is rejected. Wisp cannot know an arbitrary image's OS up front,
so it attempts the create and maps the daemon's OS/platform rejection to a clear
`this host is in <os> container mode; the requested image is not compatible` error (a `400`).

The `capacity` block is **always present** and carries the host's aggregate capacity posture:
`{ max_contracts, active_contracts, total_cpus, used_cpus, total_memory_mb, used_memory_mb }`, the
operator budgets (`max_contracts` / `total_cpus` / `total_memory_mb`, `0` ⇒ unlimited) alongside current
usage (`active_contracts` is the live non-terminal contract count; `used_cpus` / `used_memory_mb` are the
aggregate capacity allocator's live reserved totals across those leases). Like the `gpu` block its
snake_case field names are an **authoritative cross-repo wire contract**, wisp-agent forwards it
verbatim and wisper-api consumes it, so they are kept in lockstep across repos.

**Cold-start escape hatch.** Installing a toolchain on every contract costs provisioning time. To
skip it, a client builds its own image `FROM wisp-base` with its tools baked in and the operator
adds that tag to the allow-list. That image is just data, Wisp still never knows what's inside.
So: bare-and-generic by default, warm-and-ready by choice; the pre-baked image is configuration,
never part of Wisp.

### GPU leasing (v1)

GPUs are the first **host-detected, operator-gated hardware dimension** a lease can request. The v1
model is deliberately narrow, *whole-device exclusive leases on a single isolation tier*, and every
seam that a richer model would touch (VM passthrough, per-isolation attach) is present as **data**, so
growing it later is filling a slot, not rewiring the create path. It builds directly on the isolation
model above: GPU attach is selected from the isolation level the same data-driven way the container
runtime is, and the capability posture is computed as *policy ∩ host* exactly like the isolation
posture.

**The v1 model: whole devices, exclusive.** A GPU is leased as a *whole device*, two live contracts
never share a device ID. The marketplace above wisp only ever asks for a **count** (`resources.gpus`);
wisp turns that count into a set of specific, currently-free device IDs and reserves them exclusively
until the owning contract reaches a terminal state. There is no fractional / MIG / time-sliced sharing
in v1. Whole-device exclusivity is what makes the count billing-meaningful upstream and keeps the
allocator a plain set of opaque IDs.

**Detection (two halves, behind an interface).** A host supports GPU leasing iff **both** signals
agree: the daemon advertises the `nvidia` OCI runtime (the NVIDIA Container Runtime is installed and
registered, the daemon-side half, read from `docker info` runtimes), **and** `nvidia-smi` enumerates
at least one device (the hardware half). `nvidia-smi` is the only vendor tool wired in v1 and it sits
behind a narrow `CommandRunner` seam (`internal/runtime/gpu.go`): the enumeration+parse logic is
unit-tested with canned CSV output, so nothing shells out for real in tests, the CI/runner container
has no GPU and no NVIDIA tooling. Enumeration runs `nvidia-smi --query-gpu=uuid,name,memory.total
--format=csv,noheader,nounits` and parses each row into `{ID, Class, VRAMMB}`, where `Class` is the
product name normalized to an opaque lowercase-hyphenated label (`NVIDIA GeForce RTX 4090` →
`nvidia-geforce-rtx-4090`). Detection is **best-effort**: a missing daemon-info call, an absent
`nvidia-smi`, or an unparseable line all degrade to *unsupported* with a single startup log line, never a fatal error. wisp only bothers enumerating when the daemon reports the `nvidia` runtime, so a
GPU-less host avoids a needless shell-out and scary log line; the policy layer still re-derives support
authoritatively from the runtimes + devices data.

**Operator policy knobs.** Two config knobs gate leasing (see the `limits` block above), both optional
and both mirroring the `max_*`/"zero = no cap" convention of the other limits: `max_gpus` caps the GPUs a single lease may
request (`0`/omitted ⇒ no operator cap → all detected devices), and `gpus_disabled: true` turns GPU
leasing off entirely regardless of hardware (omitted ⇒ enabled, the default). `EffectiveGPU` intersects
these with the detected host facts to yield the **effective posture**: supported iff the host supports
GPUs *and* the operator did not disable leasing; effective per-lease cap = `min(operator cap, detected
count)`. Capacity the policy withholds (leasing disabled with GPUs present, or a cap below the device
count) is logged at startup, mirroring the isolation model's dropped-levels warning. A negative
`max_gpus` is a config-load error.

**The capability block on `GET /images`.** The discovery document (§10) always carries a `gpu` block, present even on a GPU-less host, with this exact wire shape:

```jsonc
"gpu": {
  "supported": true,                       // may GPUs be leased on this host at all?
  "devices": [                             // the enumerated GPUs (always an array, never null)
    { "id": "GPU-<uuid>", "class": "nvidia-geforce-rtx-4090", "vram_mb": 24564 }
  ],
  "max_gpus": 1,                           // effective per-lease cap = min(operator cap, detected count)
  "isolations": ["shared"]                 // isolation levels at which GPU attach is available
}
```

An unsupported host reports the empty shape `{ "supported": false, "devices": [], "max_gpus": 0,
"isolations": [] }`. `devices` and `isolations` are always non-null arrays so a consumer never has to
distinguish `null` from `[]`. `isolations` is the crux of the forward-compat design: it lists the
isolation levels at which a GPU can be attached, computed **as data** from a per-level attach map, and
in v1 it is **at most `["shared"]`**. This is the surface wisp-agent scrapes to report GPU capacity
upward, so the field names are the authoritative wire contract across repos.

**Create-path semantics: reject, don't clamp.** `resources.gpus` on `POST /contracts` (§4) is a count,
and unlike `cpus`/`memory_mb`/`pids` it is **rejected, never clamped**: those dimensions are shaped to
fit, but silently reducing a GPU count would misprice a billing-meaningful lease, so an over-ask is an
error the caller must see. A positive count is a **`400`** when the host has no GPU support, when it
exceeds the effective per-lease cap, or when GPU attach is **not available at the resolved isolation
level**, the last check reads the same per-level attach map the capability block advertises (v1: only
`shared`), so a GPU request at `sandboxed`/`vm` is refused at the create path and never reaches an
attach mechanism that has no backend. Isolation gating is therefore **data, not an inline
`isolation == "shared"` check** anywhere. Only after all three checks pass does the request consume real
capacity: the allocator hands out that many free device IDs. An exhausted allocator, the host
advertises GPUs but every device is already held by another live lease, is a **`409`** capacity
conflict, distinct from the `400` over-ask, because the ask itself was within limits. Allocation
happens last, immediately before the contract records the IDs, so a rejected create never strands a
device. The assigned device IDs are echoed on `GET /contracts/:id` as a `gpus` array (omitted for a
GPU-less lease).

**The allocator.** `internal/gpu.Allocator` is a concurrency-safe, whole-device exclusive allocator
over the detected inventory of opaque device-ID strings. It is deliberately **backend-agnostic**, it
carries no runc/Docker/VFIO assumptions, only IDs, so the same allocator serves today's runc path and
a future VM path unchanged. `Allocate(n)` is atomic (all `n` reserved in detection order, or none, with
an `*InsufficientDevicesError` the create path maps to the `409`); `Free` is idempotent, so a lease the
reaper sweeps after a release never double-frees; `Reserve` re-occupies specific IDs during startup
reconcile. A single shared allocator instance is threaded through the create path, every terminal path
(release, provision-failure, and the reaper's expiry hook), and the reconcile, that one shared
instance is what makes whole-device exclusivity hold across all of them.

**Restart reconcile via the `wisp.gpus` label.** A lease's assigned device IDs are written at create
time into a `wisp.gpus` container label (comma-joined, alongside `wisp.contract` and
`wisp.expires_at`; device IDs never contain a comma, so the join is unambiguous). On startup, after a
crash or restart, the reconcile (§13, `internal/server/reconcile.go`) lists surviving leased
containers, re-adopts each contract, and `Reserve`s its `wisp.gpus` devices back into the allocator, so a restarted wispd never re-hands a device a surviving lease still holds. A device ID the host no
longer detects (e.g. a GPU physically removed since the container launched) is logged and skipped
rather than crashing the reconcile. This must run before the reaper starts, exactly like the rest of
reconcile.

**The per-launch-mechanism attachment seam (the Kata intent).** Attaching a GPU is done *differently by
each launch mechanism*, exactly like the isolation level selects the container runtime (above). The
mapping lives in one place, `gpuAttachment(isolation, deviceIDs)` in `internal/runtime/gpu_attach.go`
,  kept structurally parallel to `launchMechanism`, so the two dimensions of a launch (its runtime and
its device attachment) are selected the same data-driven way. v1 ships exactly **one** GPU backend:
`shared`/runc, which attaches whole devices via Docker `DeviceRequests` naming the `nvidia` driver, the
explicit device IDs, and the `gpu` capability, the SDK equivalent of `docker run --gpus
device=<id>,<id>`. Naming explicit device IDs (not a count) is what makes the attach exclusive to the
allocator-chosen devices. The `vm` slot **exists** but has no backend yet, returning the typed
`ErrGPUAttachUnsupported`; that slot is the intended insertion point for **Kata Containers + VFIO
whole-GPU passthrough** as the VM backend. Because attach is never gated on `isolation == "shared"`
inline, adding VM-backed passthrough later is confined to two edits: implement the Kata + VFIO strategy
in this seam and flip the `vm` entry of the policy attach map (`gpuAttachByIsolation`) to `true`, the
capability block, the create-path gate, and every call site pick it up with no further change. (In v1
the create path already rejects a GPU request at any non-`shared` level, so the `vm` slot's error is
unreachable through the create path, but it is the honest typed answer for a mechanism that cannot
attach a GPU, and it is exercised directly by unit tests.)

**Isolation posture in v1.** Because the only GPU backend is `shared`/runc, GPU leasing is available at
**shared isolation only**, the weakest boundary, where the GPU driver surface is exposed to the lease.
The testing record (`docs/ISOLATION_TESTING.md`) documents what v1 ships, what is planned (Kata + VFIO;
gVisor `nvproxy` as a possible middle tier), and why none of it can be verified in CI (no GPU hardware).

## 8. Auth & security

- **Root = root *in the container*, not the host.** The host is protected by container isolation.
  Ephemeral + TTL-bounded + isolated is what makes handing out root acceptable.
- **Every contract gets a bearer token** (returned at creation). It's required on every `/exec`
  call and on the shell WebSocket handshake, because exec-into-container is arbitrary code
  execution, this is non-optional.
- **Creating contracts / using the bus** requires an app-level credential (per-satellite token).
- Wisp binds `127.0.0.1` by default; the OS user remains the outer boundary. Tokens gate the
  inner, cross-app surface.
- The operator config (image allow-list + limits) caps blast radius (image, network, TTL,
  resources) per contract; `GET /images` is the one unauthenticated discovery endpoint.
- **The contract list is privileged.** `GET /contracts` returns every live contract *with its
  current bearer token* so a restarted local agent can rebuild its lease map, so it is gated by the
  app token exactly like create. `GET /contracts/:id` and `DELETE /contracts/:id` accept either
  the app token or the contract's own token; when no app token is configured they are open, like
  everything else the app gate covers.
- **Restart re-keys leases.** A contract re-adopted from container labels after a wispd restart
  gets a fresh token (the old one died with the previous process); the agent recovers it from
  `GET /contracts`.
- **Create-time `env` injection**, `POST /contracts` accepts an optional, opaque `KEY->VALUE`
  `env` map that becomes the container's `Config.Env` (inherited by every exec/shell). It is
  write-only: never returned on `GET /contracts/:id` and never logged. It is bounded (at most 128
  entries and 256 KiB of `KEY=VALUE` text; keys must be non-empty and free of `=` and NUL; values
  free of NUL), and a bad map is a `400` with no contract created.
- **Hardening applied to every lease:** `no-new-privileges` and the PID limit on a Linux daemon
  (a Windows daemon rejects both, so they are skipped there); CPU and memory caps everywhere.
  - **TODO (harden):** lease `env` is currently passed as plaintext container environment
    (visible via `docker inspect` and to anything that can read the container config). A future
    task should deliver secret env via a tmpfs-backed file fed on the create call's stdin,     mirroring grunt's secret-injection pattern, so secret values never appear in `docker inspect`
    or logs. v1 is plaintext, intended for local/trusted use only.

## 9. Architecture & stack

Single Go binary (daemon). As built:

- **HTTP:** `net/http` with the standard library `ServeMux` (method + path patterns); JSON
  structured logging via `log/slog`, one line per request.
- **Docker control:** official Docker SDK for Go (`github.com/docker/docker/client`), container
  create/start/remove, `ContainerExecCreate` / `ContainerExecAttach` (native hijacked stream for the
  PTY bridge), `ContainerExecResize`, on-demand image pull, and a label-filtered container list for
  the restart reconcile.
- **WebSocket:** `gorilla/websocket`.
- **Event bus:** in-process channels + goroutines fan-out; HTTP ingest + WS egress.
- **Persistence:** none. Contracts and tokens live in memory; the container labels (§13) are the
  only durable state, and a restart rebuilds tracking from them. SQLite for contracts/events remains
  an option (§14).
- **TTL reaper:** a ticker goroutine (every `WISP_REAP_INTERVAL_SECONDS`, default 1 s) that
  transitions `expiring`/`expired`, expires contracts whose container died out of band, destroys
  containers, returns capacity and GPUs to the allocators, and purges terminal contracts from the
  store one sweep after they become terminal.

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
POST   /contracts                     create + boot + run userdata (optional env, external_id); 201 {contract_id, token, status}
GET    /contracts                     list every live contract: {"contracts":[{id, external_id, token, status, expires_at, ttl_seconds_remaining, reserved_cpus, reserved_memory_mb, gpus}]}
                                      status is one of provisioning|ready|expiring (terminal contracts and the transient `requested` / `releasing` states are excluded; gpus is always an array)
GET    /contracts/:id                 {contract_id, status, ttl_seconds_remaining, gpus?, meta?, external_id}; status is provisioning|ready|expiring|releasing|released|expired (releasing is non-terminal); 404 once purged
DELETE /contracts/:id                 release now (destroy container); same body as GET; idempotent 200 on an already-terminal contract, and 200 echoing status=releasing when a concurrent release is already in flight
POST   /contracts/:id/exec            run a command (sync)         { command } -> {stdout, stderr, exit_code}; 409 unless ready
POST   /contracts/:id/exec?stream=1   run a command (Server-Sent Events: chunk / exit / error)
WS     /contracts/:id/shell           interactive PTY console (binary frames; text {"type":"resize","rows","cols"} control frame)

POST   /events                        publish an event to the bus; 202
WS     /events                        subscribe (optional ?type=a,b filter)
GET    /images                        os + allow-list + default + limits + effective isolation + gpu + capacity (unauthenticated, §7)
GET    /healthz                       liveness

Auth: Authorization: Bearer <contract token> on /exec; ?token= or a bearer.<token> subprotocol on /shell;
      GET /contracts and POST /contracts require the app token;
      GET /contracts/:id and DELETE /contracts/:id accept EITHER the app token
      or the per-contract token;
      the app token (header or ?token=) to use the bus.
      Missing/invalid credentials are 401. With no WISP_APP_TOKEN configured every app-token gate is open.
      Errors are {"error": "..."}.
```

## 11. End-to-end walkthrough, a "task runner" satellite

The old task-runner/scheduler/hierarchy stops being part of core and becomes one satellite:

1. Satellite gets a unit of work (however it decides, its own queue, a bus event, a human).
2. `POST /contracts { ttl_seconds: 3600, image: "wisp-base", network: "open", userdata: "install
   a language runtime + git + build tools, configure credentials, clone <repo>, checkout <branch>" }`
, or request an allow-listed image that already has the toolchain baked in, and userdata just
   clones + checks out.
3. On `contract.ready`, it drives the job on a **streaming exec**:
   `cd /repo && make build` (or whatever command the workload is).
4. While that streams, it **watches latency** and periodically fires a **sync exec**
   `cd /repo && git diff --stat`. Long silence + empty diff ⇒ it treats the run as stuck and reacts
   (retry, escalate, abort). *This is the old "Doctor" logic, now client-side, because the client
   has a live window into the work.*
5. On success it runs `git push` inside the contract (its own credentials), maybe POSTs a result
   event to the bus.
6. It `DELETE`s the contract (or lets the TTL reap it). Container gone; credentials gone.

Wisp knew none of: git, the repo, the branch, "a task," or the tool that ran it. It leased a box
and brokered access.

## 12. Windows + Linux containers

The base image exists per platform (`examples/wisp-base`, `examples/wisp-base-windows`); Wisp selects
by the Docker daemon's current mode. Wisp **detects** that mode from the daemon (`docker info` →
`OSType`), **advertises** the result as `os` in `GET /images`, and, when the daemon is in `windows`
mode, boots the Windows base by default and drives the container with the Windows keep-alive
(`cmd /c ping -t localhost >NUL` as PID 1) and a `cmd.exe` shell for exec/interactive sessions.
The **operator** sets the mode (the Docker Desktop engine switch, or daemon config); **Wisp never
switches it**, it only serves whichever mode the host is in.

**Constraints:**

- **One daemon, one mode at a time**, a single Docker daemon cannot run Windows and Linux containers
  simultaneously. On a Windows host you switch the engine between modes; a given Wisp instance serves
  whichever mode its host is in.
- **Windows containers require a Windows host**, on a Linux host you can't run Windows containers at
  all, so a Linux-hosted Wisp only ever reports `os: "linux"`.
- **OS version + isolation must be compatible**, the container's base-image OS build and the host's
  OS build, plus the isolation mode (process vs Hyper-V), must line up per Windows-container rules;
  a mismatch fails at launch. Wisp surfaces such a rejection as a clear OS-mismatch error rather than
  guessing or switching modes.

## 13. Design constraints / gotchas (bake these in)

- **Each exec is a fresh process**, no shared cwd/env between calls. Clients send compound commands
  (`cd X && …`) or Wisp pins a per-contract working dir.
- **Bare ≠ empty**, the base must include a shell + package manager (e.g. `debian-slim`/`alpine`)
  or "install it yourself" is impossible; and the requested network must allow egress for installs.
- **Provisioning race**, reject execs until `ready` (they stay usable through `expiring`); surface userdata failures.
- **TTL is a hard kill**, emit `contract.expiring` with enough lead time; clients must exfiltrate
  before then.
- **Output backpressure**, the shell/stream bridge must handle large/fast output without blowing
  memory (stream, don't buffer whole).
- **Reaping guarantees**, the reaper destroys containers even across Wisp restarts. Each container
  is labeled with its contract id and expiry (`wisp.contract`, `wisp.expires_at` as Unix seconds),
  its post-clamp reservation (`wisp.cpus`, `wisp.memory_mb`), its GPU devices (`wisp.gpus`,
  comma-joined, only when held), and the caller's `wisp.external_id` (only when supplied). On boot,
  before the reaper starts, Wisp lists every container carrying `wisp.contract` (running or not):
  a **running** container with well-formed labels is re-adopted in `ready` with a fresh token and
  its cpus / memory / GPUs re-reserved (a GPU id the host no longer detects is logged and skipped);
  a **non-running** one is a dead lease and is force-removed, never adopted; and a container with a
  missing or malformed `wisp.contract` / `wisp.expires_at` label is force-removed with a warning
  rather than left running unaccounted for. A failure to list containers is logged and the daemon
  still starts (`internal/server/reconcile.go`).
- **Container death is a terminal event**, every sweep, the reaper inspects each `ready` /
  `expiring` contract's container; one that has stopped or been removed out of band expires the
  contract through the normal path (capacity and GPUs freed exactly once). Paused or restarting
  containers still report running on the daemon side and are left alone; a transport error is
  inconclusive and retried next sweep.
- **Release grace bounds the fence**, the reaper skips a `releasing` contract for a short grace
  (`WISP_RELEASE_GRACE_SECONDS`, default 30 s) after the DELETE handler installed the fence, so
  the handler owns the tear down without the reaper racing it. Past the grace the release is
  presumed stuck (the handler died mid-release, or its request was cancelled before the final
  mark-released transition) and the reaper expires the contract like any other non-terminal one,
  returning its capacity and GPUs to the allocators so a stalled release never leaks a lease's
  reservation until wispd restarts.
- **Reaper Kill is bounded**, each reaper `Kill` runs under a 30 s timeout so a hung Docker
  daemon (a wedged `ContainerRemove`) cannot stall the sweep. On timeout the contract is left
  in place for the next tick to retry, and the sweep proceeds with the remaining contracts, so
  one wedged container never freezes reaping for the whole store or keeps other capacity
  reservations from returning to the allocators.

## 14. Open questions

- Does `/exec` also return a declared **output file** on release, or is "push it yourself" enough?
- Event bus delivery guarantees: fire-and-forget vs at-least-once with replay?
- Image allow-list + limits config: static file (`WISP_CONFIG`, today) vs a management API?
- Pooling: keep a warm base container ready to cut cold-start, or always cold?
- Multi-host later (one Wisp fanning across several Docker hosts), or strictly single-host?

---

*Wisp is deliberately small. The discipline is: if a decision requires knowing what the work
means, it belongs in a satellite, not in Wisp.*
