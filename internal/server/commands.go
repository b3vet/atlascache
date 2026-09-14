package server

import (
	"errors"
	"math"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/b3vet/atlascache/internal/protocol"
)

// Store is the keyspace the data commands run against.
//
// The transport layer declares the seam it needs rather than importing the
// engine that fills it, so nothing under internal/server knows what a shard, an
// entry or an eviction policy is. cmd/atlascache is where the two are joined,
// and it is also where the engine's errors are translated into the ones below.
//
// Get returns a read-only view the store owns (ADR-0013). A handler may encode
// it into the reply it is building — which happens before the command returns —
// and must neither mutate it nor keep it afterwards. Keys and Scan return
// copies the handler owns, because they are built from the keyspace's own map
// keys and holding those would pin entries the cache has finished with.
type Store interface {
	// MaxValueSize is the configured storage.max_value_size, which the protocol
	// limits derive from. Without it the parser would accept a bulk string 16x
	// larger than anything the engine will store, which is the difference
	// between the limit ISSUE-0016 set and the one it meant to set.
	MaxValueSize() int

	Get(key []byte) (value []byte, exists bool)
	Set(key, value []byte, ttl time.Duration) error

	// SetNX stores only if no live key holds it, reporting whether it stored.
	// It runs the same admission path as Set, so it is subject to max_memory
	// and to eviction in the same way (ISSUE-0010).
	SetNX(key, value []byte, ttl time.Duration) (stored bool, err error)

	Delete(key []byte) bool

	// Exists reports whether a live key holds the name.
	Exists(key []byte) bool

	// Keys returns every live key matching a Redis glob. It is O(keyspace) and
	// blocks the caller for the length of the walk.
	Keys(pattern string) [][]byte

	// Scan returns one page of a snapshot scan and the cursor for the page
	// after it, filtering the page by a Redis glob. owner scopes the cursor to
	// one connection. A returned cursor of ScanCursorStart means the scan has
	// finished; any other value means more pages remain, even when the page
	// itself came back empty.
	Scan(owner uint64, cursor string, count int, match string) (keys [][]byte, next string, err error)

	// ReleaseScans drops every cursor owned by a connection that has gone away.
	ReleaseScans(owner uint64)

	// GetTTL reports the remaining life of a live key: a negative duration when
	// the key has no expiry at all, and exists false when there is no live key.
	// The two are different answers and TTL replies differently to each.
	GetTTL(key []byte) (ttl time.Duration, exists bool)

	// SetTTL attaches an expiry to a live key, reporting whether it found one.
	SetTTL(key []byte, ttl time.Duration) bool

	// Stats reports the keyspace accounting INFO, STATS and DBSIZE render. It
	// must be cheap enough to poll every second, because that is what a
	// monitoring dashboard does with INFO: O(shards), never O(keys), and with
	// no stop-the-world on the path (ISSUE-0012, ISSUE-0015).
	Stats() Stats
}

// Stats is the accounting INFO, STATS and DBSIZE report.
//
// It is declared here rather than imported from the engine for the same reason
// Store is: the transport layer names what it needs, and cmd/atlascache fills
// it in. That also means the wire format is not hostage to a field rename in
// the storage package.
type Stats struct {
	Keys        uint64
	KeysWithTTL uint64

	MemoryUsed uint64
	MemoryMax  uint64

	Gets    uint64
	Sets    uint64
	Deletes uint64
	Hits    uint64
	Misses  uint64

	Evictions   uint64
	Expirations uint64
	OOMRejected uint64

	ScanCursors       uint64
	ScanSnapshotBytes uint64

	// Process is what the Go runtime reports about this process. It is sampled
	// on a timer rather than read here, because reading it stops the world and
	// INFO is on a path clients poll (ISSUE-0015). A zero SampledAt means no
	// sample has been taken and the figures are not meaningful.
	Process ProcessStats
}

