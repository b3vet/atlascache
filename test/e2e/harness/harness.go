// Package harness runs one real AtlasCache server process per spec.
//
// The server under test is a spawned subprocess, not an in-process library
// ([[ADR-0003]]): a real OS process means SIGKILL works, which the crash
// recovery specs need, and it exercises binding, signal handling and startup
// for real rather than in a mock of them.
//
// Everything one spec touches is private to it — its ports, its data directory,
// its config file — so specs run in parallel without collision, and cleanup is
// unconditional so a crashed run leaves nothing behind holding a port.
package harness

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/b3vet/atlascache/test/e2e/client"
	"github.com/b3vet/atlascache/test/e2e/runner"
)

// Defaults for the timings a harness runs to. Every one of them is a bound on
// something that would otherwise hang a run.
const (
	// DefaultReadyTimeout is how long Start waits for GET /health to answer 200.
	DefaultReadyTimeout = 10 * time.Second
	// DefaultGracefulWindow is how long Stop gives the server to exit after
	// SIGTERM before killing it and failing the spec.
	DefaultGracefulWindow = 10 * time.Second
	// DefaultStartAttempts is how many times Start relaunches with fresh ports
	// when the server reports its port as taken.
	DefaultStartAttempts = 5

	readyPollInterval = 20 * time.Millisecond
	healthTimeout     = 2 * time.Second
	killWait          = 5 * time.Second
	runBinaryTimeout  = 20 * time.Second
)

// errPortTaken means the server lost the race between the harness releasing a
// probe listener and the server binding the port. It is retried, not reported.
var errPortTaken = errors.New("the port was taken before the server could bind it")

// Settings tune every harness a Factory hands out. The zero value is the
// documented default for each field.
type Settings struct {
	ReadyTimeout   time.Duration
	GracefulWindow time.Duration
	StartAttempts  int
	// BaseDir is where per-spec temp directories are created. Empty means the
	// system temp directory.
	BaseDir string
}

func (s Settings) readyTimeout() time.Duration {
	if s.ReadyTimeout <= 0 {
		return DefaultReadyTimeout
	}
	return s.ReadyTimeout
}

func (s Settings) gracefulWindow() time.Duration {
	if s.GracefulWindow <= 0 {
		return DefaultGracefulWindow
	}
	return s.GracefulWindow
}

func (s Settings) startAttempts() int {
	if s.StartAttempts <= 0 {
		return DefaultStartAttempts
	}
	return s.StartAttempts
}

// Factory returns a runner.HarnessFactory building one Process per spec.
func Factory(settings Settings) runner.HarnessFactory {
	return func(opts runner.HarnessOptions) (runner.Harness, error) {
		return New(opts, settings)
	}
}

// Process implements the contract the runner drives specs through.
var _ runner.Harness = (*Process)(nil)

// Process is a runner.Harness driving one server subprocess.
type Process struct {
	settings Settings
	specName string
	binary   string
	config   map[string]any

	root    string // the harness's private temp directory
	dataDir string // preserved across Restart; removed by Close
	cfgPath string

	logs   *logBuffer
	health *http.Client

	mu         sync.Mutex
	current    *proc
	conn       *client.Client
	clientAddr string
	adminAddr  string
	closed     bool
}

// proc is one launch of the server. A new one is built by every Start, so a
// restarted server never inherits the previous one's state.
type proc struct {
	cmd    *exec.Cmd
	pid    int
	ports  []int
	exited chan struct{}
	// waitErr is written before exited is closed, so reading it after the
	// channel closes needs no further synchronization.
	waitErr error
}

