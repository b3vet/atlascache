package scenarios_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/b3vet/atlascache/test/e2e/certs"
	"github.com/b3vet/atlascache/test/e2e/runner"
	"github.com/b3vet/atlascache/test/e2e/runner/fakeharness"
)

// The security scenarios are driven here against servers that misbehave in the
// specific ways each one exists to catch. A scenario that cannot fail proves
// nothing, and in the suite these only ever meet a correct server.
//
// The model below is a RESP server over TLS with two switches: whether it
// validates a replacement certificate before swapping it in, and what protocol
// versions it will accept. Those are the two decisions FEAT-0023 makes, and a
// model that gets either wrong is a server whose rotation takes the service
// down.

// tlsModel is a minimal RESP-over-TLS server backed by real certificate files.
type tlsModel struct {
	certFile string
	keyFile  string
	addr     string

	// validate is the behavior under test: a validating server keeps the
	// certificate it has when the files on disk stop being a usable pair.
	validate bool
	// plaintext serves without TLS at all, which is the defect
	// tls_rejects_plaintext exists to catch.
	plaintext bool
	// mangle answers wrongly over a perfectly good connection.
	mangle bool

	mu      sync.Mutex
	current *tls.Certificate
	values  map[string]string
	log     []string
}

type tlsModelOptions struct {
	validate   bool
	plaintext  bool
	minVersion uint16
	// mangle answers commands wrongly, for the scenarios whose assertion is
	// that an encrypted connection is also a working one.
	mangle bool
}

// newTLSModel starts a model server and returns it. It stops with the test.
func newTLSModel(t *testing.T, opts tlsModelOptions) *tlsModel {
	t.Helper()

	dir := t.TempDir()
	certFile, keyFile, err := certs.Write(filepath.Join(dir, "certs"), "atlascache-e2e")
	if err != nil {
		t.Fatalf("generating the model's certificate: %v", err)
	}

	model := &tlsModel{
		certFile:  certFile,
		keyFile:   keyFile,
		validate:  opts.validate,
		plaintext: opts.plaintext,
		mangle:    opts.mangle,
		values:    map[string]string{},
	}

	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatalf("loading the model's certificate: %v", err)
	}
	model.current = &pair

	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	if !opts.plaintext {
		minVersion := opts.minVersion
		if minVersion == 0 {
			minVersion = tls.VersionTLS13
		}
		listener = tls.NewListener(listener, &tls.Config{
			MinVersion:     minVersion,
			GetCertificate: model.getCertificate,
		})
	}

	model.addr = listener.Addr().String()
	stop := make(chan struct{})
	t.Cleanup(func() {
		close(stop)
		_ = listener.Close()
	})

	go model.accept(listener)
	go model.watch(stop)
	return model
}

// watch stands in for the fsnotify watch: it notices the files changing and
// decides what to do about it, away from any connection.
//
// The decision is the one FEAT-0023 is about. A validating server parses the
// replacement first and keeps what it has when the parse fails; the other kind
// takes whatever is on disk and discovers the problem during a handshake, by
// which time it is an outage.
func (m *tlsModel) watch(stop <-chan struct{}) {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			m.reload()
		}
	}
}

func (m *tlsModel) reload() {
	pair, err := tls.LoadX509KeyPair(m.certFile, m.keyFile)

	m.mu.Lock()
	defer m.mu.Unlock()

	if err != nil {
		// Both models say they rejected it. Only one of them means it — which
		// is the point: a log line claiming a certificate was rejected proves
		// nothing about what the server is actually serving.
		m.logf("certificate reload rejected; still serving the certificate already loaded: %v", err)
		if !m.validate {
			m.current = nil
		}
		return
	}
	m.current = &pair
}

// getCertificate is the per-handshake hook, which reads whatever the watch
// last settled on.
func (m *tlsModel) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.current == nil {
		return nil, errors.New("no certificate is loaded")
	}
	return m.current, nil
}

func (m *tlsModel) accept(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go m.serve(conn)
	}
}

func (m *tlsModel) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)
	for {
		args, err := readRESPCommand(reader)
		if err != nil {
			return
		}
		if _, err := writer.WriteString(m.reply(args)); err != nil {
			return
		}
		if err := writer.Flush(); err != nil {
			return
		}
	}
}

