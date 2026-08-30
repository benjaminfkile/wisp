# Example `wisp-base-windows` image - the Windows counterpart to
# ../wisp-base, and the built-in default on a windows-mode daemon (see
# ../../internal/policy/policy.go and ../../README.md §"Contracts, images & limits").
#
# wisp runs a contract's main process (PID 1) as an OS-appropriate keep-alive -
# on Windows that is `cmd /c ping -t localhost >NUL` - and drives ALL real work
# through exec/shell, so the image needs no tooling of its own: `cmd.exe`, which
# every Windows base ships, is enough. Bring your own tools per lease via
# userdata, or bake them in here. This mirrors the Linux example's discipline:
# keep the base minimal and domain-blind (docs/DESIGN.md §7).
#
# Windows-container rule: the base image tag and the host must match. A given
# Docker daemon runs Windows OR Linux containers, never both at once, and a
# Windows container needs a Windows host whose OS build and isolation mode
# (process vs Hyper-V) are compatible with the image. Pick the servercore tag
# that matches your host (e.g. ltsc2022 for Windows Server 2022 / matching
# Windows 11 builds); a mismatch fails at run time. wisp DETECTS the daemon's
# mode and serves it - it never switches modes (docs/DESIGN.md §12).
#
# Build (on a Windows host in windows-container mode):
#   docker build -t wisp-base-windows examples/wisp-base-windows
FROM mcr.microsoft.com/windows/servercore:ltsc2022

# Nano Server is smaller but lacks a full cmd/PowerShell baseline; servercore
# gives a usable interactive shell. Everything else belongs in per-lease
# userdata, keeping the base image domain-blind (docs/DESIGN.md §7).
CMD ["cmd.exe"]