// New builds a harness for one spec. It creates the spec's private directory
// but launches nothing; Close removes the directory whether Start was ever
// called or not.
func New(opts runner.HarnessOptions, settings Settings) (*Process, error) {
	if opts.SpecName == "" {
		return nil, errors.New("a harness needs the spec name, for its temp directory and node id")
	}

	binary, err := resolveBinary(opts.Binary)
	if err != nil {
		return nil, err
	}

	root, err := os.MkdirTemp(settings.BaseDir, "atlas-e2e-"+sanitize(opts.SpecName)+"-")
	if err != nil {
		return nil, fmt.Errorf("creating the spec's temp directory: %w", err)
	}
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		_ = os.RemoveAll(root)
		return nil, fmt.Errorf("creating the data directory: %w", err)
	}

	return &Process{
		settings: settings,
		specName: opts.SpecName,
		binary:   binary,
		config:   opts.Config,
		root:     root,
		dataDir:  dataDir,
		cfgPath:  filepath.Join(root, "config.yaml"),
		logs:     &logBuffer{},
		health: &http.Client{
			Timeout: healthTimeout,
			// Keep-alives would hold a connection open across Stop and make the
			// admin server's graceful shutdown wait on the test client.
			Transport: &http.Transport{DisableKeepAlives: true},
		},
	}, nil
}

// resolveBinary checks the server binary exists and is executable before any
// spec runs, so a forgotten `make build` reports that rather than a launch
// failure per spec.
func resolveBinary(path string) (string, error) {
	if path == "" {
		return "", errors.New("no server binary was given; pass --binary")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolving the server binary %s: %w", path, err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("server binary %s: %w (run `make build`)", absolute, err)
	}
	if info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("server binary %s is not an executable file", absolute)
	}
	return absolute, nil
}

// Start launches the server and returns once GET /health answers 200.
//
// Readiness is that positive signal and never a sleep: a sleep is either slow
// or flaky, and usually both. A launch that dies because its port was taken in
// the gap between probing the port and binding it is retried with fresh ports.
// Starting an already-running server is a no-op.
func (p *Process) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return errors.New("the harness is closed")
	}
	if p.current != nil {
		return nil
	}
	return p.startLocked(ctx)
}

// startLocked launches the server, retrying with fresh ports when the launch
// lost the race for one. The caller holds the lock.
func (p *Process) startLocked(ctx context.Context) error {
	attempts := p.settings.startAttempts()
	for attempt := 1; ; attempt++ {
		err := p.startOnce(ctx)
		if err == nil {
			return nil
		}
		if !errors.Is(err, errPortTaken) || attempt >= attempts {
			return err
		}
		p.logs.Printf("attempt %d lost the race for its port; retrying with fresh ports", attempt)
	}
}

func (p *Process) startOnce(ctx context.Context) error {
	ports, err := freePorts(2)
	if err != nil {
		return err
	}
	clientPort, adminPort := ports[0], ports[1]

	if err := writeConfig(p.cfgPath, p.dataDir, p.specName, clientPort, adminPort, p.config); err != nil {
		release(ports...)
		return err
	}

	clientAddr := net.JoinHostPort(loopback, strconv.Itoa(clientPort))
	adminAddr := net.JoinHostPort(loopback, strconv.Itoa(adminPort))

	// The server outlives the context of the call that started it — Stop, Kill
	// and Close own its lifetime — so it is deliberately not a CommandContext.
	cmd := exec.Command(p.binary, "--config", p.cfgPath) //nolint:gosec,noctx // the binary under test, with a lifetime the harness manages
	cmd.Dir = p.root
	cmd.Stdout = p.logs
	cmd.Stderr = p.logs
	cmd.Env = childEnv()
	cmd.SysProcAttr = sysProcAttr()

	mark := p.logs.Offset()
	p.logs.Printf("launching %s --config %s (client %s, admin %s)", p.binary, p.cfgPath, clientAddr, adminAddr)

	if err := startWithRetry(cmd); err != nil {
		release(ports...)
		return fmt.Errorf("launching %s: %w", p.binary, err)
	}

	running := &proc{cmd: cmd, pid: cmd.Process.Pid, ports: ports, exited: make(chan struct{})}
	go func() {
		running.waitErr = cmd.Wait()
		close(running.exited)
	}()

	p.current = running
	p.clientAddr = clientAddr
	p.adminAddr = adminAddr

	if err := p.waitReady(ctx, running, adminAddr, mark); err != nil {
		if killErr := p.terminate(running); killErr != nil {
			p.logs.Printf("could not kill the server after a failed start: %v", killErr)
		}
		p.current = nil
		release(ports...)
		return err
	}

	p.logs.Printf("server ready (pid %d)", running.pid)
	return nil
}

