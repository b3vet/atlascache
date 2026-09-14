package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// errUnexpected stands for a store failure the server has no specific reply
// for, which must still reach the client rather than being swallowed.
var errUnexpected = errors.New("the engine fell over")

// fakeStore is a keyspace with no engine behind it: a map, an expiry per key,
// and switches for the two failures the server has a specific reply for.
//
// The real engine is exercised end to end by the E2E suite and by
// cmd/atlascache; what these tests need is a store that can be made to fail on
// demand, which a real one cannot without contorting its configuration.
// MaxValueSize mirrors the engine default; the protocol limits derive from it.
func (f *fakeStore) MaxValueSize() int { return 1 << 20 }

type fakeStore struct {
	mu     sync.Mutex
	values map[string][]byte
	expiry map[string]time.Time

	setErr  error
	scanErr error
	// gone reports a key as absent from GetTTL while leaving Get alone, which
	// is how a key that expires mid-command looks to the TTL handler.
	zeroTTL map[string]bool

	// stats is what Stats reports beside the live key count, so a test can
	// pick the numbers INFO and STATS are supposed to render.
	stats Stats

	// scanCalls records what the handler asked for, which is how the option
	// parsing is checked without a real cursor behind it, and released records
	// the connections whose cursors were dropped.
	scanCalls []scanCall
	released  []uint64
}

// scanCall is one recorded Scan request.
type scanCall struct {
	owner  uint64
	cursor string
	count  int
	match  string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		values:  map[string][]byte{},
		expiry:  map[string]time.Time{},
		zeroTTL: map[string]bool{},
	}
}

func (f *fakeStore) Get(key []byte) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	name := string(key)
	if f.expired(name) {
		return nil, false
	}
	value, ok := f.values[name]
	return value, ok
}

func (f *fakeStore) Set(key, value []byte, ttl time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.setErr != nil {
		return f.setErr
	}

	name := string(key)
	// Copying is what the real engine does, and a fake that aliased the
	// caller's buffer would hide exactly the defect ISSUE-0009 is about.
	f.values[name] = append([]byte(nil), value...)
	delete(f.expiry, name)
	if ttl > 0 {
		f.expiry[name] = time.Now().Add(ttl)
	}
	return nil
}

func (f *fakeStore) Delete(key []byte) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	name := string(key)
	_, existed := f.values[name]
	delete(f.values, name)
	delete(f.expiry, name)
	return existed
}

func (f *fakeStore) GetTTL(key []byte) (time.Duration, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	name := string(key)
	if _, ok := f.values[name]; !ok || f.expired(name) {
		return 0, false
	}
	if f.zeroTTL[name] {
		return 0, true
	}
	deadline, ok := f.expiry[name]
	if !ok {
		return -1, true
	}
	return time.Until(deadline), true
}

func (f *fakeStore) SetTTL(key []byte, ttl time.Duration) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	name := string(key)
	if _, ok := f.values[name]; !ok || f.expired(name) {
		return false
	}
	f.expiry[name] = time.Now().Add(ttl)
	return true
}

func (f *fakeStore) SetNX(key, value []byte, ttl time.Duration) (bool, error) {
	f.mu.Lock()
	name := string(key)
	if _, taken := f.values[name]; taken && !f.expired(name) {
		f.mu.Unlock()
		return false, nil
	}
	f.mu.Unlock()

	if err := f.Set(key, value, ttl); err != nil {
		return false, err
	}
	return true, nil
}

func (f *fakeStore) Exists(key []byte) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	name := string(key)
	_, ok := f.values[name]
	return ok && !f.expired(name)
}

func (f *fakeStore) Keys(pattern string) [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()

	// The fake matches literally, or on a trailing star. Glob semantics are the
	// engine's and are tested against real Redis there; what is being tested
	// here is the reply shape.
	var keys [][]byte
	for name := range f.values {
		if f.expired(name) {
			continue
		}
		if fakeMatches(pattern, name) {
			keys = append(keys, []byte(name))
		}
	}
	sort.Slice(keys, func(i, j int) bool { return string(keys[i]) < string(keys[j]) })
	return keys
}

func fakeMatches(pattern, name string) bool {
	switch {
	case pattern == "*":
		return true
	case strings.HasSuffix(pattern, "*"):
		return strings.HasPrefix(name, strings.TrimSuffix(pattern, "*"))
	default:
		return pattern == name
	}
}

// Scan hands out one key per page, which is the shape that matters here: the
// handler must render whatever cursor it is given and must not decide on its
// own when a scan has finished.
func (f *fakeStore) Scan(owner uint64, cursor string, count int, match string) ([][]byte, string, error) {
	f.mu.Lock()
	f.scanCalls = append(f.scanCalls, scanCall{owner: owner, cursor: cursor, count: count, match: match})
	err := f.scanErr
	f.mu.Unlock()

	if err != nil {
		return nil, "0", err
	}

	keys := f.Keys(match)
	position, convErr := strconv.Atoi(cursor)
	if convErr != nil {
		return nil, "0", convErr
	}
	if position >= len(keys) {
		return nil, "0", nil
	}

	next := "0"
	if position+1 < len(keys) {
		next = strconv.Itoa(position + 1)
	}
	return keys[position : position+1], next, nil
}

