# Protocol

AtlasCache speaks RESP2 over TCP, so existing Redis clients and tooling work
against it unmodified (ADR-0006). This page documents the command surface as of
v0.1.0, and — more importantly — the places where AtlasCache's behavior is
deliberately or unavoidably not Redis's.

RESP3 is not spoken. `HELLO 3` is answered with `-NOPROTO`, which redis-cli and
go-redis read as "this server speaks RESP2" and fall back on. The RESP3 encoder
lands in P6 (FEAT-0048, ADR-0028).

## Commands

| Command | Reply |
|---------|-------|
| `PING [message]` | `+PONG`, or the message as a bulk string |
| `QUIT` | `+OK`, then the connection closes |
| `HELLO [2] [AUTH user pass]` | Server properties; any other version is `-NOPROTO`. With auth on, a `HELLO` carrying no credentials is `-NOAUTH` |
| `AUTH [username] password` | `+OK`, or `-WRONGPASS`; `-ERR` when no password is set |
| `SET key value [EX s \| PX ms]` | `+OK` |
| `SETNX key value` | `:1` if stored, `:0` if a live key was in the way |
| `GET key` | The value, or the null bulk string on a miss |
| `DEL key [key ...]` | How many keys were actually removed |
| `EXISTS key [key ...]` | How many of the arguments exist |
| `KEYS pattern` | Array of matching keys, possibly empty |
| `SCAN cursor [MATCH p] [COUNT n]` | Two elements: the next cursor as a bulk string, then the key array |
| `TTL key` | Seconds remaining, `-1` with no expiry, `-2` with no key |
| `EXPIRE key seconds` | `:1` if applied, `:0` if there was no key |
| `DBSIZE` | Number of keys |
| `INFO [section ...]` | Redis-format text |
| `STATS` | AtlasCache's own statistics as a map |
| `ECHO message` | The message, unchanged and binary-safe |
| `COMMAND [...]` | An empty array (see below) |
| `CONFIG GET pattern [pattern ...]` | Name/value pairs for the parameters matched (see below) |

A miss is never an error. `GET` on an absent or expired key answers with the
null bulk string, because clients read null as "not cached" and an error as
"the server is broken".

An argument error is a reply, not a hang-up. A wrong argument count, a bad
option and an unknown command all leave the connection usable.

### Authentication

Authentication is off by default (ADR-0009). The server says so at startup,
naming what it costs, and every command is served to anyone who can reach the
port.

With `auth.enabled` set, one shared token guards the connection (ADR-0020):

| Invocation | Reply |
|-----------|-------|
| `AUTH <token>` | `+OK` |
| `AUTH default <token>` | `+OK`; `default` is the only username there is |
| `AUTH <other> <token>` | `-WRONGPASS invalid username-password pair or user is disabled.` |
| `HELLO 2 AUTH default <token>` | Server properties, and the connection is authenticated |
| `HELLO` with no credentials | `-NOAUTH HELLO must be called with the client already authenticated, …` |
| `HELLO 3`, authenticated or not | `-NOPROTO unsupported protocol version` |
| Any other command first | `-NOAUTH Authentication required.` |
| `AUTH` with no password set | `-ERR Client sent AUTH, but no password is set. Did you mean AUTH <username> <password>?` |

Those strings are Redis's, byte for byte, measured against `redis:7-alpine`
rather than copied from its documentation — with one deliberate exception, the
last row, listed under **Known divergences** below.

`AUTH`, `HELLO`, `PING` and `QUIT` are the only commands served before
authenticating, and `HELLO` only in the form that carries credentials:
`HELLO 2 AUTH default <token>` negotiates and authenticates in one round trip,
which is the reason `HELLO` is reachable at all, while a `HELLO` with no
credentials is refused rather than answered with the properties map — that map
names the server, its version, its mode and its role, and none of it is owed to
a client that has presented nothing.

The protocol version is settled before the credentials are, which is Redis's
own order: `HELLO 3` is `-NOPROTO` whether or not the connection has
authenticated, because a client probing for RESP3 needs the reply it knows how
to fall back from.

Authentication is per-connection and ends with the connection. A wrong token is
answered and not hung up on, and an unknown command from an unauthenticated
client is answered `-NOAUTH` rather than with the name of a command that does
not exist — a deliberate divergence, listed below.

Rotating `auth.token` in the config file takes effect without a restart, for
authentications made after it; connections that have already authenticated are
left alone.

### TLS

TLS is off by default too, and the same startup warning applies. With
`tls.enabled` set, the client port speaks TLS 1.3 and nothing older — there is
no configurable floor, because an adjustable one is the one that gets lowered.
A plaintext client connecting to a TLS port is refused rather than left waiting.

