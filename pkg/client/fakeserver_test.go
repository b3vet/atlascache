package client

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServer is an AtlasCache server good enough to test a client against: the
// P2 command surface, in memory, plus the hooks a test needs to make it
// misbehave on purpose.
//
// It exists because the SDK is its own module and cannot import the real
// server (ADR-0015), and because most of what needs testing here is not the
// server's behavior but the client's response to a server that stops, lies, or
// answers twice. The tests that do need the real thing run against the real
// binary — see realserver_test.go.
type fakeServer struct {
	t *testing.T

	mu      sync.Mutex
	ln      net.Listener
	addr    string
	data    map[string]fakeEntry
	token   string
	conns   map[net.Conn]struct{}
	opened  int
	stopped bool

	// helloReply, when set, replaces the answer to HELLO. The default is the
	// -NOPROTO a v0.1.0 server sends (ADR-0028).
	helloReply func(args [][]byte) string

	// hook takes over a command entirely: it may write whatever bytes it likes
	// or close the connection under the client. It is how the
	// desynchronization, malformed-reply and dropped-connection tests are
	// built.
	hook func(w *bufio.Writer, nc net.Conn, name string, args [][]byte) bool

	tlsConfig *tls.Config
	wg        sync.WaitGroup
}

type fakeEntry struct {
	value   []byte
	expires time.Time
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	s := &fakeServer{
		t:     t,
		data:  make(map[string]fakeEntry),
		conns: make(map[net.Conn]struct{}),
	}
	s.start(t, "127.0.0.1:0")
	t.Cleanup(s.stop)
	return s
}

// newFakeServerAt starts a server on a port the caller chose, which is what the
// restart tests need: a client configured with an address has to find the
// server there again afterwards.
func newFakeServerAt(t *testing.T, addr string) *fakeServer {
	t.Helper()
	s := &fakeServer{
		t:     t,
		data:  make(map[string]fakeEntry),
		conns: make(map[net.Conn]struct{}),
	}
	s.start(t, addr)
	t.Cleanup(s.stop)
	return s
}

func (s *fakeServer) start(t *testing.T, addr string) {
	t.Helper()

	var config net.ListenConfig
	ln, err := config.Listen(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatalf("listening on %s: %v", addr, err)
	}
	if s.tlsConfig != nil {
		ln = tls.NewListener(ln, s.tlsConfig)
	}

	s.mu.Lock()
	s.ln = ln
	s.addr = ln.Addr().String()
	s.stopped = false
	s.mu.Unlock()

	s.wg.Add(1)
	go s.accept(ln)
}

func (s *fakeServer) accept(ln net.Listener) {
	defer s.wg.Done()
	for {
		nc, err := ln.Accept()
		if err != nil {
			return
		}

		s.mu.Lock()
		if s.stopped {
			s.mu.Unlock()
			_ = nc.Close()
			return
		}
		s.conns[nc] = struct{}{}
		s.opened++
		s.mu.Unlock()

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serve(nc)
		}()
	}
}

// stop closes the listener and every connection, the way a killed server does.
func (s *fakeServer) stop() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	ln := s.ln
	conns := make([]net.Conn, 0, len(s.conns))
	for nc := range s.conns {
		conns = append(conns, nc)
	}
	s.conns = make(map[net.Conn]struct{})
	s.mu.Unlock()

	if ln != nil {
		_ = ln.Close()
	}
	for _, nc := range conns {
		_ = nc.Close()
	}
	s.wg.Wait()
}

func (s *fakeServer) address() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// connectionsOpened is how many connections the server has accepted in its
// lifetime. A client that discards a connection shows up here as one more.
func (s *fakeServer) connectionsOpened() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opened
}

// liveConnections is how many are open right now, which is what proves a Close
// or a canceled call left nothing behind.
func (s *fakeServer) liveConnections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

func (s *fakeServer) requireAuth(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = token
}

func (s *fakeServer) setHook(hook func(w *bufio.Writer, nc net.Conn, name string, args [][]byte) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hook = hook
}

func (s *fakeServer) setHelloReply(reply func(args [][]byte) string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.helloReply = reply
}

func (s *fakeServer) serve(nc net.Conn) {
	defer func() {
		s.mu.Lock()
		delete(s.conns, nc)
		s.mu.Unlock()
		_ = nc.Close()
	}()

	r := bufio.NewReader(nc)
	w := bufio.NewWriter(nc)
	authed := false

	for {
		args, err := readRequest(r)
		if err != nil {
			return
		}
		if len(args) == 0 {
			continue
		}

		name := strings.ToUpper(string(args[0]))

		s.mu.Lock()
		hook := s.hook
		s.mu.Unlock()
		if hook != nil && hook(w, nc, name, args[1:]) {
			if err := w.Flush(); err != nil {
				return
			}
			continue
		}

		closeAfter := s.dispatch(w, name, args[1:], &authed)
		if err := w.Flush(); err != nil {
			return
		}
		if closeAfter {
			return
		}
	}
}