// reply answers the handful of commands the security scenarios send.
func (m *tlsModel) reply(args []string) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	switch strings.ToUpper(args[0]) {
	case "PING":
		if m.mangle {
			return "+PANG\r\n"
		}
		return "+PONG\r\n"
	case "SET":
		if len(args) < 3 {
			return "-ERR wrong number of arguments\r\n"
		}
		m.values[args[1]] = args[2]
		return "+OK\r\n"
	case "GET":
		value, ok := m.values[args[1]]
		if !ok {
			return "$-1\r\n"
		}
		if m.mangle {
			value = "something else"
		}
		return "$" + strconv.Itoa(len(value)) + "\r\n" + value + "\r\n"
	default:
		return "-ERR unknown command '" + args[0] + "'\r\n"
	}
}

func (m *tlsModel) logf(format string, args ...any) {
	m.log = append(m.log, fmt.Sprintf(format, args...))
}

func (m *tlsModel) logs() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.Join(m.log, "\n")
}

// readCommand reads one RESP array of bulk strings, which is all a client of
// this model ever sends.
func readRESPCommand(r *bufio.Reader) ([]string, error) {
	header, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	header = strings.TrimRight(header, "\r\n")
	if !strings.HasPrefix(header, "*") {
		return nil, fmt.Errorf("not an array: %q", header)
	}
	count, err := strconv.Atoi(header[1:])
	if err != nil || count < 1 {
		return nil, fmt.Errorf("bad array header %q", header)
	}

	args := make([]string, 0, count)
	for i := 0; i < count; i++ {
		sizeLine, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		size, err := strconv.Atoi(strings.TrimRight(sizeLine, "\r\n")[1:])
		if err != nil || size < 0 {
			return nil, fmt.Errorf("bad bulk header %q", sizeLine)
		}
		payload := make([]byte, size+2)
		if _, err := io.ReadFull(r, payload); err != nil {
			return nil, err
		}
		args = append(args, string(payload[:size]))
	}
	return args, nil
}

// tlsStub is a harness pointing at a model server, with the TLS accessors the
// security scenarios reach for.
type tlsStub struct {
	*stub
	model   *tlsModel
	enabled bool

	mu      sync.Mutex
	trusted [][]byte
}

func newTLSStub(t *testing.T, model *tlsModel) *tlsStub {
	t.Helper()

	base := newStub()
	base.info = runner.ServerInfo{ClientAddr: model.addr, AdminAddr: model.addr}
	base.root = t.TempDir()

	return &tlsStub{stub: base, model: model, enabled: true}
}

func (s *tlsStub) TLSEnabled() bool            { return s.enabled }
func (s *tlsStub) CertPaths() (string, string) { return s.model.certFile, s.model.keyFile }
func (s *tlsStub) Logs() string                { return s.model.logs() }
func (s *tlsStub) Send(context.Context, string) (runner.Reply, error) {
	return runner.StatusReply("PONG"), nil
}

// ClientTLSConfig trusts every certificate seen so far, as the real harness
// does, so a client keeps working across a rotation.
func (s *tlsStub) ClientTLSConfig() (*tls.Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if pemBytes, err := certs.ReadCertificate(s.model.certFile); err == nil {
		known := false
		for _, seen := range s.trusted {
			if string(seen) == string(pemBytes) {
				known = true
				break
			}
		}
		if !known {
			s.trusted = append(s.trusted, pemBytes)
		}
	}
	if len(s.trusted) == 0 {
		return nil, fmt.Errorf("no usable certificate at %s", s.model.certFile)
	}
	return certs.ClientConfig(s.trusted...)
}

func TestTLSConnectionIsEncrypted(t *testing.T) {
	t.Run("a TLS 1.3 server passes", func(t *testing.T) {
		harness := newTLSStub(t, newTLSModel(t, tlsModelOptions{validate: true}))
		if failure := runScenario(t, "tls_connection_is_encrypted", harness); failure != nil {
			t.Fatalf("the scenario failed against a correct server: %s", failure.Message)
		}
	})

	t.Run("a harness with TLS off is reported rather than silently skipped", func(t *testing.T) {
		harness := newTLSStub(t, newTLSModel(t, tlsModelOptions{validate: true}))
		harness.enabled = false

		requireFailure(t, runScenario(t, "tls_connection_is_encrypted", harness), "tls.enabled: true")
	})

	t.Run("a plaintext server cannot be dialed over TLS", func(t *testing.T) {
		harness := newTLSStub(t, newTLSModel(t, tlsModelOptions{plaintext: true}))
		requireFailure(t, runScenario(t, "tls_connection_is_encrypted", harness), "over TLS")
	})
}

