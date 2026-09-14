package protocol

import "fmt"

// Reply is a value the codec can serialize as a RESP response.
//
// The hierarchy is logical rather than RESP2-shaped. It names every type RESP3
// defines, and every one of them has a RESP2 rendering, so v0.1.0 ships one
// encoder without the hierarchy being cut down to what that encoder needs
// (ADR-0028). FEAT-0048 adds the RESP3 encoder in P6 and adds no reply
// types; that it can is the test of whether this seam is real.
//
// Handlers build these values and never branch on protocol version. With one
// encoder there is nothing to branch on, which is what keeps the second encoder
// an addition rather than a rewrite.
//
// The RESP2 rendering of each type is documented on the type and implemented in
// encode_resp2.go. Where RESP2 has no equivalent frame, the rendering is the one
// Redis itself gives a RESP2 client: a map flattens, a boolean becomes 1 or 0, a
// double becomes a bulk string, attributes are dropped.
type Reply interface {
	isReply()
}

// KV is one key/value pair of a Map or an Attribute. Pairs are ordered because
// the wire format is: RESP2 flattens a map into an array, and an array whose
// order changed between two identical requests is a diff in every client's
// output.
type KV struct {
	Key   Reply
	Value Reply
}

// SimpleString is a status reply such as +OK or +PONG. RESP2 and RESP3 render
// it identically. Its text may not contain CR or LF — the encoder replaces
// them, because a status line carrying either would frame a second reply.
type SimpleString string

func (SimpleString) isReply() {}

// Error is a RESP error such as "-ERR unknown command". Kind is the machine
// readable prefix clients switch on (ERR, WRONGTYPE, NOAUTH, NOPROTO, OOM) and
// may be empty. RESP2 and RESP3 render it identically.
type Error struct {
	Kind    string
	Message string
}

func (Error) isReply() {}

// Error renders the reply as the text that follows the '-' on the wire.
func (e Error) Error() string {
	if e.Kind == "" {
		return e.Message
	}
	return e.Kind + " " + e.Message
}

// BulkError is RESP3's length-prefixed error, which exists so an error message
// may contain newlines. RESP2 has no such frame, so it renders as a simple
// error with its newlines folded to spaces — the same downgrade Redis applies.
type BulkError struct {
	Kind    string
	Message string
}

func (BulkError) isReply() {}

// Error renders the reply as the text a client reads as the error.
func (e BulkError) Error() string {
	return Error(e).Error()
}

// BulkString is a length-prefixed binary-safe string, e.g. "$5\r\nhello".
// RESP2 and RESP3 render it identically.
type BulkString []byte

func (BulkString) isReply() {}

// Verbatim is RESP3's verbatim string: a bulk string carrying a three-byte
// format hint such as "txt" or "mkd", so a client knows the payload is meant to
// be displayed rather than parsed. RESP2 renders the text as a plain bulk
// string and drops the hint, which is what a RESP2 client of Redis sees.
type Verbatim struct {
	Format string // three bytes, e.g. VerbatimText; the encoder defaults it
	Text   string
}

func (Verbatim) isReply() {}

// VerbatimText is the format hint for plain text, and the default when a
// Verbatim reply does not set one.
const VerbatimText = "txt"

// Integer is a RESP integer, e.g. ":1". RESP2 and RESP3 render it identically.
type Integer int64

func (Integer) isReply() {}

// BigNumber is RESP3's arbitrary-precision integer, carried as its decimal
// text because no Go integer type holds it. RESP2 renders it as a bulk string,
// which is what Redis sends a RESP2 client.
type BigNumber string

func (BigNumber) isReply() {}

// Boolean is RESP3's true/false. RESP2 renders it as the integer 1 or 0, which
// is how every Redis command that returns a boolean has always answered.
type Boolean bool

func (Boolean) isReply() {}

// Double is RESP3's floating point number. RESP2 renders it as a bulk string
// holding the shortest text that reads back as the same value, with the
// infinities written as "inf" and "-inf" as Redis writes them.
type Double float64

func (Double) isReply() {}

// Array is an ordered list of replies. RESP2 and RESP3 render it identically.
type Array []Reply

func (Array) isReply() {}

// Set is RESP3's unordered collection. RESP2 has no set frame, so it renders as
// an array; the order on the wire is the order the handler built, and no client
// may depend on it.
type Set []Reply

func (Set) isReply() {}

// Push is RESP3's out-of-band frame, which pub/sub in P10 delivers messages
// through. RESP2 has no push frame and delivers the same payload as an array,
// which is exactly how RESP2 pub/sub works today.
type Push []Reply

func (Push) isReply() {}

// Map is an ordered list of pairs. RESP2 has no map frame, so it renders as an
// array of 2n elements, key then value — the shape INFO-like commands have
// always had in RESP2.
type Map []KV

func (Map) isReply() {}

// Attribute is RESP3's out-of-band metadata attached to a reply. RESP2 has no
// attribute frame, so it renders as the wrapped value alone and the metadata is
// dropped, which is what Redis does for a RESP2 client.
type Attribute struct {
	Attrs []KV
	Value Reply
}

func (Attribute) isReply() {}

type nilReply struct{}

func (nilReply) isReply() {}

// Nil is the null reply: "$-1" in RESP2, "_" in RESP3. It is a value rather
// than a type so that every handler returning a miss returns the same one.
var Nil Reply = nilReply{}

// kindErr is the generic error kind, the one a client reads as "this request
// was wrong" with nothing more specific to say about it.
const kindErr = "ERR"

// Errorf builds a generic ERR reply.
func Errorf(format string, args ...any) Error {
	return Error{Kind: kindErr, Message: fmt.Sprintf(format, args...)}
}