// waitReady polls /health until it answers 200, the server dies, or the clock
// runs out. A death is noticed immediately rather than after the full timeout,
// because the common failure — a bad config — is instant.
func (p *Process) waitReady(ctx context.Context, running *proc, adminAddr string, mark int) error {
	deadline := time.Now().Add(p.settings.readyTimeout())
	ticker := time.NewTicker(readyPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-running.exited:
			return p.startupExit(running, mark)
		default:
		}

		if p.healthy(ctx, adminAddr) {
			return nil
		}

		select {
		case <-running.exited:
			return p.startupExit(running, mark)
		case <-ctx.Done():
			return fmt.Errorf("waiting for GET /health: %w", ctx.Err())
		case <-ticker.C:
			if time.Now().After(deadline) {
				return fmt.Errorf("GET http://%s/health did not answer 200 within %s",
					adminAddr, p.settings.readyTimeout())
			}
		}
	}
}

// healthy reports whether the admin health endpoint answers 200 right now.
func (p *Process) healthy(ctx context.Context, adminAddr string) bool {
	probeCtx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, "http://"+adminAddr+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := p.health.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == http.StatusOK
}

// startupExit explains a server that died before it was ready. A lost port race
// is reported as errPortTaken so Start can retry it.
func (p *Process) startupExit(running *proc, mark int) error {
	<-running.exited
	output := p.logs.Since(mark)
	if strings.Contains(output, "address already in use") {
		return fmt.Errorf("%w: %s", errPortTaken, describeExit(running.waitErr))
	}
	return fmt.Errorf("the server exited during startup: %s", describeExit(running.waitErr))
}

// Stop shuts the server down with SIGTERM and waits for it to exit within the
// graceful window. Stopping a server that is already down is a no-op, so a spec
// may end with the server killed.
func (p *Process) Stop(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stopLocked()
}

func (p *Process) stopLocked() error {
	running := p.current
	if running == nil {
		return nil
	}
	p.dropConn()

	window := p.settings.gracefulWindow()
	p.logs.Printf("sending SIGTERM to process group %d", running.pid)
	if err := signalGroup(running.pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("sending SIGTERM to process group %d: %w", running.pid, err)
	}

	timer := time.NewTimer(window)
	defer timer.Stop()

	select {
	case <-running.exited:
	case <-timer.C:
		killErr := p.terminate(running)
		p.finish(running)
		if killErr != nil {
			return fmt.Errorf("the server ignored SIGTERM for %s and could not be killed: %w", window, killErr)
		}
		return fmt.Errorf("the server did not exit within %s of SIGTERM and was killed", window)
	}

	p.finish(running)
	if err := exitError(running.waitErr); err != nil {
		return fmt.Errorf("the server exited badly after SIGTERM: %w", err)
	}
	p.logs.Printf("server exited cleanly")
	return nil
}

// Kill sends SIGKILL, for crash-recovery specs. Killing a dead server is a
// no-op. It waits only long enough to reap the process, so no zombie or orphan
// survives the call.
func (p *Process) Kill() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	running := p.current
	if running == nil {
		return nil
	}
	p.dropConn()
	p.logs.Printf("sending SIGKILL to process group %d", running.pid)
	err := p.terminate(running)
	p.finish(running)
	return err
}

// Restart stops the server and starts it again against the same data
// directory. The ports change; callers that need them re-read Info.
func (p *Process) Restart(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return errors.New("the harness is closed")
	}
	p.logs.Printf("restarting, preserving data directory %s", p.dataDir)
	if err := p.stopLocked(); err != nil {
		return fmt.Errorf("stopping before restart: %w", err)
	}
	if _, err := os.Stat(p.dataDir); err != nil {
		return fmt.Errorf("the data directory did not survive the stop: %w", err)
	}
	if err := p.startLocked(ctx); err != nil {
		return fmt.Errorf("starting after restart: %w", err)
	}
	return nil
}