// TestTLSScenariosNeedTheProcessHarness checks the message a scenario gives
// when it is handed a harness that cannot do what it needs, rather than
// silently passing against nothing.
func TestTLSScenariosNeedTheProcessHarness(t *testing.T) {
	for _, name := range []string{
		"tls_connection_is_encrypted",
		"tls_rejects_plaintext",
		"tls_refuses_old_versions",
		"tls_rotates_certificates",
	} {
		t.Run(name, func(t *testing.T) {
			requireFailure(t, runScenario(t, name, newStub()), "--harness process")
		})
	}

	t.Run("auth_token_never_logged", func(t *testing.T) {
		requireFailure(t, runScenario(t, "auth_token_never_logged", fakeharness.New(nil)),
			"--harness process")
	})
}

// TestTLSServesBadly covers the other half of "encrypted": a connection that
// handshakes perfectly and then answers wrongly is not a working server.
func TestTLSServesBadly(t *testing.T) {
	harness := newTLSStub(t, newTLSModel(t, tlsModelOptions{validate: true, mangle: true}))
	requireFailure(t, runScenario(t, "tls_connection_is_encrypted", harness), "PING answered")
}

func TestTLSRejectsPlaintext(t *testing.T) {
	t.Run("a TLS server refuses a plaintext client", func(t *testing.T) {
		harness := newTLSStub(t, newTLSModel(t, tlsModelOptions{validate: true}))
		if failure := runScenario(t, "tls_rejects_plaintext", harness); failure != nil {
			t.Fatalf("the scenario failed against a correct server: %s", failure.Message)
		}
	})

	t.Run("a server that answers plaintext on a TLS port is caught", func(t *testing.T) {
		// The defect that matters: TLS was configured, and the port answers
		// plaintext anyway. An operator believes traffic is encrypted and it
		// is not.
		harness := newTLSStub(t, newTLSModel(t, tlsModelOptions{plaintext: true}))
		requireFailure(t, runScenario(t, "tls_rejects_plaintext", harness), "must not be served")
	})
}

func TestTLSRefusesOldVersions(t *testing.T) {
	t.Run("a floor of 1.3 passes", func(t *testing.T) {
		harness := newTLSStub(t, newTLSModel(t, tlsModelOptions{validate: true}))
		if failure := runScenario(t, "tls_refuses_old_versions", harness); failure != nil {
			t.Fatalf("the scenario failed against a correct server: %s", failure.Message)
		}
	})

	t.Run("a server that still offers TLS 1.2 is caught", func(t *testing.T) {
		harness := newTLSStub(t, newTLSModel(t, tlsModelOptions{validate: true, minVersion: tls.VersionTLS10}))
		requireFailure(t, runScenario(t, "tls_refuses_old_versions", harness), "the floor is TLS 1.3")
	})
}

func TestTLSRotatesCertificates(t *testing.T) {
	t.Run("a server that validates before swapping passes", func(t *testing.T) {
		harness := newTLSStub(t, newTLSModel(t, tlsModelOptions{validate: true}))
		if failure := runScenario(t, "tls_rotates_certificates", harness); failure != nil {
			t.Fatalf("the scenario failed against a correct server: %s", failure.Message)
		}
	})

	t.Run("a server that swaps in a broken certificate is caught", func(t *testing.T) {
		// The expensive failure: the renewal wrote something unusable, the
		// server took it anyway, and it can no longer complete a handshake.
		// It even logs that it rejected the certificate — which is why the
		// scenario asserts on connections rather than on the log.
		harness := newTLSStub(t, newTLSModel(t, tlsModelOptions{validate: false}))
		requireFailure(t, runScenario(t, "tls_rotates_certificates", harness), "stopped accepting connections")
	})
}

// authStub scripts the replies an auth scenario sees, and the log it reads.
type authStub struct {
	*stub
	replies func(cmd string) runner.Reply
	logs    string
}