func (f *fakeStore) ReleaseScans(owner uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.released = append(f.released, owner)
}

// Stats reports whatever the test pinned, with the live key count filled in
// when it pinned none — so a test about INFO's field names can state a key
// count without writing a thousand keys, and a test about DBSIZE can write
// keys without restating the number.
func (f *fakeStore) Stats() Stats {
	f.mu.Lock()
	defer f.mu.Unlock()

	stats := f.stats
	if stats.Keys != 0 {
		return stats
	}

	for name := range f.values {
		if !f.expired(name) {
			stats.Keys++
		}
	}
	return stats
}

// expired reports whether the key's deadline has passed. The caller holds the
// lock.
func (f *fakeStore) expired(name string) bool {
	deadline, ok := f.expiry[name]
	return ok && time.Now().After(deadline)
}

// exchange sends one command and returns the whole reply, including any bulk
// payload, so a test asserts the bytes on the wire rather than a decoded shape.
func exchange(t *testing.T, srv *Server, args ...string) string {
	t.Helper()

	var request strings.Builder
	request.WriteString("*" + strconv.Itoa(len(args)) + "\r\n")
	for _, arg := range args {
		request.WriteString("$" + strconv.Itoa(len(arg)) + "\r\n" + arg + "\r\n")
	}

	conn, reader := dial(t, srv)
	send(t, conn, request.String())

	line := readReply(t, conn, reader)
	if strings.HasPrefix(line, "$") && line != "$-1\r\n" {
		line += readReply(t, conn, reader)
	}
	return line
}

func TestSetGetDel(t *testing.T) {
	srv, serveErr := newTestServer(t)
	defer shutdownServer(t, srv, serveErr)

	conn, r := dial(t, srv)

	send(t, conn, "*3\r\n$3\r\nSET\r\n$2\r\nk1\r\n$5\r\nhello\r\n")
	assert.Equal(t, "+OK\r\n", readReply(t, conn, r))

	send(t, conn, "*2\r\n$3\r\nGET\r\n$2\r\nk1\r\n")
	assert.Equal(t, "$5\r\n", readReply(t, conn, r))
	assert.Equal(t, "hello\r\n", readReply(t, conn, r))

	// A miss is the null bulk string, not an error.
	send(t, conn, "*2\r\n$3\r\nGET\r\n$4\r\nnope\r\n")
	assert.Equal(t, "$-1\r\n", readReply(t, conn, r))

	// An empty value round-trips, which only the bulk form can express.
	send(t, conn, "*3\r\n$3\r\nSET\r\n$2\r\nk2\r\n$0\r\n\r\n")
	assert.Equal(t, "+OK\r\n", readReply(t, conn, r))
	send(t, conn, "*2\r\n$3\r\nGET\r\n$2\r\nk2\r\n")
	assert.Equal(t, "$0\r\n", readReply(t, conn, r))
	assert.Equal(t, "\r\n", readReply(t, conn, r), "an empty bulk string still carries its terminator")

	send(t, conn, "*3\r\n$3\r\nDEL\r\n$2\r\nk1\r\n$4\r\nnope\r\n")
	assert.Equal(t, ":1\r\n", readReply(t, conn, r))

	send(t, conn, "*2\r\n$3\r\nGET\r\n$2\r\nk1\r\n")
	assert.Equal(t, "$-1\r\n", readReply(t, conn, r))
}

func TestSetOverwritesValue(t *testing.T) {
	srv, serveErr := newTestServer(t)
	defer shutdownServer(t, srv, serveErr)

	assert.Equal(t, "+OK\r\n", exchange(t, srv, "SET", "k", "first"))
	assert.Equal(t, "+OK\r\n", exchange(t, srv, "SET", "k", "second"))
	assert.Equal(t, "$6\r\nsecond\r\n", exchange(t, srv, "GET", "k"))
}

func TestSetWithExpiry(t *testing.T) {
	srv, serveErr := newTestServer(t)
	defer shutdownServer(t, srv, serveErr)

	t.Run("EX sets seconds", func(t *testing.T) {
		assert.Equal(t, "+OK\r\n", exchange(t, srv, "SET", "ex", "v", "EX", "10"))
		// Rounded to the nearest second, as Redis does, so 9.999s reads as 10.
		assert.Equal(t, ":10\r\n", exchange(t, srv, "TTL", "ex"))
	})

	t.Run("PX sets milliseconds", func(t *testing.T) {
		assert.Equal(t, "+OK\r\n", exchange(t, srv, "SET", "px", "v", "PX", "4000"))
		assert.Equal(t, ":4\r\n", exchange(t, srv, "TTL", "px"))
	})

	t.Run("the unit is case insensitive", func(t *testing.T) {
		assert.Equal(t, "+OK\r\n", exchange(t, srv, "SET", "lower", "v", "ex", "9"))
		assert.Equal(t, ":9\r\n", exchange(t, srv, "TTL", "lower"))
	})

	t.Run("a rewrite without an expiry clears the old one", func(t *testing.T) {
		assert.Equal(t, "+OK\r\n", exchange(t, srv, "SET", "ex", "v"))
		assert.Equal(t, ":-1\r\n", exchange(t, srv, "TTL", "ex"))
	})
}

