package scenarios_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/b3vet/atlascache/test/e2e/certs"
	"github.com/b3vet/atlascache/test/e2e/runner"
)

// The SDK and CLI scenarios are driven here against a model server: a small,
// independent RESP implementation with switches for the specific ways a server
// or a client can be wrong.
//
// The model is not AtlasCache and proves nothing about it. What it proves is
// that each scenario passes against something that behaves and fails against
// something that does not — which is the only thing that makes a scenario worth
// running. Every defect below is a real failure mode: a miss that reads as an
// empty value, a value cut at its first null byte, a scan that claims to be
// finished, a refused token reported as a generic error, a server that hangs up
// after every command, and a read that answers with the previous caller's data.

// sdkDefects configures one model server, including the defects that each
// scenario exists to catch.
type sdkDefects struct {
	// token, when set, makes the model require AUTH for everything outside the
	// pre-auth allowlist.
	token string
	// serveTLS makes the model serve TLS with a generated certificate.
	serveTLS bool

	// missAsEmpty answers a missing key with an empty bulk string rather than a
	// null: the distinction ADR-0022 was amended for, gone.
	missAsEmpty bool
	// truncateAtNull stores values as a C string would, losing everything from
	// the first null byte on.
	truncateAtNull bool
	// dedupeExists counts distinct keys rather than arguments, which is the
	// obvious "fix" that breaks every client written against Redis.
	dedupeExists bool
	// scanStopsEarly answers the first page with the finished cursor, so a scan
	// silently returns a fraction of the keyspace.
	scanStopsEarly bool
	// acceptsAnything answers an unknown command with +OK instead of an error.
	acceptsAnything bool
	// genericAuthError reports a wrong token as -ERR, so a caller cannot tell a
	// credential problem from anything else.
	genericAuthError bool
	// acceptsAnyToken lets every credential through.
	acceptsAnyToken bool
	// aliasReads answers a read with the value of the previous read, which is
	// ISSUE-0009's shape and exactly what a reused poisoned connection does.
	aliasReads bool
	// hangUpEachTime closes the connection after every reply, so a pool cannot
	// reuse anything.
	hangUpEachTime bool
	// wrongValues answers every read with something else entirely.
	wrongValues bool
}

// sdkModel is the model server: RESP over TCP, optionally over TLS, with a
// keyspace and the defects above.
type sdkModel struct {
	defects sdkDefects
	root    string

	certFile string
	keyFile  string

	mu       sync.Mutex
	data     map[string][]byte
	expiry   map[string]time.Time
	lastRead []byte
	conns    map[net.Conn]struct{}

	listener atomic.Value // net.Listener
	addr     atomic.Value // string
	up       atomic.Bool

	wg   sync.WaitGroup
	stop chan struct{}
}

func newSDKModel(t *testing.T, defects sdkDefects) *sdkModel {
	t.Helper()

	model := &sdkModel{
		defects: defects,
		root:    t.TempDir(),
		data:    map[string][]byte{},
		expiry:  map[string]time.Time{},
		conns:   map[net.Conn]struct{}{},
		stop:    make(chan struct{}),
	}

	if defects.serveTLS {
		certFile, keyFile, err := certs.Write(filepath.Join(model.root, "certs"), "atlascache-sdk-model")
		if err != nil {
			t.Fatalf("generating the model's certificate: %v", err)
		}
		model.certFile, model.keyFile = certFile, keyFile
	}

	// The config file the scenarios read the token out of, exactly as the real
	// harness writes one.
	config := "node:\n  id: sdk-model\n"
	if defects.token != "" {
		config += "auth:\n  enabled: true\n  token: \"" + defects.token + "\"\n"
	}
	if err := os.WriteFile(filepath.Join(model.root, "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatalf("writing the model's config: %v", err)
	}

	if err := model.listen(); err != nil {
		t.Fatalf("listening: %v", err)
	}

	t.Cleanup(func() {
		close(model.stop)
		if listener, ok := model.listener.Load().(net.Listener); ok {
			_ = listener.Close()
		}
		model.hangUp()
		model.wg.Wait()
	})
	return model
}

// listen opens a new listener, which is also how a restart works: the address
// changes, exactly as it does when the harness relaunches a real server.
func (m *sdkModel) listen() error {
	var config net.ListenConfig
	listener, err := config.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	if m.defects.serveTLS {
		pair, loadErr := tls.LoadX509KeyPair(m.certFile, m.keyFile)
		if loadErr != nil {
			_ = listener.Close()
			return loadErr
		}
		listener = tls.NewListener(listener, &tls.Config{
			Certificates: []tls.Certificate{pair},
			MinVersion:   tls.VersionTLS13,
		})
	}

	m.listener.Store(listener)
	m.addr.Store(listener.Addr().String())
	m.up.Store(true)

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.accept(listener)
	}()
	return nil
}

