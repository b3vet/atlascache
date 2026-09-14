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

## v0.2.0 — Fast to reach, then trustworthy · *planned*

- **Faster node-to-node communication.** Benchmarking found that on an
  unpipelined request, 99% of the time is spent in the network path rather than
  in the cache itself — AtlasCache is within 1.7% of a server that stores
  nothing at all. A distributed cache pays that cost on every hop, so the link
  between instances gets measured and made cheap before clustering is built on
  top of it.
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

## v0.4.0 — *under review*

Originally the performance release: a purpose-built binary protocol alongside
RESP, and an event-loop networking transport.

Benchmarking in v0.1.0 put both in doubt, and they are being re-examined rather
than assumed. Protocol decoding turned out to be roughly 0.7% of a request's
cost, and an event-loop transport measured faster only for unpipelined traffic
— it was slower than the current design under the batching a cache actually
does. What replaces this is decided on measurements, not on the original plan.

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
