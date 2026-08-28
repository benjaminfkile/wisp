# Build Check

Build-environment smoke test for the Wisp runner, run before the real build.
Historical record: the package list and test timing below reflect the scaffold
at the time (a single package); `go build ./...` and `go test ./...` remain the
build and test commands.

## TEST 2: clean build on rebuilt image (2026-07-11)

Re-run on the **rebuilt runner image** that bakes in Go and a git `safe.directory`
config. Goal: confirm a clean Go build works with **NO manual git workaround**.

- **Go version:** `go version go1.22.5 linux/amd64` (already on PATH; no install needed)
- **`go build ./...`:** passed (exit 0)
- **`go test ./...`:** passed, `ok  github.com/benjaminfkile/wisp  0.002s`
- **Git workaround:** none. No `git config ... safe.directory` was run.
- **Dubious ownership:** no VCS-stamping / "dubious ownership" error. The image
  fix took, the checkout is trusted out of the box.

Result: build + test are green on the rebuilt image with no manual git workaround.
