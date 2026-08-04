package scenarios_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/b3vet/atlascache/test/e2e/client"
	"github.com/b3vet/atlascache/test/e2e/runner"
)

// A P1 scenario asserts things a cache does — what it evicts, what it expires,
// what bytes it hands back — and none of that can be checked against a harness
// that answers from a map: every assertion would either pass vacuously or fail
// for the wrong reason. So the scenarios are driven here against a model cache:
// a small, independent implementation of the semantics they assert over, with
// its own byte budget, eviction policy and expiry sweeper.
//
// The model is not AtlasCache and proves nothing about it. What it proves is
// that each scenario passes against a server that behaves and fails against one
// that does not — which is the only thing that makes a scenario worth running.
// Every defect below is a real issue from the plan, reproduced on purpose.

// modelEntryOverhead is the per-entry cost the eviction scenarios size their
// max_memory against; it has to match evictEntrySize in eviction.go or the
// keyspace arithmetic those scenarios do would not describe this cache.
const modelEntryOverhead = 80

// modelOptions configures one model cache, including the defects that each
// scenario exists to catch.
type modelOptions struct {
	maxMemory int
	policy    string

	// leakExpiredMemory is ISSUE-0007: an expired entry stops answering but
	// never gives its bytes back, so the budget shrinks with every wave of TTLs.
	leakExpiredMemory bool
	// aliasStoredValues is ISSUE-0009: the engine kept the caller's slice
	// instead of copying it, so a stored value reads as whatever arrived on the
	// connection most recently.
	aliasStoredValues bool
	// honorStaleHints is the ADR-0016 failure: an expiry hint left behind by a
	// superseded TTL still fires, expiring a key that is alive.
	honorStaleHints bool
	// tornExpiry is ISSUE-0008: an expiry written while the key is being read
	// concurrently is occasionally stored torn, and a half-written deadline
	// lands in the past — so the next read reclaims a key that is alive.
	tornExpiry bool
}

type modelEntry struct {
	value  []byte
	cost   int
	seq    int64 // insertion order, for fifo
	used   int64 // last access, for lru
	hits   int64 // access count, for lfu
	expiry time.Time
	hint   time.Time // the deadline the sweeper was told about
}

// modelCache is the cache itself. Every command is serialized under one lock,
// which is all the concurrency this model owes: the scenarios care about what
// the answers are, not about how many cores produced them.
type modelCache struct {
	opts modelOptions

	mu      sync.Mutex
	entries map[string]*modelEntry
	used    int
	clock   int64
	expires int64
	policy  string
	shared  []byte
	log     []string
}

// tornEvery is how often the torn-expiry defect strikes. A race that had to be
// waited for would make this test slow; a race that struck every time would let
// a scenario pass by noticing the very first write.
const tornEvery = 20

func newModelCache(opts modelOptions) *modelCache {
	if opts.policy == "" {
		opts.policy = "lru"
	}
	if opts.maxMemory == 0 {
		opts.maxMemory = 1 << 30
	}
	return &modelCache{opts: opts, entries: map[string]*modelEntry{}, policy: opts.policy}
}

func (m *modelCache) exec(args []string) runner.Reply {
	m.mu.Lock()
	defer m.mu.Unlock()

	switch strings.ToUpper(args[0]) {
	case "PING":
		return runner.StatusReply("PONG")
	case "QUIT":
		return runner.StatusReply("OK")
	case "SET":
		return m.set(args[1:])
	case "GET":
		return m.get(args[1:])
	case "DEL":
		return m.del(args[1:])
	case "EXPIRE":
		return m.expire(args[1:])
	case "TTL":
		return m.ttl(args[1:])
	default:
		return runner.ErrorReply("ERR unknown command '" + args[0] + "'")
	}
}

func (m *modelCache) set(args []string) runner.Reply {
	if len(args) < 2 {
		return runner.ErrorReply("ERR wrong number of arguments for 'set' command")
	}
	key, value := args[0], args[1]

	var expiry time.Time
	if len(args) >= 4 {
		amount, err := strconv.Atoi(args[3])
		if err != nil {
			return runner.ErrorReply("ERR value is not an integer or out of range")
		}
		switch strings.ToUpper(args[2]) {
		case "EX":
			expiry = time.Now().Add(time.Duration(amount) * time.Second)
		case "PX":
			expiry = time.Now().Add(time.Duration(amount) * time.Millisecond)
		default:
			return runner.ErrorReply("ERR syntax error")
		}
	}

	// An overwrite releases the old entry first, so replacing a value in place
	// cannot be refused for want of room it already holds.
	if existing, ok := m.entries[key]; ok {
		m.used -= existing.cost
		delete(m.entries, key)
	}

	cost := len(key) + len(value) + modelEntryOverhead
	if !m.makeRoom(cost) {
		return runner.ErrorReply("OOM command not allowed when used memory > 'maxmemory'")
	}

	stored := []byte(value)
	m.shared = stored
	m.clock++
	m.entries[key] = &modelEntry{
		value: stored, cost: cost, seq: m.clock, used: m.clock, expiry: expiry, hint: expiry,
	}
	m.used += cost
	return runner.StatusReply("OK")
}

