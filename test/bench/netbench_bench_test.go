package netbench

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// FEAT-0026 -- what a client sees, through a real socket, against a real
// atlascache process.
//
// Run it:
//
//	make bench-network
//	go test ./test/bench -run TestNetworkBaseline -netbench -timeout 30m -v
//
// It is skipped without the flag. It takes minutes, needs a built binary, and
// its numbers are meaningless on a machine doing anything else, so it is not
// something `go test ./...` should be dragging along.

var (
	flagRun      = flag.Bool("netbench", false, "run the P2 network benchmark sweep")
	flagBinary   = flag.String("netbench.binary", "../../bin/atlascache", "server binary under test")
	flagDuration = flag.Duration("netbench.duration", 5*time.Second, "measurement window per cell")
	flagWarmup   = flag.Duration("netbench.warmup", time.Second, "warmup before each measurement window")
	flagOut      = flag.String("netbench.out", "", "write the raw result table to this file")
	flagProcs    = flag.Int("netbench.gomaxprocs", 2, "GOMAXPROCS for the generator; 0 leaves it alone")
)

const (
	baselineConns    = 50
	baselinePipeline = 1
	baselineValue    = 64
	delKeys          = 1_000_000
	missKeys         = 100_000
	preloadConns     = 50
	preloadPipeline  = 100
)

type row struct {
	group     string
	name      string
	conns     int
	pipeline  int
	valueSize int
	tlsOn     bool
	res       loadResult
	note      string
}

type runner struct {
	t      *testing.T
	rows   []row
	notes  []string
	binary string
}

// session is one server process plus the client-side facts about it.
type session struct {
	srv       *serverProc
	valueSize int
	tlsConf   *tls.Config
	tlsOn     bool
	keyspace  int
}

func TestNetworkBaseline(t *testing.T) {
	if !*flagRun {
		t.Skip("pass -netbench to run the network benchmark sweep (see `make bench-network`)")
	}
	if _, err := os.Stat(*flagBinary); err != nil {
		t.Fatalf("server binary %s: %v (run `make build`)", *flagBinary, err)
	}

	// The generator's GOMAXPROCS is part of the experiment, not an
	// environment detail. Client and server share ten cores here, and a
	// generator given all ten spends three of them in the Go scheduler and
	// takes them from the server it is measuring: at 50 connections and
	// pipeline 1 the same server measures 130k ops/sec through a
	// ten-P generator and 144k through a two-P one. Two is the setting that
	// drove every shape hardest; it is pinned so the baseline does not move
	// with whatever the shell happened to export.
	if *flagProcs > 0 {
		runtime.GOMAXPROCS(*flagProcs)
	}

	r := &runner{t: t, binary: *flagBinary}
	r.header()
	r.control()
	r.plaintext()
	r.tlsSweep()
	r.emit()
}

func (r *runner) header() {
	r.t.Logf("go %s, GOMAXPROCS=%d, NumCPU=%d, %s/%s",
		runtime.Version(), runtime.GOMAXPROCS(0), runtime.NumCPU(), runtime.GOOS, runtime.GOARCH)
	r.t.Logf("window %s, warmup %s, binary %s", *flagDuration, *flagWarmup, r.binary)
}

// control establishes the generator's ceiling before anything is claimed about
// the server. See nullserver_bench_test.go for why this comes first.
func (r *runner) control() {
	for _, c := range []struct {
		conns    int
		pipeline int
		value    int
	}{
		{baselineConns, 1, baselineValue},
		{baselineConns, 10, baselineValue},
		{baselineConns, 100, baselineValue},
		{200, 1, baselineValue},
		{baselineConns, 1, 1024},
		{baselineConns, 1, 64 * 1024},
	} {
		ns, err := startNullServerProc(c.value)
		if err != nil {
			r.t.Fatalf("null server: %v", err)
		}
		spec := loadSpec{
			name:     fmt.Sprintf("null-GET/c%d/p%d/v%d", c.conns, c.pipeline, c.value),
			conns:    c.conns,
			pipeline: c.pipeline,
			duration: *flagDuration,
			warmup:   *flagWarmup,
			requests: getTable(4096),
			addr:     ns.addr,
		}
		res, err := runLoad(context.Background(), spec, ns)
		ns.Stop()
		if err != nil {
			r.t.Fatalf("control run %s: %v", spec.name, err)
		}
		r.record(row{group: "control", name: "GET (null srv)", conns: c.conns,
			pipeline: c.pipeline, valueSize: c.value, res: res,
			note: "same transport, no keyspace: the cost of AtlasCache's own work is the gap to the matching row below"})
	}
}

