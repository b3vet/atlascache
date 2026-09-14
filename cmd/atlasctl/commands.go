package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/b3vet/atlascache/pkg/client"
)

// The command names. They are constants because this file, the help text and
// the tests all name them, and a typo in one of those should be a compile error
// rather than a command that quietly does not exist.
const (
	namePing   = "ping"
	nameGet    = "get"
	nameSet    = "set"
	nameDel    = "del"
	nameExists = "exists"
	nameKeys   = "keys"
	nameScan   = "scan"
	nameStats  = "stats"
	nameInfo   = "info"
)

// runFunc is one command, after its flags have been parsed. args is what was
// left over: the keys, the pattern, the sections.
type runFunc func(ctx context.Context, e *env, c client.Client, args []string) (result, error)

// command is one subcommand.
type command struct {
	name     string
	synopsis string
	summary  string
	details  string

	// setup registers the command's own flags and returns the function that
	// runs it. The closure is how a flag value reaches the run — the
	// alternative, a struct per command, buys nothing for nine of them.
	setup func(fs *flag.FlagSet) runFunc
}

// commands is the whole surface, in the order `atlasctl help` lists it: the
// order somebody learns them in, not alphabetical.
func commands() []command {
	return []command{
		pingCommand(), getCommand(), setCommand(), delCommand(), existsCommand(),
		keysCommand(), scanCommand(), statsCommand(), infoCommand(),
	}
}

// helpText is what `atlasctl help <command>` prints.
func (c command) helpText() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Usage:\n  atlasctl %s\n\n%s\n", c.synopsis, c.summary)
	if c.details != "" {
		fmt.Fprintf(&b, "\n%s\n", c.details)
	}

	fs := flag.NewFlagSet("atlasctl "+c.name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c.setup(fs)
	if own := flagHelp(fs); own != "" {
		fmt.Fprintf(&b, "\nFlags:\n%s", own)
	}

	fmt.Fprintf(&b, "\nGlobal flags:\n%s", globalFlagHelp())
	return b.String()
}

// ---- ping -------------------------------------------------------------------

type pingData struct {
	LatencyMS float64 `json:"latency_ms"`
}

func pingCommand() command {
	return command{
		name:     namePing,
		synopsis: namePing,
		summary:  "Check that the server answers.",
		details: "Exits 3 when the server cannot be reached, which is what makes\n" +
			"`atlasctl ping || restart-something` a usable line in a script.",
		setup: func(*flag.FlagSet) runFunc {
			return func(ctx context.Context, _ *env, c client.Client, args []string) (result, error) {
				if len(args) > 0 {
					return result{}, usageErrorf("ping takes no arguments, got %d", len(args))
				}

				started := time.Now()
				if err := c.Ping(ctx); err != nil {
					return result{}, err
				}
				elapsed := time.Since(started)

				return result{
					data: pingData{LatencyMS: float64(elapsed.Microseconds()) / 1000},
					text: func(o *output) error { return o.line("PONG") },
				}, nil
			}
		},
	}
}

// ---- get --------------------------------------------------------------------

type getData struct {
	Key      string  `json:"key"`
	Found    bool    `json:"found"`
	Value    *string `json:"value,omitempty"`
	Encoding string  `json:"encoding,omitempty"`
}

func getCommand() command {
	return command{
		name:     nameGet,
		synopsis: "get <key>",
		summary:  "Print the value held at a key.",
		details: "The value is written to stdout as the bytes the server holds, with no\n" +
			"trailing newline and no escaping, whenever stdout is not a terminal — so\n" +
			"`atlasctl get k > file` round-trips a value of arbitrary bytes.\n\n" +
			"A key that is not there prints nothing and exits 1. A key holding an empty\n" +
			"value prints nothing and exits 0. The exit code is the only place that\n" +
			"distinction can live, because the output cannot carry it.",
		setup: func(*flag.FlagSet) runFunc {
			return func(ctx context.Context, _ *env, c client.Client, args []string) (result, error) {
				if len(args) != 1 {
					return result{}, usageErrorf("get takes one key, got %d", len(args))
				}
				key := args[0]

				value, found, err := c.Get(ctx, key)
				if err != nil {
					return result{}, err
				}
				if !found {
					return result{data: getData{Key: key, Found: false}}, notFoundError(key)
				}

				text, encoding := encodeValue(value)
				return result{
					data: getData{Key: key, Found: true, Value: &text, Encoding: encoding},
					text: func(o *output) error { return o.value(value) },
				}, nil
			}
		},
	}
}

// ---- set --------------------------------------------------------------------

type setData struct {
	Key        string   `json:"key"`
	TTLSeconds *float64 `json:"ttl_seconds"`
	Bytes      int      `json:"bytes"`
}