func TestSetExpiryErrors(t *testing.T) {
	srv, serveErr := newTestServer(t)
	defer shutdownServer(t, srv, serveErr)

	cases := map[string]struct {
		args  []string
		reply string
	}{
		"EX and PX together": {
			args:  []string{"SET", "k", "v", "EX", "1", "PX", "1000"},
			reply: "-ERR syntax error\r\n",
		},
		"a unit with no amount": {
			args:  []string{"SET", "k", "v", "EX"},
			reply: "-ERR syntax error\r\n",
		},
		"an unknown option": {
			args:  []string{"SET", "k", "v", "NX", "1"},
			reply: "-ERR syntax error\r\n",
		},
		"a non-integer amount": {
			args:  []string{"SET", "k", "v", "EX", "soon"},
			reply: "-ERR value is not an integer or out of range\r\n",
		},
		"zero seconds": {
			args:  []string{"SET", "k", "v", "EX", "0"},
			reply: "-ERR invalid expire time in 'set' command\r\n",
		},
		"negative seconds": {
			args:  []string{"SET", "k", "v", "EX", "-1"},
			reply: "-ERR invalid expire time in 'set' command\r\n",
		},
		"an expiry past the end of time": {
			args:  []string{"SET", "k", "v", "EX", "9223372036854775807"},
			reply: "-ERR invalid expire time in 'set' command\r\n",
		},
		"an expiry whose deadline overflows": {
			args:  []string{"SET", "k", "v", "EX", "9223372036"},
			reply: "-ERR invalid expire time in 'set' command\r\n",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.reply, exchange(t, srv, tc.args...))
		})
	}

	// Nothing was stored by any of the rejected writes.
	assert.Equal(t, "$-1\r\n", exchange(t, srv, "GET", "k"))
}

func TestTTLReturns(t *testing.T) {
	srv, serveErr := newTestServer(t)
	defer shutdownServer(t, srv, serveErr)

	// -2 is no such key; -1 is a key that never expires. Clients tell the two
	// apart, so the server must too.
	assert.Equal(t, ":-2\r\n", exchange(t, srv, "TTL", "absent"))

	require.Equal(t, "+OK\r\n", exchange(t, srv, "SET", "forever", "v"))
	assert.Equal(t, ":-1\r\n", exchange(t, srv, "TTL", "forever"))

	require.Equal(t, "+OK\r\n", exchange(t, srv, "SET", "doomed", "v", "PX", "30"))
	time.Sleep(80 * time.Millisecond)
	assert.Equal(t, ":-2\r\n", exchange(t, srv, "TTL", "doomed"))
	assert.Equal(t, "$-1\r\n", exchange(t, srv, "GET", "doomed"))
}

// TestTTLOnAKeyExpiringMidCommand covers the narrow window where the key is
// still present when it is looked up and has no life left by the time the reply
// is built. It answers -2, because that is what the key looks like to anyone.
func TestTTLOnAKeyExpiringMidCommand(t *testing.T) {
	store := newFakeStore()
	srv, serveErr := newTestServerWith(t, store)
	defer shutdownServer(t, srv, serveErr)

	require.Equal(t, "+OK\r\n", exchange(t, srv, "SET", "racing", "v", "EX", "10"))
	store.mu.Lock()
	store.zeroTTL["racing"] = true
	store.mu.Unlock()

	assert.Equal(t, ":-2\r\n", exchange(t, srv, "TTL", "racing"))
}

func TestExpire(t *testing.T) {
	srv, serveErr := newTestServer(t)
	defer shutdownServer(t, srv, serveErr)

	require.Equal(t, "+OK\r\n", exchange(t, srv, "SET", "k", "v"))
	assert.Equal(t, ":-1\r\n", exchange(t, srv, "TTL", "k"))

	assert.Equal(t, ":1\r\n", exchange(t, srv, "EXPIRE", "k", "30"))
	assert.Equal(t, ":30\r\n", exchange(t, srv, "TTL", "k"))

	// An absent key is 0, not an error, and nothing is created by asking.
	assert.Equal(t, ":0\r\n", exchange(t, srv, "EXPIRE", "absent", "30"))
	assert.Equal(t, ":-2\r\n", exchange(t, srv, "TTL", "absent"))

	t.Run("an expiry in the past deletes the key", func(t *testing.T) {
		require.Equal(t, "+OK\r\n", exchange(t, srv, "SET", "past", "v"))
		assert.Equal(t, ":1\r\n", exchange(t, srv, "EXPIRE", "past", "-1"))
		assert.Equal(t, "$-1\r\n", exchange(t, srv, "GET", "past"))

		// And on a key that was not there, it is still 0.
		assert.Equal(t, ":0\r\n", exchange(t, srv, "EXPIRE", "past", "0"))
	})

	t.Run("a non-integer is rejected", func(t *testing.T) {
		assert.Equal(t, "-ERR value is not an integer or out of range\r\n", exchange(t, srv, "EXPIRE", "k", "soon"))
	})

	t.Run("an expiry past the end of time is rejected", func(t *testing.T) {
		assert.Equal(t, "-ERR invalid expire time in 'expire' command\r\n",
			exchange(t, srv, "EXPIRE", "k", "9223372036854775807"))
		assert.Equal(t, ":30\r\n", exchange(t, srv, "TTL", "k"), "and the existing expiry is untouched")
	})
}