func (m *sdkModel) accept(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		m.track(conn)
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			defer m.forget(conn)
			m.session(conn)
		}()
	}
}

func (m *sdkModel) session(conn net.Conn) {
	reader := bufio.NewReader(conn)
	authenticated := false

	for {
		args, err := readRESPCommand(reader)
		if err != nil {
			return
		}
		reply, closes := m.dispatch(args, &authenticated)
		if _, err := conn.Write([]byte(reply)); err != nil {
			return
		}
		if closes || m.defects.hangUpEachTime {
			return
		}
	}
}

func (m *sdkModel) track(conn net.Conn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.conns[conn] = struct{}{}
}

func (m *sdkModel) forget(conn net.Conn) {
	m.mu.Lock()
	delete(m.conns, conn)
	m.mu.Unlock()
	_ = conn.Close()
}

// shutdown takes the server down the way a process exiting does: the listener
// stops accepting and every connection it accepted is closed. A listener closed
// on its own leaves those connections serving, and a client would never notice
// the outage at all.
func (m *sdkModel) shutdown() {
	if listener, ok := m.listener.Load().(net.Listener); ok {
		_ = listener.Close()
	}
	m.up.Store(false)
	m.hangUp()

	// A restart loses the keyspace: v0.1.0 has no persistence.
	m.mu.Lock()
	m.data = map[string][]byte{}
	m.expiry = map[string]time.Time{}
	m.mu.Unlock()
}

// hangUp closes every open connection, which is what a server process exiting
// does and what closing a listener on its own does not.
func (m *sdkModel) hangUp() {
	m.mu.Lock()
	conns := make([]net.Conn, 0, len(m.conns))
	for conn := range m.conns {
		conns = append(conns, conn)
	}
	m.conns = map[net.Conn]struct{}{}
	m.mu.Unlock()

	for _, conn := range conns {
		_ = conn.Close()
	}
}

// sdkPreAuth is the allowlist the real server has.
var sdkPreAuth = map[string]bool{"PING": true, "HELLO": true, "AUTH": true, "QUIT": true}

func (m *sdkModel) dispatch(args []string, authenticated *bool) (reply string, closes bool) {
	command := strings.ToUpper(args[0])

	switch command {
	case "HELLO":
		// What a v0.1.0 server answers HELLO 3 with (ADR-0028).
		return sdkErr("NOPROTO unsupported protocol version"), false
	case "AUTH":
		return m.auth(args, authenticated), false
	case "QUIT":
		return sdkStatus("OK"), true
	case "PING":
		return sdkStatus("PONG"), false
	}

	if m.defects.token != "" && !*authenticated && !sdkPreAuth[command] {
		return sdkErr("NOAUTH Authentication required"), false
	}
	return m.keyspaceCommand(command, args), false
}

func (m *sdkModel) auth(args []string, authenticated *bool) string {
	if m.defects.token == "" {
		return sdkErr("ERR Client sent AUTH, but no password is set")
	}
	if m.defects.acceptsAnyToken || args[len(args)-1] == m.defects.token {
		*authenticated = true
		return sdkStatus("OK")
	}
	if m.defects.genericAuthError {
		return sdkErr("ERR invalid password")
	}
	return sdkErr("WRONGPASS invalid username-password pair")
}

func (m *sdkModel) keyspaceCommand(command string, args []string) string {
	switch command {
	case "ECHO":
		return bulkReply(args[1])
	case "SET":
		return m.set(args)
	case "SETNX":
		return m.setNX(args)
	case "GET":
		return m.get(args[1])
	case "DEL":
		return m.del(args[1:])
	case "EXISTS":
		return m.exists(args[1:])
	case "EXPIRE":
		return m.expire(args)
	case "TTL":
		return m.ttl(args[1])
	case "KEYS":
		return m.keys(args[1])
	case "SCAN":
		return m.scan(args)
	case "DBSIZE":
		m.mu.Lock()
		defer m.mu.Unlock()
		return sdkInt(int64(len(m.data)))
	case "INFO":
		return bulkReply(sdkInfoText(args[1:]))
	case "STATS":
		m.mu.Lock()
		keys := int64(len(m.data))
		m.mu.Unlock()
		return sdkArray([]string{
			bulkReply("keys"), sdkInt(keys),
			bulkReply("commands_processed"), sdkInt(99),
			bulkReply("connected_clients"), sdkInt(1),
		})
	default:
		if m.defects.acceptsAnything {
			return sdkStatus("OK")
		}
		return sdkErr("ERR unknown command '" + command + "'")
	}
}

