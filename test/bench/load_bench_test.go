package netbench

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// The load generator.
//
// Closed loop, on purpose: `conns` connections each keep `pipeline` requests in
// flight and issue the next batch only once the previous one has been answered.
// That is what redis-benchmark does and what a client library with a connection
// pool does, and it is the only shape in which "throughput" and "latency" are
// the same experiment. Its known limitation is coordinated omission -- a slow
// server is also offered less load, so the tail is the tail *at that
// concurrency*, not the tail an open-loop arrival process would see. The
// connection sweep is what exposes that: it is one server measured at four
// different offered loads.
//
// Latency is measured from the moment a batch is flushed to the moment each of
// its replies is decoded. At pipeline 1 that is the round trip. At pipeline 100
// it includes the queueing behind the other 99, which is the honest number: a
// pipelined client really does wait that long for its hundredth answer.

// loadSpec is one measured cell of the sweep.
type loadSpec struct {
	name     string
	conns    int
	pipeline int
	duration time.Duration
	warmup   time.Duration

	// requests is the pre-encoded request table, shared read-only by every
	// worker. Encoding before the clock starts is what keeps the generator's
	// hot loop free of allocation.
	requests [][]byte
	// exhaust sends every request in the table exactly once, split across the
	// workers, instead of cycling for a fixed duration. DEL uses it: a delete
	// benchmark that cycled would be measuring misses after the first pass.
	exhaust bool

	addr    string
	tlsConf *tls.Config
}

// loadResult is what one cell produced.
type loadResult struct {
	spec    loadSpec
	ops     uint64
	elapsed time.Duration
	lat     *hist
	// ones counts `:1` integer replies, which is how a DEL run reports how many
	// of its deletes actually found a key.
	ones uint64
	// clientCores is CPU-seconds the generator burned per second of wall time.
	// If it approaches the core count, the generator is the thing being
	// measured and the server number below it is meaningless.
	clientCores float64
	serverCores float64
}

func (r loadResult) opsPerSec() float64 {
	if r.elapsed <= 0 {
		return 0
	}
	return float64(r.ops) / r.elapsed.Seconds()
}

// dial opens one connection, plaintext or TLS, with Nagle's algorithm off.
//
// TCP_NODELAY is not a thumb on the scale: every real Redis client sets it, and
// leaving it on would measure the delayed-ACK interaction rather than the
// server.
func dial(ctx context.Context, spec loadSpec) (net.Conn, error) {
	d := &net.Dialer{Timeout: 10 * time.Second}
	if spec.tlsConf != nil {
		td := &tls.Dialer{NetDialer: d, Config: spec.tlsConf}
		return td.DialContext(ctx, "tcp", spec.addr)
	}
	conn, err := d.DialContext(ctx, "tcp", spec.addr)
	if err != nil {
		return nil, err
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		if err := tcp.SetNoDelay(true); err != nil {
			return nil, err
		}
	}
	return conn, nil
}

// share is the slice of the request table one worker owns: a starting offset,
// and how many requests it sends before stopping (0 means "never stop, wrap").
type share struct {
	off int
	n   int
}

type worker struct {
	conn net.Conn
	r    *bufio.Reader
	w    *bufio.Writer
	lat  *hist
	ops  uint64
	ones uint64
	err  error
}

func (w *worker) close() {
	if w.conn != nil {
		_ = w.conn.Close()
	}
}

// cpuSampler is what runLoad reads CPU time from; the server process
// implements it, and the null-server control passes a stub.
type cpuSampler interface {
	cpuSeconds() (float64, bool)
}