func newAuthStub(t *testing.T, token string, replies func(cmd string) runner.Reply) *authStub {
	t.Helper()

	base := newStub()
	base.root = t.TempDir()

	// The harness writes the server's config, and the scenario reads the token
	// back out of it rather than being told: the two cannot then disagree about
	// what the secret is.
	config := "auth:\n  enabled: true\n"
	if token != "" {
		config += fmt.Sprintf("  token: %q\n", token)
	}
	if err := os.WriteFile(filepath.Join(base.root, "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatalf("writing the stub config: %v", err)
	}

	stub := &authStub{stub: base, replies: replies, logs: "authentication failed"}
	base.Respond = func(cmd string) (runner.Reply, error) { return stub.replies(cmd), nil }
	return stub
}

func (s *authStub) Logs() string { return s.logs }

// gated answers everything with NOAUTH except the pre-auth allowlist, which is
// what a correct server does to an unauthenticated connection.
func gated(cmd string) runner.Reply {
	name := strings.ToUpper(strings.Fields(cmd)[0])
	switch name {
	case "PING":
		return runner.StatusReply("PONG")
	case "HELLO":
		return runner.ArrayReply(runner.BulkReply("proto"), runner.IntegerReply(2))
	case "AUTH":
		return runner.ErrorReply("WRONGPASS invalid username-password pair")
	case "QUIT":
		return runner.StatusReply("OK")
	default:
		return runner.ErrorReply("NOAUTH Authentication required")
	}
}

func TestAuthGatesEveryCommand(t *testing.T) {
	t.Run("a server that gates everything outside the allowlist passes", func(t *testing.T) {
		harness := newAuthStub(t, "tok", gated)
		if failure := runScenario(t, "auth_gates_every_command", harness); failure != nil {
			t.Fatalf("the scenario failed against a correct server: %s", failure.Message)
		}
	})

	t.Run("one command escaping the gate is caught", func(t *testing.T) {
		// The realistic defect: a command added later that the gate does not
		// cover. A per-handler check is how that happens.
		harness := newAuthStub(t, "tok", func(cmd string) runner.Reply {
			if strings.HasPrefix(cmd, "DBSIZE") {
				return runner.IntegerReply(0)
			}
			return gated(cmd)
		})
		requireFailure(t, runScenario(t, "auth_gates_every_command", harness), "it must be gated")
	})

	t.Run("an unknown command leaking the command table is caught", func(t *testing.T) {
		harness := newAuthStub(t, "tok", func(cmd string) runner.Reply {
			if strings.HasPrefix(cmd, "NOSUCH") {
				return runner.ErrorReply("ERR unknown command 'NOSUCHCOMMAND'")
			}
			return gated(cmd)
		})
		requireFailure(t, runScenario(t, "auth_gates_every_command", harness), "command table")
	})

	t.Run("gating a command on the allowlist is caught", func(t *testing.T) {
		// PING is on the allowlist because health checks and connection pools
		// send it first; gating it breaks both.
		harness := newAuthStub(t, "tok", func(cmd string) runner.Reply {
			if strings.HasPrefix(cmd, "PING") {
				return runner.ErrorReply("NOAUTH Authentication required")
			}
			return gated(cmd)
		})
		requireFailure(t, runScenario(t, "auth_gates_every_command", harness), "pre-auth allowlist")
	})

	t.Run("the wrong error text is caught", func(t *testing.T) {
		harness := newAuthStub(t, "tok", func(cmd string) runner.Reply {
			if strings.HasPrefix(cmd, "GET") {
				return runner.ErrorReply("ERR not authenticated")
			}
			return gated(cmd)
		})
		requireFailure(t, runScenario(t, "auth_gates_every_command", harness), "NOAUTH Authentication required")
	})
}

func TestAuthTokenNeverLogged(t *testing.T) {
	const token = "the-secret-token-1a2b3c"

	t.Run("a server that logs the outcome and not the secret passes", func(t *testing.T) {
		harness := newAuthStub(t, token, gated)
		harness.logs = `{"level":"warn","remote_addr":"127.0.0.1:1","message":"authentication failed"}`

		if failure := runScenario(t, "auth_token_never_logged", harness); failure != nil {
			t.Fatalf("the scenario failed against a correct server: %s", failure.Message)
		}
	})

	t.Run("a token in the log is caught", func(t *testing.T) {
		harness := newAuthStub(t, token, gated)
		harness.logs = `{"level":"warn","token":"` + token + `","message":"authentication failed"}`

		requireFailure(t, runScenario(t, "auth_token_never_logged", harness), "appears in the server log")
	})

	t.Run("a server that logs nothing at all is caught", func(t *testing.T) {
		// Silence is not safety: a failed authentication has to be recorded,
		// with its source address, or nobody can see an attack in progress.
		harness := newAuthStub(t, token, gated)
		harness.logs = ""

		requireFailure(t, runScenario(t, "auth_token_never_logged", harness), "not logged at all")
	})

	t.Run("a spec with no token is reported", func(t *testing.T) {
		harness := newAuthStub(t, "", gated)
		requireFailure(t, runScenario(t, "auth_token_never_logged", harness), "auth.token")
	})
}

func TestTLSMissingCertificateFailsFast(t *testing.T) {
	cases := []struct {
		name        string
		run         func(args []string) (int, string, error)
		wantFailure string
	}{
		{
			name: "refused, naming the file",
			run: func(args []string) (int, string, error) {
				config := args[len(args)-1]
				if strings.Contains(config, "no-paths") {
					return 1, "invalid config: tls.cert_file - must be set when tls.enabled is true\n" +
						"invalid config: tls.key_file - must be set", nil
				}
				return 1, "failed to load the TLS certificate: tls.cert_file " +
					certPathFrom(config) + ": no such file or directory", nil
			},
		},
		{
			name:        "a server that starts anyway is caught",
			run:         func([]string) (int, string, error) { return 0, "atlascache ready", nil },
			wantFailure: "must never fall back to plaintext",
		},
		{
			name:        "a message that does not name the file is caught",
			run:         func([]string) (int, string, error) { return 1, "something went wrong", nil },
			wantFailure: "does not name the missing file",
		},
		{
			name: "tls.enabled with no paths at all must be refused too",
			run: func(args []string) (int, string, error) {
				config := args[len(args)-1]
				if strings.Contains(config, "no-paths") {
					return 0, "atlascache ready", nil
				}
				return 1, "tls.cert_file " + certPathFrom(config) + ": no such file", nil
			},
			wantFailure: "no cert_file or key_file",
		},
		{
			name: "a message naming only one of the two fields is caught",
			run: func(args []string) (int, string, error) {
				config := args[len(args)-1]
				if strings.Contains(config, "no-paths") {
					return 1, "invalid config: tls.cert_file - must be set", nil
				}
				return 1, "tls.cert_file " + certPathFrom(config) + ": no such file", nil
			},
			wantFailure: "tls.key_file",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			harness := newStub()
			harness.root = t.TempDir()
			harness.binaryRun = tc.run

			failure := runScenario(t, "tls_missing_certificate_fails_fast", harness)
			if tc.wantFailure == "" {
				if failure != nil {
					t.Fatalf("the scenario failed against a correct server: %s", failure.Message)
				}
				return
			}
			requireFailure(t, failure, tc.wantFailure)
		})
	}
}