func TestArgumentErrorsKeepTheConnectionOpen(t *testing.T) {
	srv, serveErr := newTestServer(t)
	defer shutdownServer(t, srv, serveErr)

	cases := []struct {
		payload string
		reply   string
	}{
		{"*1\r\n$3\r\nSET\r\n", "-ERR wrong number of arguments for 'set' command\r\n"},
		{"*2\r\n$3\r\nSET\r\n$1\r\nk\r\n", "-ERR wrong number of arguments for 'set' command\r\n"},
		{"*1\r\n$3\r\nGET\r\n", "-ERR wrong number of arguments for 'get' command\r\n"},
		{"*3\r\n$3\r\nGET\r\n$1\r\na\r\n$1\r\nb\r\n", "-ERR wrong number of arguments for 'get' command\r\n"},
		{"*1\r\n$3\r\nDEL\r\n", "-ERR wrong number of arguments for 'del' command\r\n"},
		{"*1\r\n$3\r\nTTL\r\n", "-ERR wrong number of arguments for 'ttl' command\r\n"},
		{"*2\r\n$6\r\nEXPIRE\r\n$1\r\nk\r\n", "-ERR wrong number of arguments for 'expire' command\r\n"},
		{"*4\r\n$6\r\nEXPIRE\r\n$1\r\nk\r\n$1\r\n1\r\n$2\r\nNX\r\n", "-ERR wrong number of arguments for 'expire' command\r\n"},
	}

	// One connection for the lot: every error must leave it usable, and the
	// PING after each is what proves it.
	conn, r := dial(t, srv)
	for _, tc := range cases {
		send(t, conn, tc.payload)
		assert.Equal(t, tc.reply, readReply(t, conn, r))

		send(t, conn, "*1\r\n$4\r\nPING\r\n")
		require.Equal(t, "+PONG\r\n", readReply(t, conn, r))
	}
}

func TestCommandNamesAreCaseInsensitive(t *testing.T) {
	srv, serveErr := newTestServer(t)
	defer shutdownServer(t, srv, serveErr)

	assert.Equal(t, "+OK\r\n", exchange(t, srv, "set", "k", "v"))
	assert.Equal(t, "$1\r\nv\r\n", exchange(t, srv, "Get", "k"))
	assert.Equal(t, ":1\r\n", exchange(t, srv, "dEl", "k"))
}

func TestStoreFailuresBecomeReplies(t *testing.T) {
	cases := map[string]struct {
		err   error
		reply string
	}{
		"out of memory": {
			err:   ErrOutOfMemory,
			reply: "-OOM command not allowed when used memory > 'maxmemory'.\r\n",
		},
		"value too large": {
			err:   ErrValueTooLarge,
			reply: "-ERR value exceeds max_value_size\r\n",
		},
		"invalid key": {
			err:   ErrInvalidKey,
			reply: "-ERR invalid key\r\n",
		},
		"anything else": {
			err:   errUnexpected,
			reply: "-ERR the engine fell over\r\n",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store := newFakeStore()
			store.setErr = tc.err
			srv, serveErr := newTestServerWith(t, store)
			defer shutdownServer(t, srv, serveErr)

			assert.Equal(t, tc.reply, exchange(t, srv, "SET", "k", "v"))

			// The connection survives a rejected write.
			assert.Equal(t, "$-1\r\n", exchange(t, srv, "GET", "k"))
		})
	}
}

func TestNewRequiresAKeyspace(t *testing.T) {
	srv, err := New(context.Background(), "127.0.0.1:0", zerolog.Nop(), nil)

	require.Error(t, err)
	assert.Nil(t, srv)
	assert.Contains(t, err.Error(), "keyspace")
}

func TestCommandSpecArity(t *testing.T) {
	exact := commandSpec{minArgs: 1, maxArgs: 1}
	assert.False(t, exact.accepts(0))
	assert.True(t, exact.accepts(1))
	assert.False(t, exact.accepts(2))

	open := commandSpec{minArgs: 1, maxArgs: unbounded}
	assert.False(t, open.accepts(0))
	assert.True(t, open.accepts(1))
	assert.True(t, open.accepts(1000))
}

