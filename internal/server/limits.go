package server

import (
	"time"

	"github.com/b3vet/atlascache/internal/protocol"
)

// ConnLimits bound what one client may cost the server.
//
// Every field here exists because the connection layer is the only place that
// can see the quantity it bounds. The protocol limits bound one field of one
// request each and are individually correct; nothing below them can see how
// many connections there are, how long one has been silent, how much of a
// request has arrived, or how much reply is waiting to go out. Those are the
// four ways a client that has sent nothing illegal still costs the server
// without limit (ADR-0021, ISSUE-0018).
//
// The zero value is not usable: normalize replaces every unset field with its
// default, so a partly filled ConnLimits cannot silently switch a limit off.
type ConnLimits struct {
	// MaxConnections is the ceiling on connections being served at once.
	MaxConnections int

	// IdleTimeout closes a connection that has completed no command for this
	// long. It is refreshed per command, never per byte: a client dribbling
	// bytes without finishing a request is idle in the only sense that matters.
	// Zero disables it.
	IdleTimeout time.Duration

	// MaxRequestBytes is the wire bytes one request may draw from the socket.
	//
	// It is the bound the per-field protocol limits leave open. Each of them is
	// correct on its own and none of them bounds their product: a request of a
	// million one-byte elements breaks no limit, is 7MB to send, and costs
	// 154MB to decode (ISSUE-0018). Bounding the bytes one request may consume
	// bounds the product, because every element costs wire bytes to send.
	MaxRequestBytes int

	// MaxPipelineCommands is how many pipelined commands are executed before
	// the accumulated replies are flushed. It is the count half of the batch
	// cap; MaxBatchBytes is the size half, and whichever is reached first ends
	// the batch.
	MaxPipelineCommands int

	// MaxOutputBytes is the ceiling on reply bytes buffered for one connection.
	// A client that issues KEYS * against a large keyspace and stops reading is
	// what it is for. Zero leaves output uncapped.
	MaxOutputBytes int
}

// The defaults, and the floors a configured value is held to.
const (
	// defaultMaxConnections matches Redis's own maxclients. ADR-0021 costed a
	// connection at ~8KB of goroutine stack; with the read and write buffers
	// counted the real figure is nearer 40KB, so this is about 400MB of
	// connection overhead at the ceiling — which is the number that makes the
	// limit mandatory rather than advisory.
	defaultMaxConnections = 10000

	// defaultIdleTimeout is what FEAT-0024 specifies. Redis defaults to no
	// timeout; a cache with a connection limit cannot afford to, because a
	// connection that opens and says nothing holds a slot as effectively as one
	// doing work.
	defaultIdleTimeout = 30 * time.Second

	// defaultMaxRequestBytes is what a request may consume when nothing has
	// derived a budget from storage.max_value_size.
	defaultMaxRequestBytes = 2 * 1024 * 1024

	// minRequestBytes keeps a configured budget above one read buffer. The
	// budget is charged against bytes drawn from the socket, and a single fill
	// of the read buffer may carry a whole pipeline batch; a budget at or below
	// that would refuse the first request of a batch for the sin of arriving
	// alongside others.
	minRequestBytes = 4 * connBufferSize

	defaultMaxPipelineCommands = 1024
	defaultMaxOutputBytes      = 64 * 1024 * 1024

	// maxRequestElements caps the arguments in one request below the protocol's
	// own million-element ceiling.
	//
	// The ceiling is Redis's, and Redis pays about 30 bytes an element for it.
	// This decoder pays 147 (measured by TestOneAcceptedRequestHasABoundedCost),
	// so a million elements is 154MB of decoding for a request that costs 7MB
	// to send. No client sends a command with more arguments than this — the
	// widest real ones are MGET and DEL over a key list — and the cost of the
	// ones that do is what ISSUE-0018 is.
	maxRequestElements = 128 * 1024

	// minElementWire is the fewest bytes one multibulk element can occupy:
	// "$1\r\nk\r\n". It converts a byte budget into an element count, so a
	// tightened request budget tightens the element limit with it instead of
	// leaving a second knob to forget.
	minElementWire = 7
)

// DefaultConnLimits returns the limits a server runs with when it is not told
// otherwise.
func DefaultConnLimits() ConnLimits {
	return ConnLimits{
		MaxConnections:      defaultMaxConnections,
		IdleTimeout:         defaultIdleTimeout,
		MaxRequestBytes:     defaultMaxRequestBytes,
		MaxPipelineCommands: defaultMaxPipelineCommands,
		MaxOutputBytes:      defaultMaxOutputBytes,
	}
}

// normalize replaces unset or unusable fields with their defaults.
//
// IdleTimeout and MaxOutputBytes are the two that may legitimately be zero, so
// only a negative value is corrected there: zero means "off", and an operator
// who wrote it meant it.
func (l ConnLimits) normalize() ConnLimits {
	defaults := DefaultConnLimits()

	if l.MaxConnections <= 0 {
		l.MaxConnections = defaults.MaxConnections
	}
	if l.IdleTimeout < 0 {
		l.IdleTimeout = defaults.IdleTimeout
	}
	if l.MaxRequestBytes <= 0 {
		l.MaxRequestBytes = defaults.MaxRequestBytes
	}
	if l.MaxRequestBytes < minRequestBytes {
		l.MaxRequestBytes = minRequestBytes
	}
	if l.MaxPipelineCommands <= 0 {
		l.MaxPipelineCommands = defaults.MaxPipelineCommands
	}
	if l.MaxOutputBytes < 0 {
		l.MaxOutputBytes = defaults.MaxOutputBytes
	}

	return l
}

// codecLimits derives the protocol limits from the connection limits and the
// configured value size.
//
// This is the one place the two layers meet. The bulk limit comes from
// storage.max_value_size so the parser and the engine cannot disagree about the
// largest value (ISSUE-0016); the element limit comes from the request budget
// so the parser and the connection cannot disagree about the largest request
// (ISSUE-0018). Both are derived rather than configured, because a limit an
// operator has to keep in step with another limit is one that drifts.
func (l ConnLimits) codecLimits(maxValueSize int) protocol.Limits {
	limits := protocol.LimitsForValueSize(maxValueSize)

	// Every element costs at least minElementWire bytes to send, so the budget
	// already implies an element count; taking the smaller of that and the
	// standing cap keeps a generous budget from reopening ISSUE-0018.
	fromBudget := l.MaxRequestBytes / minElementWire
	limits.MaxMultiBulkLength = min(limits.MaxMultiBulkLength, maxRequestElements, fromBudget)

	return limits
}
