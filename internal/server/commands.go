package server

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/b3vet/atlascache/internal/protocol"
)

// Store is the keyspace the data commands run against.
//
// The transport layer declares the seam it needs rather than importing the
// engine that fills it, so nothing under internal/server knows what a shard, an
// entry or an eviction policy is. cmd/atlascache is where the two are joined,
// and it is also where the engine's errors are translated into the two below.
//
// Get returns a read-only view the store owns (ADR-0013). A handler may encode
// it into the reply it is building — which happens before the command returns —
// and must neither mutate it nor keep it afterwards.
type Store interface {
	Get(key []byte) (value []byte, exists bool)
	Set(key, value []byte, ttl time.Duration) error
	Delete(key []byte) bool

	// GetTTL reports the remaining life of a live key: a negative duration when
	// the key has no expiry at all, and exists false when there is no live key.
	// The two are different answers and TTL replies differently to each.
	GetTTL(key []byte) (ttl time.Duration, exists bool)

	// SetTTL attaches an expiry to a live key, reporting whether it found one.
	SetTTL(key []byte, ttl time.Duration) bool
}

// Failures a Store may report that have a RESP reply of their own. Anything
// else is surfaced as a generic error, so an unexpected engine failure reaches
// the client legibly rather than as a dropped connection.
var (
	// ErrValueTooLarge means the value is over max_value_size.
	ErrValueTooLarge = errors.New("value exceeds max_value_size")

	// ErrOutOfMemory means the write did not fit under max_memory and eviction
	// could not make room for it.
	ErrOutOfMemory = errors.New("out of memory")

	// ErrInvalidKey means the store refused the key itself.
	ErrInvalidKey = errors.New("invalid key")
)

// What TTL answers when there is nothing to count down. The two are distinct
// because clients read them differently: -1 is a key that lives forever, -2 is
// no key at all.
const (
	ttlNoExpiry = -1
	ttlNoKey    = -2
)

// unbounded is maxArgs for a command that takes any number of arguments.
const unbounded = -1

// The command names this server answers to, upper-cased as the codec delivers
// them.
const (
	cmdPing   = "PING"
	cmdQuit   = "QUIT"
	cmdSet    = "SET"
	cmdGet    = "GET"
	cmdDel    = "DEL"
	cmdTTL    = "TTL"
	cmdExpire = "EXPIRE"
)

// commandSpec is one row of the dispatch table: how many arguments the command
// takes, what serves it, and whether the connection ends afterwards.
//
// Arity is checked once, centrally, so every handler may index cmd.Args without
// re-deriving what it was already guaranteed.
type commandSpec struct {
	minArgs int
	maxArgs int // unbounded for no upper limit
	closes  bool
	handler func(*Server, protocol.Command) protocol.Reply
}

// commands is the dispatch table. P1 adds the five data commands to the
// skeleton's two (FEAT-0017); P2 extends this map rather than replacing it, and
// neither the codec nor the Transport interface moves to accommodate it.
var commands = map[string]commandSpec{
	cmdPing: {minArgs: 0, maxArgs: 1, handler: (*Server).ping},
	cmdQuit: {minArgs: 0, maxArgs: unbounded, closes: true, handler: (*Server).quit},

	// SET takes any tail so that `SET k v EX 1 PX 1` is the syntax error Redis
	// reports rather than an arity error; the option parser rejects it.
	cmdSet:    {minArgs: 2, maxArgs: unbounded, handler: (*Server).set},
	cmdGet:    {minArgs: 1, maxArgs: 1, handler: (*Server).get},
	cmdDel:    {minArgs: 1, maxArgs: unbounded, handler: (*Server).del},
	cmdTTL:    {minArgs: 1, maxArgs: 1, handler: (*Server).ttl},
	cmdExpire: {minArgs: 2, maxArgs: 2, handler: (*Server).expire},
}

// accepts reports whether the command was given a workable number of arguments.
func (c commandSpec) accepts(args int) bool {
	if args < c.minArgs {
		return false
	}
	return c.maxArgs == unbounded || args <= c.maxArgs
}

// ping answers PONG, or echoes its argument as a bulk string.
func (s *Server) ping(cmd protocol.Command) protocol.Reply {
	if len(cmd.Args) == 1 {
		return protocol.BulkString(cmd.Args[0])
	}
	return protocol.SimpleString("PONG")
}

// quit acknowledges; the dispatcher closes the connection after the reply.
func (s *Server) quit(protocol.Command) protocol.Reply {
	return protocol.SimpleString("OK")
}

// set stores a value, with an optional expiry given as EX seconds or PX
// milliseconds.
func (s *Server) set(cmd protocol.Command) protocol.Reply {
	ttl, bad := parseSetExpiry(cmd.Args[2:])
	if bad != nil {
		return bad
	}

	if err := s.store.Set(cmd.Args[0], cmd.Args[1], ttl); err != nil {
		return storeFailure(err)
	}
	return protocol.SimpleString("OK")
}