// dispatch answers one command, reporting whether the connection should close.
func (s *fakeServer) dispatch(w *bufio.Writer, name string, args [][]byte, authed *bool) bool {
	if name == "QUIT" {
		writeStatus(w, statusOK)
		return true
	}
	if s.handshakeCommand(w, name, args, authed) {
		return false
	}

	s.mu.Lock()
	needsAuth := s.token != "" && !*authed
	s.mu.Unlock()
	if needsAuth {
		writeError(w, "NOAUTH Authentication required")
		return false
	}

	s.dataCommand(w, name, args)
	return false
}

// handshakeCommand answers the commands a connection may send before it has
// authenticated.
func (s *fakeServer) handshakeCommand(w *bufio.Writer, name string, args [][]byte, authed *bool) bool {
	switch name {
	case "HELLO":
		s.mu.Lock()
		custom := s.helloReply
		s.mu.Unlock()
		if custom != nil {
			writeRaw(w, custom(args))
			return true
		}
		if len(args) > 0 && string(args[0]) != "2" {
			// What a v0.1.0 server answers a client probing for RESP3.
			writeError(w, "NOPROTO unsupported protocol version")
			return true
		}
		writeHelloProperties(w)
		return true

	case "AUTH":
		s.mu.Lock()
		want := s.token
		s.mu.Unlock()
		if want == "" {
			writeError(w, "ERR Client sent AUTH, but no password is set")
			return true
		}
		got := ""
		if len(args) == 1 {
			got = string(args[0])
		} else if len(args) == 2 {
			got = string(args[1])
		}
		if got != want {
			writeError(w, "WRONGPASS invalid username-password pair")
			return true
		}
		*authed = true
		writeStatus(w, statusOK)
		return true

	case "PING":
		if len(args) == 1 {
			writeBulk(w, args[0])
		} else {
			writeStatus(w, "PONG")
		}
		return true

	default:
		return false
	}
}

//nolint:gocyclo // a command switch is one decision per command; splitting it hides the surface
func (s *fakeServer) dataCommand(w *bufio.Writer, name string, args [][]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch name {
	case "GET":
		entry, ok := s.live(string(args[0]))
		if !ok {
			writeNil(w)
			return
		}
		writeBulk(w, entry.value)

	case "SET":
		expires, ok := parseExpiry(args[2:])
		if !ok {
			writeError(w, "ERR syntax error")
			return
		}
		s.data[string(args[0])] = fakeEntry{value: append([]byte(nil), args[1]...), expires: expires}
		writeStatus(w, statusOK)

	case "SETNX":
		if _, ok := s.live(string(args[0])); ok {
			writeInt(w, 0)
			return
		}
		s.data[string(args[0])] = fakeEntry{value: append([]byte(nil), args[1]...)}
		writeInt(w, 1)

	case "DEL":
		var removed int64
		for _, key := range args {
			if _, ok := s.live(string(key)); ok {
				delete(s.data, string(key))
				removed++
			}
		}
		writeInt(w, removed)

	case "EXISTS":
		var found int64
		for _, key := range args {
			if _, ok := s.live(string(key)); ok {
				found++
			}
		}
		writeInt(w, found)

	case "EXPIRE":
		seconds, err := strconv.ParseInt(string(args[1]), 10, 64)
		if err != nil {
			writeError(w, "ERR value is not an integer or out of range")
			return
		}
		key := string(args[0])
		entry, ok := s.live(key)
		if !ok {
			writeInt(w, 0)
			return
		}
		if seconds <= 0 {
			delete(s.data, key)
			writeInt(w, 1)
			return
		}
		entry.expires = time.Now().Add(time.Duration(seconds) * time.Second)
		s.data[key] = entry
		writeInt(w, 1)

	case "TTL":
		entry, ok := s.live(string(args[0]))
		switch {
		case !ok:
			writeInt(w, -2)
		case entry.expires.IsZero():
			writeInt(w, -1)
		default:
			writeInt(w, int64(time.Until(entry.expires).Round(time.Second)/time.Second))
		}

	case "KEYS":
		matches := s.matching(string(args[0]))
		writeArrayHeader(w, len(matches))
		for _, key := range matches {
			writeBulk(w, []byte(key))
		}

	case "SCAN":
		matches := s.matching(scanMatch(args[1:]))
		writeArrayHeader(w, 2)
		writeBulk(w, []byte(ScanStart))
		writeArrayHeader(w, len(matches))
		for _, key := range matches {
			writeBulk(w, []byte(key))
		}

	case "DBSIZE":
		writeInt(w, int64(len(s.data)))

	case "ECHO":
		writeBulk(w, args[0])

	case "INFO":
		writeBulk(w, []byte("# Server\r\natlascache_version:0.1.0-fake\r\n"))

	case "STATS":
		// A flat array of alternating names and values, which is how RESP2
		// renders the map the server's handler builds.
		writeArrayHeader(w, 4)
		writeBulk(w, []byte("keys"))
		writeInt(w, int64(len(s.data)))
		writeBulk(w, []byte("hits"))
		writeInt(w, 7)

	default:
		writeError(w, "ERR unknown command '"+name+"'")
	}
}

