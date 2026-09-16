# Changelog

All notable changes to AtlasCache are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Entries are drafted from Conventional Commits with `git cliff` and then edited
by hand, so this file reads as release notes rather than a commit log.

## [Unreleased]

The cache is usable from `redis-cli`: it stores, expires and evicts, and
answers `SET`, `GET`, `DEL`, `TTL` and `EXPIRE` exactly as Redis does. The rest
of the command set arrives in the next phase.

### Added

- `SET key value [EX seconds | PX milliseconds]`, `GET`, `DEL`, `TTL` and
  `EXPIRE`, wired to the storage engine, the TTL manager and the eviction
  controller. Replies match Redis command for command, including a null bulk
  string for a miss and the `-1`/`-2` distinction `TTL` draws between a key
  with no expiry and no key at all.
- A server binary, `atlascache`, that loads configuration, listens on the RESP
  port, answers `PING` and `QUIT`, serves `GET /health`, and shuts down
  gracefully on `SIGTERM`.
- `server` and `admin` configuration sections. The admin listener binds to
  loopback by default.
- An end-to-end test suite that runs declarative YAML specs against a real
  server process, with its own RESP client so a bug in the server cannot hide
  behind a matching bug in the client.
- `make phase-check`, which decides mechanically whether a phase is complete.
- `tools/devindex`, which regenerates `dev/INDEX.md` from item front-matter and
  verifies it is current with `--check`.
- Commit convention tooling: a `commit-msg` hook enforcing Conventional
  Commits, installed by `make tools`.
- An MIT `LICENSE`, which the README had claimed without one being present.
- Pipelining. A client that writes many commands without waiting gets them
  executed in order and answered in one flush, so a batch costs one read and
  one write rather than one of each per command. A batch that ends in a partial
  frame is answered anyway: the partial frame is kept for the next read instead
  of stalling the replies already earned.
- Connection limits, under `server:` and all with working defaults.
  `max_connections` (10000) turns a connection past the ceiling away with
  `-ERR max number of clients reached` and closes it, rather than refusing to
  accept and leaving the client with an unexplained `ECONNREFUSED`.
  `client_idle_timeout` (30s) reaps a connection that has completed no command,
  refreshed per command rather than per byte so that dribbling bytes does not
  defeat it. `max_output_buffer` (64MB) disconnects a client that asks for a
  reply it will not read. `max_pipeline_commands` (1024) bounds a batch.
- `INFO` reports the connection bounds and why connections ended: `maxclients`
  and `rejected_connections` in Redis's spelling, plus `atlascache_idle_closed`,
  `atlascache_output_limit_closed`, `atlascache_stalled_closed`,
  `atlascache_request_limit_closed` and `atlascache_handler_panics`. `STATS`
  carries the same figures.
- Panic isolation per connection. A handler that panics closes that connection,
  logs the stack, and leaves every other client served.

- The rest of the command set: `SETNX`, `EXISTS`, `KEYS`, `SCAN`, `DBSIZE`,
  `ECHO`, `STATS`, `COMMAND`, `HELLO` and `AUTH`. Replies match Redis, including
  the details that catch people out — `EXISTS k k k` counts duplicates
  separately, and `KEYS` matches Redis's glob rather than Go's, so a wildcard
  crosses `/`, which cache keys are full of.
- Per-key TTL with both lazy and active expiration, the latter through a
  four-level time wheel whose entries are hints rather than truth: an expiry
  that moves does not have to be found and rewritten, because the wheel is
  allowed to be wrong in the one direction that is safe.
- Eviction under a memory limit — `lru`, `lfu`, `fifo` and `none` — sampled
  rather than globally ordered, as Redis does it.
- TLS, with a TLS 1.3 floor that is deliberately not configurable, and
  certificates re-read on change so a renewal needs no restart. A replacement
  that does not parse is refused and the running certificate keeps serving.
- Token authentication, compared in constant time, rotatable without
  disconnecting clients that have already authenticated.
- **A Go SDK**, `pkg/client`, as its own module with **no dependencies** — so
  importing it does not pull the server's dependency tree into your build. It
  pools connections, reconnects with jittered backoff, honours `context` at
  every blocking point, and retries only what is safe to retry, opt-in.
- **`atlasctl`**, a command-line client built on that SDK rather than on a
  second protocol implementation, so the two cannot drift. Exit codes
  distinguish a missing key from an unreachable server, because a script has to
  respond differently to each.