// ProcessStats is the process-level memory picture, as of SampledAt.
type ProcessStats struct {
	HeapAlloc   uint64
	HeapSys     uint64
	HeapInuse   uint64
	HeapObjects uint64
	StackInuse  uint64
	Sys         uint64
	NumGC       uint32
	Goroutines  int
	SampledAt   time.Time
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

	// ErrInvalidKey means the store refused the key itself. An empty key is no
	// longer one of those (ISSUE-0013); the sentinel stays because a future
	// limit — a maximum key length, say — would land here.
	ErrInvalidKey = errors.New("invalid key")

	// ErrScanCursorUnknown means the cursor is not one this connection holds:
	// never issued, another connection's, already finished, idle past the
	// timeout, or dropped to reclaim snapshot memory.
	ErrScanCursorUnknown = errors.New("scan cursor is unknown")

	// ErrTooManyScanCursors means the connection is at its open-scan limit.
	ErrTooManyScanCursors = errors.New("too many open scan cursors")

	// ErrScanMemoryExhausted means a shard's snapshot does not fit under the
	// global snapshot cap. See ADR-0017: this is a functional cliff, and it is
	// an error Redis can never produce.
	ErrScanMemoryExhausted = errors.New("scan snapshot memory limit reached")
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
	cmdSetNX  = "SETNX"
	cmdGet    = "GET"
	cmdDel    = "DEL"
	cmdExists = "EXISTS"
	cmdKeys   = "KEYS"
	cmdScan   = "SCAN"
	cmdTTL    = "TTL"
	cmdExpire = "EXPIRE"
	cmdHello  = "HELLO"

	cmdStats   = "STATS"
	cmdInfo    = "INFO"
	cmdDBSize  = "DBSIZE"
	cmdEcho    = "ECHO"
	cmdCommand = "COMMAND"
)

// What HELLO and INFO report about this server. The protocol version is a
// constant because v0.1.0 speaks RESP2 only (ADR-0028): there is no negotiated
// state, per connection or otherwise, for a handler to read.
const (
	serverName  = "atlascache"
	serverMode  = "standalone"
	serverRole  = "master"
	respVersion = 2
)

// Version is the server version HELLO and INFO report. cmd/atlascache sets it
// from the build stamp at startup (FEAT-0021); the default is what an
// unstamped build — `go run ./cmd/atlascache` — reports.
var Version = "0.1.0-dev"

// processStarted is when this process came up, which is what INFO's uptime
// counts from. Taken at package initialization rather than at server
// construction, because a client asking how long the server has been up means
// the process, not the listener.
var processStarted = time.Now()

// ScanCursorStart is the cursor a client presents to begin a scan, and the
// cursor returned once one has finished. It is decimal, not hex, and not an
// opaque token, because every mainstream client parses the SCAN cursor as an
// unsigned integer (ADR-0017).
const ScanCursorStart = "0"

// commandSpec is one row of the dispatch table: how many arguments the command
// takes, what serves it, and whether the connection ends afterwards.
//
// Arity is checked once, centrally, so every handler may index cmd.Args without
// re-deriving what it was already guaranteed.
type commandSpec struct {
	minArgs int
	maxArgs int // unbounded for no upper limit
	closes  bool
	handler func(*session, protocol.Command) protocol.Reply
}

// commands is the dispatch table. P1 added the five data commands to the
// skeleton's two (FEAT-0017); P2 extends this map rather than replacing it, and
// neither the codec nor the Transport interface moved to accommodate it.
var commands = map[string]commandSpec{
	cmdPing: {minArgs: 0, maxArgs: 1, handler: (*session).ping},
	cmdQuit: {minArgs: 0, maxArgs: unbounded, closes: true, handler: (*session).quit},

	// SET takes any tail so that `SET k v EX 1 PX 1` is the syntax error Redis
	// reports rather than an arity error; the option parser rejects it.
	cmdSet:    {minArgs: 2, maxArgs: unbounded, handler: (*session).set},
	cmdSetNX:  {minArgs: 2, maxArgs: 2, handler: (*session).setnx},
	cmdGet:    {minArgs: 1, maxArgs: 1, handler: (*session).get},
	cmdDel:    {minArgs: 1, maxArgs: unbounded, handler: (*session).del},
	cmdExists: {minArgs: 1, maxArgs: unbounded, handler: (*session).exists},
	cmdKeys:   {minArgs: 1, maxArgs: 1, handler: (*session).keys},
	cmdTTL:    {minArgs: 1, maxArgs: 1, handler: (*session).ttl},
	cmdExpire: {minArgs: 2, maxArgs: 2, handler: (*session).expire},

	// SCAN takes any tail so that an unknown option is the syntax error Redis
	// reports rather than an arity error. TYPE lands here when it arrives.
	cmdScan: {minArgs: 1, maxArgs: unbounded, handler: (*session).scan},

	// HELLO takes any tail so that `HELLO 3 AUTH user pass` — what a client
	// probing for RESP3 sends — is answered with -NOPROTO, the reply it knows
	// how to fall back from, rather than an arity error it does not.
	cmdHello: {minArgs: 0, maxArgs: unbounded, handler: (*session).hello},

	cmdStats:  {minArgs: 0, maxArgs: 0, handler: (*session).stats},
	cmdDBSize: {minArgs: 0, maxArgs: 0, handler: (*session).dbsize},
	cmdEcho:   {minArgs: 1, maxArgs: 1, handler: (*session).echo},

	// INFO takes any number of section names, which is what Redis 7 accepts.
	cmdInfo: {minArgs: 0, maxArgs: unbounded, handler: (*session).info},

	// COMMAND swallows its subcommands — DOCS, COUNT, INFO — and answers the
	// same empty array to all of them. See the handler for why that is better
	// than a fabricated table.
	cmdCommand: {minArgs: 0, maxArgs: unbounded, handler: (*session).command},
}

