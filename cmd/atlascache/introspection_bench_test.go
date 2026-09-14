package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/b3vet/atlascache/internal/server"
	"github.com/b3vet/atlascache/internal/storage"
)

// Introspection benchmarks (FEAT-0021, ISSUE-0012, ISSUE-0015)
//
// Two properties are being measured, and both are properties the plan says are
// load-bearing rather than nice to have:
//
//   - INFO, DBSIZE and STATS cost the same at a thousand keys and at a million.
//     Anything that walks the keyspace to answer them reintroduces ISSUE-0012
//     through a different door, and the door P2 opened is a monitoring
//     dashboard polling INFO every second.
//   - None of them calls runtime.ReadMemStats, which stops the world
//     (ISSUE-0015). A profile of these benchmarks must not contain it; the
//     source-level guarantee is asserted in internal/storage's
//     TestReadMemStatsIsNotOnARequestPath.
//
// They run over the wire against a real engine, because the cost being claimed
// is the cost a client sees, not the cost of the handler in isolation.
//
//	go test ./cmd/atlascache -bench Introspection -benchmem -run '^$'
//	go test ./cmd/atlascache -bench Introspection -run '^$' -cpuprofile cpu.out
//	go tool pprof -top -nodecount=40 cpu.out | grep -i readmemstats   # expect nothing

// benchKeyCounts are the two sizes the comparison needs: small enough that a
// walk would be invisible, and large enough that it could not be.
var benchKeyCounts = []int{1_000, 1_000_000}

func BenchmarkIntrospection(b *testing.B) {
	for _, keys := range benchKeyCounts {
		conn, reader, cleanup := benchServer(b, keys)

		for _, cmd := range []string{"INFO", "DBSIZE", "STATS", "INFO memory"} {
			name := fmt.Sprintf("%s/%s", strings.ReplaceAll(cmd, " ", "_"), keyCountLabel(keys))
			b.Run(name, func(b *testing.B) {
				request := encodeCommand(strings.Fields(cmd))

				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := conn.Write(request); err != nil {
						b.Fatal(err)
					}
					if err := drainReply(reader); err != nil {
						b.Fatal(err)
					}
				}
			})
		}

		cleanup()
	}
}

// BenchmarkKeyspaceStats measures the accounting call itself, with no socket in
// the way.
//
// The wire benchmark above is the number a client sees and carries a TCP round
// trip's worth of noise; this one is the number ISSUE-0012 and ISSUE-0015 are
// about. It was ~22µs when runtime.ReadMemStats was on the path, essentially
// all of it the stop-the-world, and flat in key count either way — which was
// exactly the problem: flat and still far too expensive to poll.
func BenchmarkKeyspaceStats(b *testing.B) {
	for _, keys := range benchKeyCounts {
		b.Run(keyCountLabel(keys), func(b *testing.B) {
			engine := storage.NewShardedEngine(storage.EngineConfig{ShardCount: 64, MaxValueSize: 1024})
			defer engine.Close()

			value := []byte("v")
			for i := 0; i < keys; i++ {
				if err := engine.Set([]byte("bench:key:"+strconv.Itoa(i)), value, 0); err != nil {
					b.Fatal(err)
				}
			}

			procmem := storage.NewProcessMemorySampler(time.Hour)
			procmem.Start()
			defer procmem.Stop()

			store := keyspace{engine: engine, procmem: procmem}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if store.Stats().Keys == 0 {
					b.Fatal("no keys")
				}
			}
		})
	}
}

// benchServer starts a real server over a real engine holding keys entries, and
// returns one connected client.
func benchServer(b *testing.B, keys int) (net.Conn, *bufio.Reader, func()) {
	b.Helper()

	engine := storage.NewShardedEngine(storage.EngineConfig{
		ShardCount:   64,
		MaxValueSize: 1024,
	})

	value := []byte("v")
	for i := 0; i < keys; i++ {
		if err := engine.Set([]byte("bench:key:"+strconv.Itoa(i)), value, 0); err != nil {
			b.Fatal(err)
		}
	}

	procmem := storage.NewProcessMemorySampler(time.Hour)
	procmem.Start()

	ctx, cancel := context.WithCancel(context.Background())
	srv, err := server.New(ctx, "127.0.0.1:0", zerolog.Nop(), keyspace{engine: engine, procmem: procmem})
	if err != nil {
		b.Fatal(err)
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve() }()

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", srv.Addr())
	if err != nil {
		b.Fatal(err)
	}

	return conn, bufio.NewReader(conn), func() {
		_ = conn.Close()
		_ = srv.Shutdown(context.Background())
		<-serveErr
		cancel()
		procmem.Stop()
		_ = engine.Close()
	}
}

func keyCountLabel(keys int) string {
	if keys >= 1_000_000 {
		return strconv.Itoa(keys/1_000_000) + "M_keys"
	}
	return strconv.Itoa(keys/1_000) + "K_keys"
}

func encodeCommand(args []string) []byte {
	var req strings.Builder
	fmt.Fprintf(&req, "*%d\r\n", len(args))
	for _, arg := range args {
		fmt.Fprintf(&req, "$%d\r\n%s\r\n", len(arg), arg)
	}
	return []byte(req.String())
}

// drainReply reads one whole RESP reply and throws it away. Only the frames the
// introspection commands answer with are understood.
func drainReply(r *bufio.Reader) error {
	line, err := r.ReadString('\n')
	if err != nil {
		return err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return fmt.Errorf("empty reply frame")
	}

	switch line[0] {
	case '+', ':', '-':
		return nil
	case '$':
		size, convErr := strconv.Atoi(line[1:])
		if convErr != nil || size < 0 {
			return convErr
		}
		_, err = r.Discard(size + 2)
		return err
	case '*':
		count, convErr := strconv.Atoi(line[1:])
		if convErr != nil {
			return convErr
		}
		for i := 0; i < count; i++ {
			if err := drainReply(r); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("unexpected reply frame %q", line)
}
