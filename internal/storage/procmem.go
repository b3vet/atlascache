package storage

import (
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// Process-level memory, off the request path (ISSUE-0015)
//
// runtime.ReadMemStats stops the world. It used to be called from
// MemoryTracker.Stats, which meant every STATS, every INFO and every admin
// /stats request paused the whole process for the length of a heap scan. That
// was tolerable while nothing but a test called it; it stopped being tolerable
// the moment P2 put STATS and INFO on the wire, where a dashboard polls them
// every second and a Prometheus scraper every fifteen.
//
// The fix is not to make the call cheaper — it cannot be made cheaper — but to
// move it off every path a client can reach. A sampler owns the call, runs it on
// its own timer, and publishes the result; readers take a pointer load. The
// numbers are then up to one interval stale, which is the correct trade: nobody
// reading heap_inuse on a dashboard needs it accurate to the millisecond, and
// everybody reading it needs their p99 not to move because they read it.

// DefaultProcessMemoryInterval is how often the sampler reads the runtime when
// no interval is configured. It is deliberately slower than a scrape: sampling
// faster than anyone reads only pays the stop-the-world cost more often.
const DefaultProcessMemoryInterval = 10 * time.Second

// ProcessMemory is what the Go runtime reports about the process, as of
// SampledAt. It is not cache accounting and must not be confused with it: these
// figures include the heap the server needs to serve requests, the garbage not
// yet collected, and everything else in the process.
type ProcessMemory struct {
	HeapAlloc   uint64
	HeapSys     uint64
	HeapInuse   uint64
	HeapObjects uint64
	StackInuse  uint64
	Sys         uint64
	NumGC       uint32
	Goroutines  int

	// SampledAt is when the reading was taken, so a consumer can tell a stale
	// sample from a fresh one and a never-sampled zero value from a real zero.
	SampledAt time.Time
}

// ProcessMemorySampler reads the Go runtime on a timer and publishes the last
// reading for anyone who asks.
//
// The zero value is not usable; call NewProcessMemorySampler. A sampler that was
// never started answers with a zero ProcessMemory rather than sampling on
// demand — sampling on demand is the behavior this type exists to prevent, and
// a lazily-refreshing cache would put ReadMemStats back on the first request
// after every interval.
type ProcessMemorySampler struct {
	interval time.Duration

	// latest is swapped whole, so a reader never sees a half-written sample and
	// never blocks the sampler.
	latest atomic.Pointer[ProcessMemory]

	startOnce sync.Once
	stopOnce  sync.Once
	started   atomic.Bool
	stop      chan struct{}
	done      chan struct{}
}

// NewProcessMemorySampler returns a sampler that reads the runtime every
// interval. A non-positive interval means DefaultProcessMemoryInterval.
func NewProcessMemorySampler(interval time.Duration) *ProcessMemorySampler {
	if interval <= 0 {
		interval = DefaultProcessMemoryInterval
	}
	return &ProcessMemorySampler{
		interval: interval,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Start takes the first reading and launches the loop that refreshes it.
//
// The first reading is taken synchronously, on the caller's goroutine, so that
// INFO answers with real numbers from the first request rather than with zeros
// until the first tick. That one call is on the startup path, which is not a
// path a client can trigger.
//
// Calling Start more than once is a no-op.
func (p *ProcessMemorySampler) Start() {
	p.startOnce.Do(func() {
		p.sample()
		p.started.Store(true)
		go p.loop()
	})
}

// Stop halts the sampler and waits for its goroutine to finish. The last
// reading stays readable afterwards. Calling it more than once, or without
// Start, is a no-op.
func (p *ProcessMemorySampler) Stop() {
	p.stopOnce.Do(func() {
		close(p.stop)
	})

	// A sampler that was never started has no goroutine to wait for, and
	// blocking on one would hang the caller.
	if p.started.Load() {
		<-p.done
	}
}

// Sample returns the most recent reading. It is one atomic load: safe to call
// from a request handler, from several at once, and as often as anyone likes.
func (p *ProcessMemorySampler) Sample() ProcessMemory {
	if latest := p.latest.Load(); latest != nil {
		return *latest
	}
	return ProcessMemory{}
}

func (p *ProcessMemorySampler) loop() {
	defer close(p.done)

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
			p.sample()
		}
	}
}

// sample is the only place in the package that reads the Go runtime. Keeping it
// to one function is what makes "ReadMemStats is not on a request path" a claim
// a reader can check rather than take on trust.
func (p *ProcessMemorySampler) sample() {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	p.latest.Store(&ProcessMemory{
		HeapAlloc:   mem.HeapAlloc,
		HeapSys:     mem.HeapSys,
		HeapInuse:   mem.HeapInuse,
		HeapObjects: mem.HeapObjects,
		StackInuse:  mem.StackInuse,
		Sys:         mem.Sys,
		NumGC:       mem.NumGC,
		Goroutines:  runtime.NumGoroutine(),
		SampledAt:   time.Now(),
	})
}