// TestHello covers the negotiation ADR-0028 settles on: this server speaks
// RESP2, says so, and refuses every other version with -NOPROTO — the reply
// redis-cli, go-redis and redis-py all fall back to RESP2 on. Returning "-ERR
// unknown command" instead, which omitting HELLO would do, is a hard failure to
// some of them.
func TestHello(t *testing.T) {
	srv, serveErr := newTestServer(t)
	defer shutdownServer(t, srv, serveErr)

	t.Run("with no argument it reports the protocol and the server", func(t *testing.T) {
		conn, r := dial(t, srv)
		send(t, conn, "*1\r\n$5\r\nHELLO\r\n")

		// The handler returns a protocol.Map. RESP2 has no map frame, so it
		// arrives flattened into an array of 2n — which is what this asserts,
		// and what FEAT-0048's RESP3 encoder will render as %6 instead without
		// the handler changing.
		want := "*12\r\n" +
			"$6\r\nserver\r\n$10\r\natlascache\r\n" +
			fmt.Sprintf("$7\r\nversion\r\n$%d\r\n%s\r\n", len(Version), Version) +
			"$5\r\nproto\r\n:2\r\n" +
			"$4\r\nmode\r\n$10\r\nstandalone\r\n" +
			"$4\r\nrole\r\n$6\r\nmaster\r\n" +
			"$7\r\nmodules\r\n*0\r\n"
		assert.Equal(t, want, readExactly(t, conn, r, len(want)))

		// Nothing was left on the wire: the array header counted its elements
		// correctly, so the next reply starts where the client expects.
		send(t, conn, "*1\r\n$4\r\nPING\r\n")
		assert.Equal(t, "+PONG\r\n", readReply(t, conn, r))
	})

	t.Run("HELLO 2 answers with the properties map, as Redis does", func(t *testing.T) {
		// Not +OK: Redis returns the same map for HELLO 2 as for a bare HELLO,
		// and a client asking for 2 may well be parsing a map.
		// exchange returns the first line only, so assert on the frame header
		// and on it being identical to what a bare HELLO answers.
		reply := exchange(t, srv, "HELLO", "2")
		assert.Equal(t, exchange(t, srv, "HELLO"), reply)
		assert.Equal(t, "*12\r\n", reply, "six key/value pairs flattened into RESP2")
	})

	noProto := "-NOPROTO unsupported protocol version\r\n"
	for _, version := range []string{"3", "4", "0", "-1", "2.0", "three", ""} {
		t.Run("HELLO "+version+" is refused with NOPROTO", func(t *testing.T) {
			assert.Equal(t, noProto, exchange(t, srv, "HELLO", version))
		})
	}

	t.Run("HELLO 3 AUTH is refused with NOPROTO, not an arity error", func(t *testing.T) {
		// What a client probing for RESP3 with credentials sends. An arity
		// error is not something it knows how to fall back from.
		assert.Equal(t, noProto, exchange(t, srv, "HELLO", "3", "AUTH", "default", "secret"))
	})

	t.Run("an option on HELLO 2 names what was refused", func(t *testing.T) {
		assert.Equal(t, "-ERR syntax error in HELLO option 'AUTH'\r\n",
			exchange(t, srv, "HELLO", "2", "AUTH", "default", "secret"))
		assert.Equal(t, "-ERR syntax error in HELLO option 'SETNAME'\r\n",
			exchange(t, srv, "HELLO", "2", "SETNAME", "client"))
	})

	t.Run("the connection survives the refusal, which is the whole point", func(t *testing.T) {
		conn, r := dial(t, srv)

		send(t, conn, "*2\r\n$5\r\nHELLO\r\n$1\r\n3\r\n")
		assert.Equal(t, noProto, readReply(t, conn, r))

		// The client now carries on in RESP2, exactly as it would against a
		// Redis old enough not to know RESP3.
		send(t, conn, "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n")
		assert.Equal(t, "+OK\r\n", readReply(t, conn, r))
		send(t, conn, "*2\r\n$3\r\nGET\r\n$1\r\nk\r\n")
		assert.Equal(t, "$1\r\n", readReply(t, conn, r))
		assert.Equal(t, "v\r\n", readReply(t, conn, r))
	})
}

// readExactly reads n bytes of a reply, for the replies that span more frames
// than readReply's single line.
func readExactly(t *testing.T, conn net.Conn, r *bufio.Reader, n int) string {
	t.Helper()

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	buf := make([]byte, n)
	_, err := io.ReadFull(r, buf)
	require.NoError(t, err)

	return string(buf)
}

// readFullReply decodes one whole reply, nested arrays included, into a form a
// test can compare. It is deliberately not the codec: asserting against a
// decoder that shares the encoder's assumptions proves only that they agree.
func readFullReply(t *testing.T, conn net.Conn, r *bufio.Reader) string {
	t.Helper()

	line := strings.TrimRight(readReply(t, conn, r), "\r\n")
	require.NotEmpty(t, line)

	switch line[0] {
	case '+', '-', ':':
		return string(line[0]) + line[1:]

	case '$':
		size, err := strconv.Atoi(line[1:])
		require.NoError(t, err)
		if size < 0 {
			return "NIL"
		}
		buf := make([]byte, size+2)
		require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
		_, err = io.ReadFull(r, buf)
		require.NoError(t, err)
		return "$" + string(buf[:size])

	case '*':
		count, err := strconv.Atoi(line[1:])
		require.NoError(t, err)
		items := make([]string, 0, count)
		for i := 0; i < count; i++ {
			items = append(items, readFullReply(t, conn, r))
		}
		return "[" + strings.Join(items, " ") + "]"
	}

	t.Fatalf("unexpected reply frame %q", line)
	return ""
}