func (m *modelCache) get(args []string) runner.Reply {
	if len(args) != 1 {
		return runner.ErrorReply("ERR wrong number of arguments for 'get' command")
	}
	entry, ok := m.live(args[0])
	if !ok {
		return runner.NilReply()
	}
	m.clock++
	entry.used = m.clock
	entry.hits++
	if m.opts.aliasStoredValues {
		return runner.BulkReply(string(m.shared))
	}
	return runner.BulkReply(string(entry.value))
}

func (m *modelCache) del(args []string) runner.Reply {
	removed := int64(0)
	for _, key := range args {
		entry, ok := m.entries[key]
		if !ok {
			continue
		}
		m.used -= entry.cost
		delete(m.entries, key)
		removed++
	}
	return runner.IntegerReply(removed)
}

func (m *modelCache) expire(args []string) runner.Reply {
	if len(args) != 2 {
		return runner.ErrorReply("ERR wrong number of arguments for 'expire' command")
	}
	seconds, err := strconv.Atoi(args[1])
	if err != nil {
		return runner.ErrorReply("ERR value is not an integer or out of range")
	}
	entry, ok := m.live(args[0])
	if !ok {
		return runner.IntegerReply(0)
	}
	m.expires++
	entry.expiry = time.Now().Add(time.Duration(seconds) * time.Second)
	if m.opts.tornExpiry && m.expires%tornEvery == 0 {
		entry.expiry = time.Now().Add(-time.Second)
	}
	if !m.opts.honorStaleHints {
		// The new deadline supersedes the hint the wheel is holding.
		entry.hint = entry.expiry
	}
	return runner.IntegerReply(1)
}

func (m *modelCache) ttl(args []string) runner.Reply {
	if len(args) != 1 {
		return runner.ErrorReply("ERR wrong number of arguments for 'ttl' command")
	}
	entry, ok := m.live(args[0])
	if !ok {
		return runner.IntegerReply(-2)
	}
	if entry.expiry.IsZero() {
		return runner.IntegerReply(-1)
	}
	return runner.IntegerReply(int64(math.Ceil(time.Until(entry.expiry).Seconds())))
}

// live returns an entry that is present and not past its deadline. A read of an
// expired key is a miss whether or not the sweeper has reached it yet.
func (m *modelCache) live(key string) (*modelEntry, bool) {
	entry, ok := m.entries[key]
	if !ok {
		return nil, false
	}
	if !entry.expiry.IsZero() && time.Now().After(entry.expiry) {
		return nil, false
	}
	return entry, true
}

// makeRoom evicts until cost fits and reports whether it succeeded. Policy none
// never evicts, so it fails instead — which is the OOM refusal.
func (m *modelCache) makeRoom(cost int) bool {
	if cost > m.opts.maxMemory {
		return false
	}
	for m.used+cost > m.opts.maxMemory {
		key, entry, ok := m.victim()
		if !ok {
			return false
		}
		m.used -= entry.cost
		delete(m.entries, key)
	}
	return true
}

// victim picks the entry the active policy would drop.
func (m *modelCache) victim() (string, *modelEntry, bool) {
	var (
		chosenKey   string
		chosenEntry *modelEntry
	)
	for key, entry := range m.entries {
		if chosenEntry == nil || m.evictsFirst(entry, chosenEntry) {
			chosenKey, chosenEntry = key, entry
		}
	}
	if chosenEntry == nil || m.policy == "none" {
		return "", nil, false
	}
	return chosenKey, chosenEntry, true
}

// evictsFirst reports whether a is a better victim than b under the policy.
func (m *modelCache) evictsFirst(a, b *modelEntry) bool {
	switch m.policy {
	case "fifo":
		return a.seq < b.seq
	case "lfu":
		if a.hits != b.hits {
			return a.hits < b.hits
		}
		return a.seq < b.seq
	case "lru":
		return a.used < b.used
	default:
		return false
	}
}

// sweep is the active-expiration path: it reclaims entries that are due even
// though nothing has read them, which is the property ttl_reclaims_memory
// exists to check. It acts on the hint rather than the entry when the model is
// configured with that defect.
func (m *modelCache) sweep() {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	for key, entry := range m.entries {
		deadline := entry.expiry
		if m.opts.honorStaleHints && !entry.hint.IsZero() {
			deadline = entry.hint
		}
		if deadline.IsZero() || now.Before(deadline) {
			continue
		}
		delete(m.entries, key)
		if !m.opts.leakExpiredMemory {
			m.used -= entry.cost
		}
	}
}

// applyPolicy adopts a policy from the config file and says so in the log, the
// way a server with config hot-reload does.
func (m *modelCache) applyPolicy(policy string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if policy == "" || policy == m.policy {
		return
	}
	m.policy = policy
	m.log = append(m.log, `{"level":"info","msg":"eviction policy applied","policy":"`+policy+`"}`)
}

func (m *modelCache) logs() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.Join(m.log, "\n")
}