// accepts reports whether the command was given a workable number of arguments.
func (c commandSpec) accepts(args int) bool {
	if args < c.minArgs {
		return false
	}
	return c.maxArgs == unbounded || args <= c.maxArgs
}

// connCounters is the per-server accounting INFO's Clients and Stats sections
// report, plus the generator for connection ids.
//
// Connection ids exist because SCAN cursors are scoped to a connection
// (ADR-0017): a cursor issued to one client must not be usable by another, and
// "the connection" needs a name for that rule to be checkable. They are
// sequential and process-local; they are not secrets, and nothing about the
// scoping rule depends on them being unguessable — the cursor ids carry that.
type connCounters struct {
	nextID              atomic.Uint64
	connectionsReceived atomic.Uint64
	commandsProcessed   atomic.Uint64
	connected           atomic.Int64
}

// session is one connection's view of the server: what it can reach, and the
// identity its scan cursors are filed under.
//
// Handlers hang off this rather than off Server so that a command needing to
// know which connection asked — SCAN, today — can, without every other handler
// growing a parameter it ignores.
type session struct {
	srv *Server
	id  uint64
}

// newSession registers a connection and returns its handle.
func (s *Server) newSession() *session {
	s.conns.connectionsReceived.Add(1)
	s.conns.connected.Add(1)

	return &session{srv: s, id: s.conns.nextID.Add(1)}
}

// close releases everything the connection was holding. The scan cursors are
// the part that matters: an abandoned scan would otherwise hold its snapshot
// until the idle timeout, for a connection that can never come back to finish
// it.
func (s *session) close() {
	s.srv.conns.connected.Add(-1)
	s.srv.store.ReleaseScans(s.id)
}

// store is the keyspace this session runs against.
func (s *session) store() Store { return s.srv.store }

// dispatch resolves a command to a reply, reporting whether the connection
// should close afterwards.
//
// Every failure here is a reply and not a hang-up: an unknown command, a wrong
// argument count and a malformed argument all leave the session usable, because
// a client that mistypes one command has not lost the right to send the next
// one. Only a request the codec cannot resynchronize after closes a connection.
func (s *session) dispatch(cmd protocol.Command) (protocol.Reply, bool) {
	s.srv.conns.commandsProcessed.Add(1)

	spec, known := commands[cmd.Name]
	if !known {
		return protocol.Errorf("unknown command '%s'", cmd.Name), false
	}
	if !spec.accepts(len(cmd.Args)) {
		return protocol.Errorf("wrong number of arguments for '%s' command", strings.ToLower(cmd.Name)), false
	}

	return spec.handler(s, cmd), spec.closes
}

// ping answers PONG, or echoes its argument as a bulk string.
func (s *session) ping(cmd protocol.Command) protocol.Reply {
	if len(cmd.Args) == 1 {
		return protocol.BulkString(cmd.Args[0])
	}
	return protocol.SimpleString("PONG")
}

// quit acknowledges; the dispatcher closes the connection after the reply.
func (s *session) quit(protocol.Command) protocol.Reply {
	return protocol.SimpleString("OK")
}

// echo returns its argument unchanged.
//
// Binary safety is not incidental here: clients use ECHO as a connection health
// probe and some send arbitrary bytes through it, so the argument is handed
// straight to a bulk string rather than being treated as text.
func (s *session) echo(cmd protocol.Command) protocol.Reply {
	return protocol.BulkString(cmd.Args[0])
}

