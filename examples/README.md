# Examples

Complete, runnable programs for the AtlasCache Go SDK, plus a tour of the
`atlasctl` CLI. Every one of them is run against a real server on every CI run
and exits non-zero if anything behaved differently from what it documents.

That is the whole point of their existing. An example that has never been
executed is a guess, and a guess that lives in a README outlives every refactor
that invalidated it.

## Running them

```sh
./examples/run.sh                            # build a server from this checkout and use it
./examples/run.sh --binary bin/atlascache    # use a server you already built
./examples/run.sh --keep                     # leave the temporary directory behind
make examples                                # the same thing, the way CI runs it
```

`run.sh` starts three servers on loopback — plaintext, plaintext with auth, and
TLS with auth — on ports it picks at run time, generates a certificate for the
TLS one, runs everything, and stops them all on the way out. It needs `go`,
`bash` and `openssl`, and it writes nothing outside a temporary directory.

To run one program on its own, against a server you are already running:

```sh
go run ./examples/quickstart/main.go -addr 127.0.0.1:6379
```

Every program takes `-addr` and exits 0 only if each step behaved as it says it
will.

Note the `/main.go` on the end. Each program carries the `ignore` build tag, so
that `go test ./...` in the root module does not sweep in eight packages with no
test files and several hundred uncovered statements, which would pull the module
under the coverage floor the phase gate enforces. The tag keeps them out of
`./...` patterns while leaving them buildable, runnable and vettable by file
name — which is how `run.sh` drives them, and how you run one by hand.

| Program | What it demonstrates |
|---|---|
| [`quickstart`](quickstart) | Connect, write, read, tell a cache miss from an empty value, close. |
| [`ttl`](ttl) | Expiry on the write, `Expire` after the fact, both TTL sentinels, a key expiring. |
| [`errors`](errors) | One failure of every error category, each classified with `errors.Is`. |
| [`retries`](retries) | What `WithRetries` retries, what it refuses to, and what it costs. |
| [`pool`](pool) | Pool size against throughput, the ceiling holding, exhaustion as a deadline. |
| [`scan`](scan) | A complete multi-page keyspace walk, checked for exactly-once delivery. |
| [`do`](do) | Reaching a command the SDK does not wrap, argument rendering, reply conversion. |
| [`tls`](tls) | Verifying a private CA, authenticating from the environment. |
| [`atlasctl/tour.sh`](atlasctl/tour.sh) | Every `atlasctl` exit code and `--json` field in `docs/atlasctl.md`. |

Two of them take more than `-addr`:

```sh
go run ./examples/errors/main.go -addr 127.0.0.1:6379 -auth-addr 127.0.0.1:6380
ATLASCACHE_AUTH=… go run ./examples/tls/main.go -addr localhost:6380 -ca certs/server.crt
go run ./examples/scan/main.go -addr 127.0.0.1:6379 -keys 500 -count 50
```

## Where the rest of the documentation is

- [`docs/sdk-go.md`](../docs/sdk-go.md) — the task-oriented SDK guide.
- [`docs/atlasctl.md`](../docs/atlasctl.md) — every CLI command, flag, exit code
  and JSON shape.
- `go doc github.com/b3vet/atlascache/pkg/client` — the API reference, including
  the shorter `Example` functions that `go test` compiles.

## Adding one

Keep the property that makes these worth having: **a program that checks
itself**. Print what happened, and return an error when the SDK did something
other than what the program claims it does, so `run.sh` fails rather than
printing a paragraph nobody reads. Clean up the keys you wrote — the programs
share a server — and prefix them with `example:<name>:`.

Then add it to `run.sh`, to the table above, and to the one in
`docs/sdk-go.md`.