func (r *runner) plaintext() {
	sess := r.open(serverConfig{}, baselineValue)
	defer sess.srv.Stop()

	r.preload(sess, setTable(sess.keyspace, sess.valueSize))
	r.connectCost(sess, "plaintext")

	group := "commands"
	r.measure(sess, group, "GET", getTable(sess.keyspace), baselineConns, baselinePipeline)
	r.measure(sess, group, "GET (miss)", getMissTable(missKeys), baselineConns, baselinePipeline)
	r.measure(sess, group, "SET", setTable(sess.keyspace, sess.valueSize), baselineConns, baselinePipeline)
	r.measure(sess, group, "PING", pingTable(4096), baselineConns, baselinePipeline)
	r.measure(sess, group, "MIXED 90/10", mixedTable(sess.keyspace, sess.valueSize), baselineConns, baselinePipeline)

	for _, c := range []int{1, 10, 50, 200} {
		r.measure(sess, "connections", "GET", getTable(sess.keyspace), c, baselinePipeline)
		r.measure(sess, "connections", "SET", setTable(sess.keyspace, sess.valueSize), c, baselinePipeline)
	}
	for _, p := range []int{1, 10, 100} {
		r.measure(sess, "pipeline", "GET", getTable(sess.keyspace), baselineConns, p)
		r.measure(sess, "pipeline", "SET", setTable(sess.keyspace, sess.valueSize), baselineConns, p)
	}

	// DEL last: it empties the keyspace it measures. Every key is preloaded
	// first and deleted exactly once, so every delete is a hit -- a DEL
	// benchmark that cycled a table would be measuring misses after one pass.
	r.preload(sess, setTable(delKeys, sess.valueSize))
	r.exhaust(sess, "commands", "DEL", delTable(delKeys), baselineConns, baselinePipeline)

	r.valueSizes(false)
}

// valueSizes reruns the baseline cell at each value size, with a fresh server
// per size because the keyspace and the memory it costs both change.
func (r *runner) valueSizes(tlsOn bool) {
	group, sizes := "value size", []int{1024, 64 * 1024}
	for _, size := range sizes {
		sess := r.open(serverConfig{tlsOn: tlsOn}, size)
		r.preload(sess, setTable(sess.keyspace, size))
		r.measure(sess, group, "GET", getTable(sess.keyspace), baselineConns, baselinePipeline)
		if !tlsOn {
			r.measure(sess, group, "SET", setTable(sess.keyspace, size), baselineConns, baselinePipeline)
			r.measure(sess, group, "GET", getTable(sess.keyspace), baselineConns, 10)
			r.measure(sess, group, "SET", setTable(sess.keyspace, size), baselineConns, 10)
		}
		sess.srv.Stop()
	}
}

func (r *runner) tlsSweep() {
	sess := r.open(serverConfig{tlsOn: true}, baselineValue)
	defer sess.srv.Stop()

	r.preload(sess, setTable(sess.keyspace, sess.valueSize))
	r.connectCost(sess, "TLS 1.3")

	group := "tls"
	r.measure(sess, group, "GET", getTable(sess.keyspace), baselineConns, baselinePipeline)
	r.measure(sess, group, "SET", setTable(sess.keyspace, sess.valueSize), baselineConns, baselinePipeline)
	r.measure(sess, group, "GET", getTable(sess.keyspace), baselineConns, 10)
	r.measure(sess, group, "GET", getTable(sess.keyspace), 200, baselinePipeline)

	r.valueSizes(true)
}

// connectCost times establishing then closing the baseline connection count.
// For TLS that is the handshake, which is the part of the TLS bill a
// steady-state throughput number never shows.
func (r *runner) connectCost(sess *session, label string) {
	spec := sess.spec("connect", 200, 1)
	start := time.Now()
	workers, err := connectAll(context.Background(), spec)
	elapsed := time.Since(start)
	for _, w := range workers {
		w.close()
	}
	if err != nil {
		r.t.Fatalf("opening 200 %s connections: %v", label, err)
	}
	r.notes = append(r.notes, fmt.Sprintf("200 %s connections established in %s (%s each, serially)",
		label, elapsed.Round(time.Millisecond), (elapsed/200).Round(time.Microsecond)))
}

func (r *runner) open(cfg serverConfig, valueSize int) *session {
	srv, err := startServer(r.binary, cfg)
	if err != nil {
		r.t.Fatalf("starting the server: %v", err)
	}
	sess := &session{srv: srv, valueSize: valueSize, tlsOn: cfg.tlsOn, keyspace: keyspaceFor(valueSize)}
	if cfg.tlsOn {
		if sess.tlsConf, err = srv.clientTLS(); err != nil {
			srv.Stop()
			r.t.Fatalf("building the client TLS config: %v", err)
		}
	}
	return sess
}

func (s *session) spec(name string, conns, pipeline int) loadSpec {
	return loadSpec{
		name:     name,
		conns:    conns,
		pipeline: pipeline,
		duration: *flagDuration,
		warmup:   *flagWarmup,
		addr:     s.srv.addr,
		tlsConf:  s.tlsConf,
	}
}