// command answers the introspection call clients make on connect.
//
// It is deliberately an empty array. redis-cli sends COMMAND DOCS on startup
// and tolerates an empty reply, which is all this needs to do. A fabricated
// command table would be worse than nothing: clients use it for client-side
// arity validation, so a table that disagreed with the dispatch table above
// would make them reject commands this server accepts — a failure that looks
// like the server's and is not.
func (s *session) command(protocol.Command) protocol.Reply {
	return protocol.Array{}
}

// hello reports the protocol in use, and refuses to speak any other.
//
// A client that asks for RESP3 gets -NOPROTO, which redis-cli, go-redis and
// redis-py all read as "this server speaks RESP2" and fall back on silently.
// Omitting HELLO entirely would answer -ERR unknown command instead, which some
// clients treat as a hard failure (ADR-0028). RESP3 itself is FEAT-0048, in P6.
func (s *session) hello(cmd protocol.Command) protocol.Reply {
	if len(cmd.Args) == 0 {
		return helloProperties()
	}

	version, err := strconv.Atoi(string(cmd.Args[0]))
	if err != nil || version != respVersion {
		return errNoProto()
	}

	// AUTH and SETNAME arrive with FEAT-0022 and FEAT-0024. Naming the option
	// that was refused is what tells a client which of the two it was.
	if len(cmd.Args) > 1 {
		return protocol.Errorf("syntax error in HELLO option '%s'", cmd.Args[1])
	}

	// Redis answers HELLO 2 with the same properties map as a bare HELLO, not a
	// status reply. A client that asks for 2 and parses a map would otherwise
	// break on a +OK it did not expect.
	return helloProperties()
}

// helloProperties is the map HELLO answers with when it is asked for no
// particular version. It is a protocol.Map, which the RESP2 codec flattens into
// an array — the handler neither knows nor cares that it did.
func helloProperties() protocol.Reply {
	return protocol.Map{
		{Key: protocol.BulkString("server"), Value: protocol.BulkString(serverName)},
		{Key: protocol.BulkString("version"), Value: protocol.BulkString(Version)},
		{Key: protocol.BulkString("proto"), Value: protocol.Integer(respVersion)},
		{Key: protocol.BulkString("mode"), Value: protocol.BulkString(serverMode)},
		{Key: protocol.BulkString("role"), Value: protocol.BulkString(serverRole)},
		{Key: protocol.BulkString("modules"), Value: protocol.Array{}},
	}
}

// set stores a value, with an optional expiry given as EX seconds or PX
// milliseconds.
func (s *session) set(cmd protocol.Command) protocol.Reply {
	ttl, bad := parseSetExpiry(cmd.Args[2:])
	if bad != nil {
		return bad
	}

	if err := s.store().Set(cmd.Args[0], cmd.Args[1], ttl); err != nil {
		return storeFailure(err)
	}
	return protocol.SimpleString("OK")
}

// setnx stores a value only if the key is free, answering 1 when it stored and
// 0 when a live key was in the way.
//
// It goes through the store's own conditional write rather than through a GET
// and a SET: the two would race, and the whole point of the command is that it
// does not. The memory limit applies exactly as it does to SET, which is what
// ISSUE-0010 was about and what the shared admission path now guarantees.
func (s *session) setnx(cmd protocol.Command) protocol.Reply {
	stored, err := s.store().SetNX(cmd.Args[0], cmd.Args[1], 0)
	if err != nil {
		return storeFailure(err)
	}
	return applied(stored)
}