// session opens a connection and returns a function that sends one command on
// it and decodes the whole reply. The connection is reused across calls, which
// is what a SCAN needs: its cursors are scoped to one connection.
func conversation(t *testing.T, srv *Server) func(args ...string) string {
	t.Helper()

	conn, reader := dial(t, srv)

	return func(args ...string) string {
		t.Helper()

		var request strings.Builder
		request.WriteString("*" + strconv.Itoa(len(args)) + "\r\n")
		for _, arg := range args {
			request.WriteString("$" + strconv.Itoa(len(arg)) + "\r\n" + arg + "\r\n")
		}
		send(t, conn, request.String())

		return readFullReply(t, conn, reader)
	}
}

func TestSetNX(t *testing.T) {
	store := newFakeStore()
	srv, serveErr := newTestServerWith(t, store)
	defer shutdownServer(t, srv, serveErr)

	assert.Equal(t, ":1", ask(t, srv, "SETNX", "k", "first"))
	assert.Equal(t, ":0", ask(t, srv, "SETNX", "k", "second"), "a live key blocks the write")
	assert.Equal(t, "$first", ask(t, srv, "GET", "k"), "and the value it holds is untouched")

	t.Run("a store failure is reported, not swallowed", func(t *testing.T) {
		store.setErr = ErrOutOfMemory
		defer func() { store.setErr = nil }()

		assert.Equal(t, "-OOM command not allowed when used memory > 'maxmemory'.",
			ask(t, srv, "SETNX", "other", "v"))
	})

	t.Run("arity", func(t *testing.T) {
		assert.Equal(t, "-ERR wrong number of arguments for 'setnx' command", ask(t, srv, "SETNX", "k"))
		assert.Equal(t, "-ERR wrong number of arguments for 'setnx' command", ask(t, srv, "SETNX", "k", "v", "x"))
	})
}

// TestExistsCountsDuplicates is the reply semantics clients depend on. It reads
// like a bug and is not: Redis counts each argument, so naming one key three
// times answers 3, and de-duplicating would break every client that relies on
// the documented behavior.
func TestExistsCountsDuplicates(t *testing.T) {
	store := newFakeStore()
	require.NoError(t, store.Set([]byte("a"), []byte("1"), 0))
	require.NoError(t, store.Set([]byte("b"), []byte("2"), 0))

	srv, serveErr := newTestServerWith(t, store)
	defer shutdownServer(t, srv, serveErr)

	assert.Equal(t, ":1", ask(t, srv, "EXISTS", "a"))
	assert.Equal(t, ":0", ask(t, srv, "EXISTS", "missing"))
	assert.Equal(t, ":2", ask(t, srv, "EXISTS", "a", "b"))
	assert.Equal(t, ":3", ask(t, srv, "EXISTS", "a", "a", "a"))
	assert.Equal(t, ":3", ask(t, srv, "EXISTS", "a", "missing", "a", "missing", "a"))
	assert.Equal(t, ":0", ask(t, srv, "EXISTS", "missing", "missing"))

	assert.Equal(t, "-ERR wrong number of arguments for 'exists' command", ask(t, srv, "EXISTS"))
}

func TestKeys(t *testing.T) {
	store := newFakeStore()
	for _, key := range []string{"user:1", "user:2", "session:1"} {
		require.NoError(t, store.Set([]byte(key), []byte("v"), 0))
	}

	srv, serveErr := newTestServerWith(t, store)
	defer shutdownServer(t, srv, serveErr)

	assert.Equal(t, "[$session:1 $user:1 $user:2]", ask(t, srv, "KEYS", "*"))
	assert.Equal(t, "[$user:1 $user:2]", ask(t, srv, "KEYS", "user:*"))
	assert.Equal(t, "[$session:1]", ask(t, srv, "KEYS", "session:1"))
	assert.Equal(t, "[]", ask(t, srv, "KEYS", "nothing:*"))

	assert.Equal(t, "-ERR wrong number of arguments for 'keys' command", ask(t, srv, "KEYS"))
	assert.Equal(t, "-ERR wrong number of arguments for 'keys' command", ask(t, srv, "KEYS", "a", "b"))
}

// ask sends one command on a fresh connection and returns the whole decoded
// reply. exchange, which the P1 tests use, reads raw wire bytes; these commands
// answer with arrays, and asserting an array as raw bytes reads terribly.
func ask(t *testing.T, srv *Server, args ...string) string {
	t.Helper()
	return conversation(t, srv)(args...)
}

