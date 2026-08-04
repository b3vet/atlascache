package runner

import (
	"context"
	"sort"
	"strconv"
	"strings"
)

// Harness drives one server process for the lifetime of one spec.
//
// The runner owns the calling order: Close is always called exactly once, in a
// defer, after Start returns — including when a step fails or panics.
//
// Implementations must honor these contracts:
//
//   - Start blocks until the server is ready to serve, or the context expires.
//     Readiness is a positive signal (an admin health probe), never a sleep.
//   - Stop shuts the server down gracefully and waits for the process to exit
//     within the graceful window. Stopping an already-stopped server is a
//     no-op that returns nil, so a spec may end with the server already killed.
//   - Restart is Stop followed by Start against the same data directory. The
//     ports may change; ServerInfo is re-read by callers that need them.
//   - Kill sends SIGKILL and does not wait for a graceful exit. Killing an
//     already-dead server is a no-op that returns nil.
//   - Send transparently reconnects when the connection was lost to a Kill,
//     a Restart, or the server closing it. A protocol-level error reply is
//     returned as a Reply of kind KindError with a nil error; the error return
//     is reserved for transport failures and timeouts.
//   - Logs returns everything captured from the server's stdout and stderr so
//     far. It is only read on failure, so a passing run stays quiet.
//   - Close releases every resource the harness owns: the process is killed if
//     still alive and the temp data directory is removed. Close is idempotent.
type Harness interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Restart(ctx context.Context) error
	Kill() error
	Send(ctx context.Context, cmd string) (Reply, error)
	Info() ServerInfo
	Logs() string
	Close() error
}

// ServerInfo describes the running server, for scenarios that need to reach it
// outside the command protocol — an admin HTTP probe, or the data directory.
type ServerInfo struct {
	ClientAddr string
	AdminAddr  string
	DataDir    string
	PID        int
}

// HarnessOptions is everything the runner knows about the server a spec needs.
// The Config map is the spec's `config:` block verbatim; the harness writes it
// to a temp config file and points the server at it.
type HarnessOptions struct {
	SpecName string
	Binary   string
	Config   map[string]any
}

// HarnessFactory builds one harness per spec. Specs run in parallel, so a
// factory must hand out isolated ports and data directories on every call.
type HarnessFactory func(HarnessOptions) (Harness, error)

// ReplyKind is the shape of a server reply. The zero value is deliberately
// invalid so that a half-built Reply fails an assertion rather than passing it.
type ReplyKind int

// nilText is how a null reply is written in a spec, and invalidText is what a
// half-built reply renders as so that it fails an assertion legibly.
const (
	nilText     = "NIL"
	invalidText = "<invalid reply>"
)

// Reply kinds. KindInvalid is the zero value and never matches an assertion.
const (
	KindInvalid ReplyKind = iota
	KindStatus
	KindBulk
	KindInteger
	KindNil
	KindError
	KindArray
	KindMap
)

// String names the kind for diagnostics.
func (k ReplyKind) String() string {
	switch k {
	case KindStatus:
		return "status"
	case KindBulk:
		return "bulk"
	case KindInteger:
		return "integer"
	case KindNil:
		return "nil"
	case KindError:
		return "error"
	case KindArray:
		return "array"
	case KindMap:
		return "map"
	case KindInvalid:
		return "invalid"
	default:
		return "invalid"
	}
}

// Reply is one decoded server reply. The runner compares replies as text, so
// the protocol client decides the kind and the runner never re-parses the wire
// format.
type Reply struct {
	Kind    ReplyKind
	Text    string
	Integer int64
	Items   []Reply
	Pairs   map[string]Reply
}

// StatusReply builds a simple-status reply such as PONG or OK.
func StatusReply(text string) Reply { return Reply{Kind: KindStatus, Text: text} }

// BulkReply builds a bulk-string reply.
func BulkReply(text string) Reply { return Reply{Kind: KindBulk, Text: text} }

// IntegerReply builds an integer reply.
func IntegerReply(n int64) Reply { return Reply{Kind: KindInteger, Integer: n} }

// NilReply builds the null reply, which specs assert as NIL.
func NilReply() Reply { return Reply{Kind: KindNil} }

// ErrorReply builds an error reply. Its text is what expect_error matches.
func ErrorReply(text string) Reply { return Reply{Kind: KindError, Text: text} }

// ArrayReply builds an array reply.
func ArrayReply(items ...Reply) Reply { return Reply{Kind: KindArray, Items: items} }

// MapReply builds a map reply, the natural shape for STATS and INFO.
func MapReply(pairs map[string]Reply) Reply { return Reply{Kind: KindMap, Pairs: pairs} }

// String renders the reply as the text an `expect` assertion compares against.
// A nil reply renders as NIL, which is what specs write.
func (r Reply) String() string {
	switch r.Kind {
	case KindStatus, KindBulk, KindError:
		return r.Text
	case KindInteger:
		return strconv.FormatInt(r.Integer, 10)
	case KindNil:
		return nilText
	case KindArray:
		parts := make([]string, len(r.Items))
		for i, item := range r.Items {
			parts[i] = item.String()
		}
		return "[" + strings.Join(parts, " ") + "]"
	case KindMap:
		keys := sortedPairKeys(r.Pairs)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, key+"="+r.Pairs[key].String())
		}
		return "{" + strings.Join(parts, " ") + "}"
	case KindInvalid:
		return invalidText
	default:
		return invalidText
	}
}

// Fields flattens a reply into the key/value pairs that expect_field asserts
// over. Three shapes are understood, so the protocol client may decode STATS
// however the wire format presents it:
//
//   - a map reply, used directly;
//   - an array reply of alternating key and value elements;
//   - a status or bulk reply of "key:value" or "key=value" lines, where blank
//     lines and lines beginning with # are ignored.
//
// Any other shape yields no fields, and the assertion reports the reply as
// having none.
func (r Reply) Fields() map[string]string {
	fields := map[string]string{}
	switch r.Kind {
	case KindMap:
		for key, value := range r.Pairs {
			fields[key] = value.String()
		}
	case KindArray:
		if len(r.Items)%2 != 0 {
			return fields
		}
		for i := 0; i < len(r.Items); i += 2 {
			fields[r.Items[i].String()] = r.Items[i+1].String()
		}
	case KindStatus, KindBulk:
		for _, line := range strings.Split(r.Text, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, value, ok := splitPair(line)
			if !ok {
				continue
			}
			fields[key] = value
		}
	case KindInvalid, KindInteger, KindNil, KindError:
		return fields
	}
	return fields
}

func splitPair(line string) (key, value string, ok bool) {
	idx := strings.IndexAny(line, ":=")
	if idx <= 0 {
		return "", "", false
	}
	return strings.TrimSpace(line[:idx]), strings.TrimSpace(line[idx+1:]), true
}

func sortedPairKeys(pairs map[string]Reply) []string {
	keys := make([]string, 0, len(pairs))
	for key := range pairs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func describeFields(fields map[string]string) string {
	if len(fields) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// tailLines returns the last n non-empty-trimmed lines of s, as a block.
func tailLines(s string, n int) string {
	if s == "" || n <= 0 {
		return ""
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