Certificate and key are re-read when the files change, so a renewal needs no
restart, and a replacement that does not parse is rejected with the running
certificate left in place. Connections established before a rotation keep the
certificate they handshook with.

### `EXISTS` counts duplicates

`EXISTS k k k` on one existing key returns `3`, not `1`. Each argument is
counted separately. This looks like a bug and is not: it is Redis's documented
behavior, and clients depend on it.

### `KEYS` blocks, and is O(keyspace)

`KEYS` walks every shard and holds each shard's read lock while it does. On a
large keyspace it will stall other traffic for the length of the walk, exactly
as Redis's `KEYS` does.

It exists because tooling expects it. **Use `SCAN` in anything that runs more
than once.**

### Glob patterns

`KEYS`, `SCAN MATCH` and `CONFIG GET` use Redis's matcher — a direct port of
`stringmatchlen`, differentially tested against `redis:7-alpine` — not Go's
`filepath.Match`. One port, shared by all three, so they cannot disagree about a
pattern. The supported syntax:

| Construct | Meaning |
|-----------|---------|
| `*` | Any sequence of bytes, **including `/`** |
| `?` | Exactly one byte |
| `[abc]` | Any one of those bytes |
| `[a-c]` | Any byte in the range; inverted ranges are swapped, so `[c-a]` is the same |
| `[^abc]` | Any byte *not* listed. Note `^`, not `!` |
| `\x` | The byte `x` literally, inside a character class or out |

Three consequences worth knowing:

- Matching is over **bytes, not runes**. `?` matches one byte, so a two-byte
  character needs `??`. This is what Redis does.
- A **malformed pattern is not an error**. `KEYS [abc` matches what it can and
  answers normally; Redis never returns an error for a pattern.
- An **empty pattern is not a wildcard**. `KEYS ""` matches the empty key and
  nothing else. Only a bare `*` means "every key", and it is answered without
  consulting the matcher at all — which is why `KEYS *` returns the empty key
  and `KEYS *x` cannot.

### `SCAN`

`SCAN 0` starts an iteration. The reply is the cursor to present next, then the
page of keys.

**A cursor of `0` is the only thing that means the iteration is over.** A short
page means nothing, and neither does an empty one: `COUNT` bounds the work a
page does rather than the number of keys it returns, and `MATCH` filters what
that work found. A client that stops on an empty page will miss keys.

Cursors are decimal unsigned 64-bit integers, as Redis's are, because every
mainstream client parses them as one — go-redis with `ParseUint`, redis-py with
`int()`, `redis-cli --scan` with `strtoull` (ADR-0017). A cursor that is not a
decimal integer is answered with `ERR invalid cursor` rather than looked up.

#### The guarantee, and where it is weaker than Redis's

> Every key present when the scan began is returned exactly once, unless it is
> deleted or expires mid-scan. Keys created after the scan began may or may not
> appear.

That is weaker than Redis's. Redis walks hash buckets in reverse-binary order,
which tolerates the table resizing underneath it; Go's built-in map does not
expose bucket structure, so AtlasCache snapshots a shard's key list instead
(ADR-0017 records the alternatives and why they were rejected for v0.1.0).

Consequences a client has to live with:

- **Cursors are server-side state, scoped to the connection that created them.**
  A cursor presented on another connection is refused exactly as an unknown one
  is. A connection that goes away takes its cursors with it.
- **A cursor expires.** It is dropped after 60 seconds idle, when the scan
  finishes, or when snapshot memory has to be reclaimed. Presenting it then
  returns `ERR invalid cursor` — an error Redis never sends. It is an error
  rather than a silent restart on purpose: a scan that quietly reset would hand
  back duplicates the caller has no way to detect.
- **There is a per-connection cursor limit** (16 by default) and a **global
  snapshot memory cap** (64 MiB by default).

#### The snapshot cap is a functional cliff

When one shard's key list alone exceeds `scan_max_snapshot_bytes`, that shard
becomes permanently unscannable and `SCAN` returns
`ERR scan snapshot memory limit reached`. At the 64 MiB default and roughly 36
bytes accounted per key, that is about two million keys **in a single shard** —
so the practical bound scales with `shard_count`. Raise the cap or the shard
count before approaching it. ADR-0017 records this as accepted for v0.1.0 and
names the custom hash table that removes it.

### `INFO`

`INFO` renders Redis's format: `key:value` lines under `# Section` headers, CRLF
line endings, one blank line between sections. Sections are `Server`, `Clients`,
`Memory`, `Stats` and `Keyspace`; `INFO <section>` returns one of them, `INFO
default`/`all`/`everything` returns all, and an unrecognized section returns an
empty string rather than an error.