// certPathFrom recovers the certificate path a config file names, so the stub
// can answer with a message that mentions it.
func certPathFrom(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "no-such-certificate.pem")
}

func TestTTLBothDisabledRefused(t *testing.T) {
	const bothNamed = "invalid config: ttl.active_expiration - must not be false while " +
		"ttl.lazy_expiration is also false"

	cases := []struct {
		name        string
		code        int
		output      string
		wantFailure string
	}{
		{name: "refused, naming both fields", code: 1, output: bothNamed},
		{
			name:        "a server that accepts the combination is caught",
			code:        0,
			output:      "atlascache ready",
			wantFailure: "nothing to reclaim expired keys",
		},
		{
			name:        "a message naming only one field is caught",
			code:        1,
			output:      "invalid config: ttl.active_expiration - must be true",
			wantFailure: "ttl.lazy_expiration",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			harness := newStub()
			harness.root = t.TempDir()
			harness.binaryRun = func([]string) (int, string, error) { return tc.code, tc.output, nil }

			failure := runScenario(t, "ttl_both_disabled_refused", harness)
			if tc.wantFailure == "" {
				if failure != nil {
					t.Fatalf("the scenario failed against a correct server: %s", failure.Message)
				}
				return
			}
			requireFailure(t, failure, tc.wantFailure)
		})
	}
}