// get answers with the stored value, or with the null bulk string.
func (s *session) get(cmd protocol.Command) protocol.Reply {
	value, exists := s.store().Get(cmd.Args[0])
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
func (s *session) del(cmd protocol.Command) protocol.Reply {
	var removed int64
	for _, key := range cmd.Args {
		if s.store().Delete(key) {
			removed++
		}
	}
	return protocol.Integer(removed)
}

// exists counts the keys that are there, counting duplicates separately.
//
// `EXISTS k k k` on one existing key answers 3, not 1. That reads like a bug
// and is not: it is Redis's documented behavior, clients rely on it, and the
// obvious "fix" of de-duplicating would break them.
func (s *session) exists(cmd protocol.Command) protocol.Reply {
	var found int64
	for _, key := range cmd.Args {
		if s.store().Exists(key) {
			found++
		}
	}
	return protocol.Integer(found)
}

// keys answers with every key matching the pattern.
//
// It walks the whole keyspace and holds the connection for the length of the
// walk. That is Redis's behavior too, and it is why SCAN exists; the command is
// kept because tooling expects it, not because it is a good idea on a large
// keyspace.
func (s *session) keys(cmd protocol.Command) protocol.Reply {
	matches := s.store().Keys(string(cmd.Args[0]))

	reply := make(protocol.Array, 0, len(matches))
	for _, key := range matches {
		reply = append(reply, protocol.BulkString(key))
	}
	return reply
}

// scan returns one page of the keyspace and the cursor for the next one.
//
// The reply is a two-element array: the cursor as a bulk string, then the keys.
// A cursor of "0" means the iteration is over — and nothing else does. A short
// page, or an empty one, says nothing about whether more remain, because COUNT
// bounds the work a page does rather than the keys it returns and MATCH filters
// what that work found.
func (s *session) scan(cmd protocol.Command) protocol.Reply {
	cursor, bad := parseScanCursor(cmd.Args[0])
	if bad != nil {
		return bad
	}

	opts, bad := parseScanOptions(cmd.Args[1:])
	if bad != nil {
		return bad
	}

	keys, next, err := s.store().Scan(s.id, cursor, opts.count, opts.match)
	if err != nil {
		return scanFailure(err)
	}

	page := make(protocol.Array, 0, len(keys))
	for _, key := range keys {
		page = append(page, protocol.BulkString(key))
	}

	return protocol.Array{protocol.BulkString(next), page}
}

// ttl reports the remaining life of a key in whole seconds.
func (s *session) ttl(cmd protocol.Command) protocol.Reply {
	remaining, exists := s.store().GetTTL(cmd.Args[0])
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
func (s *session) expire(cmd protocol.Command) protocol.Reply {
	seconds, err := strconv.ParseInt(string(cmd.Args[1]), 10, 64)
	if err != nil {
		return errNotAnInteger()
	}

	// An expiry already in the past deletes the key rather than making it
	// permanent, and still counts as applied. This is Redis's behavior, and the
	// alternative — passing a non-positive TTL down to the store, which reads
	// it as "no expiry" — would turn `EXPIRE k -1` into a key that never dies.
	if seconds <= 0 {
		return applied(s.store().Delete(cmd.Args[0]))
	}

	ttl, ok := expiryFor(seconds, time.Second)
	if !ok {
		return protocol.Errorf("invalid expire time in 'expire' command")
	}
	return applied(s.store().SetTTL(cmd.Args[0], ttl))
}

// dbsize answers with the number of keys in the keyspace.
//
// It reads the counters the shards maintain as they go, so it costs O(shards)
// and not O(keys) — which is the whole of ISSUE-0012, and would be reintroduced
// by anything here that walked the keyspace to be exact.
//
// The cost of not walking is that a key which has expired but has not yet been
// reclaimed is still counted, for as long as that takes. Redis has the same
// property for the same reason.
func (s *session) dbsize(protocol.Command) protocol.Reply {
	return protocol.Integer(int64(s.store().Stats().Keys)) //nolint:gosec // a key count cannot exceed int64
}

// stats is the native introspection command, and the one atlasctl and the admin
// API read.
//
// It builds a protocol.Map. RESP2 has no map frame, so the codec flattens it
// into an array of alternating keys and values — and the handler does not know
// that, which is the point: when FEAT-0048 adds the RESP3 encoder in P6 this
// same handler starts returning a real map with no edit here (ADR-0028).
func (s *session) stats(protocol.Command) protocol.Reply {
	stats := s.store().Stats()

	return protocol.Map{
		counter("keys", stats.Keys),
		counter("keys_with_ttl", stats.KeysWithTTL),
		counter("memory_used", stats.MemoryUsed),
		counter("memory_max", stats.MemoryMax),
		counter("gets", stats.Gets),
		counter("sets", stats.Sets),
		counter("deletes", stats.Deletes),
		counter("hits", stats.Hits),
		counter("misses", stats.Misses),
		counter("evictions", stats.Evictions),
		counter("expirations", stats.Expirations),
		counter("oom_rejected", stats.OOMRejected),
		counter("scan_cursors", stats.ScanCursors),
		counter("scan_snapshot_bytes", stats.ScanSnapshotBytes),
		counter("connections_received", s.srv.conns.connectionsReceived.Load()),
		counter("commands_processed", s.srv.conns.commandsProcessed.Load()),
		{Key: protocol.BulkString("connected_clients"), Value: protocol.Integer(s.srv.conns.connected.Load())},
		counter("uptime_seconds", uint64(uptime().Seconds())),
	}
}

// counter renders one unsigned statistic as a map pair.
func counter(name string, value uint64) protocol.KV {
	return protocol.KV{
		Key: protocol.BulkString(name),
		//nolint:gosec // counters are bounded by the keyspace and by memory, not by 2^63
		Value: protocol.Integer(int64(value)),
	}
}

// scanOptions is a parsed SCAN tail.
type scanOptions struct {
	count int
	match string
}

// parseScanCursor validates the cursor a client presented.
//
// It must parse as a decimal unsigned integer, because that is what every
// mainstream client can round-trip: go-redis reads it with ParseUint, redis-py
// with int(), `redis-cli --scan` with strtoull. Validating it here means a
// client that mangles a cursor hears "invalid cursor" rather than having its
// garbage looked up and reported as an unknown one.
func parseScanCursor(arg []byte) (string, protocol.Reply) {
	text := string(arg)

	value, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return "", protocol.Errorf("invalid cursor")
	}

	// Canonical form, so "007" finds the cursor issued as "7".
	return strconv.FormatUint(value, 10), nil
}

// parseScanOptions reads SCAN's MATCH and COUNT clauses.
func parseScanOptions(args [][]byte) (scanOptions, protocol.Reply) {
	opts := scanOptions{match: MatchAllPattern}

	for i := 0; i < len(args); i += 2 {
		if i+1 >= len(args) {
			return opts, errSyntax()
		}

		switch strings.ToUpper(string(args[i])) {
		case "MATCH":
			opts.match = string(args[i+1])

		case "COUNT":
			count, err := strconv.Atoi(string(args[i+1]))
			if err != nil {
				return opts, errNotAnInteger()
			}
			// Redis answers a non-positive COUNT with a syntax error rather
			// than clamping it, and a client that computed one wrongly is
			// better served by hearing about it.
			if count < 1 {
				return opts, errSyntax()
			}
			opts.count = count

		default:
			// TYPE lands here until it is implemented, which is the reply Redis
			// gives for any option it does not know.
			return opts, errSyntax()
		}
	}

	return opts, nil
}

// MatchAllPattern is the glob that selects the whole keyspace.
const MatchAllPattern = "*"

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

// uptime is how long this process has been running.
func uptime() time.Duration { return time.Since(processStarted) }

// listenPort is the port INFO reports, read back from the bound address so it
// is the port actually in use rather than the one that was configured — they
// differ whenever the configuration asked for port 0.
func listenPort(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "0"
	}
	return port
}

