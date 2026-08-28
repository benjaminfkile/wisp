# Isolation Model: Testing Record

**Scope:** the tiered isolation feature across the wisp stack (wisp, wisp-agent, wisper-api),
implemented on branch `secure-lease-isolation`. This document records what has been tested, how,
the bugs that testing found, and what remains to test.

Last updated: 2026-08-28 (model description refreshed against the code; test matrix unchanged since 2026-08-07).

## The model under test

- A lease requests an **ordered isolation level**: `shared` < `sandboxed` < `vm`
  (`confidential` is reserved and rejected as "not supported yet"; unknown values are rejected).
  Omitted → the host's effective default (`default_isolation`, `shared` unless configured otherwise).
- **Capability = policy ∩ host.** A host advertises the *effective* set = the operator's allowed
  levels (`WISP_CONFIG` `isolations`) intersected with what the daemon can actually run (detected
  from Docker `Info`: `shared` always; `sandboxed` iff `runsc` registered; `vm` iff a Kata runtime
  (`kata-runtime` or `kata`) is registered on Linux **or** the daemon is in Windows mode).
  Advertised on `GET /images` under `isolation:{supported,default}`.
- **Fail loud, never downgrade** (added after this record's test runs): a policy-allowed level the
  host cannot run is dropped with a startup warning; if the configured default was dropped the
  effective default is `shared` only when the operator allow-listed it, else the first runnable
  configured level; and if *none* of the configured levels are runnable (and `shared` was excluded)
  wisp logs an error and advertises an **empty** `supported` list instead of falling back to
  `shared`. `POST /contracts` on that degraded host is refused with `409 no runnable isolation
  tier`, whether the request names an isolation or omits it, because omitting `isolation` would
  otherwise default to the empty string and silently downgrade to `runc` at launch.
- **Level → launch mechanism** (`internal/runtime/docker.go` `launchMechanism`):
  `shared`→ default runc; `sandboxed`→ `HostConfig.Runtime="runsc"`; `vm`→ `HostConfig.Runtime="kata-runtime"`
  on Linux or `HostConfig.Isolation="hyperv"` on Windows. Never both `Runtime` and `Isolation`.
  (Detection accepts the short `kata` alias but launch always names `kata-runtime`, so a daemon that
  registered Kata only as `kata` will advertise `vm` and then fail the launch; register `kata-runtime`.)
- **Per-OS hardening:** `no-new-privileges` (SecurityOpt) and `PidsLimit` are Linux-only; a Windows
  daemon rejects them, so they are applied only on a Linux daemon. `NanoCPUs`/`Memory` are cross-platform.
- End to end, `isolation` rides the tunnel `LeaseCreate` frame (JSON key `isolation`) from wisper-api
  → agent → wisp; hosts report supported levels upward via the tunnel hello/heartbeat
  (`isolation_levels`, `default_isolation`), which wisper-api advertises in `GET /v1/catalog`.

## Test matrix

| Behavior | Windows (Docker Desktop) | Linux runc (Docker Desktop) | Linux gVisor (EC2 t4g arm64) |
|---|---|---|---|
| `shared` launches a real container | ✅ | ✅ | ✅ (implicit) |
| Linux-only hardening applied + kernel-enforced | n/a (correctly dropped) | ✅ `pids.max=512` inside container | ✅ |
| `sandboxed` launches under gVisor (`runsc`) | ⛔ correctly reported unavailable | ⛔ correctly reported unavailable | ✅ **proven separate kernel** |
| `vm` launches | ✅ Hyper-V | needs Kata (unavailable) | needs Kata (unavailable) |
| Capability detection (policy ∩ host) | ✅ `[shared,vm]` | ✅ `[shared]` | ✅ `[shared,sandboxed]` |
| `egress` = dedicated ICC-disabled bridge | n/a | ✅ `enable_icc=false` | not run |
| Full marketplace tunnel (api→agent→wisp→Docker) | ✅ | not run (same code path) | not run |
| Rejections (`confidential`/unknown/disallowed) propagate | ✅ | ✅ | ✅ |
| `vm` on Linux via Kata | ⛔ needs bare-metal KVM | ⛔ needs bare-metal KVM | ⛔ needs bare-metal KVM |

## How the tests were run

All tests drove the real HTTP APIs against a real Docker daemon and inspected the actual containers
(`docker inspect`) plus ran commands *inside* the leased container via wisp's `exec` endpoint to
prove behavior from the guest's point of view. No mocks.

### Environment A, Windows Docker Desktop (Windows-container mode)

Build & run: `go build -o wispd.exe ./cmd/wispd`; `WISP_ADDR=127.0.0.1:18080 ./wispd.exe`.
Base image `wisp-base-windows` (already built).

Validated:
- Rejections via `POST /contracts`: `confidential` → 400 "not supported yet"; unknown level → 400
  "unknown isolation level (want shared, sandboxed, or vm)"; a level not in the policy allow-list → 400 "not allowed".
- `shared` lease launched a real Windows container. `docker inspect` confirmed `SecurityOpt=[]` and
  `PidsLimit=<nil>` (Linux-only hardening correctly dropped on Windows), running under `io.containerd.runhcs.v1`.
- With `WISP_CONFIG` allowing `[shared,sandboxed,vm]`, `GET /images` advertised `[shared,vm]`, `sandboxed` dropped by the intersection (no runsc on a Windows daemon). `sandboxed` → 400 "not allowed".
- `vm` lease launched with `HostConfig.Isolation=hyperv` (forced).

Full tunnel (marketplace path): `wisper-api` run in Development + in-memory
(`ASPNETCORE_ENVIRONMENT=Development`, `Tunnel__EnableDevEndpoints=true`, `Tunnel__HostTokens__<token>=dev-host-1`,
no connection string) on `:18090`; `wisp-agent` with `--manager ws://127.0.0.1:18090/agent
--host-token <token> --wisp http://127.0.0.1:18080`. Then `POST /dev/leases {hostId, isolation:"vm"}`
relayed api→agent→wisp→Docker and produced a running `hyperv` container (`status: ready`).
`sandboxed`/`confidential` requests returned the wisp 400 back through the tunnel (HTTP 502 carrying the message).

### Environment B, Linux Docker Desktop (runc)

Switched Docker Desktop to Linux containers. Built the Alpine base: `docker build -t wisp-base ./examples/wisp-base`.
`WISP_CONFIG` allowed `isolations:[shared,sandboxed,vm]` and `networks:[none,open,egress]`.

Validated:
- `GET /images` → `os:linux`, `isolation.supported:[shared]` (sandboxed/vm dropped, no runsc/kata).
- `shared` lease: `docker inspect` → `Runtime=runc`, `SecurityOpt=[no-new-privileges:true]`,
  `PidsLimit=512`, the hardening the Windows daemon rejects is **applied** here.
- `exec` inside the container: `cat /sys/fs/cgroup/pids.max` → `512` (the PID cap is live and
  kernel-enforced, not just set on HostConfig); `/etc/os-release` → Alpine Linux.
- `egress` lease: container attached to the dedicated `wisp-egress` network; `docker network inspect`
  → `com.docker.network.bridge.enable_icc=false`, `Internal=false` (no inter-container traffic, egress allowed).
- `sandboxed` → 400 "not allowed" (no runsc).

### Environment C, AWS EC2 `t4g.medium` (Amazon Linux 2023, arm64), gVisor

Driven entirely through **AWS SSM `send-command`** (no SSH; the instance is SSM-managed). wisp source
was shipped from a local clone via an **S3 presigned URL** (the repo was private at the time) and built on the box.
gVisor installed for `aarch64` and registered as a Docker runtime (`runsc install`; `systemctl restart docker`),
giving Docker runtimes `runc` + `runsc`. `wispd` launched as a transient systemd unit with `WISP_CONFIG`
allowing `[shared,sandboxed,vm]`.

Validated:
- `GET /images` → `isolation.supported:[shared,sandboxed]`, `sandboxed` lit up because `runsc` is now
  registered; `vm` stayed out (no Kata, not bare-metal). This proves capability detection tracks the
  actually-installed runtimes.
- `sandboxed` lease: `docker inspect` → `Runtime=[runsc]`, `SecurityOpt=[no-new-privileges:true]`, `Pids=512`, running.
- **Proof the guest runs on gVisor's kernel, not the host:** `exec` inside the container →
  `/proc/version` = `Linux version 4.19.0-gvisor`, `dmesg` = `Starting gVisor...`, while the host kernel
  is `6.1.156-...amzn2023.aarch64`.

## Bugs found by end-to-end testing (all fixed on the branch)

1. **Windows container creation was fully broken.** The new default `PidsLimit` (always non-zero) and
   the always-applied `no-new-privileges` SecurityOpt are both rejected by a Windows daemon
   (`Windows does not support PidsLimit`; `invalid security option`). Fixed by gating both on
   `d.containerOS == OSLinux` (`internal/runtime/docker.go`). Unit tests missed it because the fake
   runtime does not enforce Docker's platform constraints.
2. **Host capability advertisement was silently broken.** wisp-agent fetched capabilities from a
   non-existent `GET /capabilities`; wisp serves them on `GET /images` (nested under `isolation`), so
   every host fell back to advertising only `[shared]`. Fixed in wisp-agent to read `/images`.
3. **The `/dev/leases` test harness did not carry `isolation`.** Added the field to
   `wisper-api` `DevLeaseEndpoints` so the tunnel path can be exercised for testing.

## Tests still to do

- **`vm` / Kata on Linux.** The only untested launch path. Requires hardware virtualization (KVM),
  which normal EC2 instances do not expose, needs a bare-metal (`*.metal`) instance or another
  KVM-capable Linux host. Would validate `HostConfig.Runtime="kata-runtime"` → a real microVM.
- **`sandboxed` through the full marketplace tunnel.** `sandboxed` is proven directly against wisp on
  Linux+gVisor, and the tunnel is proven on Windows; not yet combined (drive a `sandboxed` lease
  through wisper-api → agent → a Linux+gVisor wisp).
- **Full tunnel on a Linux host** generally (only run against a Windows wisp so far; same code path).
- **Orphan reconcile across a real restart.** Unit-tested (`internal/server/reconcile_test.go`); an
  E2E check would kill/restart `wispd` with live labeled containers and confirm running ones are
  re-tracked (fresh token, cpus/memory/GPUs re-reserved) and stopped or malformed-label ones are
  force-removed rather than adopted.
- **Container-death detection.** Unit-tested (`internal/reaper`, `internal/server/reconcile_test.go`);
  an E2E check would `docker kill` / `docker rm` a leased container and confirm the contract expires
  and the `/images` capacity block drops within a sweep or two.
- **Concurrency / soak.** Many simultaneous leases, TTL-reaper behavior under load, mixed isolation levels.
- **CI integration tests.** Current E2E is manual; a scripted harness (ideally including a gVisor runner)
  would make this repeatable.
- **Confidential tier (`confidential`).** Not implemented, reserved and rejected today. Future work,
  requires TEE hardware + remote attestation.

## GPU isolation

GPU leasing (v1) is the first hardware dimension layered on top of this isolation model. It leases
**whole devices exclusively** and selects the attach mechanism from the isolation level the same
data-driven way this model selects the container runtime (see `docs/DESIGN.md` §7). Its isolation
posture:

- **What v1 ships, runc passthrough, the weakest boundary.** The only GPU backend is `shared`/runc,
  which attaches whole devices via Docker `DeviceRequests` (the SDK form of `docker run --gpus
  device=<id>`). This is passthrough with **no additional boundary**: the NVIDIA driver surface (the
  `/dev/nvidia*` device nodes and the kernel driver behind them) is exposed directly to the lease, so a
  GPU lease is only as isolated as `shared`/runc itself. GPU attach is therefore advertised at `shared`
  **only**, the capability block's `gpu.isolations` is at most `["shared"]` in v1.
- **What is planned, VM passthrough, with a possible middle tier.** The `vm` slot in the attach seam
  (`internal/runtime/gpu_attach.go`) exists but has no backend yet. The intended VM backend is **Kata
  Containers + VFIO whole-GPU passthrough**, which hands the physical device to a microVM guest, a far
  stronger boundary than runc passthrough. **gVisor `nvproxy`** is a possible middle tier: it
  intermediates the NVIDIA ioctl surface from a `sandboxed` guest without full VM cost. Adding either is
  implementing the strategy in the seam and flipping the corresponding entry of the policy attach map to
  `true`; nothing on the create path changes.
- **What cannot be verified in CI, no GPU hardware.** The CI/runner container has no GPU and no NVIDIA
  tooling, so none of the GPU paths run against real hardware here. The v1 test strategy is therefore
  **fakes-based**: `nvidia-smi` enumeration sits behind a `CommandRunner` seam exercised with canned
  CSV output; the allocator, the create-path validation (reject-not-clamp, the `409` exhaustion case),
  the `wisp.gpus` restart reconcile, and the attach seam (`DeviceRequests` for `shared`, the typed
  `ErrGPUAttachUnsupported` for `vm`) are all unit-tested without a daemon or a device. A real end-to-end
  GPU lease, like `vm`/Kata below, needs GPU hardware and is untested in CI. This record documents that
  gap rather than papering over it.

## Quick reproduction notes

- Local: build `wispd`, point `WISP_CONFIG` at a JSON policy that lists the `isolations` you want to
  allow, run `wispd`, then `curl` `POST /contracts` with `{"isolation":"<level>"}` and `docker inspect`
  the resulting container. Use `POST /contracts/{id}/exec` (Bearer the create token) to inspect the guest.
- gVisor: install `runsc` on a Linux host and `runsc install` to register the Docker runtime; wisp will
  then advertise `sandboxed`. (See Environment C.)
- Full tunnel: run `wisper-api` in Development with `Tunnel__EnableDevEndpoints=true` and a
  `Tunnel__HostTokens__<token>=<hostId>` mapping; run `wisp-agent` pointing `--manager` at it and
  `--wisp` at the local `wispd`; drive `POST /dev/leases`.
