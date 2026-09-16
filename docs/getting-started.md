# Getting started

This gets you from nothing to a working cache. It should take about five
minutes, most of which is Go compiling.

> AtlasCache is **pre-alpha**. It has no persistence — everything is in memory
> and is lost on restart — and it ships with TLS and authentication disabled.
> Treat it as something to try, not something to depend on.

## Requirements

- Go 1.24 or later, to build from source
- Optionally `redis-cli`, which works against AtlasCache unmodified

## Build and run

```bash
git clone https://github.com/b3vet/atlascache.git
cd atlascache
make build
```

That produces `bin/atlascache`. Give it a configuration file and start it:

```bash
cp config.example.yaml config.yaml
./bin/atlascache --config config.yaml
```

The server prints one startup line reporting **every effective setting** — port,
shard count, memory limit, eviction policy, whether TLS and auth are on. It is
worth reading once, because it is the fastest way to confirm the server is
running the configuration you think it is.

It will also warn that TLS and auth are disabled. That is expected on a default
configuration; see [configuration.md](configuration.md) when you want them on.

## Connect

In another shell:

```bash
redis-cli -p 6379 PING
redis-cli -p 6379 SET hello world
redis-cli -p 6379 GET hello
```

```
PONG
OK
"world"
```

`redis-cli` is talking to AtlasCache, not Redis — AtlasCache speaks the same
wire protocol, so existing Redis clients and tools work against it without
modification.

If you are unsure which server you reached — easy to do if Redis is also running
on this machine — ask it:

```bash
redis-cli -p 6379 INFO server | head -2
```

```
# Server
atlascache_version:0.1.0-dev
```

## Expiry

Keys can carry a time to live, in seconds (`EX`) or milliseconds (`PX`):

```bash
redis-cli -p 6379 SET session abc123 EX 60
redis-cli -p 6379 TTL session
```

```
OK
(integer) 60
```

`TTL` answers `-1` for a key with no expiry and `-2` for a key that is not
there — the same distinction Redis makes, and the reason a miss is not an error.

## Scanning keys

`SCAN` walks the keyspace without blocking the server:

```bash
redis-cli -p 6379 SET user:1 alice
redis-cli -p 6379 SET user:2 bob
redis-cli -p 6379 SCAN 0 MATCH 'user:*'
```

One thing to know upfront, because discovering it by surprise is worse than
reading it here: **AtlasCache's `SCAN` cursors belong to the connection that
created them.** A cursor from one connection is meaningless on another, and it
expires. Redis's cursors are stateless and have neither property. Walk the whole
keyspace on one connection, and do not store a cursor to resume later.
[protocol.md](protocol.md) states the full guarantee.

## Check on it

The admin API serves health and statistics on a separate port, bound to
localhost:

```bash
curl localhost:8080/health
curl localhost:8080/stats
```

`/stats` reports hits, misses, evictions, expirations, memory in use, and
connection counters — the numbers you want when the cache is not behaving as
expected.

## Use it from Go

```bash
go get github.com/b3vet/atlascache/pkg/client
```

The SDK is its own module with **no dependencies**, so importing it does not
pull the server's dependency tree into your build.

```go
c, err := client.New(client.WithAddr("localhost:6379"))
if err != nil {
    return err
}
defer c.Close()

if err := c.Set(ctx, "hello", []byte("world"), 0); err != nil {
    return err
}

val, found, err := c.Get(ctx, "hello")
```

`Get` returns three values because a cache miss is not an error and an empty
value is legal — any two-value shape conflates one of them. See
[sdk-go.md](sdk-go.md) for the full guide, and [`examples/`](../examples) for
complete programs that CI runs against a live server.

## From the command line

`atlasctl` is a first-class client built on that same SDK:

```bash
make build-ctl
./bin/atlasctl set greeting hello
./bin/atlasctl get greeting
```

It exits `0` on success, `1` when a key is missing, `2` on a usage error, and
`3` when the server is unreachable — so shell scripts can branch without parsing
text. [atlasctl.md](atlasctl.md) has the full reference.

## Where to go next

| You want to | Read |
|-------------|------|
| Turn on TLS and authentication | [configuration.md](configuration.md) |
| Know exactly what a command guarantees | [protocol.md](protocol.md) |
| Use it from Go | [sdk-go.md](sdk-go.md) |
| Run it in Docker or Kubernetes | [deployment.md](deployment.md) |
| Know what is coming | [ROADMAP.md](ROADMAP.md) |