// runLoad executes one cell and returns its result.
func runLoad(ctx context.Context, spec loadSpec, cpu cpuSampler) (loadResult, error) {
	workers, err := connectAll(ctx, spec)
	defer func() {
		for _, w := range workers {
			w.close()
		}
	}()
	if err != nil {
		return loadResult{}, err
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	var measuring atomic.Bool
	shares := partition(spec, len(workers))

	for i, w := range workers {
		wg.Add(1)
		go func(w *worker, s share) {
			defer wg.Done()
			<-start
			w.err = w.pump(spec, s, &measuring)
		}(w, shares[i])
	}

	res := drive(spec, cpu, start, &measuring, &wg)

	for _, w := range workers {
		if w.err != nil {
			return loadResult{}, fmt.Errorf("%s: %w", spec.name, w.err)
		}
		res.ops += w.ops
		res.ones += w.ones
		res.lat.merge(w.lat)
	}
	// The request table is dropped rather than carried into the result: the
	// sweep keeps every result for the final report, and a 64KB table is a
	// quarter of a gigabyte.
	res.spec = spec
	res.spec.requests = nil
	return res, nil
}

// drive opens and closes the measurement window around the running workers.
func drive(spec loadSpec, cpu cpuSampler, start chan struct{}, measuring *atomic.Bool, wg *sync.WaitGroup) loadResult {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	if spec.exhaust {
		clientCPU0, serverCPU0, serverOK := sampleCPU(cpu)
		t0 := time.Now()
		measuring.Store(true)
		close(start)
		<-done
		return finish(t0, clientCPU0, serverCPU0, serverOK, cpu)
	}

	close(start)
	select {
	case <-done:
	case <-time.After(spec.warmup):
	}

	clientCPU0, serverCPU0, serverOK := sampleCPU(cpu)
	t0 := time.Now()
	measuring.Store(true)
	select {
	case <-done:
	case <-time.After(spec.duration):
	}
	measuring.Store(false)
	res := finish(t0, clientCPU0, serverCPU0, serverOK, cpu)
	<-done
	return res
}

func sampleCPU(cpu cpuSampler) (client, server float64, ok bool) {
	server, ok = cpu.cpuSeconds()
	return selfCPU(), server, ok
}

func finish(t0 time.Time, clientCPU0, serverCPU0 float64, serverOK bool, cpu cpuSampler) loadResult {
	elapsed := time.Since(t0)
	res := loadResult{elapsed: elapsed, lat: newHist()}
	if elapsed <= 0 {
		return res
	}
	res.clientCores = (selfCPU() - clientCPU0) / elapsed.Seconds()
	if serverCPU1, ok := cpu.cpuSeconds(); ok && serverOK {
		res.serverCores = (serverCPU1 - serverCPU0) / elapsed.Seconds()
	}
	return res
}

func connectAll(ctx context.Context, spec loadSpec) ([]*worker, error) {
	workers := make([]*worker, 0, spec.conns)
	for i := 0; i < spec.conns; i++ {
		conn, err := dial(ctx, spec)
		if err != nil {
			return workers, fmt.Errorf("%s: connection %d of %d: %w", spec.name, i+1, spec.conns, err)
		}
		workers = append(workers, &worker{
			conn: conn,
			r:    bufio.NewReaderSize(conn, readBufSize),
			w:    bufio.NewWriterSize(conn, readBufSize),
			lat:  newHist(),
		})
	}
	return workers, nil
}

// partition decides which requests each worker sends.
//
// Cycling workers all share the whole table but start at different offsets, so
// 200 connections do not march over the keyspace in lockstep and produce a
// cache-locality artifact. Exhausting workers get disjoint runs, so every
// request is sent exactly once.
func partition(spec loadSpec, n int) []share {
	out := make([]share, n)
	if n == 0 {
		return out
	}
	if !spec.exhaust {
		stride := len(spec.requests) / n
		for i := range out {
			out[i] = share{off: stride * i}
		}
		return out
	}
	per := (len(spec.requests) + n - 1) / n
	for i := range out {
		lo := min(i*per, len(spec.requests))
		out[i] = share{off: lo, n: min(per, len(spec.requests)-lo)}
	}
	return out
}

// pump is the hot loop: fill a pipeline, flush it, read its replies, timing
// each one from the flush.
func (w *worker) pump(spec loadSpec, s share, measuring *atomic.Bool) error {
	if len(spec.requests) == 0 {
		return nil
	}
	pos, started := 0, false
	for {
		if spec.exhaust {
			if pos >= s.n {
				return nil
			}
		} else if started && !measuring.Load() {
			return nil
		}

		batch := spec.pipeline
		if spec.exhaust && pos+batch > s.n {
			batch = s.n - pos
		}

		for i := 0; i < batch; i++ {
			if _, err := w.w.Write(spec.requests[(s.off+pos+i)%len(spec.requests)]); err != nil {
				return err
			}
		}
		if err := w.w.Flush(); err != nil {
			return err
		}
		sent := time.Now()

		counting := spec.exhaust || measuring.Load()
		started = started || counting
		if err := w.drain(batch, sent, counting); err != nil {
			return err
		}
		pos += batch
	}
}

func (w *worker) drain(batch int, sent time.Time, counting bool) error {
	for i := 0; i < batch; i++ {
		rep, err := readReply(w.r)
		if err != nil {
			return err
		}
		if !counting {
			continue
		}
		w.lat.record(uint64(time.Since(sent)))
		w.ops++
		if rep.kind == ':' && rep.n == 1 {
			w.ones++
		}
	}
	return nil
}
