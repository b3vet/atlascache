# AtlasCache

[![CI](https://github.com/b3vet/atlascache/actions/workflows/ci.yml/badge.svg)](https://github.com/b3vet/atlascache/actions/workflows/ci.yml)
[![Fuzz](https://github.com/b3vet/atlascache/actions/workflows/fuzz.yml/badge.svg)](https://github.com/b3vet/atlascache/actions/workflows/fuzz.yml)
[![Go](https://img.shields.io/badge/go-1.24+-00ADD8.svg)](https://go.dev/dl/)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A high-performance, distributed in-memory key-value store written in Go.

> **Pre-alpha — no release yet.** The server runs and is feature-complete for
> v0.1.0, but nothing has been tagged, packaged, or published. Treat it as
> something to read and build, not something to depend on.
> See [docs/ROADMAP.md](docs/ROADMAP.md) for what is coming and in what order.

AtlasCache speaks the Redis wire protocol, so `redis-cli` and existing Redis
client libraries work against it without modification.

## Status

What works today:

- **19 commands over RESP2**: `GET` `SET` `SETNX` `DEL` `EXISTS` `KEYS` `SCAN`
  `TTL` `EXPIRE` `DBSIZE` `PING` `ECHO` `INFO` `STATS` `CONFIG` `AUTH` `HELLO`
  `QUIT` `COMMAND` — see [docs/protocol.md](docs/protocol.md) for exact replies,
  and for what is **not** implemented
- Sharded in-memory storage engine with per-shard locking
- Per-key TTL, expired lazily on access and actively by a time wheel
- Eviction under a memory limit: `lru`, `lfu`, `fifo`, or `none`
- TLS and token auth, both supported and both off by default
- Go SDK (`pkg/client`) with pooling, reconnection, and typed errors
- `atlasctl` command-line client
- Admin HTTP API: health, stats, and config
- 51 end-to-end specs run against a real server on every commit

Not yet done — see the roadmap:

- Docker image, configuration reference, and a signed v0.1.0 release
- Persistence and crash recovery (v0.2.0)
- Clustering and replication (v0.3.0)

RESP3 is not supported: `HELLO 3` is answered with `-NOPROTO`, which every
mainstream client handles by staying on RESP2.

## Quick start

Requires Go 1.24 or later.

```bash
git clone https://github.com/b3vet/atlascache.git
cd atlascache
make build

cp config.example.yaml config.yaml
./bin/atlascache --config config.yaml
```

In another shell:

```bash
redis-cli -p 6379 SET hello world   # -> OK
redis-cli -p 6379 GET hello         # -> "world"
curl localhost:8080/health          # -> {"status":"ok"}
```

That is `redis-cli` talking to AtlasCache. Every command above, and every one in
[docs/getting-started.md](docs/getting-started.md), is executed against a real
server by the test suite on each commit, so nothing here can quietly stop
working.

The admin port binds to `127.0.0.1` by default and is not reachable from another
host unless you change `admin.bind_addr`. TLS and authentication ship disabled
and the server says so at startup; [docs/configuration.md](docs/configuration.md)
shows the hardened configuration.

## Development

```bash
make help      # list all targets
make test      # unit tests with race detection, all three modules
make lint      # golangci-lint, all three modules
make check     # fmt, vet, lint, test
make tools     # install golangci-lint and the git hooks
```

This repository contains three Go modules, tied together by `go.work`:

| Module | Purpose |
|--------|---------|
| `github.com/b3vet/atlascache` | server, engine, tooling |
| `github.com/b3vet/atlascache/pkg/client` | the Go SDK — **zero dependencies** |
| `github.com/b3vet/atlascache/test/e2e` | the end-to-end suite |

The SDK is its own module so that importing it does not pull in the server's
dependencies. Makefile targets cover all three; a bare `go test ./...` at the
root reaches none of the others.

Commits follow [Conventional Commits](https://www.conventionalcommits.org/).
`make tools` installs a hook that enforces the format.

## Project layout

```
atlascache/
├── cmd/atlascache/       server binary
├── internal/
│   ├── admin/            admin HTTP server
│   ├── config/           configuration loading and validation
│   ├── logging/          structured logging
│   ├── protocol/         RESP codec
│   ├── server/           transport and command dispatch
│   └── storage/          sharded storage engine
├── pkg/client/           Go SDK (separate module, no dependencies)
├── cmd/atlasctl/         command-line client
├── tools/                repository tooling
├── test/e2e/             end-to-end suite (separate module)
├── examples/             runnable programs, executed in CI
└── docs/                 public documentation
```

## Documentation

| Guide | Covers |
|-------|--------|
| [docs/getting-started.md](docs/getting-started.md) | Install, run, connect, first commands |
| [docs/configuration.md](docs/configuration.md) | Every option, its default, and production hardening |
| [docs/protocol.md](docs/protocol.md) | Supported commands and the guarantees they carry |
| [docs/sdk-go.md](docs/sdk-go.md) | Using the Go SDK |
| [docs/atlasctl.md](docs/atlasctl.md) | The CLI: commands, flags, exit codes, JSON output |
| [docs/deployment.md](docs/deployment.md) | Docker, Kubernetes, and the container caveats |
| [docs/ROADMAP.md](docs/ROADMAP.md) | What is coming, and in what order |

Runnable programs live in [`examples/`](examples/) and are executed against a
real server in CI, so they cannot drift from the code.

## Contributing

AtlasCache is **not accepting pull requests yet** — the foundational releases
are being built out first, and the architecture is still moving underneath them.
Bug reports, questions and ideas are welcome now, through
[issues](https://github.com/b3vet/atlascache/issues).

[CONTRIBUTING.md](CONTRIBUTING.md) covers the build, the test gate, and the
commit conventions. For a security issue, see [SECURITY.md](SECURITY.md) rather
than opening a public issue.

## License

MIT — see [LICENSE](LICENSE).