func setCommand() command {
	return command{
		name:     nameSet,
		synopsis: "set <key> <value> [--ttl 60s] | set <key> --stdin [--ttl 60s]",
		summary:  "Store a value, optionally with an expiry.",
		details: "--stdin reads the value from standard input instead of the command line.\n" +
			"It is the only way to store a value containing a null byte, which an\n" +
			"argument list cannot carry — and it keeps a value out of `ps` and the\n" +
			"shell history, which matters for the same reason --auth does.\n\n" +
			"Write `--` before a value that begins with a dash.",
		setup: func(fs *flag.FlagSet) runFunc {
			ttl := fs.Duration("ttl", 0, "expire the key after this `duration`; 0 means no expiry")
			fromStdin := fs.Bool("stdin", false, "read the value from standard input")

			return func(ctx context.Context, e *env, c client.Client, args []string) (result, error) {
				key, value, err := setOperands(e, args, *fromStdin)
				if err != nil {
					return result{}, err
				}
				if *ttl < 0 {
					return result{}, usageErrorf("--ttl must not be negative, got %s", *ttl)
				}

				if err := c.Set(ctx, key, value, *ttl); err != nil {
					return result{}, err
				}

				var seconds *float64
				if *ttl > 0 {
					s := ttl.Seconds()
					seconds = &s
				}
				return result{
					data: setData{Key: key, TTLSeconds: seconds, Bytes: len(value)},
					text: func(o *output) error { return o.line("OK") },
				}, nil
			}
		},
	}
}

// setOperands reads the key and the value, from the command line or from stdin.
func setOperands(e *env, args []string, fromStdin bool) (key string, value []byte, err error) {
	if fromStdin {
		if len(args) != 1 {
			return "", nil, usageErrorf("set --stdin takes one key and no value, got %d arguments", len(args))
		}
		value, err = io.ReadAll(e.stdin)
		if err != nil {
			return "", nil, usageErrorf("reading the value from stdin: %v", err)
		}
		return args[0], value, nil
	}

	if len(args) != 2 {
		return "", nil, usageErrorf("set takes a key and a value, got %d arguments; use --stdin to read the value from a pipe", len(args))
	}
	return args[0], []byte(args[1]), nil
}

// ---- del --------------------------------------------------------------------

type delData struct {
	Keys    []string `json:"keys"`
	Removed int64    `json:"removed"`
}

func delCommand() command {
	return countingCommand(command{
		name:     nameDel,
		synopsis: "del <key>...",
		summary:  "Remove keys, and report how many were removed.",
		details: "Removing a key that is not there is not a failure: the exit code is 0 and\n" +
			"the count is what says how many keys the command actually found.",
	}, func(ctx context.Context, c client.Client, keys []string) (int64, any, error) {
		removed, err := c.Del(ctx, keys...)
		return removed, delData{Keys: keys, Removed: removed}, err
	})
}

// ---- exists -----------------------------------------------------------------

type existsData struct {
	Keys  []string `json:"keys"`
	Count int64    `json:"count"`
}

func existsCommand() command {
	return countingCommand(command{
		name:     nameExists,
		synopsis: "exists <key>...",
		summary:  "Count how many of the given keys are present.",
		details: "A key named twice counts twice, which is what the server does and what a\n" +
			"client written against Redis expects.\n\n" +
			"A count of zero exits 0: the command answered the question it was asked.\n" +
			"Branch on the number, or use `get` when absence should be a failure.",
	}, func(ctx context.Context, c client.Client, keys []string) (int64, any, error) {
		count, err := c.Exists(ctx, keys...)
		return count, existsData{Keys: keys, Count: count}, err
	})
}

// countingCommand builds a command whose whole answer is one number over a list
// of keys. del and exists differ only in which number, so they are one shape
// with two calls rather than two copies of the same twenty lines.
func countingCommand(spec command, call func(context.Context, client.Client, []string) (int64, any, error)) command {
	spec.setup = func(*flag.FlagSet) runFunc {
		return func(ctx context.Context, _ *env, c client.Client, args []string) (result, error) {
			if len(args) == 0 {
				return result{}, usageErrorf("%s takes at least one key", spec.name)
			}

			count, data, err := call(ctx, c, args)
			if err != nil {
				return result{}, err
			}
			return result{
				data: data,
				text: func(o *output) error { return o.line("%d", count) },
			}, nil
		}
	}
	return spec
}

// ---- keys -------------------------------------------------------------------

type keysData struct {
	Pattern  string   `json:"pattern"`
	Keys     []string `json:"keys"`
	Count    int      `json:"count"`
	Encoding string   `json:"encoding"`
}

func keysCommand() command {
	return command{
		name:     nameKeys,
		synopsis: "keys <pattern>",
		summary:  "List every key matching a glob.",
		details: "KEYS walks the whole keyspace and holds a connection for the length of the\n" +
			"walk. Use `scan` against anything you would mind blocking.",
		setup: func(*flag.FlagSet) runFunc {
			return func(ctx context.Context, _ *env, c client.Client, args []string) (result, error) {
				if len(args) != 1 {
					return result{}, usageErrorf("keys takes one pattern, got %d", len(args))
				}

				keys, err := c.Keys(ctx, args[0])
				if err != nil {
					return result{}, err
				}
				encoded, encoding := encodeKeys(keys)

				return result{
					data: keysData{Pattern: args[0], Keys: encoded, Count: len(keys), Encoding: encoding},
					text: func(o *output) error { return o.write(indentedLines(keys)) },
				}, nil
			}
		},
	}
}