// modelHarness is a runner.Harness in front of a model cache. It also satisfies
// the serverBinary escape hatch the eviction scenarios reach for, since
// switching policy goes through the config file the harness owns.
type modelHarness struct {
	cache *modelCache
	root  string
	addr  string
}

func (h *modelHarness) Start(context.Context) error   { return nil }
func (h *modelHarness) Stop(context.Context) error    { return nil }
func (h *modelHarness) Restart(context.Context) error { return nil }
func (h *modelHarness) Kill() error                   { return nil }
func (h *modelHarness) Close() error                  { return nil }
func (h *modelHarness) Logs() string                  { return h.cache.logs() }
func (h *modelHarness) Root() string                  { return h.root }

func (h *modelHarness) RunBinary(context.Context, ...string) (int, string, error) {
	return 0, "", nil
}

func (h *modelHarness) Info() runner.ServerInfo {
	return runner.ServerInfo{ClientAddr: h.addr, AdminAddr: h.addr, DataDir: h.root, PID: 1}
}

// Send parses the command line with the same parser the real harness uses, so a
// scenario's quoting and escapes are exercised here too rather than only in
// production.
func (h *modelHarness) Send(_ context.Context, cmd string) (runner.Reply, error) {
	args, err := client.ParseCommand(cmd)
	if err != nil {
		return runner.Reply{}, err
	}
	return h.cache.exec(args), nil
}

// newModelHarness starts a model cache, its expiry sweeper, its config watcher
// and a loopback listener, and hands back the harness in front of them. The
// listener exists for the one scenario that opens its own connections.
func newModelHarness(t *testing.T, opts modelOptions) *modelHarness {
	t.Helper()

	cache := newModelCache(opts)
	root := t.TempDir()
	writeModelConfig(t, filepath.Join(root, "config.yaml"), cache.policy)

	var config net.ListenConfig
	ln, err := config.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	harness := &modelHarness{cache: cache, root: root, addr: ln.Addr().String()}

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); runSweeper(cache, done) }()
	go func() { defer wg.Done(); watchModelConfig(cache, filepath.Join(root, "config.yaml"), done) }()
	go serveModel(ln, cache)

	t.Cleanup(func() {
		close(done)
		_ = ln.Close()
		wg.Wait()
	})
	return harness
}

func runSweeper(cache *modelCache, done <-chan struct{}) {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			cache.sweep()
		}
	}
}

// watchModelConfig re-reads eviction.policy from the config file, which is how
// switch_policy in the eviction scenario changes it: there is no command for it.
func watchModelConfig(cache *modelCache, path string, done <-chan struct{}) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			cache.applyPolicy(readModelPolicy(path))
		}
	}
}

func readModelPolicy(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var config struct {
		Eviction struct {
			Policy string `yaml:"policy"`
		} `yaml:"eviction"`
	}
	if err := yaml.Unmarshal(raw, &config); err != nil {
		return ""
	}
	return config.Eviction.Policy
}

func writeModelConfig(t *testing.T, path, policy string) {
	t.Helper()
	body := "eviction:\n  policy: " + policy + "\nstorage:\n  max_memory: 900\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the model config: %v", err)
	}
}

// serveModel answers commands over TCP, for the scenarios that open their own
// connections rather than going through the harness.
func serveModel(ln net.Listener, cache *modelCache) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = conn.Close() }()
			reader := bufio.NewReader(conn)
			for {
				args, err := readArgs(reader)
				if err != nil {
					return
				}
				if _, err := io.WriteString(conn, encodeReply(cache.exec(args))); err != nil {
					return
				}
			}
		}()
	}
}

// readArgs consumes one RESP array of bulk strings, which is the only request
// form this client sends.
func readArgs(r *bufio.Reader) ([]string, error) {
	header, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	count, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(header, "*")))
	if err != nil {
		return nil, errors.New("not a command array: " + header)
	}

	args := make([]string, 0, count)
	for i := 0; i < count; i++ {
		sizeLine, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		size, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(sizeLine, "$")))
		if err != nil {
			return nil, err
		}
		payload := make([]byte, size+2)
		if _, err := io.ReadFull(r, payload); err != nil {
			return nil, err
		}
		args = append(args, string(payload[:size]))
	}
	return args, nil
}

func encodeReply(reply runner.Reply) string {
	switch reply.Kind {
	case runner.KindStatus:
		return "+" + reply.Text + "\r\n"
	case runner.KindError:
		return "-" + reply.Text + "\r\n"
	case runner.KindInteger:
		return ":" + strconv.FormatInt(reply.Integer, 10) + "\r\n"
	case runner.KindNil:
		return "$-1\r\n"
	case runner.KindBulk:
		return "$" + strconv.Itoa(len(reply.Text)) + "\r\n" + reply.Text + "\r\n"
	case runner.KindInvalid, runner.KindArray, runner.KindMap:
		return "-ERR the model cache cannot encode a " + reply.Kind.String() + " reply\r\n"
	default:
		return "-ERR the model cache cannot encode this reply\r\n"
	}
}
