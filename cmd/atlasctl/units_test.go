package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/b3vet/atlascache/pkg/client"
)

func TestClassifyMapsEveryCategory(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		code int
		kind string
	}{
		{"a usage mistake", usageErrorf("bad flag"), exitUsage, kindUsage},
		{"a missing key", notFoundError("k"), exitFailure, kindNotFound},
		{"a dial failure", client.ErrNetwork, exitConnection, kindConnection},
		{"a deadline", client.ErrTimeout, exitConnection, kindTimeout},
		{"a refused token", client.ErrAuth, exitFailure, kindAuth},
		{"a reply that would not decode", client.ErrProtocol, exitFailure, kindProtocol},
		{"a server refusal", client.ErrServer, exitFailure, kindServer},
		{"a closed client", client.ErrClosed, exitFailure, kindClosed},
		{"an expired context", context.DeadlineExceeded, exitConnection, kindTimeout},
		{"a canceled context", context.Canceled, exitFailure, kindCanceled},
		{"anything else", errors.New("unclassified"), exitFailure, kindError},
	}

	for _, tc := range cases {
		code, kind := classify(tc.err)
		if code != tc.code || kind != tc.kind {
			t.Errorf("%s classified as (%d, %s), want (%d, %s)", tc.name, code, kind, tc.code, tc.kind)
		}
	}

	// Wrapped, which is how every one of these actually arrives.
	wrapped := &cliError{code: exitUsage, kind: kindUsage, err: errors.New("inner")}
	if !errors.Is(wrapped, wrapped.err) {
		t.Error("a cliError does not unwrap to its cause")
	}
}