// Send issues one command and returns the reply.
//
// The connection is opened on demand and reconnected transparently when the
// server closed it — after a QUIT, a kill, or a restart. An error reply is a
// value: it comes back as a Reply of kind KindError with a nil error, and the
// error return means the transport failed.
func (p *Process) Send(ctx context.Context, cmd string) (runner.Reply, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.current == nil {
		return runner.Reply{}, errors.New("the server is not running")
	}

	// One retry, and only on a connection that was already open: a command sent
	// down a fresh connection that then failed may well have been executed, and
	// resending it would turn a transport blip into a duplicated write.
	for attempt := 0; ; attempt++ {
		conn, reused, err := p.connect(ctx)
		if err != nil {
			return runner.Reply{}, err
		}

		reply, err := conn.Do(ctx, cmd)
		if err == nil {
			if closesConnection(cmd) {
				p.dropConn()
			}
			return reply, nil
		}

		p.dropConn()
		if attempt > 0 || !reused || !client.IsConnError(err) || ctx.Err() != nil {
			return runner.Reply{}, err
		}
		p.logs.Printf("reconnecting after %v", err)
	}
}

// connect returns the open connection, dialing one if there is none. reused
// reports whether the connection predates this call.
func (p *Process) connect(ctx context.Context) (*client.Client, bool, error) {
	if p.conn != nil {
		return p.conn, true, nil
	}
	conn, err := client.Dial(ctx, p.clientAddr)
	if err != nil {
		return nil, false, err
	}
	p.conn = conn
	return conn, false, nil
}

func (p *Process) dropConn() {
	if p.conn == nil {
		return
	}
	_ = p.conn.Close()
	p.conn = nil
}

// closesConnection reports whether the server is expected to hang up after this
// command. Dropping our side too keeps the next Send from spending a round trip
// discovering it.
func closesConnection(cmd string) bool {
	args, err := client.ParseCommand(cmd)
	if err != nil {
		return false
	}
	return strings.EqualFold(args[0], "QUIT")
}

// Info describes the server: where to reach it, where its data lives, and its
// pid. The pid is zero when nothing is running.
func (p *Process) Info() runner.ServerInfo {
	p.mu.Lock()
	defer p.mu.Unlock()

	info := runner.ServerInfo{
		ClientAddr: p.clientAddr,
		AdminAddr:  p.adminAddr,
		DataDir:    p.dataDir,
	}
	if p.current != nil {
		info.PID = p.current.pid
	}
	return info
}

// Logs returns everything captured from the server, plus the harness's own
// annotations. The runner reads it only on failure, so a passing run stays
// quiet and a failing one is diagnosable without a rerun.
func (p *Process) Logs() string { return p.logs.String() }

// Root is the harness's private directory. Scenarios that need a scratch file
// alongside the server write it here, so cleanup takes it with everything else.
func (p *Process) Root() string { return p.root }

// Binary is the server binary under test.
func (p *Process) Binary() string { return p.binary }

// RunBinary runs the server binary out of band and returns its exit code and
// combined output. It exists for the specs that must observe the binary itself
// rather than the protocol — an invalid config that must fail fast, and
// --version. The error return is reserved for a launch that never happened.
func (p *Process) RunBinary(ctx context.Context, args ...string) (int, string, error) {
	ctx, cancel := context.WithTimeout(ctx, runBinaryTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, p.binary, args...) //nolint:gosec // the binary is the one under test
	cmd.Dir = p.root
	cmd.Env = childEnv()
	cmd.SysProcAttr = sysProcAttr()

	invocation := filepath.Base(p.binary) + " " + strings.Join(args, " ")
	output, err := cmd.CombinedOutput()

	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		p.logs.Printf("ran %s -> %v", invocation, err)
		return -1, string(output), fmt.Errorf("running %s: %w", invocation, err)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		p.logs.Printf("ran %s -> %v", invocation, ctxErr)
		return -1, string(output), fmt.Errorf("running %s: %w", invocation, ctxErr)
	}

	code := cmd.ProcessState.ExitCode()
	p.logs.Printf("ran %s -> exit %d", invocation, code)
	return code, string(output), nil
}