func (m *sdkModel) set(args []string) string {
	if len(args) < 3 {
		return sdkErr("ERR wrong number of arguments for 'set' command")
	}
	value := []byte(args[2])
	if m.defects.truncateAtNull {
		if i := strings.IndexByte(args[2], 0); i >= 0 {
			value = []byte(args[2][:i])
		}
	}

	var expiry time.Time
	if len(args) >= 5 {
		amount, err := strconv.Atoi(args[4])
		if err != nil {
			return sdkErr("ERR value is not an integer or out of range")
		}
		switch strings.ToUpper(args[3]) {
		case "EX":
			expiry = time.Now().Add(time.Duration(amount) * time.Second)
		case "PX":
			expiry = time.Now().Add(time.Duration(amount) * time.Millisecond)
		default:
			return sdkErr("ERR syntax error")
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[args[1]] = value
	if expiry.IsZero() {
		delete(m.expiry, args[1])
	} else {
		m.expiry[args[1]] = expiry
	}
	return sdkStatus("OK")
}

func (m *sdkModel) setNX(args []string) string {
	if len(args) != 3 {
		return sdkErr("ERR wrong number of arguments for 'setnx' command")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, live := m.liveLocked(args[1]); live {
		return sdkInt(0)
	}
	m.data[args[1]] = []byte(args[2])
	return sdkInt(1)
}

func (m *sdkModel) get(key string) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	value, live := m.liveLocked(key)
	if !live {
		if m.defects.missAsEmpty {
			return bulkReply("")
		}
		return "$-1\r\n"
	}

	switch {
	case m.defects.wrongValues:
		return bulkReply("not the value you stored")
	case m.defects.aliasReads && m.lastRead != nil:
		return bulkReply(string(m.lastRead))
	}
	m.lastRead = value
	return bulkReply(string(value))
}

func (m *sdkModel) del(keys []string) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	var removed int64
	for _, key := range keys {
		if _, ok := m.data[key]; ok {
			delete(m.data, key)
			delete(m.expiry, key)
			removed++
		}
	}
	return sdkInt(removed)
}

func (m *sdkModel) exists(keys []string) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	seen := map[string]bool{}
	var count int64
	for _, key := range keys {
		if _, live := m.liveLocked(key); !live {
			continue
		}
		if m.defects.dedupeExists {
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		count++
	}
	return sdkInt(count)
}

func (m *sdkModel) expire(args []string) string {
	if len(args) != 3 {
		return sdkErr("ERR wrong number of arguments for 'expire' command")
	}
	seconds, err := strconv.Atoi(args[2])
	if err != nil {
		return sdkErr("ERR value is not an integer or out of range")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if _, live := m.liveLocked(args[1]); !live {
		return sdkInt(0)
	}
	m.expiry[args[1]] = time.Now().Add(time.Duration(seconds) * time.Second)
	return sdkInt(1)
}

func (m *sdkModel) ttl(key string) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, live := m.liveLocked(key); !live {
		return sdkInt(-2)
	}
	expiry, ok := m.expiry[key]
	if !ok {
		return sdkInt(-1)
	}
	return sdkInt(int64(time.Until(expiry).Seconds()) + 1)
}

func (m *sdkModel) keys(pattern string) string {
	keys := m.matching(pattern)
	items := make([]string, 0, len(keys))
	for _, key := range keys {
		items = append(items, bulkReply(key))
	}
	return sdkArray(items)
}

// scan pages the keyspace two keys at a time, so that a scan which stops at the
// first page is visibly wrong rather than accidentally complete.
func (m *sdkModel) scan(args []string) string {
	cursor, err := strconv.Atoi(args[1])
	if err != nil || cursor < 0 {
		return sdkErr("ERR invalid cursor")
	}

	pattern := ""
	pageSize := 2
	for i := 2; i+1 < len(args); i += 2 {
		switch strings.ToUpper(args[i]) {
		case "MATCH":
			pattern = args[i+1]
		case "COUNT":
			if n, convErr := strconv.Atoi(args[i+1]); convErr == nil && n > 0 {
				pageSize = n
			}
		}
	}

	keys := m.matching(pattern)
	if cursor > len(keys) {
		return sdkErr("ERR invalid cursor")
	}
	end := min(cursor+pageSize, len(keys))

	next := end
	if end >= len(keys) || m.defects.scanStopsEarly {
		next = 0
	}

	page := make([]string, 0, end-cursor)
	for _, key := range keys[cursor:end] {
		page = append(page, bulkReply(key))
	}
	return sdkArray([]string{bulkReply(strconv.Itoa(next)), sdkArray(page)})
}

// matching returns the live keys matching a glob, sorted so that paging is
// stable across calls.
func (m *sdkModel) matching(pattern string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	var keys []string
	for key := range m.data {
		if _, live := m.liveLocked(key); !live {
			continue
		}
		if sdkMatches(pattern, key) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

// liveLocked returns a key's value if it is present and not expired. The caller
// holds the lock.
func (m *sdkModel) liveLocked(key string) ([]byte, bool) {
	value, ok := m.data[key]
	if !ok {
		return nil, false
	}
	if expiry, has := m.expiry[key]; has && time.Now().After(expiry) {
		return nil, false
	}
	return value, true
}

// sdkMatches is a glob with as much of one as these tests need: a trailing
// star, or an exact match.
func sdkMatches(pattern, key string) bool {
	switch {
	case pattern == "" || pattern == "*":
		return true
	case strings.HasSuffix(pattern, "*"):
		return strings.HasPrefix(key, strings.TrimSuffix(pattern, "*"))
	default:
		return pattern == key
	}
}

// sdkInfoText is INFO's format: CRLF-terminated key:value lines under #Section
// headers, which is what every tool that parses a Redis expects.
func sdkInfoText(sections []string) string {
	const crlf = "\r\n"
	server := "# Server" + crlf + "atlascache_version:0.1.0-model" + crlf + "atlascache_mode:standalone" + crlf
	keyspace := "# Keyspace" + crlf + "db0:keys=3" + crlf

	if len(sections) == 1 && strings.EqualFold(sections[0], "server") {
		return server
	}
	return server + crlf + keyspace
}

func sdkStatus(text string) string { return "+" + text + "\r\n" }
func sdkErr(text string) string    { return "-" + text + "\r\n" }
func sdkInt(n int64) string        { return ":" + strconv.FormatInt(n, 10) + "\r\n" }

func sdkArray(items []string) string {
	var b strings.Builder
	b.WriteString("*" + strconv.Itoa(len(items)) + "\r\n")
	for _, item := range items {
		b.WriteString(item)
	}
	return b.String()
}

// sdkHarness puts a runner.Harness in front of a model server, and tells the
// scenarios where the binaries and the TLS material are.
type sdkHarness struct {
	model  *sdkModel
	binary string

	// staysDead makes a restart fail to bring the server back, which is the
	// defect sdk_survives_a_server_restart exists to catch.
	staysDead bool
}

func newSDKHarness(t *testing.T, defects sdkDefects) *sdkHarness {
	t.Helper()
	return &sdkHarness{model: newSDKModel(t, defects)}
}

func (h *sdkHarness) Kill() error    { return nil }
func (h *sdkHarness) Close() error   { return nil }
func (h *sdkHarness) Logs() string   { return "" }
func (h *sdkHarness) Root() string   { return h.model.root }
func (h *sdkHarness) Binary() string { return h.binary }

// Start brings the model back on a fresh port, as the real harness does, and
// refuses to when the test asked for a server that does not come back.
func (h *sdkHarness) Start(context.Context) error {
	if h.model.up.Load() {
		return nil
	}
	if h.staysDead {
		return errors.New("the port is still held; the server did not come back")
	}
	return h.model.listen()
}

func (h *sdkHarness) Stop(context.Context) error {
	h.model.shutdown()
	return nil
}

// Restart is Stop and Start, and lands the server on a fresh port exactly as
// the real harness does.
func (h *sdkHarness) Restart(ctx context.Context) error {
	if err := h.Stop(ctx); err != nil {
		return err
	}
	return h.Start(ctx)
}

func (h *sdkHarness) Info() runner.ServerInfo {
	addr, ok := h.model.addr.Load().(string)
	if !ok {
		addr = ""
	}
	return runner.ServerInfo{ClientAddr: addr, AdminAddr: addr, DataDir: h.model.root, PID: os.Getpid()}
}

// Send is not used: these tests drive scenarios directly rather than through a
// spec's cmd steps.
func (h *sdkHarness) Send(context.Context, string) (runner.Reply, error) {
	return runner.Reply{}, errors.New("the model harness serves the SDK, not the runner's own client")
}

// RunBinary is here for the serverBinary assertion the scenarios make when they
// look for the config file and the CLI.
func (h *sdkHarness) RunBinary(context.Context, ...string) (int, string, error) {
	return 0, "", nil
}

// TLSEnabled, CertPaths and ClientTLSConfig make this a tlsHarness, which is
// what the TLS scenarios assert against.
func (h *sdkHarness) TLSEnabled() bool { return h.model.defects.serveTLS }

func (h *sdkHarness) CertPaths() (string, string) { return h.model.certFile, h.model.keyFile }

func (h *sdkHarness) ClientTLSConfig() (*tls.Config, error) {
	pemBytes, err := certs.ReadCertificate(h.model.certFile)
	if err != nil {
		return nil, fmt.Errorf("reading the model's certificate: %w", err)
	}
	return certs.ClientConfig(pemBytes)
}
