# Example `wisp-base` image - the default entry in wisp's built-in image
# allow-list (see ../wisp.config.json and ../../README.md §"Contracts, images & limits").
#
# wisp runs a contract's main process as `tail -f /dev/null` (a keep-alive) and
# drives ALL real work through exec/shell, so the image only needs a POSIX shell
# at /bin/sh - which Alpine provides. Bring your own tools per lease via userdata,
# or bake them in here.
#
# Build:
#   docker build -t wisp-base examples/wisp-base
# (or just run scripts/run-local.sh, which builds it for you)
FROM alpine:3.20

# A usable interactive baseline. Everything else belongs in per-lease userdata,
# keeping the base image small and domain-blind (docs/DESIGN.md §7).
RUN apk add --no-cache bash ca-certificates

CMD ["/bin/sh"]
