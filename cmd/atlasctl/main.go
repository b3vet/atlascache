// Command atlasctl is the AtlasCache command-line client.
//
//	atlasctl ping
//	atlasctl get <key>
//	atlasctl set <key> <value> [--ttl 60s]
//	atlasctl set <key> --stdin
//	atlasctl del <key>...
//	atlasctl exists <key>...
//	atlasctl keys <pattern>
//	atlasctl scan [--match p] [--count n]
//	atlasctl stats
//	atlasctl info [section...]
//	atlasctl help [command]
//	atlasctl version
//
// It is built on pkg/client and holds no second protocol implementation
// (FEAT-0029). That is deliberate: the CLI is the SDK's most demanding
// consumer, so an awkward SDK is an awkward CLI, and the two cannot drift.
//
// docs/atlasctl.md is the full reference — every command, every flag, every
// exit code, and the --json shape of each. What follows is the part that has to
// be true for a script to work.
//
// # Global flags
//
// Every command takes these, before it or after it: `atlasctl --json get k` and
// `atlasctl get k --json` are the same command, because a user should not have
// to know where a flag goes.
//
//	--addr host:port   the server (default 127.0.0.1:6379)
//	--auth token       the token; prefer ATLASCACHE_AUTH, below
//	--tls              connect with TLS, verifying against the host's trust store
//	--tls-ca file      PEM authorities to verify against; implies --tls
//	--json             print one JSON object instead of text
//	--timeout d        time limit for the whole command, dial included; 0 means none
//
// The timeout bounds everything, and there is no reconnect window: a dial that
// fails is reported rather than retried for five seconds. A script that wants to
// retry can, and one that wants to know now is told now.
//
// # Exit codes
//
// The exit code is the part a shell script reads, so it carries meaning:
//
//	0  success
//	1  the command failed — a missing key, a server error, a refused token
//	2  usage — an unknown flag, a missing argument, an unparsable duration
//	3  the server could not be reached, or did not answer in time
//
// Distinguishing 1 from 3 is the point. "The key is not there" and "the server
// is down" call for different things from a script, and a client that answers
// both with 1 forces it to parse English to tell them apart.
//
// A missing key is the case that needs the exit code rather than the output:
// GET's own reply cannot distinguish a key that is absent from a key holding an
// empty value, and both are legal. So `atlasctl get` prints nothing and exits 1
// for a key that is not there, and prints nothing and exits 0 for a key holding
// nothing. `atlasctl exists` reports a count and exits 0 either way — it
// answered the question it was asked, and so does a `del` that removed nothing.
//
// The SDK's six error categories map onto those four codes, and the interesting
// part is where the mapping is not one to one: a timeout is a connection
// failure (3) because the caller is in the same position an unreachable server
// leaves them in, while an authentication failure and a protocol error are
// command failures (1) because the socket worked and a retry changes nothing.
//
// # Output
//
// Data goes to stdout, diagnostics to stderr, so pipelines work. A usage error
// writes nothing to stdout at all: a script parsing output gets nothing to
// parse.
//
// A value is written to stdout as the bytes the server holds, with nothing
// added — no trailing newline, no escaping — whenever stdout is not a
// terminal. `atlasctl get k > file` therefore round-trips a value containing
// null bytes or invalid UTF-8. Escaping is for humans and applies only when
// stdout is a terminal, where the alternative is a mangled prompt.
//
// `set --stdin` is the reverse, and the only way to store a value containing a
// null byte — an argument list cannot carry one. It also keeps the value out of
// `ps` and the shell history, for the same reason --auth is discouraged.
//
// # JSON
//
// --json prints exactly one JSON object and nothing else. People will parse it,
// so its shape is part of the interface and is stable:
//
//	{"ok": true,  "command": "get", "data": {...}}
//	{"ok": false, "command": "get", "data": {...}, "error": {"code": 1, "kind": "not_found", "message": "..."}}
//
// "ok" is true exactly when the exit code is 0, and "error".code is always the
// exit code, so a script needs only one of the two. "kind" is a stable token —
// usage, connection, timeout, auth, server, protocol, not_found, closed,
// canceled, error — and "message" is prose that may change.
//
// A failure is still a JSON object: a client that emitted a bare error string
// here would break every script in the case the script most needs to handle.
//
// The "data" object per command:
//
//	ping    {"latency_ms": 0.42}
//	get     {"key": k, "found": bool, "value": v, "encoding": "utf8"|"base64"}
//	set     {"key": k, "ttl_seconds": n|null, "bytes": n}
//	del     {"keys": [...], "removed": n}
//	exists  {"keys": [...], "count": n}
//	keys    {"pattern": p, "keys": [...], "count": n, "encoding": "utf8"|"base64"}
//	scan    {"match": p, "keys": [...], "count": n, "cursor": "0", "complete": bool, "encoding": ...}
//	stats   {"stats": {"keys": 12, ...}}
//	info    {"sections": [...], "fields": {"atlascache_version": "0.1.0", ...}, "text": "..."}
//	version {"version": "0.1.0", "commit": "...", "built": "..."}
//
// A value is a JSON string when it is valid UTF-8 and base64 otherwise, and
// "encoding" says which. A key list carries one "encoding" for the whole list,
// base64 as soon as any one key needs it. `help` is the one command that
// ignores --json: it prints its usage text either way.
//
// # Secrets
//
// ATLASCACHE_AUTH is the preferred way to pass a token. --auth also works and
// is warned about, because a token on the command line is visible in `ps` to
// every other user on the host, and lands in the shell history besides.
//
// `ping` is on the server's pre-authentication allowlist, so it answers with or
// without a token: a successful ping is not evidence that a token works. Run a
// data command for that.
package main

import "os"

// Build metadata, injected via -ldflags by the Makefile.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	os.Exit(run(newEnv(), os.Args[1:]))
}