// preload writes the keyspace a read benchmark will read. It is not measured;
// it is the setup that makes the measurement mean something.
func (r *runner) preload(sess *session, table [][]byte) {
	spec := sess.spec("preload", preloadConns, preloadPipeline)
	spec.requests = table
	spec.exhaust = true
	start := time.Now()
	res, err := runLoad(context.Background(), spec, sess.srv)
	if err != nil {
		r.t.Fatalf("preloading %d keys: %v", len(table), err)
	}
	r.t.Logf("preloaded %d keys of %dB in %s (%.0f ops/s)",
		res.ops, sess.valueSize, time.Since(start).Round(time.Millisecond), res.opsPerSec())
}

func (r *runner) measure(sess *session, group, name string, table [][]byte, conns, pipeline int) {
	spec := sess.spec(fmt.Sprintf("%s/%s/c%d/p%d", group, name, conns, pipeline), conns, pipeline)
	spec.requests = table
	res, err := runLoad(context.Background(), spec, sess.srv)
	if err != nil {
		r.t.Fatalf("%s: %v", spec.name, err)
	}
	r.record(row{group: group, name: name, conns: conns, pipeline: pipeline,
		valueSize: sess.valueSize, tlsOn: sess.tlsOn, res: res})
}

func (r *runner) exhaust(sess *session, group, name string, table [][]byte, conns, pipeline int) {
	spec := sess.spec(fmt.Sprintf("%s/%s/c%d/p%d", group, name, conns, pipeline), conns, pipeline)
	spec.requests = table
	spec.exhaust = true
	res, err := runLoad(context.Background(), spec, sess.srv)
	if err != nil {
		r.t.Fatalf("%s: %v", spec.name, err)
	}
	note := fmt.Sprintf("%d of %d deletes found a key", res.ones, res.ops)
	r.record(row{group: group, name: name, conns: conns, pipeline: pipeline,
		valueSize: sess.valueSize, tlsOn: sess.tlsOn, res: res, note: note})
}

func (r *runner) record(rw row) {
	r.rows = append(r.rows, rw)
	r.t.Logf("%-12s %-12s c=%-3d p=%-3d v=%-5s tls=%-5t  %10.0f ops/s  p50=%-9s p99=%-9s max=%-9s  cpu client=%.1f server=%.1f",
		rw.group, rw.name, rw.conns, rw.pipeline, sizeLabel(rw.valueSize), rw.tlsOn,
		rw.res.opsPerSec(), dur(rw.res.lat.quantile(0.50)), dur(rw.res.lat.quantile(0.99)),
		dur(rw.res.lat.max), rw.res.clientCores, rw.res.serverCores)
}

func (r *runner) emit() {
	var b strings.Builder
	fmt.Fprintf(&b, "%-12s %-12s %5s %5s %7s %5s %12s %10s %10s %10s %10s %10s %10s %7s %7s\n",
		"group", "command", "conns", "pipe", "value", "tls", "ops/sec", "p50", "p95", "p99", "p99.9", "max", "mean", "cl.cpu", "sv.cpu")
	for _, rw := range r.rows {
		h := rw.res.lat
		fmt.Fprintf(&b, "%-12s %-12s %5d %5d %7s %5t %12.0f %10s %10s %10s %10s %10s %10s %7.2f %7.2f\n",
			rw.group, rw.name, rw.conns, rw.pipeline, sizeLabel(rw.valueSize), rw.tlsOn,
			rw.res.opsPerSec(), dur(h.quantile(0.50)), dur(h.quantile(0.95)), dur(h.quantile(0.99)),
			dur(h.quantile(0.999)), dur(h.max), dur(uint64(h.mean())),
			rw.res.clientCores, rw.res.serverCores)
		if rw.note != "" {
			fmt.Fprintf(&b, "%-12s   note: %s\n", "", rw.note)
		}
	}
	for _, n := range r.notes {
		fmt.Fprintf(&b, "note: %s\n", n)
	}

	r.t.Logf("\n%s", b.String())
	if *flagOut == "" {
		return
	}
	if err := os.WriteFile(*flagOut, []byte(b.String()), 0o600); err != nil {
		r.t.Fatalf("writing %s: %v", *flagOut, err)
	}
	r.t.Logf("wrote %s", *flagOut)
}

func sizeLabel(n int) string {
	switch {
	case n >= 1024 && n%1024 == 0:
		return fmt.Sprintf("%dKB", n/1024)
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// dur renders a nanosecond count at a fixed three significant figures, so a
// column of latencies can be read down rather than parsed.
func dur(ns uint64) string {
	switch {
	case ns < 1000:
		return fmt.Sprintf("%dns", ns)
	case ns < 1_000_000:
		return fmt.Sprintf("%.1fus", float64(ns)/1e3)
	case ns < 1_000_000_000:
		return fmt.Sprintf("%.2fms", float64(ns)/1e6)
	default:
		return fmt.Sprintf("%.2fs", float64(ns)/1e9)
	}
}
