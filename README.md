# AtlasCache

A high-performance, distributed in-memory key-value store written in Go.

> **Pre-alpha — no release yet.** The server runs and is feature-complete for
> v0.1.0, but nothing has been tagged, packaged, or published. Treat it as
> something to read and build, not something to depend on.
> See [docs/ROADMAP.md](docs/ROADMAP.md) for what is coming and in what order.

AtlasCache speaks the Redis wire protocol, so `redis-cli` and existing Redis
client libraries work against it without modification.

## Status

What works today:

- **18 commands over RESP2**: `GET` `SET` `SETNX` `DEL` `EXISTS` `KEYS` `SCAN`
  `TTL` `EXPIRE` `DBSIZE` `PING` `ECHO` `INFO` `STATS` `AUTH` `HELLO` `QUIT`
  `COMMAND` — see [docs/protocol.md](docs/protocol.md) for exact reply shapes
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

## Requirements

- Go 1.24 or later

## Building

```bash
make build
```

Produces `bin/atlascache`.

## Running

```bash
cp config.example.yaml config.yaml
./bin/atlascache --config config.yaml
```

In another shell:

```bash
redis-cli -p 6379 SET hello world   # -> OK
redis-cli -p 6379 GET hello         # -> "world"
curl localhost:8080/health          # -> {"status":"ok"}
```

The admin port binds to `127.0.0.1` by default and is not reachable from
another host unless you change `admin.bind_addr`.

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
| [docs/sdk-go.md](docs/sdk-go.md) | Using the Go SDK |
| [docs/atlasctl.md](docs/atlasctl.md) | The CLI: commands, flags, exit codes, JSON output |
| [docs/protocol.md](docs/protocol.md) | Supported commands and the guarantees they carry |
| [docs/ROADMAP.md](docs/ROADMAP.md) | What is coming, and in what order |

Runnable programs live in [`examples/`](examples/) and are executed against a
real server in CI, so they cannot drift from the code.

## Contributing

AtlasCache is not accepting outside contributions yet — the foundational
releases are being built out first. Issues and ideas are welcome once v0.1.0
ships.

## License

MIT — see [LICENSE](LICENSE).
