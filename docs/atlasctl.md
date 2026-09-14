# atlasctl

`atlasctl` is the AtlasCache command-line client. It is built on
[`pkg/client`](sdk-go.md) and holds no second protocol implementation, so an
awkward SDK is an awkward CLI and the two cannot drift.

Two parts of it are an interface rather than an implementation detail, because
people write scripts against them: **the exit codes** and **the `--json`
object**. Both are documented here, and both are asserted against a live server
on every CI run by `examples/atlasctl/tour.sh` — if this page and the program
disagree, the build fails.

AtlasCache is pre-alpha and nothing is released yet, so the way to get
`atlasctl` is to build it:

```sh
make build-ctl          # writes bin/atlasctl
./bin/atlasctl help
```

## Synopsis

```sh
atlasctl [flags] <command> [arguments] [flags]
```

Global flags may appear before the command, after it, or both:
`atlasctl --json get k` and `atlasctl get k --json` are the same command. A bare
`--` ends flag parsing, which is how you store a value that begins with a dash:

```sh
atlasctl set mykey -- -1
```

## Commands

| Command | Synopsis | Summary |
|---|---|---|
| `ping` | `ping` | Check that the server answers. |
| `get` | `get <key>` | Print the value held at a key. |
| `set` | `set <key> <value> [--ttl d]` · `set <key> --stdin [--ttl d]` | Store a value, optionally with an expiry. |
| `del` | `del <key>...` | Remove keys, and report how many were removed. |
| `exists` | `exists <key>...` | Count how many of the given keys are present. |
| `keys` | `keys <pattern>` | List every key matching a glob. |
| `scan` | `scan [--match glob] [--count n]` | Walk the keyspace a page at a time. |
| `stats` | `stats` | Print the server's counters. |
| `info` | `info [section...]` | Print the server's INFO report. |
| `help` | `help [command]` | Print usage. |
| `version` | `version` | Print the build stamp. |

`help` and `version` are also accepted as flags: `--help`, `-h`, `--version`,
`-v`.

## Global flags

| Flag | Default | Meaning |
|---|---|---|
| `--addr host:port` | `127.0.0.1:6379` | The server to talk to. |
| `--auth token` | — | Authentication token. Prefer `ATLASCACHE_AUTH`; see below. |
| `--tls` | off | Connect with TLS, verifying against the host's trust store. |
| `--tls-ca file` | — | PEM file of certificate authorities. Implies `--tls`. |
| `--json` | off | Print one JSON object instead of text. |
| `--timeout d` | `5s` | Time limit for the whole command, dial included. `0` means none. |

`--timeout` bounds everything: the dial, the TLS handshake, the authentication
exchange, and the command itself. There is no reconnect window — a long-running
service riding out a server restart wants one, and a command whose caller is
waiting does not, so a dial that fails is reported rather than retried. A script
that wants to retry can, and one that wants to know now is told now.

### Authentication

Pass the token in the environment:

```sh
export ATLASCACHE_AUTH='…'
atlasctl --addr cache.internal:6379 get session:abc
```

`--auth` works too, and warns on stderr every time it is used. The warning is
not suppressible, because the two reasons behind it are ones the person typing
the command may not know: a token in an argument list is visible in `ps` output
to **every other user on the host**, and it is written to the shell history file
besides. An environment variable is neither.

`ping` is on the server's pre-authentication allowlist, so it answers with or
without a token. A successful `ping` is not evidence that your token works — run
a data command for that.

### TLS

```sh
atlasctl --tls --addr cache.internal:6379 ping                   # public CA
atlasctl --tls-ca /etc/atlascache/ca.crt --addr cache.internal:6379 ping
```

`--tls-ca` implies `--tls`. Reading it any other way would let a caller who
named a CA and forgot the flag connect in plaintext while believing otherwise,
and nothing in the output would say so.

The certificate is verified against the address you gave, so connect to the name
on the certificate: `localhost:6379` rather than `127.0.0.1:6379` unless the
certificate has an IP SAN. A certificate that does not verify fails as a
connection error — exit 3 — because the connection never came up, and so does
`--tls` against a server that is not speaking TLS. The message names the real
cause in both cases:

```sh
atlasctl: atlascache: dial localhost:6380: tls: failed to verify certificate: …
atlasctl: atlascache: dial 127.0.0.1:6379: tls: first record does not look like a TLS handshake
```

## Exit codes

| Code | Meaning |
|---|---|
| `0` | Success. |
| `1` | The command failed: a missing key, a server error, a refused token. |
| `2` | Usage: an unknown command or flag, a missing argument, an unparsable value. |
| `3` | The server could not be reached, or did not answer in time. |

Distinguishing 1 from 3 is the point of the table. "The key is not there" and
"the server is down" call for different things from a script, and a client that
answers both with 1 forces it to parse English to tell them apart.

Where the SDK's error categories land:

| SDK category | Exit code | JSON `kind` | Why |
|---|---|---|---|
| `ErrNetwork` | 3 | `connection` | The command may not have run. |
| `ErrTimeout` | 3 | `timeout` | The command may have run; the caller is in the same position as an unreachable server leaves them. |
| `ErrAuth` | 1 | `auth` | The socket worked. Retrying the same token fails the same way, and logs another failure. |
| `ErrProtocol` | 1 | `protocol` | The bytes arrived and could not be read. A retry produces the same thing. |
| `ErrServer` | 1 | `server` | The server received the command and refused it. |
| `ErrClosed` | 1 | `closed` | A client lifecycle bug. |
| context canceled | 1 | `canceled` | The caller stopped it. |
| — (a missing key) | 1 | `not_found` | |
| — (a bad command line) | 2 | `usage` | |
| anything else | 1 | `error` | |

`--timeout` expiring is exit 3, not 2: the command line was fine, the server was
not there in time.

### The missing-key case

`get` is the command that needs the exit code rather than the output. A key that
is not there and a key holding an empty value both print nothing, and both are
legal, so the distinction has nowhere else to live:

| Situation | stdout | Exit |
|---|---|---|
| Key holds `hello` | `hello` | 0 |
| Key holds an empty value | *(nothing)* | 0 |
| Key is not there | *(nothing)* | 1 |

`exists` answers with a count and exits 0 either way — it answered the question
it was asked. `del` of a key that is not there is also exit 0, with `removed: 0`.
Branch on the number, or use `get` when absence should be a failure.

## Output

Data goes to stdout and diagnostics go to stderr, so pipelines work. A usage
error writes nothing at all to stdout: a script parsing output gets nothing to
parse.

A value is written to stdout as the bytes the server holds — no trailing
newline, no escaping, nothing added — whenever stdout is **not** a terminal. So
this round-trips a value containing null bytes or invalid UTF-8:

```sh
atlasctl get blob > blob.bin
atlasctl set blob --stdin < blob.bin
```

On a terminal a newline is added so the shell prompt lands in the right place,
and a value that would disturb the terminal — a control character, a byte
sequence that is not UTF-8 — is printed in Go's quoted form. That is a courtesy
to a human and a corruption of anything else, which is why it is decided by what
stdout is rather than by a flag.

`--stdin` is the only way to store a value containing a null byte, which an
argument list cannot carry. It also keeps the value out of `ps` and the shell
history, for the same reason `--auth` is discouraged.

## The `--json` object

`--json` prints exactly one JSON object and nothing else — on success, on
failure, and on a command line too broken to parse.

```json
{"ok": true,  "command": "get", "data": {…}}
{"ok": false, "command": "get", "data": {…}, "error": {"code": 1, "kind": "not_found", "message": "…"}}
```

- `ok` is `true` exactly when the exit code is `0`.
- `error.code` is always the process exit code, repeated inside the document so
  a caller reading JSON off a pipe need not also have kept the exit status.
- `error.kind` is a stable token — `usage`, `connection`, `timeout`, `auth`,
  `server`, `protocol`, `not_found`, `closed`, `canceled`, `error`. Branch on
  this.
- `error.message` is prose. It may be reworded in any release. Do not parse it.
- `data` is kept on failure where a failing command still produced some: a `get`
  that found no key still reports which key it looked for.

A failure is still a JSON object. A client that emitted a bare error string here
would break every script in exactly the case the script most needs to handle.

Values and keys are JSON strings when they are valid UTF-8 and base64 otherwise;
`encoding` says which, and is `utf8` or `base64`. A key list carries one
`encoding` for the whole list — base64 as soon as any one key needs it — so the
shape of the array does not change with its contents.

### `data` per command

| Command | `data` |
|---|---|
| `ping` | `{"latency_ms": 0.62}` |
| `get` | `{"key": "k", "found": true, "value": "hello", "encoding": "utf8"}` |
| `set` | `{"key": "k", "ttl_seconds": 60, "bytes": 5}` |
| `del` | `{"keys": ["k"], "removed": 1}` |
| `exists` | `{"keys": ["a","b"], "count": 1}` |
| `keys` | `{"pattern": "k*", "keys": ["k1"], "count": 1, "encoding": "utf8"}` |
| `scan` | `{"match": "k*", "keys": ["k1"], "count": 1, "cursor": "0", "complete": true, "encoding": "utf8"}` |
| `stats` | `{"stats": {"keys": 2, "hits": 1, …}}` |
| `info` | `{"sections": ["server"], "fields": {"atlascache_version": "0.1.0", …}, "text": "# Server\r\n…"}` |
| `version` | `{"version": "0.1.0", "commit": "79a7cd7", "built": "2026-09-14T21:29:25Z"}` |

Notes on individual fields:

- `get`: `value` and `encoding` are absent when `found` is `false`.
- `set`: `ttl_seconds` is `null` when no expiry was given, and a fraction for a
  sub-second TTL (`--ttl 500ms` reports `0.5`).