// Close kills anything still running and removes the spec's directory. It is
// idempotent, and the runner calls it in a defer, so cleanup happens on a
// panic and on an interrupt as well as on the ordinary path.
func (p *Process) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil
	}
	p.closed = true
	p.dropConn()

	var problems []error
	if running := p.current; running != nil {
		p.logs.Printf("killing process group %d on cleanup", running.pid)
		if err := p.terminate(running); err != nil {
			problems = append(problems, err)
		}
		p.finish(running)
	}
	if err := os.RemoveAll(p.root); err != nil {
		problems = append(problems, fmt.Errorf("removing %s: %w", p.root, err))
	}
	return errors.Join(problems...)
}

// terminate SIGKILLs the process group and waits for the process to be reaped.
// The caller holds the lock.
func (p *Process) terminate(running *proc) error {
	select {
	case <-running.exited:
		return nil
	default:
	}

	if err := signalGroup(running.pid, syscall.SIGKILL); err != nil {
		return fmt.Errorf("killing process group %d: %w", running.pid, err)
	}

	timer := time.NewTimer(killWait)
	defer timer.Stop()
	select {
	case <-running.exited:
		return nil
	case <-timer.C:
		return fmt.Errorf("process group %d did not die within %s of SIGKILL", running.pid, killWait)
	}
}

// finish forgets a process that is no longer running and returns its ports.
// The caller holds the lock.
func (p *Process) finish(running *proc) {
	release(running.ports...)
	if p.current == running {
		p.current = nil
	}
}

// exitError turns a non-zero exit into an error and a clean one into nil.
func exitError(waitErr error) error {
	if waitErr == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		return errors.New(describeExit(waitErr))
	}
	return waitErr
}

// describeExit renders how a process ended, in the terms a reader diagnoses
// with: an exit code, or the signal that ended it.
func describeExit(waitErr error) string {
	if waitErr == nil {
		return "exit status 0"
	}
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		return waitErr.Error()
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if ok && status.Signaled() {
		return "killed by " + status.Signal().String()
	}
	return "exit status " + strconv.Itoa(exitErr.ExitCode())
}

// childEnv is the environment the server runs in: the caller's, minus anything
// that would reconfigure the server behind the spec's back. The server reads
// ATLAS_-prefixed variables as config overrides, so a developer's shell must
// not be able to change what a spec tests.
func childEnv() []string {
	environ := os.Environ()
	filtered := make([]string, 0, len(environ))
	for _, entry := range environ {
		if strings.HasPrefix(entry, "ATLAS_") {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

// sanitize makes a spec name safe for a directory name.
func sanitize(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, name)
}

// startWithRetry works around ETXTBSY, which Linux returns when the binary
// being exec'd is open for writing anywhere on the system. It happens without
// anyone doing anything wrong: a Go program that both writes executables and
// forks can have a child briefly holding the write descriptor between its fork
// and its exec, and an exec landing in that window fails.
//
// Two ways in here. `make build` writes bin/atlascache and the suite execs it
// moments later; and the harness's own tests write stand-in scripts and launch
// them while other tests are forking. Neither is a real failure, and both
// resolve in microseconds.
//
// Retrying is the standard remedy — the alternative is a gate that goes red for
// reasons unrelated to the code under test, which is how a gate stops being
// read. Anything that is not ETXTBSY fails immediately.
func startWithRetry(cmd *exec.Cmd) error {
	const attempts = 20

	var err error
	for i := 0; i < attempts; i++ {
		if err = cmd.Start(); !errors.Is(err, syscall.ETXTBSY) {
			return err
		}
		time.Sleep(5 * time.Millisecond)
	}
	return fmt.Errorf("%w (still busy after %d attempts)", err, attempts)
}
