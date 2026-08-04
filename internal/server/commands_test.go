package server

import (
	"context"
	"errors"
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
type fakeStore struct {
	mu     sync.Mutex
	values map[string][]byte
	expiry map[string]time.Time

	setErr error
	// gone reports a key as absent from GetTTL while leaving Get alone, which
	// is how a key that expires mid-command looks to the TTL handler.
	zeroTTL map[string]bool
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