// ---- scan -------------------------------------------------------------------

type scanData struct {
	Match    string   `json:"match"`
	Keys     []string `json:"keys"`
	Count    int      `json:"count"`
	Cursor   string   `json:"cursor"`
	Complete bool     `json:"complete"`
	Encoding string   `json:"encoding"`
}

func scanCommand() command {
	return command{
		name:     nameScan,
		synopsis: "scan [--match <glob>] [--count <n>]",
		summary:  "Walk the keyspace a page at a time and print every key.",
		details: "The iteration runs to completion here rather than exposing a cursor: a\n" +
			"cursor belongs to the connection that issued it (ADR-0017), and the next\n" +
			"invocation of a CLI is a different process on a different connection, so a\n" +
			"cursor handed back to the shell could not be used.\n\n" +
			"--count bounds the work one page does, not how many keys come back.",
		setup: func(fs *flag.FlagSet) runFunc {
			match := fs.String("match", "", "only keys matching this `glob`")
			count := fs.Int("count", 0, "how much work one page may do; 0 leaves it to the server")

			return func(ctx context.Context, _ *env, c client.Client, args []string) (result, error) {
				if len(args) > 0 {
					return result{}, usageErrorf("scan takes no arguments; did you mean --match %s?", args[0])
				}
				if *count < 0 {
					return result{}, usageErrorf("--count must not be negative, got %d", *count)
				}

				var keys []string
				cursor := client.ScanStart
				for {
					page, err := c.Scan(ctx, cursor, *match, *count)
					if err != nil {
						return result{}, err
					}
					keys = append(keys, page.Keys...)
					cursor = page.Cursor
					if page.Done() {
						break
					}
				}
				encoded, encoding := encodeKeys(keys)

				return result{
					data: scanData{
						Match: *match, Keys: encoded, Count: len(keys),
						Cursor: cursor, Complete: true, Encoding: encoding,
					},
					text: func(o *output) error { return o.write(indentedLines(keys)) },
				}, nil
			}
		},
	}
}

// ---- stats ------------------------------------------------------------------

type statsData struct {
	Stats map[string]int64 `json:"stats"`
}

func statsCommand() command {
	return command{
		name:     nameStats,
		synopsis: nameStats,
		summary:  "Print the server's counters.",
		details:  "Counters a later server adds appear here without an update to this client.",
		setup: func(*flag.FlagSet) runFunc {
			return func(ctx context.Context, _ *env, c client.Client, args []string) (result, error) {
				if len(args) > 0 {
					return result{}, usageErrorf("stats takes no arguments, got %d", len(args))
				}

				stats, err := c.Stats(ctx)
				if err != nil {
					return result{}, err
				}

				names := make([]string, 0, len(stats))
				for name := range stats {
					names = append(names, name)
				}
				sort.Strings(names)

				lines := make([]string, 0, len(names))
				for _, name := range names {
					lines = append(lines, name+": "+strconv.FormatInt(stats[name], 10))
				}

				return result{
					data: statsData{Stats: stats},
					text: func(o *output) error { return o.write(indentedLines(lines)) },
				}, nil
			}
		},
	}
}

// ---- info -------------------------------------------------------------------

type infoData struct {
	Sections []string          `json:"sections"`
	Fields   map[string]string `json:"fields"`
	Text     string            `json:"text"`
}

func infoCommand() command {
	return command{
		name:     nameInfo,
		synopsis: "info [section...]",
		summary:  "Print the server's INFO report.",
		details: "The text is the server's own, in Redis's format, so anything that already\n" +
			"parses INFO keeps working. --json adds the same fields parsed into an\n" +
			"object, with the raw text alongside.",
		setup: func(*flag.FlagSet) runFunc {
			return func(ctx context.Context, _ *env, c client.Client, args []string) (result, error) {
				text, err := c.Info(ctx, args...)
				if err != nil {
					return result{}, err
				}

				sections := args
				if sections == nil {
					sections = []string{}
				}
				return result{
					data: infoData{Sections: sections, Fields: parseInfo(text), Text: text},
					text: func(o *output) error { return o.write(withTrailingNewline(text)) },
				}, nil
			}
		},
	}
}

// parseInfo reads INFO's `key:value` lines, skipping the `# Section` headers
// and the blank lines between sections. Line endings are CRLF, which is what
// Redis writes and what every INFO parser already expects.
func parseInfo(text string) map[string]string {
	fields := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields[strings.TrimSpace(name)] = strings.TrimSpace(value)
	}
	return fields
}

// withTrailingNewline keeps a terminal's prompt on its own line without adding
// a second newline to text that already ends in one.
func withTrailingNewline(text string) string {
	if text == "" || strings.HasSuffix(text, "\n") {
		return text
	}
	return text + "\n"
}