Field names follow Redis's spelling wherever there is a direct equivalent —
`used_memory`, `maxmemory`, `keyspace_hits`, `keyspace_misses`, `expired_keys`,
`evicted_keys`, `connected_clients`, `total_connections_received`,
`total_commands_processed`, `uptime_in_seconds`, `db0` — so a dashboard built
for Redis reads them unchanged. Everything AtlasCache-specific carries an
`atlascache_` prefix, so a field Redis adds later cannot collide with one
invented here.

`used_memory` is the **cache's own accounting**: the bytes the entries occupy,
which is what `max_memory` is enforced against. The Go heap is reported
separately as `atlascache_heap_*`, and is sampled on a timer rather than read
when you ask — `runtime.ReadMemStats` stops the world, and INFO sits on a path
dashboards poll by the second (ISSUE-0015). Those figures may be up to ten
seconds stale; `atlascache_memory_sampled_at` says when they were taken.

`INFO` and `DBSIZE` cost the same at a thousand keys and at a million. Neither
walks the keyspace.

### `STATS`

AtlasCache's own introspection command, and what `atlasctl` and the admin API
read. It returns a logical map; RESP2 has no map frame, so it arrives as a flat
array of alternating keys and values. In RESP3 it will be a real map.

### `COMMAND` is a stub

`COMMAND`, and every subcommand of it, returns an empty array. redis-cli sends
`COMMAND DOCS` on startup and tolerates that.

Returning a fabricated command table would be worse than returning nothing:
clients use it for client-side arity validation, and a table that disagreed with
the server would make them reject commands the server accepts.

### `CONFIG` is read-only

`CONFIG GET` answers with the parameters AtlasCache actually has, as a flat
array of name/value pairs — the shape Redis sends a RESP2 client. Glob patterns
work, names are matched case-insensitively, several may be asked for at once,
and an exact name is echoed back in the spelling the client used, all as Redis
does.

| Parameter | Value |
|-----------|-------|
| `maxmemory` | `storage.max_memory` in bytes, as a decimal string; `0` is unlimited |
| `maxclients` | `server.max_connections` |
| `timeout` | `server.client_idle_timeout` in seconds |
| `save` | Empty |
| `appendonly` | Empty |

`save` and `appendonly` are there because `redis-benchmark` asks for them before
it runs anything and prints `WARNING: Could not fetch server CONFIG` when the
probe comes back with fewer than two elements. They are **empty rather than
invented**: there is no persistence in v0.1.0, and `appendonly:no` would be a
durability claim with no implementation behind it. Every other Redis parameter
is absent, and `CONFIG GET` on an absent parameter is an empty reply rather than
an error — which is also what Redis answers for a name it does not know.

**There is no `CONFIG SET`.** Configuration is a file, re-read when it changes
and on `SIGHUP` (see [configuration.md](configuration.md)), and a second
mutation path would let the two disagree: the next reload would silently undo
whatever `CONFIG SET` had done. The error says so rather than answering "unknown
subcommand", because an operator reaching for it needs to be told where the knob
is. `CONFIG RESETSTAT` and `CONFIG REWRITE` are not implemented either and
answer Redis's unknown-subcommand error; `CONFIG HELP` is answered.

## Not implemented

AtlasCache speaks RESP and answers 19 commands. Everything else a Redis client
might send is answered `-ERR unknown command`, and it is better to read that
here than to find it at runtime. The one exception is `CONFIG SET`, which
answers with what to do instead:

```
ERR CONFIG SET is not supported; AtlasCache reads its configuration from the
config file and reloads it when the file changes or on SIGHUP
```

**What is missing is most of Redis.** AtlasCache is a cache with string values
and expiry, not a Redis reimplementation. If your workload needs any of the
following, it needs Redis:

| Not implemented | Includes |
|---|---|
| **Every data structure but strings** | Lists, sets, hashes, sorted sets, streams, bitmaps, HyperLogLog, geo — `LPUSH`, `SADD`, `HSET`, `ZADD`, `XADD` and their families |
| **Numeric operations** | `INCR`, `DECR`, `INCRBY`, `INCRBYFLOAT` |
| **Multi-key string operations** | `MGET`, `MSET`, `MSETNX`, `GETSET`, `APPEND`, `STRLEN`, `SETRANGE`, `GETRANGE` |
| **Transactions** | `MULTI`, `EXEC`, `DISCARD`, `WATCH` |
| **Pub/sub** | `SUBSCRIBE`, `PUBLISH`, `PSUBSCRIBE` and the rest |
| **Scripting** | `EVAL`, `EVALSHA`, `FUNCTION` |
| **Databases** | `SELECT`, `SWAPDB`, `MOVE`. There is one keyspace |
| **Keyspace administration** | `FLUSHDB`, `FLUSHALL`, `RENAME`, `RANDOMKEY`, `TYPE`, `TOUCH`, `UNLINK`, `DUMP`, `RESTORE` |
| **Further expiry control** | `PERSIST`, `PEXPIRE`, `EXPIREAT`, `PEXPIREAT`, `PTTL`, `EXPIRETIME` |
| **Persistence** | `SAVE`, `BGSAVE`, `BGREWRITEAOF`, `LASTSAVE` — there is nothing to save |
| **Replication and cluster** | `REPLICAOF`, `WAIT`, `FAILOVER`, `CLUSTER` |
| **Client and server administration** | `CLIENT`, `DEBUG`, `MEMORY`, `SLOWLOG`, `ACL`, `MONITOR`, `SHUTDOWN`; and of the `CONFIG` subcommands, `SET`, `RESETSTAT` and `REWRITE` |

