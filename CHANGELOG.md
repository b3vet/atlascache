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

[Unreleased]: https://github.com/b3vet/atlascache/commits/main
