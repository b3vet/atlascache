package netbench

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"testing"
)

// TestNetworkProbe runs a single cell against a server that is already running,
// anywhere.
//
// It exists because a sweep that only ever points at its own server cannot
// answer the question the sweep depends on -- is the generator the bottleneck?
// The probe is how the generator gets pointed at a do-nothing server, at a real
// Redis, and at AtlasCache from two processes at once, which between them are
// what settle that question.
//
//	go test ./test/bench -run TestNetworkProbe -netbench.probe \
//	    -netbench.addr 127.0.0.1:7379 -netbench.conns 50 -netbench.pipe 1
var (
	flagProbe    = flag.Bool("netbench.probe", false, "run one cell against -netbench.addr")
	flagAddr     = flag.String("netbench.addr", "127.0.0.1:6379", "server to probe")
	flagConns    = flag.Int("netbench.conns", 50, "connections")
	flagPipe     = flag.Int("netbench.pipe", 1, "pipeline depth")
	flagWorkload = flag.String("netbench.workload", "get", "get, set, ping or mixed")
	flagValue    = flag.Int("netbench.value", 64, "value size in bytes")
	flagKeyspace = flag.Int("netbench.keyspace", 100000, "distinct keys")
	flagPreload  = flag.Bool("netbench.preload", true, "write the keyspace before measuring")
	flagInsecure = flag.Bool("netbench.tls", false, "dial TLS, trusting any certificate")
	flagLabel    = flag.String("netbench.label", "probe", "label for the printed row")
)

func TestNetworkProbe(t *testing.T) {
	if !*flagProbe {
		t.Skip("pass -netbench.probe to run one cell against -netbench.addr")
	}

	base := loadSpec{
		conns:    *flagConns,
		pipeline: *flagPipe,
		duration: *flagDuration,
		warmup:   *flagWarmup,
		addr:     *flagAddr,
	}
	if *flagInsecure {
		// Trusting any certificate is correct here: the probe measures the
		// cost of TLS, and chain validation is not part of the steady state.
		base.tlsConf = &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}
	}

	if *flagPreload && *flagWorkload != "ping" {
		pre := base
		pre.name = "preload"
		pre.conns, pre.pipeline = preloadConns, preloadPipeline
		pre.requests = setTable(*flagKeyspace, *flagValue)
		pre.exhaust = true
		if _, err := runLoad(context.Background(), pre, noCPU{}); err != nil {
			t.Fatalf("preload: %v", err)
		}
	}

	spec := base
	spec.name = *flagLabel
	spec.requests = probeTable(t, *flagWorkload, *flagKeyspace, *flagValue)

	res, err := runLoad(context.Background(), spec, noCPU{})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	h := res.lat
	fmt.Printf("PROBE %s workload=%s addr=%s conns=%d pipe=%d value=%d ops/s=%.0f p50=%s p95=%s p99=%s p99.9=%s max=%s client_cores=%.2f\n",
		*flagLabel, *flagWorkload, *flagAddr, *flagConns, *flagPipe, *flagValue,
		res.opsPerSec(), dur(h.quantile(0.5)), dur(h.quantile(0.95)), dur(h.quantile(0.99)),
		dur(h.quantile(0.999)), dur(h.max), res.clientCores)
}

func probeTable(t *testing.T, workload string, keyspace, valueSize int) [][]byte {
	switch workload {
	case "get":
		return getTable(keyspace)
	case "set":
		return setTable(keyspace, valueSize)
	case "ping":
		return pingTable(4096)
	case "mixed":
		return mixedTable(keyspace, valueSize)
	default:
		t.Fatalf("unknown workload %q", workload)
		return nil
	}
}

// noCPU is the cpuSampler for a server this process did not start.
type noCPU struct{}

func (noCPU) cpuSeconds() (float64, bool) { return 0, false }
