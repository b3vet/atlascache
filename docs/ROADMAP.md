# AtlasCache Roadmap

AtlasCache is a high-performance, distributed in-memory key-value store written
in Go.

It is **pre-alpha**: not yet released, not yet usable, and not ready for
production or evaluation. This page describes where it is going and what has
landed so far.

Each release is independently useful. No dates — items move only when they are
done.

---

## v0.1.0 — Usable · *in progress*

A single node you can actually run.

- Redis-compatible RESP protocol — works with `redis-cli` and existing clients
- Sharded in-memory storage: `GET`, `SET`, `SETNX`, `DEL`, `EXISTS`, `KEYS`, `SCAN`
- Per-key TTL with active and passive expiration
- Eviction policies: LRU, LFU, FIFO, and no-eviction
- Memory limits with real-time usage tracking
- TLS and token auth — both supported, both off by default
- Go client SDK
- `atlasctl` command-line client
- Admin HTTP API: health and stats
- Docker image and configuration reference

## v0.2.0 — Trustworthy · *planned*

Durable and observable.

- Snapshot persistence with checksums
- Write-ahead log with configurable fsync
- Crash recovery from snapshot plus WAL replay
- Prometheus metrics and an OpenTelemetry trace path
- Published benchmark suite and a performance regression gate

## v0.3.0 — Scalable · *planned*

Distributed.

- Gossip-based cluster membership and failure detection
- Consistent hashing across 16,384 slots
- Configurable replication with automatic failover
- `MOVED` redirects for cluster-aware clients
- Background anti-entropy repair

## v0.4.0 — Fast · *planned*

The performance release.

- ATCP, a purpose-built binary protocol, alongside RESP
- Event-loop networking transport
- Zero-allocation hot paths

## Beyond

Under consideration, not yet scoped: TypeScript, Swift, and Elixir SDKs;
pub/sub; hash and JSON value types; vector similarity search; latency-based
edge routing; a web dashboard; and Helm charts.

---

## Status

| Version | State |
|---------|-------|
| v0.1.0 | in progress |
| v0.2.0 | planned |
| v0.3.0 | planned |
| v0.4.0 | planned |

AtlasCache is not accepting outside contributions yet — the foundational
releases are being built out first. Issues and ideas are welcome once v0.1.0
ships.