func TestScan(t *testing.T) {
	store := newFakeStore()
	for _, key := range []string{"a", "b", "c"} {
		require.NoError(t, store.Set([]byte(key), []byte("v"), 0))
	}

	srv, serveErr := newTestServerWith(t, store)
	defer shutdownServer(t, srv, serveErr)

	t.Run("a page is the cursor then the keys", func(t *testing.T) {
		send := conversation(t, srv)

		assert.Equal(t, "[$1 [$a]]", send("SCAN", "0"))
		assert.Equal(t, "[$2 [$b]]", send("SCAN", "1"))
		assert.Equal(t, "[$0 [$c]]", send("SCAN", "2"), "a cursor of 0 is the only thing that means finished")
	})

	t.Run("options are parsed and passed through", func(t *testing.T) {
		store.mu.Lock()
		store.scanCalls = nil
		store.mu.Unlock()

		send := conversation(t, srv)
		send("SCAN", "0", "MATCH", "a*", "COUNT", "42")
		send("SCAN", "0", "count", "7", "match", "b*")
		send("SCAN", "0")

		store.mu.Lock()
		calls := append([]scanCall(nil), store.scanCalls...)
		store.mu.Unlock()

		require.Len(t, calls, 3)
		assert.Equal(t, scanCall{owner: calls[0].owner, cursor: "0", count: 42, match: "a*"}, calls[0])
		assert.Equal(t, scanCall{owner: calls[1].owner, cursor: "0", count: 7, match: "b*"}, calls[1],
			"option names are case-insensitive, as they are in Redis")
		assert.Equal(t, scanCall{owner: calls[2].owner, cursor: "0", count: 0, match: MatchAllPattern}, calls[2],
			"no COUNT leaves the page size to the store")
	})

	t.Run("a cursor is decimal, and anything else is refused before it is looked up", func(t *testing.T) {
		for _, cursor := range []string{"notanumber", "-1", "0xff", "9f3c1ab2d4e5f607", "", " 1"} {
			assert.Equalf(t, "-ERR invalid cursor", ask(t, srv, "SCAN", cursor), "cursor %q", cursor)
		}

		// A canonical cursor reaches the store as it was issued, so "007" finds
		// the cursor filed under "7".
		store.mu.Lock()
		store.scanCalls = nil
		store.mu.Unlock()

		ask(t, srv, "SCAN", "007")

		store.mu.Lock()
		calls := append([]scanCall(nil), store.scanCalls...)
		store.mu.Unlock()
		require.Len(t, calls, 1)
		assert.Equal(t, "7", calls[0].cursor)
	})

	t.Run("malformed options", func(t *testing.T) {
		assert.Equal(t, "-ERR syntax error", ask(t, srv, "SCAN", "0", "COUNT", "0"))
		assert.Equal(t, "-ERR syntax error", ask(t, srv, "SCAN", "0", "COUNT", "-5"))
		assert.Equal(t, "-ERR value is not an integer or out of range", ask(t, srv, "SCAN", "0", "COUNT", "abc"))
		assert.Equal(t, "-ERR syntax error", ask(t, srv, "SCAN", "0", "NOSUCH", "x"))
		assert.Equal(t, "-ERR syntax error", ask(t, srv, "SCAN", "0", "MATCH"))
		assert.Equal(t, "-ERR wrong number of arguments for 'scan' command", ask(t, srv, "SCAN"))
	})

	t.Run("store failures each have their own reply", func(t *testing.T) {
		cases := map[error]string{
			ErrScanCursorUnknown:   "-ERR invalid cursor",
			ErrTooManyScanCursors:  "-ERR too many open scan cursors; finish or abandon one before starting another",
			ErrScanMemoryExhausted: "-ERR scan snapshot memory limit reached",
			errUnexpected:          "-ERR " + errUnexpected.Error(),
		}
		for failure, want := range cases {
			store.mu.Lock()
			store.scanErr = failure
			store.mu.Unlock()

			assert.Equalf(t, want, ask(t, srv, "SCAN", "0"), "failure %v", failure)
		}

		store.mu.Lock()
		store.scanErr = nil
		store.mu.Unlock()
	})
}

// TestScanCursorsAreReleasedWithTheConnection is the other half of the cursor
// bound. An abandoned scan holds a snapshot of a whole shard, and a connection
// that has gone away is never coming back to finish it.
func TestScanCursorsAreReleasedWithTheConnection(t *testing.T) {
	store := newFakeStore()
	srv, serveErr := newTestServerWith(t, store)
	defer shutdownServer(t, srv, serveErr)

	conn, reader := dial(t, srv)
	send(t, conn, "*1\r\n$4\r\nPING\r\n")
	assert.Equal(t, "+PONG\r\n", readReply(t, conn, reader))
	require.NoError(t, conn.Close())

	assert.Eventually(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return len(store.released) > 0
	}, 2*time.Second, 10*time.Millisecond, "the connection's cursors must be dropped when it goes")
}

