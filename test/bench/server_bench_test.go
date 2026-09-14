package netbench

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The server under test is a spawned process, not an in-process library.
//
// That is the whole point of FEAT-0026: P1 measured the engine with the network
// removed, and the number this feature exists to produce is what a client sees
// once the socket, the protocol and the scheduler are all back in the path.
// Linking the engine into the benchmark binary would quietly delete two of
// those three.

const (
	serverReadyTimeout = 15 * time.Second
	serverStopTimeout  = 10 * time.Second
)

// serverConfig is the handful of settings a run needs to vary. Everything else
// stays at the shipped default, because a benchmark of a tuned configuration
// nobody runs is not a benchmark of the product.
type serverConfig struct {
	tlsOn    bool
	maxConns int
}

type serverProc struct {
	cmd       *exec.Cmd
	dir       string
	addr      string // host:port a client dials
	port      int
	adminAddr string
	certPEM   []byte
	logPath   string
}

// startServer launches one atlascache process and waits for GET /health.
func startServer(binary string, cfg serverConfig) (*serverProc, error) {
	// Absolute, because the child runs with its working directory set to its
	// own temp directory and a relative path would resolve against that.
	binary, err := filepath.Abs(binary)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "atlas-netbench-")
	if err != nil {
		return nil, err
	}

	srv := &serverProc{dir: dir, logPath: filepath.Join(dir, "server.log")}
	if err := srv.launch(binary, cfg); err != nil {
		srv.Stop()
		return nil, err
	}
	return srv, nil
}

func (s *serverProc) launch(binary string, cfg serverConfig) error {
	ports, err := freePorts(2)
	if err != nil {
		return err
	}
	s.port, s.addr = ports[0], net.JoinHostPort("127.0.0.1", strconv.Itoa(ports[0]))
	s.adminAddr = net.JoinHostPort("127.0.0.1", strconv.Itoa(ports[1]))

	certFile, keyFile := "", ""
	if cfg.tlsOn {
		certFile, keyFile, err = s.writeCert()
		if err != nil {
			return err
		}
	}

	cfgPath := filepath.Join(s.dir, "config.yaml")
	body := renderConfig(s.dir, ports[0], ports[1], cfg, certFile, keyFile)
	if err = os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		return err
	}

	logFile, err := os.Create(s.logPath)
	if err != nil {
		return err
	}
	defer func() { _ = logFile.Close() }()

	// The server outlives the call that started it -- Stop owns its lifetime --
	// so it is deliberately not a CommandContext.
	cmd := exec.Command(binary, "--config", cfgPath) //nolint:noctx // the binary under test, with a lifetime this file manages
	cmd.Dir = s.dir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("launching %s: %w", binary, err)
	}
	s.cmd = cmd

	return s.waitReady()
}

// renderConfig is the configuration every run uses.
//
// bind_addr is 0.0.0.0 rather than loopback so that the redis-benchmark
// container can reach the same process over host.docker.internal; max_memory
// stays unlimited so that eviction, which P1 already measured, is not folded
// into a network number.
func renderConfig(dir string, clientPort, adminPort int, cfg serverConfig, certFile, keyFile string) string {
	maxConns := cfg.maxConns
	if maxConns <= 0 {
		maxConns = 10000
	}
	return fmt.Sprintf(`node:
  id: "netbench"
  name: "netbench"
  data_dir: %q
server:
  bind_addr: "0.0.0.0"
  client_port: %d
  max_connections: %d
admin:
  bind_addr: "127.0.0.1"
  port: %d
storage:
  shard_count: 0
  max_memory: "0"
  max_value_size: "1MB"
eviction:
  policy: "lru"
tls:
  enabled: %t
  cert_file: %q
  key_file: %q
logging:
  level: "info"
  format: "json"
`, filepath.Join(dir, "data"), clientPort, maxConns, adminPort, cfg.tlsOn, certFile, keyFile)
}

