# The Go SDK

`pkg/client` is the AtlasCache Go SDK, and the reference the other-language
SDKs will follow. This page answers the questions people arrive with. The API
list lives in godoc, which does not go stale:

```sh
go doc github.com/b3vet/atlascache/pkg/client
go doc github.com/b3vet/atlascache/pkg/client Client
```

**There is no Go code on this page.** Every snippet that would be here is an
`Example` function in `pkg/client/example_test.go` or a complete program under
`examples/`, because Go compiles the first kind and CI runs the second kind
against a live server. A snippet in a markdown file is checked by nobody and is
wrong within two releases. Where this page would have shown you code, it names
the example instead — and the examples are where to read it:

```sh
sed -n '/func ExampleClient_Scan/,/^}/p' pkg/client/example_test.go
cd pkg/client && go test -run Example -v   # run the ones that need no server
make examples                              # run the programs, against real servers
```

On pkg.go.dev each `Example` is rendered under the symbol it belongs to, which
is the nicest way to read them. `go doc` does not show examples, so locally the
file itself is the place to look.

## Contents

- [Installing](#installing)
- [Connecting](#connecting)
- [Reading and writing](#reading-and-writing)
- [Expiry](#expiry)
- [Errors](#errors)
- [Retries](#retries)
- [Surviving a server restart](#surviving-a-server-restart)
- [TLS](#tls)
- [Authentication](#authentication)
- [The connection pool](#the-connection-pool)
- [Walking the keyspace](#walking-the-keyspace)
- [Commands the SDK does not wrap](#commands-the-sdk-does-not-wrap)
- [Asking the server about itself](#asking-the-server-about-itself)
- [Testing code that uses the SDK](#testing-code-that-uses-the-sdk)
- [Contexts and deadlines](#contexts-and-deadlines)
- [Lifecycle and concurrency](#lifecycle-and-concurrency)
- [Known gaps](#known-gaps)
- [The examples](#the-examples)

## Installing

The import path is `github.com/b3vet/atlascache/pkg/client` and the package name
is `client`. It needs Go 1.24 or later.

```sh
go get github.com/b3vet/atlascache/pkg/client
```

AtlasCache is pre-alpha and nothing is tagged yet, so that command has nothing
to fetch until v0.1.0 ships. Until then, work inside a checkout of this
repository, where `go.work` resolves the module locally.

The SDK is a separate Go module from the server, and it depends on nothing but
the standard library. That is enforced rather than intended: `make tidy-check`
fails if its module graph ever contains more than itself, because adding a
dependency to an SDK is a decision about every project that imports it
(ADR-0015).

## Connecting

You need a server first — `make build && ./bin/atlascache --config config.yaml`,
or see the repository README. The SDK never starts one.

Build a client with `client.New` and options. It validates the options and
**opens no connections** — the first call dials. That is deliberate: a process
starting up alongside its cache should not fail to start because the cache is
briefly down. It also means the only error `New` can return is a bad option.

`Ping` is how you ask whether the server is reachable, because `New` will not
tell you.

> Example: `New`. Program: `examples/quickstart`.

Every option, and what it defaults to:

| Option | Default | |
|---|---|---|
| `WithAddr(host:port)` | `127.0.0.1:6379` | the server |
| `WithAuth(token)` | none | `AUTH <token>` on every connection |
| `WithUserAuth(user, token)` | none | the two-argument form |
| `WithTLS(*tls.Config)` | off | see [TLS](#tls) |
| `WithPoolSize(n)` | `10` | see [the connection pool](#the-connection-pool) |
| `WithMaxIdleTime(d)` | `5m` | close a connection that has sat unused this long |
| `WithDialTimeout(d)` | `5s` | one connection attempt, TLS and AUTH included |
| `WithReadTimeout(d)` | `3s` | how long one reply may take to arrive |
| `WithWriteTimeout(d)` | `3s` | how long one request may take to write |
| `WithReconnectWindow(d)` | `5s` | see [surviving a server restart](#surviving-a-server-restart) |
| `WithBackoff(base, max)` | `20ms`, `1s` | the full-jitter schedule reconnects and retries spend |

A zero duration means "no bound beyond the caller's context" for the three
timeouts, and "surface the first failure" for the reconnect window. Options are
applied in order, so a later one wins; a nil option is skipped, which makes
conditional configuration a plain expression rather than a slice to build up.

The timeout defaults are the ones a cache client wants rather than the ones a
database client wants: a read that has taken three seconds against an in-memory
store is not slow, it is broken, and a caller waiting on it has already lost
whatever the cache was saving.

## Reading and writing

Values are `[]byte` and keys are `string`. Both may hold arbitrary bytes — RESP
is binary safe and so is this SDK. `GetString` and `SetString` are conveniences
over the byte primitives, not replacements for them.

**`Get` returns three values, not two**: `(value []byte, found bool, err error)`.
A cache miss is not an error — it is the most ordinary outcome there is — and an
empty value is a legal value the server stores and returns like any other. Any
two-value shape conflates one of those with the other (ADR-0022).

**The slice `Get` returns is read-only.** Do not modify it, and copy it before
keeping it past the call that produced it. Nothing enforces this, and today the
SDK allocates a fresh slice per reply, so breaking the rule costs nothing right
now. It is stated as a contract because it is the same contract the storage
engine keeps on the other end of the wire, where returning an internal view
rather than a copy is what makes `GET` allocation-free (ADR-0013) — and because
a caller who honors it is a caller the SDK can later stop copying for. An
undocumented contract is just a trap.

`Set` copies your value onto the wire before it returns, so you may reuse your
buffer immediately.

> Examples: `Client.Get`, `Client.Set`, `Client.SetNX`.
> Program: `examples/quickstart`.

## Expiry

A TTL is part of the write: `Set(ctx, key, value, ttl)`. Zero means no expiry —
not "expire immediately" — and a negative duration is an error rather than a
guess. A TTL finer than a second is sent as milliseconds rather than rounded, so
a 500ms entry is a 500ms entry.

`Expire` attaches an expiry to a key that already exists and reports whether it
found one. A zero or negative duration there *deletes* the key, which is what
the server does and what a caller written against Redis expects.

`TTL` answers with a duration, or one of two sentinels that are not errors:

| Result | Meaning |
|---|---|
| a positive duration | time remaining, rounded to whole seconds by the server |
| `client.TTLNoExpiry` | the key exists and will not expire |
| `client.TTLNoKey` | there is no such key |

> Examples: `Client.Set`, `Client.TTL`. Program: `examples/ttl`.

## Errors

Every failure wraps exactly one of six category sentinels, so you classify with
`errors.Is` and never by matching strings:

| Category | Retryable | What it means |
|---|---|---|
| `client.ErrNetwork` | **yes** | The dial, the write or the read failed. The command may not have run. |
| `client.ErrTimeout` | **yes** | A deadline expired — your context, or a configured dial/read/write timeout. The command may have run. |
| `client.ErrProtocol` | no | A reply the SDK could not decode, or one whose shape did not match the conversion asked of it. The connection is discarded. |
| `client.ErrServer` | no | The server received the command and refused it. |
| `client.ErrAuth` | no | `-NOAUTH`, `-WRONGPASS` or `-NOPERM`. Fixed by configuration, not by waiting. |
| `client.ErrClosed` | no | A call on a client whose `Close` has run. A lifecycle bug, never transient. |

A call canceled by its own caller belongs to **no** category and wraps
`context.Canceled` instead, because the caller already knows what happened.

`client.Retryable(err)` is the middle column in one function. Use it rather than
rebuilding the table, and treat "retryable" as a statement about the *failure* —
whether the *command* may be repeated is a separate question, answered in
[Retries](#retries).

When the category is not enough, `errors.As` into `*client.Error` for the
detail: `Op` (the command, or `dial`/`pool`/`AUTH`), `Addr` (which server),
`Kind` (the server's own error word — `ERR`, `WRONGPASS`, `OOM`), `Message`, and
`Err` (the wrapped cause). The `Error()` string is for humans and logs; it is not
an interface, and parsing it is how you get a client that breaks on a reworded
message.

> Examples: `Retryable`, `Error`, `Client.Get` (the `errors` variant).
> Program: `examples/errors`, which produces one failure of every category
> against a live server and checks each one landed where this table says.

## Retries

**Retry is opt-in and per call site.** A client from `New` never retries;
`c.WithRetries(n)` returns a view that does. The view shares the parent's pool,
so it costs no extra connections — and closing either closes both.

It is opt-in because only the caller knows whether repeating a command is safe,
and the SDK will not guess on anyone's behalf.

Retries happen only on `ErrNetwork` and `ErrTimeout`, and only for commands
whose **reply** survives being asked for twice:

| Retried when asked | Never retried |
|---|---|
| `Get`, `GetString`, `Set`, `SetString`, `Exists`, `Expire`, `TTL`, `Keys`, `Ping`, `Echo`, `Info`, `DBSize`, `Stats` | `SetNX`, `Del`, `Scan`, `Do` |

The split is about replies, not effects:

- **`Del`** — deleting twice is harmless, but the second reply is `0`. A retried
  `DEL` reports that it removed nothing when it removed the key on the attempt
  whose reply was lost.
- **`SetNX`** — a retry that answers `0` cannot be told from a key that was
  already there, so the caller that won the race is told it lost. That is the
  one answer `SETNX` exists to give.
- **`Scan`** — a scan cursor is scoped to the connection that issued it
  (ADR-0017), so a retry landing on another pooled connection is told its cursor
  is invalid: an error that reads like the caller's mistake and is not.
- **`Do`** — the SDK does not know what command it is carrying. `INCRBY` and
  `GET` look alike from here, and a silently repeated write is worse than a
  surfaced error. **`Do` is never retried whatever `WithRetries` was asked
  for.** A caller who knows their command is safe to repeat can retry it in
  their own loop, which is a decision made rather than one guessed at.

`Set` *is* retried, because repeating it leaves the same key holding the same
bytes; only the expiry drifts, by however long the retry took, which is the same
drift a slow network already causes.

Attempts are spaced by full-jitter exponential backoff (`WithBackoff`), and when
every attempt fails you hear the last failure with its category intact rather
than a synthetic "gave up".

> Example: `Client.WithRetries`. Program: `examples/retries`.

## Surviving a server restart

There is no background reconnect loop, because a connection nobody is waiting
for does not need to exist. A dropped connection is replaced inside the pool, by
the next call that needs one.

That call retries the dial with full-jitter exponential backoff for as long as
`WithReconnectWindow` (5s by default) **and your context** both allow, so a
restart is a pause inside one call rather than a run of failures across many.
The window never extends a deadline — your context always wins — it only bounds
the wait for a caller who set none.

Set it to zero if you would rather serve a cache miss than wait: the first dial
failure is then surfaced immediately. That is what `atlasctl` does, because a
command whose caller is waiting should be told now.

The jitter is not a refinement. Every client that lost its connection to one
server lost it at the same instant, so an unjittered schedule has all of them
retrying in the same millisecond — and the server they are waiting for spends
its first seconds back serving a synchronized stampede instead of coming up.

Two more things keep a restart quiet. A connection that has seen any failure is
closed rather than returned to the pool, so no caller inherits another caller's
broken stream. And a connection that has sat idle is health-checked before it is
handed out, which catches the server that restarted or the middlebox that timed
it out before a command is written into a socket that is already gone —
`WithMaxIdleTime` is the blunter version of the same idea.

> Example: `WithReconnectWindow`.

## TLS

Pass a `*tls.Config` to `client.WithTLS`. The config is cloned, so you may keep
using your own copy. When it names no `ServerName`, the host from the address is
used — so connect to the name on the certificate (`cache.internal:6379`), not to
an IP, unless the certificate has an IP SAN.

- **A certificate from a public CA**: `&tls.Config{MinVersion: tls.VersionTLS13}`
  and nothing else. The host's trust store does the rest.
- **A private CA**: read the PEM, put it in an `x509.CertPool`, set `RootCAs`.
  That is the fifteen lines every caller writes identically; see
  [Known gaps](#known-gaps).

A certificate that does not verify fails as `ErrNetwork`, because the connection
never came up. It reads at first like the server being down, and the wrapped
cause is what tells you otherwise.

`InsecureSkipVerify` turns the connection into encryption without
authentication — anything on the path can present its own certificate — so it
belongs in a test and nowhere else.

The server's protocol floor is TLS 1.3 and is deliberately not configurable
there, because an adjustable floor is one somebody lowers.

> Example: `WithTLS`. Program: `examples/tls`, run in CI against a real TLS
> server with a certificate generated for the run.

## Authentication

`client.WithAuth(token)` sends `AUTH <token>` on every connection the pool
opens, including replacements for dropped ones, so nothing re-authenticates by
hand. `client.WithUserAuth(user, token)` sends the two-argument form; v0.1.0 has
one shared token and one user, `default`, and both forms check the same secret
(ADR-0020).

**Read the token from the environment or a secret store**, not from a flag and
not from the source. An argument list is visible in `ps` output to every other
user on the host, and it lands in the shell history besides. That is exactly why
`atlasctl` prefers `ATLASCACHE_AUTH` to its own `--auth` flag, and warns —
unsuppressibly — whenever the flag is used.

A bad token is `ErrAuth`, which is not retryable: the same token will fail the
same way, and every attempt is another failure in the server's log.

> Example: `WithAuth`. Program: `examples/tls`.

## The connection pool

The client holds a fixed-size pool — 10 connections by default,
`WithPoolSize(n)` to change it. It is a **ceiling, not a target**: connections
are opened on demand and an idle client holds none.

**A call that arrives when every connection is busy waits for one.** It does not
open an extra connection, and it does not fail fast. It blocks until a
connection is returned or *its own context* expires, and an expired context
surfaces as `ErrTimeout` with `Op: "pool"`.

That blocking is the design, not a hang. A pool that grew under pressure would
turn one leaked connection into exhausted memory; one that blocks turns the same
leak into timeouts that name the callers waiting on it. `Op: "pool"` on a
timeout is the signature to look for when a service appears to have hung on its
cache — it means raise the pool size, or find what is holding connections for so
long.

Two consequences worth designing around:

1. **Give every call a deadline.** Without one, an exhausted pool is an
   unbounded wait, and that is the shape most often mistaken for a deadlock.
2. **Size for concurrent in-flight calls, not for goroutines.** A connection is
   held for one round trip, not for the request that borrowed it, so a request
   path making one cache call at a time needs far fewer connections than it has
   workers.

A pool of one serializes every call through a single connection. That is what
`atlasctl` uses — a CLI runs one command — and what a multi-page `Scan`
currently needs.

> Example: `WithPoolSize`. Program: `examples/pool`, which shows the ceiling
> holding against the server's own connection counter.

## Walking the keyspace

`Keys(ctx, pattern)` returns every matching key in one reply, and holds a
connection — and the server — for the length of the walk. It is a debugging
tool.

`Scan` is the production one. Start at `client.ScanStart`, pass each result's
`Cursor` to the next call, and stop when `result.Done()` is true — **and at no
other time**. An empty page is not the end: `count` bounds the work the server
does for one page, not the keys that page finds, so an empty page in a sparse
keyspace is ordinary.

The guarantee is that every key present when the scan began is returned exactly
once, unless it is deleted mid-scan. Keys created after the scan began may or
may not appear (ADR-0017).

Every page of one walk must go over the same connection, because the cursor is
scoped to it. Until the SDK carries that constraint for you, give a scanning
client `WithPoolSize(1)` — see [Known gaps](#known-gaps).

> Examples: `Client.Scan`, `ScanResult.Done`. Program: `examples/scan`, which
> walks 500 keys and checks every one came back exactly once.

## Commands the SDK does not wrap

`Do(ctx, args...)` sends any command. It is what keeps SDK releases decoupled
from server releases: no command is ever unreachable while you wait for a
wrapper (ADR-0022). Typed methods are preferred wherever one exists.

Arguments are rendered as wire bytes: `[]byte` and `string` pass through byte
for byte, integer and float types are formatted, `bool` becomes `1` or `0`, and
`time.Duration` becomes whole seconds. Anything else is refused rather than
formatted with `%v` — a struct that reached the wire as its Go formatting would
be stored, retrieved, and only noticed to be wrong much later.

`Do` returns a `client.Reply`, which you convert: `Bytes`, `Text`, `Int64`,
`Bool`, `Float64`, `Slice`, `Strings`, `ByteSlices`, `Map`. Each accepts every
shape that can sensibly carry what you asked for and returns `ErrProtocol`,
naming both shapes, for the ones that cannot. A nil reply is a miss, not an
error: check `IsNil` rather than the length, exactly as `found` does for the
typed `Get`.

Remember that `Do` is never retried.

`Do` is also the answer to "where is pipelining?" and "where are transactions?"
— v0.1.0 has neither. Every call is one round trip on one connection, and
concurrency comes from the pool rather than from batching. The server pipelines
what a client sends it, so a future batch API would be a client-side change, but
there is nothing to reach for today.

> Examples: `Client.Do`, `Reply.Int64`, `Reply.Bytes`. Program: `examples/do`.

## Asking the server about itself

`DBSize` returns the number of keys. `Stats` returns the server's counters as a
`map[string]int64` — `hits`, `misses`, `keys`, `evictions`, `expirations`,
`connected_clients`, `memory_used` and more. It is a map rather than a struct so
that a counter a later server adds reaches you without an SDK release, and
`Stats.Value(name)` reports whether this server published the counter at all,
which plain map indexing would lose.

`Info` returns the server's INFO text in Redis's format, optionally narrowed to
sections, for anything that already parses INFO. `Ping` checks the server
answers; `Echo` returns its argument byte for byte, which is the cheapest way to
prove a connection is carrying data intact.

> Example: `Stats.Value`.

## Testing code that uses the SDK

`Client` is an interface, so a test can substitute a fake without a server and
without a network. Take a `client.Client` in your own constructors — the
implementation behind it is unexported, so there is no concrete type to depend
on by accident — and the substitution costs nothing.

When you want the real thing, run one: the E2E suite and `examples/run.sh` both
start an `atlascache` process on a port they choose, and `pkg/client`'s own
`realserver_test.go` does the same by building the server binary. A fake proves
your code; a real server proves the wire.

## Contexts and deadlines

Every method takes a `context.Context` first, including the ones where
cancellation looks pointless today — adding it later would break every signature
in every language binding modeled on this one.

Every blocking step honors it: the dial, the wait for a pooled connection, the
backoff between retries, the write, and the read. A call canceled mid-flight
returns promptly and costs the connection it was using, which is discarded
rather than handed to the next caller with an unread reply still on it.

A context deadline and the configured timeouts are both applied, whichever
expires first.

## Lifecycle and concurrency

One client per server address per process. It is safe for concurrent use and
owns a connection pool, so a second client to the same server multiplies
connections without multiplying throughput.

`Close` releases every pooled connection, is idempotent, and always returns nil.
Connections checked out when it runs are closed as they come back. Every call
after it is `ErrClosed`.

`Client` is an interface from v0.1.0 with one implementation behind it. P7 adds
a cluster-aware client satisfying the same interface, so callers switch by
changing construction rather than call sites — and a test can substitute a fake
today (ADR-0022).

## Known gaps

These are scheduled fixes, tracked in `dev/issues/ISSUE-0023.md`, not settled
design. They are listed here because working around a gap you do not know about
is worse than working around one you do.

| Gap | What it means for you today |
|---|---|
| **No `ErrConfig` sentinel.** A bad option comes back from `New` as an `*Error` with `Op: "New"` and no `Category`, so `errors.Is` against the six sentinels matches nothing. | Treat any error from `New` as a configuration error; there is nothing else it can be. |
| **`Scan` does not pin a connection.** Cursors are connection-scoped, but nothing holds one connection for the length of a walk. | Build a scanning client with `WithPoolSize(1)`. A `ScanAll`/iterator that carries the constraint is the fix. |
| **No `WithTLSCAFile`.** Loading a private CA is fifteen lines of `x509` plumbing every caller writes identically. | Copy them from `examples/tls`. |
| **Conversion errors lose context.** An error from `Reply.Bytes()` carries `Op: "reply"` and an empty `Addr`, so it prints without naming the server or the command. | Add the context yourself when wrapping it. |
| **Authenticating to a server with auth *disabled*** answers `ErrAuth` — correctly categorized, but the message points at your token rather than at the server's configuration. | If a token that works elsewhere is refused, check whether that server has auth enabled at all. |

## The examples

Every program under `examples/` runs against a live server on every CI run, and
exits non-zero if anything behaved differently from what it documents.

```sh
make examples                         # all of them, against servers it starts
./examples/run.sh --binary bin/atlascache
go run ./examples/scan/main.go -addr 127.0.0.1:6379
```

The `/main.go` is not a typo: each program carries the `ignore` build tag so it
stays out of the root module's `go test ./...`, and is therefore named by file
rather than by package. `examples/README.md` says why.

| Program | What it demonstrates |
|---|---|
| `examples/quickstart` | Connect, write, read, a miss versus an empty value, close. |
| `examples/ttl` | Expiry on the write, `Expire` after the fact, both TTL sentinels, a key expiring. |
| `examples/errors` | One failure of every category, each classified with `errors.Is`. |
| `examples/retries` | What `WithRetries` retries, what it refuses to, and what it costs. |
| `examples/pool` | Pool size against throughput, the ceiling holding, exhaustion as a deadline. |
| `examples/scan` | A complete multi-page walk, checked for exactly-once delivery. |
| `examples/do` | Reaching an unwrapped command, argument rendering, reply conversion. |
| `examples/tls` | Verifying a private CA, authenticating from the environment. |
| `examples/atlasctl/tour.sh` | Every `atlasctl` exit code and JSON field in `docs/atlasctl.md`. |

## See also

- [`docs/atlasctl.md`](atlasctl.md) — the CLI built on this SDK.
- [`docs/protocol.md`](protocol.md) — the wire protocol.
- `go doc -all github.com/b3vet/atlascache/pkg/client` — the full reference.