// live reads a key, honoring its expiry. The caller holds the lock.
func (s *fakeServer) live(key string) (fakeEntry, bool) {
	entry, ok := s.data[key]
	if !ok {
		return fakeEntry{}, false
	}
	if !entry.expires.IsZero() && time.Now().After(entry.expires) {
		delete(s.data, key)
		return fakeEntry{}, false
	}
	return entry, true
}

// matching is glob enough for a test: everything, an exact name, or a prefix.
func (s *fakeServer) matching(pattern string) []string {
	keys := make([]string, 0, len(s.data))
	for key := range s.data {
		if _, ok := s.live(key); !ok {
			continue
		}
		switch {
		case pattern == "*", pattern == "":
			keys = append(keys, key)
		case strings.HasSuffix(pattern, "*"):
			if strings.HasPrefix(key, strings.TrimSuffix(pattern, "*")) {
				keys = append(keys, key)
			}
		case key == pattern:
			keys = append(keys, key)
		}
	}
	return keys
}

func (s *fakeServer) set(key string, value []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = fakeEntry{value: append([]byte(nil), value...)}
}

func scanMatch(args [][]byte) string {
	for i := 0; i+1 < len(args); i += 2 {
		if strings.EqualFold(string(args[i]), "MATCH") {
			return string(args[i+1])
		}
	}
	return "*"
}

func parseExpiry(args [][]byte) (time.Time, bool) {
	if len(args) == 0 {
		return time.Time{}, true
	}
	if len(args) != 2 {
		return time.Time{}, false
	}
	amount, err := strconv.ParseInt(string(args[1]), 10, 64)
	if err != nil || amount <= 0 {
		return time.Time{}, false
	}
	switch strings.ToUpper(string(args[0])) {
	case "EX":
		return time.Now().Add(time.Duration(amount) * time.Second), true
	case "PX":
		return time.Now().Add(time.Duration(amount) * time.Millisecond), true
	default:
		return time.Time{}, false
	}
}

// readRequest decodes the RESP array of bulk strings a client sends. Nothing
// else is accepted, because nothing else is what this SDK sends.
func readRequest(r *bufio.Reader) ([][]byte, error) {
	header, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	header = strings.TrimSuffix(header, crlf)
	if !strings.HasPrefix(header, "*") {
		return nil, errors.New("not a multibulk request: " + strconv.Quote(header))
	}

	count, err := strconv.Atoi(header[1:])
	if err != nil || count < 0 {
		return nil, errors.New("bad multibulk count")
	}

	args := make([][]byte, 0, count)
	for range count {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimSuffix(line, crlf)
		if !strings.HasPrefix(line, "$") {
			return nil, errors.New("bad bulk header")
		}
		size, err := strconv.Atoi(line[1:])
		if err != nil || size < 0 {
			return nil, errors.New("bad bulk length")
		}
		buf := make([]byte, size+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		args = append(args, buf[:size])
	}
	return args, nil
}

// writeRaw is the one place this server drops a write error.
//
// It is a test server and the client is what is under test: a broken pipe here
// means the client hung up, which several tests arrange on purpose, and the
// serve loop notices on its next read either way.
func writeRaw(w *bufio.Writer, text string) {
	_, _ = w.WriteString(text) //nolint:errcheck // see above
}

func writeRawBytes(w *bufio.Writer, value []byte) {
	_, _ = w.Write(value) //nolint:errcheck // see writeRaw
}

func writeStatus(w *bufio.Writer, text string) { writeRaw(w, "+"+text+crlf) }
func writeError(w *bufio.Writer, text string)  { writeRaw(w, "-"+text+crlf) }
func writeNil(w *bufio.Writer)                 { writeRaw(w, "$-1"+crlf) }

func writeInt(w *bufio.Writer, n int64) {
	writeRaw(w, ":"+strconv.FormatInt(n, 10)+crlf)
}

func writeArrayHeader(w *bufio.Writer, n int) {
	writeRaw(w, "*"+strconv.Itoa(n)+crlf)
}

func writeBulk(w *bufio.Writer, value []byte) {
	writeRaw(w, "$"+strconv.Itoa(len(value))+crlf)
	writeRawBytes(w, value)
	writeRaw(w, crlf)
}

func writeHelloProperties(w *bufio.Writer) {
	writeArrayHeader(w, 6)
	writeBulk(w, []byte("server"))
	writeBulk(w, []byte("atlascache"))
	writeBulk(w, []byte("proto"))
	writeInt(w, 2)
	writeBulk(w, []byte("mode"))
	writeBulk(w, []byte("standalone"))
}

// freePort returns a port that was free a moment ago — the same approach the
// E2E harness takes, and good enough for a test that binds it immediately.
func freePort(t *testing.T) string {
	t.Helper()
	var config net.ListenConfig
	ln, err := config.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("releasing the reserved port: %v", err)
	}
	return addr
}