func TestNeedsEscaping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		value []byte
		want  bool
	}{
		{[]byte("plain text"), false},
		{[]byte("a line\nand a tab\t"), false},
		{[]byte("unicode: ünïcodé ✓"), false},
		{[]byte{0x00}, true},
		{[]byte{0x1b, '['}, true},
		{[]byte{0x7f}, true},
		{[]byte{0xff, 0xfe}, true},
		{nil, false},
	}
	for _, tc := range cases {
		if got := needsEscaping(tc.value); got != tc.want {
			t.Errorf("needsEscaping(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
}

func TestEncodeValueAndKeys(t *testing.T) {
	t.Parallel()

	text, encoding := encodeValue([]byte("hello"))
	if text != "hello" || encoding != encodingUTF8 {
		t.Errorf("encodeValue(hello) = (%q, %s)", text, encoding)
	}

	binary := []byte{0xff, 0x00}
	text, encoding = encodeValue(binary)
	if encoding != encodingBase64 {
		t.Fatalf("encodeValue of invalid UTF-8 reported %s", encoding)
	}
	decoded, err := base64.StdEncoding.DecodeString(text)
	if err != nil || !bytes.Equal(decoded, binary) {
		t.Errorf("the base64 did not decode back: %q (%v)", decoded, err)
	}

	keys, encoding := encodeKeys([]string{"a", "b"})
	if encoding != encodingUTF8 || len(keys) != 2 || keys[0] != "a" {
		t.Errorf("encodeKeys of text keys = (%v, %s)", keys, encoding)
	}

	// One key that is not valid UTF-8 moves the whole list to base64, so the
	// array's element type never changes with its contents.
	keys, encoding = encodeKeys([]string{"a", string([]byte{0xff})})
	if encoding != encodingBase64 {
		t.Fatalf("encodeKeys with one binary key reported %s", encoding)
	}
	if first, decodeErr := base64.StdEncoding.DecodeString(keys[0]); decodeErr != nil || string(first) != "a" {
		t.Errorf("the text key was not encoded alongside the binary one: %q (%v)", first, decodeErr)
	}
}

func TestWantsJSON(t *testing.T) {
	t.Parallel()

	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"ping"}, false},
		{[]string{"ping", "--json"}, true},
		{[]string{"--json", "ping"}, true},
		{[]string{"ping", "-json"}, true},
		{[]string{"ping", "--json=true"}, true},
		// Everything after `--` is an operand, including one spelled like the
		// flag, so a value of "--json" does not change the output format.
		{[]string{"set", "k", "--", "--json"}, false},
	}
	for _, tc := range cases {
		if got := wantsJSON(tc.args); got != tc.want {
			t.Errorf("wantsJSON(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

func TestSplitCommand(t *testing.T) {
	t.Parallel()

	cases := []struct {
		args    []string
		before  []string
		command string
		rest    []string
	}{
		{[]string{"ping"}, nil, "ping", []string{}},
		{[]string{"--json", "ping"}, []string{"--json"}, "ping", []string{}},
		{[]string{"--addr", "host:1", "get", "k"}, []string{"--addr", "host:1"}, "get", []string{"k"}},
		{[]string{"--addr=host:1", "get", "k"}, []string{"--addr=host:1"}, "get", []string{"k"}},
		// An unknown flag does not swallow the word after it, so the command is
		// still found and the flag is still reported.
		{[]string{"--nope", "ping"}, []string{"--nope"}, "ping", []string{}},
		{[]string{"--", "ping", "k"}, nil, "ping", []string{"k"}},
		{[]string{"--json"}, []string{"--json"}, "", nil},
		{nil, nil, "", nil},
		// A value-taking flag with nothing after it: nothing to consume, and
		// Parse is left to report it.
		{[]string{"--addr"}, []string{"--addr"}, "", nil},
	}

	for _, tc := range cases {
		before, command, rest := splitCommand(tc.args)
		if command != tc.command {
			t.Errorf("splitCommand(%v) found command %q, want %q", tc.args, command, tc.command)
		}
		if strings.Join(before, " ") != strings.Join(tc.before, " ") {
			t.Errorf("splitCommand(%v) leading flags = %v, want %v", tc.args, before, tc.before)
		}
		if strings.Join(rest, " ") != strings.Join(tc.rest, " ") {
			t.Errorf("splitCommand(%v) rest = %v, want %v", tc.args, rest, tc.rest)
		}
	}
}

func TestPermute(t *testing.T) {
	t.Parallel()

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var g globals
	g.register(fs)
	fs.String("match", "", "a glob")
	fs.Bool("stdin", false, "read stdin")

	cases := []struct {
		args []string
		want []string
	}{
		{[]string{"k", "--json"}, []string{"--json", "k"}},
		{[]string{"k", "--addr", "host:1"}, []string{"--addr", "host:1", "k"}},
		{[]string{"--match", "a*", "--stdin", "k"}, []string{"--match", "a*", "--stdin", "k"}},
		{[]string{"k", "--match=a*"}, []string{"--match=a*", "k"}},
		{[]string{"k", "--", "--not-a-flag"}, []string{"k", "--not-a-flag"}},
		{[]string{"k", "--unknown", "v"}, []string{"--unknown", "k", "v"}},
		{[]string{"-"}, []string{"-"}},
	}
	for _, tc := range cases {
		got := permute(fs, tc.args)
		if strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("permute(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

func TestParseInfo(t *testing.T) {
	t.Parallel()

	fields := parseInfo("# Server\r\natlascache_version:0.1.0\r\n\r\n# Keyspace\r\ndb0:keys=3\r\nnot a field\r\n")
	if fields["atlascache_version"] != "0.1.0" {
		t.Errorf("fields = %v", fields)
	}
	if fields["db0"] != "keys=3" {
		t.Errorf("a value containing = was not kept whole: %v", fields)
	}
	if _, ok := fields["# Server"]; ok {
		t.Error("a section header was parsed as a field")
	}
	if len(fields) != 2 {
		t.Errorf("fields = %v, want exactly the two key:value lines", fields)
	}
}

func TestOutputHelpers(t *testing.T) {
	t.Parallel()

	if got := withTrailingNewline(""); got != "" {
		t.Errorf("withTrailingNewline(\"\") = %q", got)
	}
	if got := withTrailingNewline("a\n"); got != "a\n" {
		t.Errorf("a second newline was added: %q", got)
	}
	if got := withTrailingNewline("a"); got != "a\n" {
		t.Errorf("withTrailingNewline(a) = %q", got)
	}
	if got := indentedLines(nil); got != "" {
		t.Errorf("indentedLines(nil) = %q", got)
	}
	if got := indentedLines([]string{"a", "b"}); got != "a\nb\n" {
		t.Errorf("indentedLines = %q", got)
	}
}

// failingWriter stands in for a stdout that has gone away — a closed pipe,
// most often, which is what every `atlasctl keys '*' | head` produces.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestOutputFailuresAreReported(t *testing.T) {
	t.Parallel()

	stderr := &bytes.Buffer{}
	e := &env{stdout: failingWriter{}, stderr: stderr, getenv: func(string) string { return "" }}

	code := e.reportSuccess(invocation{command: "ping", res: result{
		text: func(o *output) error { return o.line("PONG") },
	}})
	if code != exitFailure {
		t.Errorf("a failed write exited %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr.String(), "broken pipe") {
		t.Errorf("stderr = %q", stderr)
	}

	stderr.Reset()
	if code := e.writeJSON(envelope{OK: true, Command: "ping"}); code != exitFailure {
		t.Errorf("a failed JSON write exited %d, want %d", code, exitFailure)
	}

	// A result with nothing to print is not an error: `help` has output, but a
	// command that produced none should still exit 0.
	quiet := &env{stdout: io.Discard, stderr: stderr, getenv: func(string) string { return "" }}
	if code := quiet.reportSuccess(invocation{command: "ping"}); code != exitOK {
		t.Errorf("a result with no text exited %d", code)
	}
}

func TestTimeoutOfZeroMeansNoDeadline(t *testing.T) {
	t.Parallel()

	g := globals{timeout: 0}
	ctx, cancel := g.context()
	defer cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Error("a timeout of 0 set a deadline")
	}

	g = globals{addr: "127.0.0.1:1", timeout: -time.Second}
	if _, err := g.newClient(""); err == nil {
		t.Error("a negative timeout was accepted")
	}

	// An address the SDK itself rejects is a usage error, not a command one:
	// no retry will fix a command line.
	g = globals{addr: ""}
	_, err := g.newClient("")
	if code, _ := classify(err); code != exitUsage {
		t.Errorf("an empty address classified as %d, want %d", code, exitUsage)
	}
}
