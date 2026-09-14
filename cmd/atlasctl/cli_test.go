package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// writeFile is os.WriteFile with the permission these tests want.
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

// harness runs the CLI the way a shell does: a full command line in, an exit
// code and two streams out.
type harness struct {
	env    *env
	stdout *bytes.Buffer
	stderr *bytes.Buffer
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWithEnv(t, nil)
}

// newHarnessWithEnv is the same, with an environment the CLI can read.
func newHarnessWithEnv(t *testing.T, environment map[string]string) *harness {
	t.Helper()

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	return &harness{
		env: &env{
			stdin:  strings.NewReader(""),
			stdout: stdout,
			stderr: stderr,
			// Never a terminal, which is the case every script runs in and the
			// one where output must not be escaped.
			stdoutIsTTY: false,
			getenv:      func(name string) string { return environment[name] },
		},
		stdout: stdout,
		stderr: stderr,
	}
}

// run invokes the CLI with --addr already supplied.
func (h *harness) run(addr string, args ...string) int {
	h.stdout.Reset()
	h.stderr.Reset()
	return run(h.env, append([]string{"--addr", addr}, args...))
}

func TestCommandsAgainstAServer(t *testing.T) {
	t.Parallel()

	server := newFakeServer(t)
	h := newHarness(t)
	addr := server.addr()

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"ping", []string{"ping"}, "PONG\n"},
		{"set", []string{"set", "k", "v"}, "OK\n"},
		{"get", []string{"get", "k"}, "v"},
		{"exists", []string{"exists", "k"}, "1\n"},
		{"exists on an absent key", []string{"exists", "nope"}, "0\n"},
		{"keys", []string{"keys", "k"}, "k\n"},
		{"del", []string{"del", "k"}, "1\n"},
		{"del of an absent key", []string{"del", "k"}, "0\n"},
	}
	for _, tc := range cases {
		if code := h.run(addr, tc.args...); code != exitOK {
			t.Fatalf("%s exited %d, stderr %q", tc.name, code, h.stderr)
		}
		if got := h.stdout.String(); got != tc.want {
			t.Errorf("%s printed %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestGetOfAMissingKeyFailsWithoutOutput(t *testing.T) {
	t.Parallel()

	server := newFakeServer(t)
	h := newHarness(t)

	if code := h.run(server.addr(), "get", "absent"); code != exitFailure {
		t.Fatalf("a missing key exited %d, want %d", code, exitFailure)
	}
	if h.stdout.Len() != 0 {
		t.Errorf("a missing key wrote %q to stdout", h.stdout)
	}
	if !strings.Contains(h.stderr.String(), "absent") {
		t.Errorf("stderr %q does not name the key", h.stderr)
	}
}

func TestEmptyValueSucceedsWhereAMissingKeyFails(t *testing.T) {
	t.Parallel()

	server := newFakeServer(t)
	server.set("empty", []byte{})
	h := newHarness(t)

	if code := h.run(server.addr(), "get", "empty"); code != exitOK {
		t.Fatalf("a key holding an empty value exited %d, want 0", code)
	}
	if h.stdout.Len() != 0 {
		t.Errorf("an empty value printed %q", h.stdout)
	}
}

func TestBinaryValuesAreWrittenUnchanged(t *testing.T) {
	t.Parallel()

	value := []byte{0x00, 'h', 0xff, 0xfe, '\n', 0x7f}
	server := newFakeServer(t)
	server.set("binary", value)
	h := newHarness(t)

	if code := h.run(server.addr(), "get", "binary"); code != exitOK {
		t.Fatalf("exit %d, stderr %q", code, h.stderr)
	}
	if got := h.stdout.Bytes(); !bytes.Equal(got, value) {
		t.Errorf("stdout = %q, want %q; a redirected value must not be escaped or padded", got, value)
	}
}

func TestATerminalGetsEscapedOutput(t *testing.T) {
	t.Parallel()

	value := []byte{0x00, 'h', 0xff}
	server := newFakeServer(t)
	server.set("binary", value)

	h := newHarness(t)
	h.env.stdoutIsTTY = true

	if code := h.run(server.addr(), "get", "binary"); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	got := h.stdout.String()
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("a terminal did not get a trailing newline: %q", got)
	}
	if strings.ContainsRune(got, 0) {
		t.Errorf("a terminal was sent a raw null byte: %q", got)
	}
	if !strings.Contains(got, `\x00`) {
		t.Errorf("stdout = %q, want the null byte escaped", got)
	}
}

func TestSetReadsTheValueFromStdin(t *testing.T) {
	t.Parallel()

	value := []byte{'a', 0x00, 'b'}
	server := newFakeServer(t)
	h := newHarness(t)
	h.env.stdin = bytes.NewReader(value)

	if code := h.run(server.addr(), "set", "k", "--stdin"); code != exitOK {
		t.Fatalf("exit %d, stderr %q", code, h.stderr)
	}
	if code := h.run(server.addr(), "get", "k"); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if got := h.stdout.Bytes(); !bytes.Equal(got, value) {
		t.Errorf("a value read from stdin came back as %q, want %q", got, value)
	}
}

func TestScanWalksEveryPage(t *testing.T) {
	t.Parallel()

	server := newFakeServer(t)
	for _, key := range []string{"a1", "a2", "a3", "a4", "a5", "b1"} {
		server.set(key, []byte("v"))
	}
	h := newHarness(t)

	// The fake server answers two keys a page, so a scan that stopped at the
	// first page would return two of the five.
	if code := h.run(server.addr(), "scan", "--match", "a*"); code != exitOK {
		t.Fatalf("exit %d, stderr %q", code, h.stderr)
	}
	got := strings.Fields(h.stdout.String())
	if len(got) != 5 {
		t.Fatalf("scan printed %v, want all five a-keys", got)
	}
}

func TestStatsAndInfo(t *testing.T) {
	t.Parallel()

	server := newFakeServer(t)
	server.set("k", []byte("v"))
	h := newHarness(t)

	if code := h.run(server.addr(), "stats"); code != exitOK {
		t.Fatalf("stats exited %d", code)
	}
	if !strings.Contains(h.stdout.String(), "commands_processed: 42") {
		t.Errorf("stats printed %q", h.stdout)
	}

	if code := h.run(server.addr(), "info"); code != exitOK {
		t.Fatalf("info exited %d", code)
	}
	if !strings.Contains(h.stdout.String(), "atlascache_version") {
		t.Errorf("info printed %q", h.stdout)
	}
}

func TestJSONSuccessShape(t *testing.T) {
	t.Parallel()

	server := newFakeServer(t)
	server.set("k", []byte("v"))
	h := newHarness(t)

	if code := h.run(server.addr(), "get", "k", "--json"); code != exitOK {
		t.Fatalf("exit %d, stderr %q", code, h.stderr)
	}

	var document struct {
		OK      bool   `json:"ok"`
		Command string `json:"command"`
		Data    struct {
			Key      string `json:"key"`
			Found    bool   `json:"found"`
			Value    string `json:"value"`
			Encoding string `json:"encoding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(h.stdout.Bytes(), &document); err != nil {
		t.Fatalf("--json output is not JSON: %v (%q)", err, h.stdout)
	}
	if !document.OK || document.Command != "get" || !document.Data.Found {
		t.Fatalf("document = %+v", document)
	}
	if document.Data.Value != "v" || document.Data.Encoding != encodingUTF8 {
		t.Errorf("value = %q (%s), want v as utf8", document.Data.Value, document.Data.Encoding)
	}
}

func TestJSONCarriesBinaryValuesAsBase64(t *testing.T) {
	t.Parallel()

	value := []byte{0xff, 0xfe, 0x00}
	server := newFakeServer(t)
	server.set("k", value)
	h := newHarness(t)

	if code := h.run(server.addr(), "get", "k", "--json"); code != exitOK {
		t.Fatalf("exit %d", code)
	}

	var document struct {
		Data struct {
			Value    string `json:"value"`
			Encoding string `json:"encoding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(h.stdout.Bytes(), &document); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if document.Data.Encoding != encodingBase64 {
		t.Fatalf("encoding = %q, want base64", document.Data.Encoding)
	}
	decoded, err := base64.StdEncoding.DecodeString(document.Data.Value)
	if err != nil || !bytes.Equal(decoded, value) {
		t.Errorf("decoded = %q (%v), want %q", decoded, err, value)
	}
}

// TestJSONOnFailure is the assertion a script depends on: the failure case is
// the one it was written to handle, and a bare error string there breaks it.
func TestJSONOnFailure(t *testing.T) {
	t.Parallel()

	server := newFakeServer(t)
	dead := deadAddr(t)
	h := newHarness(t)

	cases := []struct {
		name string
		addr string
		args []string
		code int
		kind string
	}{
		{"a missing key", server.addr(), []string{"get", "absent", "--json"}, exitFailure, kindNotFound},
		{"an unknown flag", server.addr(), []string{"ping", "--nope", "--json"}, exitUsage, kindUsage},
		{"an unknown command", server.addr(), []string{"nosuch", "--json"}, exitUsage, kindUsage},
		{"a missing argument", server.addr(), []string{"get", "--json"}, exitUsage, kindUsage},
		{"an unreachable server", dead, []string{"ping", "--json"}, exitConnection, kindConnection},
	}

	for _, tc := range cases {
		code := h.run(tc.addr, tc.args...)
		if code != tc.code {
			t.Errorf("%s exited %d, want %d (stderr %q)", tc.name, code, tc.code, h.stderr)
		}

		var document struct {
			OK    bool `json:"ok"`
			Error struct {
				Code    int    `json:"code"`
				Kind    string `json:"kind"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(h.stdout.Bytes(), &document); err != nil {
			t.Fatalf("%s printed %q, which is not JSON: %v", tc.name, h.stdout, err)
		}
		if document.OK {
			t.Errorf("%s reported ok=true", tc.name)
		}
		if document.Error.Kind != tc.kind {
			t.Errorf("%s reported kind %q, want %q", tc.name, document.Error.Kind, tc.kind)
		}
		if document.Error.Code != code {
			t.Errorf("%s exited %d while its JSON said %d", tc.name, code, document.Error.Code)
		}
		if document.Error.Message == "" {
			t.Errorf("%s carried an empty message", tc.name)
		}
	}
}

// TestExitCodesDistinguishOutcomes is the table FEAT-0029 publishes. A script
// branches on these without parsing text, so they are four distinct numbers as
// well as the right ones.
func TestExitCodesDistinguishOutcomes(t *testing.T) {
	t.Parallel()

	server := newFakeServer(t)
	server.set("k", []byte("v"))
	dead := deadAddr(t)
	h := newHarness(t)

	cases := []struct {
		name string
		addr string
		args []string
		want int
	}{
		{"success", server.addr(), []string{"get", "k"}, exitOK},
		{"a missing key", server.addr(), []string{"get", "absent"}, exitFailure},
		{"a bad flag", server.addr(), []string{"get", "k", "--nope"}, exitUsage},
		{"an unreachable server", dead, []string{"get", "k"}, exitConnection},
	}

	seen := map[int]string{}
	for _, tc := range cases {
		code := h.run(tc.addr, tc.args...)
		if code != tc.want {
			t.Errorf("%s exited %d, want %d (stderr %q)", tc.name, code, tc.want, h.stderr)
		}
		if previous, clash := seen[code]; clash {
			t.Errorf("%s and %s both exit %d", previous, tc.name, code)
		}
		seen[code] = tc.name
	}
}

func TestUsageErrors(t *testing.T) {
	t.Parallel()

	server := newFakeServer(t)
	h := newHarness(t)

	cases := [][]string{
		{"ping", "extra"},
		{"get"},
		{"get", "a", "b"},
		{"set", "k"},
		{"set", "k", "v", "extra"},
		{"set", "k", "v", "--ttl", "nonsense"},
		{"set", "k", "v", "--ttl", "-1s"},
		{"set", "k", "--stdin", "v"},
		{"del"},
		{"exists"},
		{"keys"},
		{"keys", "a", "b"},
		{"scan", "pattern"},
		{"scan", "--count", "-1"},
		{"stats", "extra"},
		{"nosuchcommand"},
		{"get", "k", "--timeout", "-1s"},
	}
	for _, args := range cases {
		code := h.run(server.addr(), args...)
		if code != exitUsage {
			t.Errorf("`atlasctl %s` exited %d, want %d", strings.Join(args, " "), code, exitUsage)
		}
		if h.stdout.Len() != 0 {
			t.Errorf("`atlasctl %s` wrote %q to stdout; a usage error is a diagnostic",
				strings.Join(args, " "), h.stdout)
		}
		if h.stderr.Len() == 0 {
			t.Errorf("`atlasctl %s` failed silently", strings.Join(args, " "))
		}
	}
}

func TestServerErrorsAreCommandFailures(t *testing.T) {
	t.Parallel()

	server := newFakeServer(t)
	h := newHarness(t)

	server.failNext("ERR something went wrong")
	if code := h.run(server.addr(), "ping"); code != exitFailure {
		t.Fatalf("a server error exited %d, want %d", code, exitFailure)
	}
	if !strings.Contains(h.stderr.String(), "something went wrong") {
		t.Errorf("stderr = %q, want the server's own message", h.stderr)
	}
}

func TestAuthComesFromTheEnvironment(t *testing.T) {
	t.Parallel()

	const token = "s3cret"
	server := newFakeServer(t, withToken(token))

	authorized := newHarnessWithEnv(t, map[string]string{envAuth: token})
	if code := authorized.run(server.addr(), "set", "k", "v"); code != exitOK {
		t.Fatalf("with %s set, exit %d, stderr %q", envAuth, code, authorized.stderr)
	}
	if authorized.stderr.Len() != 0 {
		t.Errorf("the environment variable warned about itself: %q", authorized.stderr)
	}

	anonymous := newHarness(t)
	if code := anonymous.run(server.addr(), "get", "k"); code != exitFailure {
		t.Fatalf("without a token, exit %d, want %d", code, exitFailure)
	}
}

func TestAuthFlagWarnsAboutProcessListings(t *testing.T) {
	t.Parallel()

	const token = "s3cret"
	server := newFakeServer(t, withToken(token))
	server.set("k", []byte("v"))
	h := newHarness(t)

	if code := h.run(server.addr(), "--auth", token, "get", "k"); code != exitOK {
		t.Fatalf("exit %d, stderr %q", code, h.stderr)
	}
	if got := h.stdout.String(); got != "v" {
		t.Errorf("stdout = %q; the warning must not reach stdout", got)
	}
	if !strings.Contains(h.stderr.String(), envAuth) {
		t.Errorf("stderr = %q, want a warning naming %s", h.stderr, envAuth)
	}
}

func TestAWrongTokenIsACommandFailure(t *testing.T) {
	t.Parallel()

	server := newFakeServer(t, withToken("right"))
	h := newHarnessWithEnv(t, map[string]string{envAuth: "wrong"})

	// Exit 1 and not 3: the socket worked, the credential did not, and
	// retrying it would only produce another failure in the server's log.
	if code := h.run(server.addr(), "get", "k"); code != exitFailure {
		t.Fatalf("a refused token exited %d, want %d", code, exitFailure)
	}
}

func TestTLSConnects(t *testing.T) {
	t.Parallel()

	cert, caFile := certificate(t)
	server := newFakeServer(t, withTLS(cert))
	server.set("k", []byte("v"))
	h := newHarness(t)

	if code := h.run(server.addr(), "--tls-ca", caFile, "get", "k"); code != exitOK {
		t.Fatalf("exit %d, stderr %q", code, h.stderr)
	}
	if got := h.stdout.String(); got != "v" {
		t.Errorf("stdout = %q", got)
	}

	// --tls without the certificate authority must fail: the server's
	// certificate is self-signed, and a client that accepted it anyway would
	// be encrypting to whoever answered.
	if code := h.run(server.addr(), "--tls", "get", "k"); code == exitOK {
		t.Error("--tls without a trusted CA accepted a self-signed certificate")
	}

	// And plaintext against a TLS port fails rather than hanging.
	if code := h.run(server.addr(), "get", "k", "--timeout", "5s"); code == exitOK {
		t.Error("a plaintext client was served by a TLS port")
	}
}

func TestTLSCAFlagErrors(t *testing.T) {
	t.Parallel()

	server := newFakeServer(t)
	h := newHarness(t)

	if code := h.run(server.addr(), "--tls-ca", "/no/such/file.pem", "ping"); code != exitUsage {
		t.Errorf("a missing --tls-ca file exited %d, want %d", code, exitUsage)
	}

	notPEM := t.TempDir() + "/not-a-cert.pem"
	if err := writeFile(notPEM, "hello"); err != nil {
		t.Fatal(err)
	}
	if code := h.run(server.addr(), "--tls-ca", notPEM, "ping"); code != exitUsage {
		t.Errorf("a --tls-ca file holding no certificate exited %d, want %d", code, exitUsage)
	}
}

func TestHelpAndVersion(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	if code := run(h.env, []string{"help"}); code != exitOK {
		t.Fatalf("help exited %d", code)
	}
	help := h.stdout.String()
	for _, want := range []string{"ping", "get", "set", "del", "exists", "keys", "scan", "stats", "info",
		"--addr", "--auth", "--tls", "--tls-ca", "--json", "--timeout", envAuth} {
		if !strings.Contains(help, want) {
			t.Errorf("`atlasctl help` does not mention %q", want)
		}
	}

	h.stdout.Reset()
	if code := run(h.env, []string{"help", "set"}); code != exitOK {
		t.Fatalf("help set exited %d", code)
	}
	if !strings.Contains(h.stdout.String(), "--ttl") {
		t.Errorf("`atlasctl help set` does not document --ttl: %q", h.stdout)
	}

	h.stdout.Reset()
	if code := run(h.env, []string{"get", "--help"}); code != exitOK {
		t.Fatalf("get --help exited %d", code)
	}
	if !strings.Contains(h.stdout.String(), "get <key>") {
		t.Errorf("`atlasctl get --help` does not show the synopsis: %q", h.stdout)
	}

	h.stdout.Reset()
	if code := run(h.env, []string{"--version"}); code != exitOK {
		t.Fatalf("--version exited %d", code)
	}
	if !strings.Contains(h.stdout.String(), version) {
		t.Errorf("--version printed %q", h.stdout)
	}

	h.stdout.Reset()
	h.stderr.Reset()
	if code := run(h.env, []string{"help", "nosuchcommand"}); code != exitOK {
		t.Errorf("help for an unknown command exited %d", code)
	}
	if !strings.Contains(h.stderr.String(), "nosuchcommand") {
		t.Errorf("stderr = %q, want it to say the command is unknown", h.stderr)
	}

	h.stdout.Reset()
	h.stderr.Reset()
	if code := run(h.env, nil); code != exitUsage {
		t.Errorf("no command at all exited %d, want %d", code, exitUsage)
	}
}

func TestFlagsMayComeBeforeOrAfterTheCommand(t *testing.T) {
	t.Parallel()

	server := newFakeServer(t)
	server.set("k", []byte("v"))
	h := newHarness(t)

	// The point of permuting: a user should not have to know where a flag goes.
	if code := run(h.env, []string{"--addr", server.addr(), "get", "k"}); code != exitOK {
		t.Fatalf("a flag before the command exited %d, stderr %q", code, h.stderr)
	}
	h.stdout.Reset()
	if code := run(h.env, []string{"get", "k", "--addr", server.addr()}); code != exitOK {
		t.Fatalf("a flag after the operand exited %d, stderr %q", code, h.stderr)
	}
	if got := h.stdout.String(); got != "v" {
		t.Errorf("stdout = %q", got)
	}

	// And `--` ends the flags, so a value that looks like one can be stored.
	h.stdout.Reset()
	if code := run(h.env, []string{"--addr", server.addr(), "set", "dash", "--", "--not-a-flag"}); code != exitOK {
		t.Fatalf("a value after -- exited %d, stderr %q", code, h.stderr)
	}
	h.stdout.Reset()
	if code := run(h.env, []string{"--addr", server.addr(), "get", "dash"}); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if got := h.stdout.String(); got != "--not-a-flag" {
		t.Errorf("stored value came back as %q", got)
	}
}

func TestTimeoutIsHonoured(t *testing.T) {
	t.Parallel()

	// A server that accepts and never answers: the CLI's own deadline has to
	// be what ends this, and it must report a connection failure rather than
	// hanging or calling it a command error.
	hole := newFakeServer(t)
	hole.mu.Lock()
	// Long enough that the CLI's 200ms deadline is what ends the call, short
	// enough that the test does not wait on it during cleanup.
	hole.hangFor = 2 * time.Second
	hole.mu.Unlock()

	h := newHarness(t)
	if code := h.run(hole.addr(), "ping", "--timeout", "200ms"); code != exitConnection {
		t.Fatalf("a server that never answers exited %d, want %d", code, exitConnection)
	}
}