// applied renders the 1/0 answer EXPIRE and SETNX give.
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

// scanFailure maps a scan failure onto a reply.
//
// All three are errors Redis never sends, because Redis's cursor is stateless
// and ours is not (ADR-0017). They are reported plainly rather than disguised
// as an empty page: a scan that silently restarted would hand the caller
// duplicates it has no way to detect, and one that silently ended would hide
// keys it never reached.
func scanFailure(err error) protocol.Reply {
	switch {
	case errors.Is(err, ErrScanCursorUnknown):
		return protocol.Errorf("invalid cursor")
	case errors.Is(err, ErrTooManyScanCursors):
		return protocol.Errorf("too many open scan cursors; finish or abandon one before starting another")
	case errors.Is(err, ErrScanMemoryExhausted):
		return protocol.Errorf("scan snapshot memory limit reached")
	default:
		return protocol.Errorf("%v", err)
	}
}

// errNoProto is the documented fallback signal for a protocol version this
// server does not speak. Clients negotiate down on it rather than failing.
func errNoProto() protocol.Reply {
	return protocol.Error{Kind: "NOPROTO", Message: "unsupported protocol version"}
}

func errSyntax() protocol.Reply {
	return protocol.Errorf("syntax error")
}

func errNotAnInteger() protocol.Reply {
	return protocol.Errorf("value is not an integer or out of range")
}
