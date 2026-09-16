# Deployment

AtlasCache v0.1.0 is a **single node with no persistence**. Everything below
runs one process; there is no clustering and nothing survives a restart. Plan
for a cache, not a database.

- [The 60-second version](#the-60-second-version)
- [The container image](#the-container-image)
- [Two things that will catch you](#two-things-that-will-catch-you)
  - [The admin API binds localhost, and a probe cannot reach it](#the-admin-api-binds-localhost-and-a-probe-cannot-reach-it)
  - [Non-root, and what it does (and does not) mean for low ports](#non-root-and-what-it-does-and-does-not-mean-for-low-ports)
- [Docker Compose](#docker-compose)
- [Configuration in a container](#configuration-in-a-container)
- [Health checks](#health-checks)
- [Kubernetes](#kubernetes)
- [Hardening](#hardening)
- [Verifying what you pulled](#verifying-what-you-pulled)
- [Troubleshooting a container with no shell](#troubleshooting-a-container-with-no-shell)

## The 60-second version

```sh
docker run --rm -p 6379:6379 ghcr.io/b3vet/atlascache:0.1.0
redis-cli -p 6379 PING     # -> PONG
```

Or, from a clone:

```sh
docker compose up -d
redis-cli -p 6379 PING           # -> PONG
curl -s localhost:8080/health    # -> {"status":"ok"}
```

If 6379 is already taken — and on a machine that runs Redis, it is:

```sh
ATLASCACHE_CLIENT_PORT=6380 docker compose up -d
```

## The container image

| | |
|---|---|
| Registry | `ghcr.io/b3vet/atlascache` |
| Tags | `0.1.0`, `0.1.0-debug` |
| Base | `gcr.io/distroless/static-debian12:nonroot` |
| Platforms | `linux/amd64`, `linux/arm64` |
| Size | ~10MB |
| User | uid `65532`, no shell, no package manager |
| Ports | `6379` RESP, `8080` admin HTTP |

There is no `latest` tag. A `latest` on a pre-1.0 alpha is an invitation to run
it in production by accident.

The `-debug` variant is the same binary on a base image that has a busybox
shell. It exists so that "I can't exec into this container" never becomes a
reason to rebuild on Alpine and give up the properties of the default image.
It is a larger attack surface and is not what production should run.

## Two things that will catch you

### The admin API binds localhost, and a probe cannot reach it

The admin API defaults to `127.0.0.1` (ADR-0023). Inside a container that
means *this container only*: not a Docker health check run from the host, not a
Kubernetes `httpGet` probe, not a sidecar. A container left at the default has
an admin API nothing outside it can reach, and the symptom reads like a broken
health endpoint rather than a deliberate default.

The default is deliberate. The admin API is strictly more powerful than the
data port: `/stats` and `/config` disclose operational detail, and later
releases add rebalancing and backup triggers. It is not reachable from another
host unless you say so.

**To make it reachable, you need two settings, not one:**

```yaml
admin:
  bind_addr: "0.0.0.0"          # reachable from outside the container
  token: "a-real-secret"        # required, and refused if absent
```

Binding a non-loopback address **without** a token is a startup failure, on
purpose. Exposure plus no credential is almost always a mistake, and the server
refuses it rather than warning about it. If you see

```
admin.bind_addr is not a loopback address and admin.token is not set
```

that is the guard working, not a bug.

**What you are trading.** `0.0.0.0` exposes the admin API to every network the
container is attached to. Narrow it back:

- publish the admin port on loopback only — `-p 127.0.0.1:8080:8080`, which is
  what `docker-compose.yml` does;
- in Kubernetes, do not put port 8080 in a `Service`; probes reach the pod IP
  directly and need no Service;
- rotate `admin.token`, and keep it different from `auth.token` — a client
  holding a data token must not thereby gain administrative access, and the
  server refuses the two being equal.

**The narrower option.** If you do not need an external probe, leave
`bind_addr` at `127.0.0.1`. The image's own `HEALTHCHECK` runs *inside* the
container, where loopback is exactly the right address, so `docker ps` still
reports health. You lose external probes, `/stats` and `/config`.

Both halves of this are covered by the `docker-health` E2E spec: a configured
container answers probes from outside, and a default one does not — and is
healthy anyway.

### Non-root, and what it does (and does not) mean for low ports

The image runs as uid `65532`. That is the security property, it holds
everywhere, and it is what the `docker-nonroot` E2E spec asserts.

The familiar corollary — "a non-root container cannot bind a port below 1024" —
is **not reliably true**, and getting it backwards is worse than not knowing
it. Whether an unprivileged process may bind a low port depends on the
runtime's `net.ipv4.ip_unprivileged_port_start`, and runtimes disagree:

| Runtime | `net.ipv4.ip_unprivileged_port_start` | Can uid 65532 bind 443? | Can it bind 6379? |
|---|---|---|---|
| Docker, default | `0` | **yes** | yes |
| Docker with `--sysctl net.ipv4.ip_unprivileged_port_start=1024` | `1024` | no | yes |
| Kubernetes, unless the sysctl is allowed | `1024` | no | yes |
| Older kernels, hardened or rootless runtimes | `1024` | no | yes |

Read the last column first: **6379 and 8080 work under every one of them.**
That is the real reason the default ports are fine, and it does not depend on
anyone's sysctl.

The trap is the other column. `ATLAS_SERVER_CLIENT_PORT=443` will very likely
work on your laptop — Docker relaxes the sysctl — and then fail in a cluster:

```
failed to bind client port  error="listen on 0.0.0.0:443: bind: permission denied"
```

A configuration that works in development and fails in production is the worst
shape a configuration can have, so do not rely on either behaviour. Keep 6379
and 8080 *inside* the container and map the privileged port on the **host**
side, where the mapping is done by the runtime rather than by the server
process:

```sh
docker run -p 443:6379 ghcr.io/b3vet/atlascache:0.1.0
```

In Kubernetes, use a `Service` port; the container port stays 6379.

## Docker Compose

`docker-compose.yml` in the repository root brings up one node with a mounted
configuration:

```sh
docker compose up -d
docker compose logs -f
docker compose down
```

What it sets, and why:

| Setting | Reason |
|---|---|
| `./docker/atlascache.yaml` mounted at `/etc/atlascache/config.yaml` | A path the server searches by default, so no `--config` flag |
| `admin.bind_addr: 0.0.0.0` **and** `admin.token` | The section above |
| admin port published on `127.0.0.1` only | Narrows what `0.0.0.0` widened |
| `user: "65532:65532"` | An override elsewhere cannot quietly drop back to root |
| `read_only: true`, `cap_drop: ALL`, `no-new-privileges` | The server writes nothing to its filesystem in v0.1.0 |
| `mem_limit: 512m`, `storage.max_memory: 256MB` | The cache evicts before the kernel OOM-kills it |
| `ATLASCACHE_CLIENT_PORT`, `ATLASCACHE_ADMIN_PORT` | Host ports move without editing the file |

There is deliberately no volume. v0.1.0 stores nothing on disk, and a
Docker-created volume is owned by root, which a container running as uid 65532
cannot write to. Persistence arrives in v0.2.0, and the volume arrives with it.

## Configuration in a container

Three ways, in increasing order of precedence:

**1. Mount a file.** `/etc/atlascache/config.yaml` is on the default search
path:

```sh
docker run -p 6379:6379 \
  -v "$PWD/config.yaml:/etc/atlascache/config.yaml:ro" \
  ghcr.io/b3vet/atlascache:0.1.0
```

**2. Point at one explicitly:**

```sh
docker run -p 6379:6379 \
  -v "$PWD/my.yaml:/cfg/my.yaml:ro" \
  ghcr.io/b3vet/atlascache:0.1.0 --config /cfg/my.yaml
```

**3. Environment variables.** Every key maps to `ATLAS_` plus the path in upper
case with `.` replaced by `_`:

```sh
docker run -p 6379:6379 \
  -e ATLAS_ADMIN_BIND_ADDR=0.0.0.0 \
  -e ATLAS_ADMIN_TOKEN=a-real-secret \
  -e ATLAS_STORAGE_MAX_MEMORY=1GB \
  ghcr.io/b3vet/atlascache:0.1.0
```

Mount the file read-only. The server re-reads it when it changes — a token
rotation or a policy change needs no restart — and nothing inside the container
should be able to write it.

A misspelled key is refused at startup, naming the key and the near match. A
misspelled `ATLAS_` variable warns rather than refuses, since the environment
is not ours to validate.

## Health checks

| Endpoint | Auth | Meaning |
|---|---|---|
| `/health` | none | 200 once serving; the aggregate answer |
| `/health/live` | none | Always 200 while the process runs |
| `/health/ready` | none | 200 only when it can take traffic |
| `/stats`, `/stats/memory`, `/config` | `admin.token` | Everything else |

The split matters to an orchestrator. A failed **liveness** probe restarts the
pod, so `/health/live` reads nothing at all — not the store, not the
configuration — because every data source it could consult is a way for a slow
component to turn a delay into a crash loop. A failed **readiness** probe only
takes the node out of a load balancer, so `/health/ready` is the one allowed to
say no: it does during startup and during the drain.

The image ships its own `HEALTHCHECK`:

```dockerfile
HEALTHCHECK CMD ["/atlascache", "--health-check"]
```

The binary probes itself, because the base image has no shell and no curl. It
reads the same configuration the server does, so it follows a changed admin
port without being told, and it dials loopback when the server is bound to a
wildcard address. Exit 0 means ready.

You can run it by hand:

```sh
docker exec <container> /atlascache --health-check; echo $?
```

## Kubernetes

There are no manifests in v0.1.0 — Helm charts are planned for a later release.
A minimal `Deployment` needs the admin bind address, a token, and probes:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: atlascache
spec:
  replicas: 1            # v0.1.0 is a single node; two replicas are two caches
  selector:
    matchLabels: { app: atlascache }
  template:
    metadata:
      labels: { app: atlascache }
    spec:
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        fsGroup: 65532
      containers:
        - name: atlascache
          image: ghcr.io/b3vet/atlascache:0.1.0
          ports:
            - { name: resp,  containerPort: 6379 }
            - { name: admin, containerPort: 8080 }
          env:
            # Without this, both probes below fail: a kubelet probe reaches the
            # pod IP, and 127.0.0.1 inside the container is not it.
            - name: ATLAS_ADMIN_BIND_ADDR
              value: "0.0.0.0"
            - name: ATLAS_ADMIN_TOKEN
              valueFrom:
                secretKeyRef: { name: atlascache-admin, key: token }
          livenessProbe:
            httpGet: { path: /health/live, port: admin }
            periodSeconds: 10
          readinessProbe:
            httpGet: { path: /health/ready, port: admin }
            periodSeconds: 5
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: { drop: ["ALL"] }
          resources:
            limits: { memory: 512Mi }
```

Two things to keep in mind:

- **Expose 6379 in the `Service`, not 8080.** Probes reach the pod directly and
  need no Service; putting the admin port in one makes it reachable from every
  workload in the cluster.
- **Set `storage.max_memory` below the pod's memory limit.** Otherwise the
  first thing that happens under pressure is an OOM kill rather than an
  eviction, and a killed pod loses the whole cache instead of its coldest keys.

## Hardening

TLS and authentication are **fully implemented and both off by default**
(ADR-0009), so the first run needs no setup. The server says so at startup,
once per boot:

```
WARN  TLS disabled — traffic is unencrypted. Do not use in production.
WARN  auth disabled — any client that can reach this port has full access.
```

Take those literally. With both off, anything that can open a TCP connection to
port 6379 can read and delete everything in the cache, in cleartext. Before the
port is reachable by anything you care about:

```yaml
tls:
  enabled: true
  cert_file: "/etc/atlascache/tls/tls.crt"
  key_file: "/etc/atlascache/tls/tls.key"
auth:
  enabled: true
  token: "a-real-secret"
admin:
  bind_addr: "0.0.0.0"
  token: "a-different-secret"
```

```sh
docker run -p 6379:6379 \
  -v "$PWD/config.yaml:/etc/atlascache/config.yaml:ro" \
  -v "$PWD/tls:/etc/atlascache/tls:ro" \
  ghcr.io/b3vet/atlascache:0.1.0
```

The certificate and key are re-read when the files change, so a renewal needs
no restart; a replacement that does not parse is rejected and the running
certificate keeps serving. The TLS floor is 1.3 and is not configurable — an
adjustable floor is one somebody lowers.

See [configuration.md](configuration.md) for every option.

## Verifying what you pulled

Release artifacts are signed with [cosign] keyless, so there is no public key
to fetch: the signature carries the identity of the GitHub workflow that made
it.

```sh
cosign verify ghcr.io/b3vet/atlascache:0.1.0 \
  --certificate-identity-regexp '^https://github.com/b3vet/atlascache/\.github/workflows/release\.yml@refs/tags/v0.1.0$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

For the binaries, `checksums.txt` is signed and covers everything listed in it:

```sh
cosign verify-blob checksums.txt \
  --signature checksums.txt.sig \
  --certificate checksums.txt.pem \
  --certificate-identity-regexp '^https://github.com/b3vet/atlascache/\.github/workflows/release\.yml@refs/tags/v0.1.0$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

sha256sum --check --ignore-missing checksums.txt
```

An SBOM (syft, SPDX) ships beside each archive and is attached to each image.

## Troubleshooting a container with no shell

`docker exec ... sh` fails with `executable file not found`. That is the base
image doing its job. Four ways through it, cheapest first:

**Logs.** JSON by default; set `logging.format: console` if you are reading
them yourself.

```sh
docker logs <container>
```

**The admin API.** With `admin.token` set:

```sh
curl -H "Authorization: Bearer $TOKEN" localhost:8080/stats
curl -H "Authorization: Bearer $TOKEN" localhost:8080/config   # secrets redacted
```

**The protocol.** `INFO`, `STATS` and `atlasctl` all work against a running
container from outside it. `INFO server` is also how you confirm you are
talking to AtlasCache and not to some other Redis on the same port:

```sh
redis-cli -p 6379 INFO server | grep atlascache_version
```

**The debug image.** Same binary, same uid, plus a shell:

```sh
docker run --rm -it --entrypoint sh ghcr.io/b3vet/atlascache:0.1.0-debug
docker compose exec atlascache sh      # after switching the image in compose
```

[cosign]: https://docs.sigstore.dev/
