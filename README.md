# AtlasCache

A high-performance, distributed in-memory key-value store written in Go.

> **Pre-alpha — not usable yet.** There is no release, and the server currently
> answers only `PING`. This repository is being built toward a v0.1.0 alpha.
> See [docs/ROADMAP.md](docs/ROADMAP.md) for what is coming and in what order.

AtlasCache speaks the Redis wire protocol, so `redis-cli` and existing Redis
client libraries work against it without modification.

## Status

What works today:

- Sharded in-memory storage engine (`GET`/`SET`/`SETNX`/`DEL`/`EXISTS`/`KEYS`/`SCAN`)
  — as a Go package; not yet reachable over the network
- Configuration loading, validation, environment overrides, and hot reload
- A server binary that starts, speaks RESP well enough to answer `PING`,
  serves a health endpoint, and shuts down gracefully

Not yet implemented — all planned, see the roadmap:

- Data commands over the network (v0.1.0)
- TTL expiration and eviction policies (v0.1.0)
- TLS and authentication (v0.1.0)
- Go SDK and `atlasctl` CLI (v0.1.0)
- Persistence and crash recovery (v0.2.0)
- Clustering and replication (v0.3.0)

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
redis-cli -p 6379 PING          # -> PONG
curl localhost:8080/health      # -> {"status":"ok"}
```

The admin port binds to `127.0.0.1` by default and is not reachable from
another host unless you change `admin.bind_addr`.

## Development

```bash
make help      # list all targets
make test      # unit tests with race detection, both modules
make lint      # golangci-lint, both modules
make check     # fmt, vet, lint, test
make tools     # install golangci-lint and the git hooks
```

This repository contains two Go modules — the root module and the end-to-end
test suite under `test/e2e/` — tied together by `go.work`. Makefile targets
cover both; a bare `go test ./...` at the root does not reach the E2E module.

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
├── tools/                repository tooling
├── test/e2e/             end-to-end suite (separate module)
└── docs/                 public documentation
```

## Contributing

AtlasCache is not accepting outside contributions yet — the foundational
releases are being built out first. Issues and ideas are welcome once v0.1.0
ships.

## License

MIT — see [LICENSE](LICENSE).