- An admin HTTP API on a separate, loopback-bound port: `/health`,
  `/health/live` and `/health/ready` without a credential, because a Kubernetes
  probe cannot present one; `/stats`, `/stats/memory` and `/config` with one.
  The admin token is deliberately not the data token.
- Read-only `CONFIG GET`, so `redis-benchmark` and monitoring agents stop
  reporting a server they cannot interrogate. `save` and `appendonly` answer
  empty rather than inventing values for persistence that does not exist yet.
- A container image on distroless, running as non-root, with a `docker-compose.yml`
  that comes up usable.
- A release pipeline producing signed, checksummed binaries for six platforms
  with SBOMs, and a dry run that must pass before any tag is pushed.
- Documentation: getting started, configuration with a hardening section, the
  protocol and its guarantees, the SDK, the CLI, and deployment. Its examples
  are executed against a live server by CI, and the quick start is parsed and
  run by the test suite, so the documentation cannot quietly stop being true.

### Changed

- **A mistyped configuration key now refuses startup** instead of being silently
  ignored. It names the key, its line, and the key you probably meant, and
  reports every mistake at once rather than one per restart. The version that
  matters is not a typo'd port: it is `enbaled: true` under `auth`, which used
  to give you a server with authentication off, reachable by anyone who could
  open a socket, reporting itself perfectly healthy. A `SIGHUP` reload carrying
  an unknown key is rejected and the running configuration keeps serving, so
  this cannot take a live server down.
- An unrecognised `ATLAS_` environment variable warns rather than refusing to
  start — the opposite of the file, and deliberately. Kubernetes and Docker
  inject a block of variables for every linked service, so a Service named
  `atlas` puts `ATLAS_PORT` and `ATLAS_SERVICE_HOST` into the container before
  the process starts. Refusing to boot on those would make the check an outage
  in the deployment it exists to protect.
- A credential-less `HELLO` from an unauthenticated client is now refused rather
  than answered with the server's name, version, mode and role.
  `HELLO <n> AUTH user pass` still works, which is the reason `HELLO` is
  reachable before authentication at all.
- Three authentication errors now match Redis byte for byte.
- `Get` in the SDK returns `(value, found, err)`. A cache miss is not an error
  and an empty value is legal; any two-value shape conflates one of them.
- The SDK gained `ScanAll` and `ScanIter`, which hold one connection for the
  length of a walk. `SCAN` cursors belong to the connection that created them,
  and before this every caller had to discover that — `atlasctl` was pinning its
  pool to a single connection to work around it.
- `WithTLSCAFile` replaces the fifteen lines of `crypto/x509` plumbing every
  caller was writing to trust a private CA.

### Fixed

- The repository now builds from a clean clone. `go.sum` was never committed
  and `go.mod` was missing a direct dependency, so every Go command failed.
- CI now runs. Every job had been failing on a Go version that contradicted
  `go.mod`, and the build job targeted a binary that did not exist.
- `CalculateSize` saturates instead of overflowing. An entry large enough to
  wrap was accounted as tiny and would have slipped past the memory limit.
- `make tools` installs a `golangci-lint` that can read the project's
  configuration; it previously installed a version that could not.
- An inline command terminated with a bare `\n` is accepted, as it is in Redis.
  Requiring CRLF broke `redis-cli --pipe` with a plain-text file — the
  documented way to mass-load a Redis — along with anything piped through
  `echo`, a heredoc, or a file authored on Unix. RESP framing headers stay
  strict, which is also what Redis does.
- One accepted request can no longer cost the server twenty times the bytes it
  took to send. Every per-field parser limit was correct and nothing bounded
  their product: a request of a million single-byte elements was inside all of
  them, 7MB to send and 154MB to decode, against a port that is
  unauthenticated by default. A per-request byte budget — derived from
  `storage.max_value_size` unless `server.max_request_size` sets it — is charged
  while the request is arriving, and the element count is derived from it, so
  the same flood now moves the resident set by nothing.
- The E2E harness killed only the process it started, not the process group, so
  a binary that forked left the harness waiting on a pipe nobody would close —
  a hang rather than a failure, which in CI means a timeout with no signal.
- A spec that replaced a configuration block with a scalar silently lost every
  sibling key under it. It is now an error naming the path.

[Unreleased]: https://github.com/b3vet/atlascache/commits/main
