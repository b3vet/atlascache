# Changelog

All notable changes to AtlasCache are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Entries are drafted from Conventional Commits with `git cliff` and then edited
by hand, so this file reads as release notes rather than a commit log.

## [Unreleased]

Nothing here is usable yet. The server starts and answers `PING`; data
commands arrive in the next phase.

### Added

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

### Fixed

- The repository now builds from a clean clone. `go.sum` was never committed
  and `go.mod` was missing a direct dependency, so every Go command failed.
- CI now runs. Every job had been failing on a Go version that contradicted
  `go.mod`, and the build job targeted a binary that did not exist.
- `CalculateSize` saturates instead of overflowing. An entry large enough to
  wrap was accounted as tiny and would have slipped past the memory limit.
- `make tools` installs a `golangci-lint` that can read the project's
  configuration; it previously installed a version that could not.

[Unreleased]: https://github.com/b3vet/atlascache/commits/main