Some of these are planned — see [ROADMAP.md](ROADMAP.md) — and some are not.
Nothing above is stubbed, faked, or answered with a plausible lie: a command
that is not implemented says so.

**Pipelining works**, and is unrelated to `MULTI`: a client may write many
commands without waiting, and they are executed in order and answered in one
flush. What pipelining does not give you is atomicity across those commands.

## Known divergences from Redis

Every row here is a decision, not an oversight. The ones marked **deliberate**
would be changed only with a reason; the rest are v0.1.0 scope and close as
later phases land.

| Behaviour | Redis | AtlasCache |
|-----------|-------|------------|
| `PING` before `AUTH` | `-NOAUTH` | **Deliberate:** served, so health checks and connection pools work before a credential is presented |
| An unknown command before `AUTH` | `-ERR unknown command '<name>'` | **Deliberate:** `-NOAUTH`, so the command table — version, build, modules — is not readable by an unauthenticated client |
| `AUTH` with no password set | Redis 7: "AUTH `<password>` called without any password configured for the default user…" | **Deliberate:** Redis 6's wording, which the Go SDK classifies by prefix to produce a usable diagnostic (ISSUE-0020) |
| Unknown command, authenticated | `-ERR unknown command '<name>', with args beginning with: ` | `-ERR unknown command '<name>'`; the arguments are not echoed back |
| `HELLO <non-integer>` | `-ERR Protocol version is not an integer or out of range` | `-NOPROTO unsupported protocol version` |
| `HELLO 3` | RESP3 | `-NOPROTO`; RESP2 only until P6 (ADR-0028) |
| `CONFIG GET` | Several hundred parameters | Five (see above); `save` and `appendonly` empty rather than fabricated |
| `CONFIG SET` | Applies the change | Refused, naming the config file and `SIGHUP` as the path (ADR-0009) |
| `SCAN` cursor lifetime | Stateless; any cursor is always valid | Server-side, connection-scoped, expires (see above) |
| `SCAN` on a huge shard | Always works | May return `ERR scan snapshot memory limit reached` |
| `SCAN` guarantee | Tolerates concurrent resizing | Snapshot-based; keys created mid-scan may be missed |
| `COMMAND` | Full command table | Empty array |
| `INFO` `redis_version` | Present | Absent; `atlascache_version` instead |
| Arguments in one request | 1,048,576 | 131,072, and lower when `server.max_request_size` is (ISSUE-0018) |
| Total size of one request | Unbounded beyond the per-field limits | `server.max_request_size`, charged while the request arrives |
| Idle connections | Kept, unless `timeout` is set | Closed after `server.client_idle_timeout`, 30s by default — readable back with `CONFIG GET timeout` |
| Output buffering | `client-output-buffer-limit`, unlimited for normal clients | `server.max_output_buffer`, 64MB, disconnect on breach |
| Command set | Several hundred | The table above |
| TLS versions | 1.2 and 1.3, configurable | 1.3 only, not configurable |

Everything else on the authentication path is Redis's bytes exactly — including
the full stop in `NOAUTH Authentication required.`, the "or user is disabled."
in `WRONGPASS`, the sentence a credential-less `HELLO` is refused with, and the
capital S in `Syntax error in HELLO option '…'`. They are asserted against a
real `redis:7-alpine` by the differential run in `internal/server`, because a
string copied from a document is not a measurement: three of them had drifted
before anyone asked a real server (ISSUE-0020).

Empty keys are **not** a divergence. `SET "" v` works, and the empty key behaves
like any other (ISSUE-0013).

Nor are inline terminators. An inline command ending `\n` is accepted, with an
optional `\r` in front of it stripped, exactly as Redis does — which is what
`redis-cli --pipe` with a plain-text file sends (ISSUE-0019). RESP framing
headers still require CRLF, as they do in Redis.