- `scan`: `cursor` is always `"0"` and `complete` is always `true`, because the
  CLI runs the iteration to completion. A cursor belongs to the connection that
  issued it, and the next invocation of a CLI is a different process on a
  different connection, so a cursor handed back to the shell could not be used.
  `count` is the number of keys returned, not the `--count` that was asked for.
- `stats`: counters a later server adds appear here with no change to this
  client.
- `help` is the one command that ignores `--json`: it prints its usage text
  either way.

## Command reference

### `ping`

```sh
atlasctl ping
```

Prints `PONG`. Exits 3 when the server cannot be reached, which is what makes
`atlasctl ping || restart-something` a usable line in a script. Takes no
arguments and no flags of its own.

### `get`

```sh
atlasctl get <key>
```

Prints the value. Exits 1 and prints nothing for a key that is not there; exits
0 and prints nothing for a key holding an empty value. See
[the missing-key case](#the-missing-key-case).

### `set`

```sh
atlasctl set <key> <value> [--ttl 60s]
atlasctl set <key> --stdin [--ttl 60s]
```

| Flag | Default | Meaning |
|---|---|---|
| `--ttl d` | `0` | Expire the key after this duration. `0` means no expiry. |
| `--stdin` | off | Read the value from standard input instead of the command line. |

A negative `--ttl` is a usage error rather than an immediate delete. Durations
are Go durations: `500ms`, `30s`, `10m`, `2h`.

### `del`

```sh
atlasctl del <key>...
```

Prints the number of keys removed. Removing a key that is not there is not a
failure — the exit code is 0 and the count is what says how many keys the
command actually found.

### `exists`

```sh
atlasctl exists <key>...
```

Prints how many of the given keys are present. A key named twice counts twice,
which is what the server does and what a client written against Redis expects. A
count of zero still exits 0.

### `keys`

```sh
atlasctl keys <pattern>
```

Prints every matching key, one per line. `KEYS` walks the whole keyspace and
holds a connection — and the server — for the length of the walk. Use `scan`
against anything you would mind blocking.

The pattern syntax is Redis's, matched byte for byte: `*`, `?`, `[abc]`,
`[a-c]`, `[^abc]`, and `\` to escape any of them. `*` crosses `:` and `/` like
any other byte, so `KEYS *` really does mean every key. A malformed pattern —
an unterminated `[`, say — is matched as far as it makes sense and then taken
literally, which is also what Redis does.

### `scan`

```sh
atlasctl scan [--match <glob>] [--count <n>]
```

| Flag | Default | Meaning |
|---|---|---|
| `--match glob` | — | Only keys matching this glob. Empty means every key. |
| `--count n` | `0` | How much work one page may do. `0` leaves it to the server. |

Prints every key, one per line, having run the iteration to completion. `--count`
bounds the work one page does, not how many keys come back. A negative `--count`
is a usage error.

### `stats`

```sh
atlasctl stats
```

Prints the server's counters as `name: value` lines, sorted by name.

### `info`

```sh
atlasctl info [section...]
```

Prints the server's INFO report in Redis's format, so anything that already
parses INFO keeps working. `--json` adds the same fields parsed into an object,
with the raw text alongside.

The sections are `server`, `clients`, `memory`, `stats` and `keyspace`, rendered
in that order. With no section, or with `default`, `all` or `everything`, every
section is returned. A section this server does not recognize produces an empty
report rather than an error, because tools probe for sections they are not sure
exist.

### `help`, `version`

```sh
atlasctl help              # every command, the global flags, the exit codes
atlasctl help scan         # one command, with its own flags
atlasctl version           # version, commit, build date
atlasctl --json version    # the same as an object
```

`atlasctl help <unknown>` prints a note on stderr and then the general usage,
and exits 0.

## Recipes

Warm a cache entry only if it is absent, and tell the two outcomes apart:

```sh
if atlasctl get session:abc >/dev/null 2>&1; then
    echo "already cached"
else
    case $? in
        1) atlasctl set session:abc --stdin < payload.bin ;;
        3) echo "cache is down; carrying on without it" >&2 ;;
        *) exit $? ;;
    esac
fi
```

Read one counter out of `stats` with `jq`:

```sh
atlasctl --json stats | jq '.data.stats.keys'
```

Fail a health check on anything but a reachable server:

```sh
atlasctl --timeout 2s ping > /dev/null || exit 1
```

Delete every key matching a prefix:

```sh
atlasctl scan --match 'session:*' | while read -r key; do
    atlasctl del "$key"
done
```

## See also

- [`docs/sdk-go.md`](sdk-go.md) — the Go SDK this CLI is built on.
- `go doc github.com/b3vet/atlascache/pkg/client` — the SDK reference.
- `examples/atlasctl/tour.sh` — every exit code and JSON field on this page,
  asserted against a live server.
