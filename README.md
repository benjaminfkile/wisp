# Wisp

Wisp leases you an authenticated, root-access, throwaway container with a shell,
for a bounded time — then it vanishes. You bring your own tools.

This repository currently contains a minimal Go **hello-world**, used to verify
that grunt can clone, build, and test the repo end-to-end. The real
implementation follows the design doc.

## Build & run

```sh
go build ./...
go run .
```

## Test

```sh
go test ./...
```