// get answers with the stored value, or with the null bulk string.
func (s *Server) get(cmd protocol.Command) protocol.Reply {
	value, exists := s.store.Get(cmd.Args[0])
	if !exists {
		// A miss — including a key that has expired — is null, never an error.
		// Clients read null as "not cached" and an error as "the server is
		// broken", and a cache miss is emphatically the first of those.
		return protocol.Nil
	}

	// The value is the store's own memory. It is copied into the reply buffer
	// before this command completes and is not retained, which is the read
	// contract documented on Store and in ADR-0013.
	return protocol.BulkString(value)
}

// del removes keys, answering with how many it actually removed.
func (s *Server) del(cmd protocol.Command) protocol.Reply {
	var removed int64
	for _, key := range cmd.Args {
		if s.store.Delete(key) {
			removed++
		}
	}
	return protocol.Integer(removed)
}

// ttl reports the remaining life of a key in whole seconds.
func (s *Server) ttl(cmd protocol.Command) protocol.Reply {
	remaining, exists := s.store.GetTTL(cmd.Args[0])
	switch {
	case !exists:
		return protocol.Integer(ttlNoKey)
	case remaining < 0:
		return protocol.Integer(ttlNoExpiry)
	case remaining == 0:
		// The key expired between the lookup and this line. It is gone as far
		// as any client can tell, so it answers as gone.
		return protocol.Integer(ttlNoKey)
	default:
		return protocol.Integer(wholeSeconds(remaining))
	}
}

// expire attaches an expiry to an existing key.
func (s *Server) expire(cmd protocol.Command) protocol.Reply {
	seconds, err := strconv.ParseInt(string(cmd.Args[1]), 10, 64)
	if err != nil {
		return errNotAnInteger()
	}

	// An expiry already in the past deletes the key rather than making it
	// permanent, and still counts as applied. This is Redis's behavior, and the
	// alternative — passing a non-positive TTL down to the store, which reads
	// it as "no expiry" — would turn `EXPIRE k -1` into a key that never dies.
	if seconds <= 0 {
		return applied(s.store.Delete(cmd.Args[0]))
	}

	ttl, ok := expiryFor(seconds, time.Second)
	if !ok {
		return protocol.Errorf("invalid expire time in 'expire' command")
	}
	return applied(s.store.SetTTL(cmd.Args[0], ttl))
}

// parseSetExpiry reads SET's optional expiry clause, returning the reply to
// send instead when it is malformed.
//
// EX and PX are mutually exclusive by construction: exactly one unit and one
// amount may follow the value, so writing both is a syntax error rather than a
// silent last-one-wins.
func parseSetExpiry(opts [][]byte) (time.Duration, protocol.Reply) {
	switch len(opts) {
	case 0:
		return 0, nil
	case 2:
	default:
		return 0, errSyntax()
	}

	var unit time.Duration
	switch strings.ToUpper(string(opts[0])) {
	case "EX":
		unit = time.Second
	case "PX":
		unit = time.Millisecond
	default:
		return 0, errSyntax()
	}

	amount, err := strconv.ParseInt(string(opts[1]), 10, 64)
	if err != nil {
		return 0, errNotAnInteger()
	}

	// Zero and negative are rejected rather than quietly meaning "no expiry":
	// a client that computed a TTL wrongly should hear about it.
	ttl, ok := expiryFor(amount, unit)
	if !ok {
		return 0, protocol.Errorf("invalid expire time in 'set' command")
	}
	return ttl, nil
}

// expiryFor converts an EX or PX amount into a duration, reporting false when
// the result would not fit — in a Duration, which counts nanoseconds, or in the
// absolute deadline the store derives from it. An overflowed deadline would
// land in the past and expire the key instantly, which is worse than an error.
func expiryFor(amount int64, unit time.Duration) (time.Duration, bool) {
	if amount <= 0 || amount > math.MaxInt64/int64(unit) {
		return 0, false
	}

	ttl := time.Duration(amount) * unit
	if int64(ttl) > math.MaxInt64-time.Now().UnixNano() {
		return 0, false
	}
	return ttl, true
}

// wholeSeconds rounds a remaining TTL the way Redis does — to the nearest
// second, so a key written with EX 10 reports 10 rather than 9.
func wholeSeconds(remaining time.Duration) int64 {
	return (remaining.Milliseconds() + 500) / 1000
}

// applied renders the 1/0 answer EXPIRE gives.
func applied(ok bool) protocol.Reply {
	if ok {
		return protocol.Integer(1)
	}
	return protocol.Integer(0)
}

// storeFailure maps a write failure onto the reply a Redis client expects.
func storeFailure(err error) protocol.Reply {
	switch {
	case errors.Is(err, ErrOutOfMemory):
		// The error Redis sends when maxmemory is reached and nothing can be
		// evicted. Clients recognize the OOM kind and back off on it.
		return protocol.Error{Kind: "OOM", Message: "command not allowed when used memory > 'maxmemory'."}
	case errors.Is(err, ErrValueTooLarge):
		return protocol.Errorf("value exceeds max_value_size")
	case errors.Is(err, ErrInvalidKey):
		return protocol.Errorf("invalid key")
	default:
		return protocol.Errorf("%v", err)
	}
}

func errSyntax() protocol.Reply {
	return protocol.Errorf("syntax error")
}

func errNotAnInteger() protocol.Reply {
	return protocol.Errorf("value is not an integer or out of range")
}
