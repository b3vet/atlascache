# AtlasCache container image (FEAT-0032, ADR-0024).
#
# Two publishable stages:
#
#   docker build -t atlascache:dev .                        # distroless, no shell
#   docker build -t atlascache:dev-debug --target debug .    # busybox shell
#
# The debug variant exists so that "I cannot exec into this container" never
# becomes a reason to rebuild on Alpine and give up the security properties of
# the default image.

# ---------------------------------------------------------------------------
# Stage 1: build
# ---------------------------------------------------------------------------
# Pinned to the floor declared in go.mod (ADR-0011) rather than to `latest`.
# A release must produce the same bytes from the same tag, and a moving
# toolchain is the easiest way to lose that without noticing.
# --platform=$BUILDPLATFORM pins the builder to the machine doing the building,
# not to the image being built. Without it, `buildx --platform linux/amd64`
# from an arm64 host runs the whole Go toolchain under QEMU emulation -- minutes
# per architecture instead of seconds. With it, one native toolchain
# cross-compiles for every target, which for a CGO-free static binary produces
# exactly the same output.
FROM --platform=$BUILDPLATFORM golang:1.24-bookworm AS build

WORKDIR /src

# Dependencies first, so editing a .go file does not re-download the module
# cache. pkg/client comes with them: the root go.mod carries a `replace` to it
# (ADR-0015), and the module graph cannot be loaded while the directory it
# replaces is missing -- even for a build that imports nothing from it.
COPY go.mod go.sum ./
COPY pkg/client/go.mod ./pkg/client/
RUN go mod download

COPY . .

# Stamped by the release pipeline; the defaults are what a bare `docker build`
# produces, and they are deliberately the same strings the binary reports when
# nothing set them.
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

# Set by buildx, once per platform in a multi-arch build. They are what the
# native builder above cross-compiles *for*.
ARG TARGETOS
ARG TARGETARCH

# The flags are not incidental:
#
#   CGO_ENABLED=0  a static binary, which is the only kind the distroless
#                  static base can run at all -- it carries no libc.
#   -trimpath      no build-machine paths in the binary. This is also what
#                  makes a tag reproducible, and a signature over a
#                  non-reproducible artifact attests only that CI ran.
#   -s -w          drop the symbol table and DWARF, roughly a third of the size.
#
# They match the Makefile's exactly, so `--version` and `INFO server` report the
# same thing from a container as from `make build`.
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} GOWORK=off \
    go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
      -o /out/atlascache ./cmd/atlascache

# ---------------------------------------------------------------------------
# Stage 2: the shipped image
# ---------------------------------------------------------------------------
# distroless static: no shell, no package manager, CA certificates and timezone
# data present, and `nonroot` runs as uid 65532 (ADR-0024).
#
# Non-root is the security property, and it holds everywhere. What does *not*
# hold everywhere is the corollary people expect from it: whether uid 65532 can
# bind a port below 1024 depends on the runtime's
# net.ipv4.ip_unprivileged_port_start, which Docker commonly ships as 0 and a
# hardened runtime or a Kubernetes pod does not. So remapping the client port to
# 443 inside the container may work on a laptop and fail in a cluster. 6379 and
# 8080 work under both, which is the actual reason the defaults are fine; map a
# privileged port on the host side. docs/deployment.md has the table.
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

COPY --from=build /out/atlascache /atlascache

# 6379 is the RESP client port, 8080 the admin HTTP API.
#
# The admin API binds 127.0.0.1 by default (ADR-0023), which inside a container
# is reachable only from the container itself. That is the right default -- the
# admin API is strictly more powerful than the data port -- and it is why the
# HEALTHCHECK below runs inside the container rather than being probed from
# outside. A Kubernetes httpGet probe, a sidecar, or `curl` from the host all
# live in a different network namespace and need `admin.bind_addr: 0.0.0.0`
# **and** an `admin.token`; binding non-loopback without a token is refused at
# startup, on purpose. docker-compose.yml shows both together.
EXPOSE 6379 8080

# Distroless has no shell and no curl, so the binary probes itself. It reads the
# same configuration the server does, so it follows a changed admin port without
# being told. Readiness, not liveness: a container that reports healthy before
# it can serve makes a rolling update send traffic into a node with nothing to
# answer with.
#
# start-period covers startup, during which /health/ready answers 503 by design.
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/atlascache", "--health-check"]

USER nonroot:nonroot
ENTRYPOINT ["/atlascache"]

# ---------------------------------------------------------------------------
# Stage 3: the :debug variant
# ---------------------------------------------------------------------------
# Same binary, same uid, same entrypoint -- the only difference is busybox,
# which brings a shell and the usual handful of tools:
#
#   docker run --rm -it --entrypoint sh ghcr.io/b3vet/atlascache:0.1.0-debug
#   docker exec -it <container> sh
#
# It is a larger attack surface and is not what production should run. It exists
# so the choice is "use the debug tag for ten minutes" rather than "abandon
# distroless".
FROM gcr.io/distroless/static-debian12:debug-nonroot AS debug

COPY --from=build /out/atlascache /atlascache

EXPOSE 6379 8080

HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/atlascache", "--health-check"]

USER nonroot:nonroot
ENTRYPOINT ["/atlascache"]