// TestSessionsHaveDistinctIdentities is what makes a cursor scoped to one
// connection a checkable rule rather than a claim.
func TestSessionsHaveDistinctIdentities(t *testing.T) {
	store := newFakeStore()
	require.NoError(t, store.Set([]byte("a"), []byte("v"), 0))

	srv, serveErr := newTestServerWith(t, store)
	defer shutdownServer(t, srv, serveErr)

	first, second := conversation(t, srv), conversation(t, srv)
	first("SCAN", "0")
	second("SCAN", "0")

	store.mu.Lock()
	calls := append([]scanCall(nil), store.scanCalls...)
	store.mu.Unlock()

	require.Len(t, calls, 2)
	assert.NotEqual(t, calls[0].owner, calls[1].owner, "two connections must not share a cursor namespace")
}

func TestEchoDBSizeAndCommand(t *testing.T) {
	store := newFakeStore()
	require.NoError(t, store.Set([]byte("a"), []byte("1"), 0))
	require.NoError(t, store.Set([]byte("b"), []byte("2"), 0))

	srv, serveErr := newTestServerWith(t, store)
	defer shutdownServer(t, srv, serveErr)

	assert.Equal(t, ":2", ask(t, srv, "DBSIZE"))
	assert.Equal(t, "-ERR wrong number of arguments for 'dbsize' command", ask(t, srv, "DBSIZE", "x"))

	assert.Equal(t, "$hello", ask(t, srv, "ECHO", "hello"))
	assert.Equal(t, "$", ask(t, srv, "ECHO", ""))
	assert.Equal(t, "$a\r\nb\x00c", ask(t, srv, "ECHO", "a\r\nb\x00c"), "ECHO is binary-safe")
	assert.Equal(t, "-ERR wrong number of arguments for 'echo' command", ask(t, srv, "ECHO"))
	assert.Equal(t, "-ERR wrong number of arguments for 'echo' command", ask(t, srv, "ECHO", "a", "b"))

	// The stub. A fabricated command table would make clients reject commands
	// this server accepts, which is worse than answering nothing.
	assert.Equal(t, "[]", ask(t, srv, "COMMAND"))
	assert.Equal(t, "[]", ask(t, srv, "COMMAND", "DOCS"))
	assert.Equal(t, "[]", ask(t, srv, "COMMAND", "INFO", "GET"))
}

func TestStatsIsAFlattenedMap(t *testing.T) {
	store := newFakeStore()
	store.stats = Stats{
		KeysWithTTL: 3, MemoryUsed: 1024, MemoryMax: 4096,
		Gets: 10, Sets: 5, Deletes: 2, Hits: 7, Misses: 3,
		Evictions: 1, Expirations: 4, OOMRejected: 6,
		ScanCursors: 2, ScanSnapshotBytes: 512,
	}
	require.NoError(t, store.Set([]byte("a"), []byte("1"), 0))

	srv, serveErr := newTestServerWith(t, store)
	defer shutdownServer(t, srv, serveErr)

	fields := replyFields(t, ask(t, srv, "STATS"))

	// A logical Map rendered by the RESP2 codec is an array of 2n elements,
	// key then value. The handler builds the map and does not know that; in
	// P6 the same handler returns a real map (ADR-0028).
	assert.Equal(t, "1", fields["keys"])
	assert.Equal(t, "3", fields["keys_with_ttl"])
	assert.Equal(t, "1024", fields["memory_used"])
	assert.Equal(t, "4096", fields["memory_max"])
	assert.Equal(t, "10", fields["gets"])
	assert.Equal(t, "5", fields["sets"])
	assert.Equal(t, "2", fields["deletes"])
	assert.Equal(t, "7", fields["hits"])
	assert.Equal(t, "3", fields["misses"])
	assert.Equal(t, "1", fields["evictions"])
	assert.Equal(t, "4", fields["expirations"])
	assert.Equal(t, "6", fields["oom_rejected"])
	assert.Equal(t, "2", fields["scan_cursors"])
	assert.Equal(t, "512", fields["scan_snapshot_bytes"])
	assert.Contains(t, fields, "connected_clients")
	assert.Contains(t, fields, "commands_processed")
	assert.Contains(t, fields, "connections_received")
	assert.Contains(t, fields, "uptime_seconds")

	assert.Equal(t, "-ERR wrong number of arguments for 'stats' command", ask(t, srv, "STATS", "x"))
}

// replyFields reads a flattened map reply — "[$k $v $k $v]" — back into pairs.
func replyFields(t *testing.T, reply string) map[string]string {
	t.Helper()

	require.True(t, strings.HasPrefix(reply, "[") && strings.HasSuffix(reply, "]"), "not an array: %s", reply)
	parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(reply, "["), "]"), " ")
	require.Zero(t, len(parts)%2, "a flattened map has an even number of elements")

	fields := make(map[string]string, len(parts)/2)
	for i := 0; i < len(parts); i += 2 {
		fields[strings.TrimPrefix(parts[i], "$")] = strings.TrimPrefix(parts[i+1], ":")
	}
	return fields
}
