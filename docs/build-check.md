# Build Check

Build-environment smoke test for the Wisp runner, run before the real build.

- **Go version:** `go version go1.22.5 linux/amd64` (already present on PATH; no install needed)
- **`go build ./...`:** passed
- **`go test ./...`:** passed — `TestGreeting` (`--- PASS: TestGreeting`)

Note: the runner needed `git config --global --add safe.directory /workspace` so `go build` VCS stamping wouldn't fail on the root-owned checkout; after that, build and test are green and the push path is validated.