func (s *serverProc) waitReady() error {
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	deadline := time.Now().Add(serverReadyTimeout)
	url := "http://" + s.adminAddr + "/health"
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	logs, _ := os.ReadFile(s.logPath)
	return fmt.Errorf("server did not become ready within %s; log:\n%s", serverReadyTimeout, logs)
}

// Stop terminates the server and removes its directory.
func (s *serverProc) Stop() {
	if s == nil {
		return
	}
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _, _ = s.cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(serverStopTimeout):
			_ = s.cmd.Process.Kill()
		}
		s.cmd = nil
	}
	if s.dir != "" {
		_ = os.RemoveAll(s.dir)
		s.dir = ""
	}
}

// cpuSeconds reports the server process's cumulative CPU time.
//
// It shells out to ps because the server is a separate process and Go only
// surfaces a child's rusage once it has exited, which is too late to bracket a
// measurement window. The resolution is 10ms against windows of seconds.
func (s *serverProc) cpuSeconds() (float64, bool) {
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return 0, false
	}
	return cpuOf(s.cmd.Process.Pid)
}

// cpuOf reads one process's cumulative CPU time from ps.
func cpuOf(pid int) (float64, bool) {
	out, err := exec.Command("ps", "-o", "cputime=", "-p", strconv.Itoa(pid)).Output() //nolint:noctx // bounded, local, and run between measurement windows
	if err != nil {
		return 0, false
	}
	return parseCPUTime(strings.TrimSpace(string(out)))
}

// parseCPUTime reads ps's [[hh:]mm:]ss.ff cumulative CPU column.
func parseCPUTime(s string) (float64, bool) {
	if s == "" {
		return 0, false
	}
	parts := strings.Split(s, ":")
	total := 0.0
	for _, p := range parts {
		v, err := strconv.ParseFloat(p, 64)
		if err != nil {
			return 0, false
		}
		total = total*60 + v
	}
	return total, true
}

// selfCPU is the generator's own CPU time, user plus system.
func selfCPU() float64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return timevalSeconds(ru.Utime) + timevalSeconds(ru.Stime)
}

func timevalSeconds(tv syscall.Timeval) float64 {
	return float64(tv.Sec) + float64(tv.Usec)/1e6
}

// clientTLS trusts exactly the certificate this run generated.
func (s *serverProc) clientTLS() (*tls.Config, error) {
	if len(s.certPEM) == 0 {
		return nil, errors.New("this server was not started with TLS")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(s.certPEM) {
		return nil, errors.New("the generated certificate did not parse")
	}
	return &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS13}, nil
}

// writeCert generates the self-signed P-256 certificate the TLS runs use. It is
// generated per run into a temp directory for the reason FEAT-0023 gives: a
// committed test certificate eventually expires, or ends up in production.
func (s *serverProc) writeCert() (certFile, keyFile string, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", "", err
	}
	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "localhost", Organization: []string{"AtlasCache netbench"}},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return "", "", err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", "", err
	}

	s.certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	certFile = filepath.Join(s.dir, "server.crt")
	keyFile = filepath.Join(s.dir, "server.key")
	if err := os.WriteFile(certFile, s.certPEM, 0o600); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		return "", "", err
	}
	return certFile, keyFile, nil
}

// freePorts reserves n ports by binding and releasing them. The window between
// release and the server's bind is a race, which is why a failed launch is
// retried with fresh ports rather than reported.
func freePorts(n int) ([]int, error) {
	ports := make([]int, 0, n)
	listeners := make([]net.Listener, 0, n)
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()
	var lc net.ListenConfig
	for i := 0; i < n; i++ {
		l, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		listeners = append(listeners, l)
		addr, ok := l.Addr().(*net.TCPAddr)
		if !ok {
			return nil, errors.New("listener address was not TCP")
		}
		ports = append(ports, addr.Port)
	}
	return ports, nil
}
