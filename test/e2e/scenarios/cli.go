package scenarios

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/b3vet/atlascache/test/e2e/runner"
)

func init() {
	runner.RegisterScenario("cli_commands_work_against_a_live_server", cliCommandsWorkAgainstALiveServer)
	runner.RegisterScenario("cli_values_round_trip_through_stdout", cliValuesRoundTripThroughStdout)
	runner.RegisterScenario("cli_json_is_stable_and_parsable", cliJSONIsStableAndParsable)
	runner.RegisterScenario("cli_auth_prefers_the_environment", cliAuthPrefersTheEnvironment)
	runner.RegisterScenario("cli_exit_codes_distinguish_outcomes", cliExitCodesDistinguishOutcomes)
}

// The exit codes atlasctl promises (FEAT-0029). They are repeated here rather
// than imported: the CLI lives in another module, and a check that shared the
// constant with the thing it is checking would pass a renumbering that broke
// every script in the world.
const (
	ctlOK         = 0
	ctlFailure    = 1
	ctlUsage      = 2
	ctlConnection = 3
)

// ctlBudget bounds one CLI invocation. Every one of them is a loopback command
// against a running server, so this is a hang detector.
const ctlBudget = 30 * time.Second

// ctlBinaryEnv names a binary explicitly, for a run driven by hand rather than
// by the Makefile.
const ctlBinaryEnv = "ATLASCTL_BINARY"

// The flags, command names and kinds these scenarios assert on. They are
// constants because the CLI's interface is what is being checked: a spelling
// that drifted in one assertion and not another would be a scenario quietly
// testing two different programs.
const (
	flagAddr = "--addr"
	flagJSON = "--json"

	commandPing   = "ping"
	commandGet    = "get"
	commandSet    = "set"
	commandInfo   = "info"
	commandKeys   = "keys"
	commandScan   = "scan"
	commandStats  = "stats"
	commandDel    = "del"
	commandExists = "exists"

	kindUsageJSON = "usage"
	kindAuthJSON  = "auth"

	// The keys the scenarios write. Naming them keeps a typo from turning an
	// assertion into a check that a missing key is missing.
	keyJSON = "cli:json"
	keyExit = "exit:key"
)

// ctl invokes the atlasctl binary under test.
type ctl struct {
	path string
	// base is prepended to every invocation: where the server is, and how to
	// verify it. A scenario passes only the arguments it is asserting about.
	base []string
	env  []string
	root string
}

// newCtl locates the CLI and points it at the server under test.
func newCtl(c *runner.Ctx) (*ctl, error) {
	binary, ok := c.Harness.(serverBinary)
	if !ok {
		return nil, fmt.Errorf(
			"this scenario needs a harness that owns the binaries under test, and %T does not; run it against --harness process",
			c.Harness)
	}
	locator, ok := c.Harness.(interface{ Binary() string })
	if !ok {
		return nil, fmt.Errorf("this scenario needs a harness that knows the server binary's path, and %T does not", c.Harness)
	}

	path, err := ctlPath(locator.Binary())
	if err != nil {
		return nil, err
	}

	invocation := &ctl{
		path: path,
		base: []string{flagAddr, c.Info().ClientAddr},
		root: binary.Root(),
		// A clean environment but for what is set below: a developer's shell
		// must not be able to change what a spec tests, and ATLASCACHE_AUTH in
		// particular would make the auth assertions pass for the wrong reason.
		env: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")},
	}

	if tlsHost, ok := c.Harness.(tlsHarness); ok && tlsHost.TLSEnabled() {
		certFile, _ := tlsHost.CertPaths()
		invocation.base = append(invocation.base, "--tls-ca", certFile)
	}

	token, err := specToken(c)
	if err != nil {
		return nil, err
	}
	if token != "" {
		invocation.env = append(invocation.env, "ATLASCACHE_AUTH="+token)
	}
	return invocation, nil
}

// ctlPath finds the CLI beside the server binary, which is where `make
// build-ctl` puts it, unless one was named explicitly.
func ctlPath(serverBinaryPath string) (string, error) {
	if named := os.Getenv(ctlBinaryEnv); named != "" {
		//nolint:gosec // the path is the operator's own, from the environment of the run
		if _, err := os.Stat(named); err != nil {
			return "", fmt.Errorf("%s names %s, which is not there: %w", ctlBinaryEnv, named, err)
		}
		return named, nil
	}

	candidate := filepath.Join(filepath.Dir(serverBinaryPath), "atlasctl")
	info, err := os.Stat(candidate)
	if err != nil {
		return "", fmt.Errorf("no atlasctl at %s (run `make build-ctl`, or set %s): %w", candidate, ctlBinaryEnv, err)
	}
	if info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%s is not an executable file", candidate)
	}
	return candidate, nil
}

// ctlResult is one invocation's outcome. stdout is bytes rather than a string
// because a value is bytes, and the whole point of several assertions below is
// that nothing on the way here changed them.
type ctlResult struct {
	args   []string
	code   int
	stdout []byte
	stderr string
}

func (r ctlResult) String() string {
	return fmt.Sprintf("atlasctl %s -> exit %d, stdout %q, stderr %q",
		strings.Join(r.args, " "), r.code, r.stdout, strings.TrimSpace(r.stderr))
}

// run invokes the CLI with the scenario's base arguments prepended.
func (t *ctl) run(c *runner.Ctx, args ...string) (ctlResult, error) {
	return t.runWith(c, nil, t.env, args...)
}

// runWith invokes the CLI with an explicit stdin and environment.
//
// stdout and stderr are captured separately, deliberately: "data to stdout,
// diagnostics to stderr" is an acceptance criterion, and a rig that merged them
// could not tell whether it held.
func (t *ctl) runWith(c *runner.Ctx, stdin []byte, env []string, args ...string) (ctlResult, error) {
	full := append(append([]string{}, t.base...), args...)

	// Bounded by the scenario's own context as well as by this budget: a run
	// that is interrupted must not leave a CLI process behind it.
	ctx, cancel := context.WithTimeout(c.Context(), ctlBudget)
	defer cancel()

	cmd := exec.CommandContext(ctx, t.path, full...) //nolint:gosec // the binary under test, with arguments the scenario wrote
	cmd.Dir = t.root
	cmd.Env = env
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	result := ctlResult{args: full, stdout: stdout.Bytes(), stderr: stderr.String()}

	var exitErr *exec.ExitError
	switch {
	case err == nil:
		result.code = 0
	case errors.As(err, &exitErr):
		result.code = exitErr.ExitCode()
	default:
		return result, fmt.Errorf("running atlasctl %s: %w", strings.Join(full, " "), err)
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, fmt.Errorf("atlasctl %s did not finish within %s", strings.Join(full, " "), ctlBudget)
	}
	c.Logf("%s", result)
	return result, nil
}

// expectOK runs a command that must succeed and returns its stdout.
func (t *ctl) expectOK(c *runner.Ctx, args ...string) ([]byte, error) {
	result, err := t.run(c, args...)
	if err != nil {
		return nil, err
	}
	if result.code != ctlOK {
		return nil, fmt.Errorf("%s: want exit 0", result)
	}
	return result.stdout, nil
}

// expectLine runs a command that must succeed and print exactly one line.
func (t *ctl) expectLine(c *runner.Ctx, want string, args ...string) error {
	stdout, err := t.expectOK(c, args...)
	if err != nil {
		return err
	}
	if got := string(stdout); got != want+"\n" {
		return fmt.Errorf("atlasctl %s printed %q, want %q", strings.Join(args, " "), got, want+"\n")
	}
	return nil
}

// cliCommandsWorkAgainstALiveServer drives every command FEAT-0029 lists.
func cliCommandsWorkAgainstALiveServer(c *runner.Ctx) error {
	cli, newErr := newCtl(c)
	if newErr != nil {
		return newErr
	}

	if err := cli.expectLine(c, "PONG", commandPing); err != nil {
		return err
	}
	if err := cli.expectLine(c, "OK", commandSet, "cli:key", "hello"); err != nil {
		return err
	}

	// A value arrives with nothing added to it: no trailing newline, because
	// stdout here is a pipe and not a terminal.
	stdout, err := cli.expectOK(c, commandGet, "cli:key")
	if err != nil {
		return err
	}
	if string(stdout) != "hello" {
		return fmt.Errorf("get printed %q, want %q with nothing appended", stdout, "hello")
	}

	if err := cli.expectLine(c, "OK", commandSet, "cli:ttl", "v", "--ttl", "60s"); err != nil {
		return err
	}
	if err := cli.expectLine(c, "2", commandExists, "cli:key", "cli:ttl"); err != nil {
		return err
	}
	// A repeated key counts once per mention, as it does everywhere else.
	if err := cli.expectLine(c, "2", commandExists, "cli:key", "cli:key"); err != nil {
		return err
	}
	if err := cli.expectLine(c, "0", commandExists, "cli:absent"); err != nil {
		return err
	}

	if err := cliKeyspaceCommands(c, cli); err != nil {
		return err
	}
	if err := cliIntrospectionCommands(c, cli); err != nil {
		return err
	}

	if err := cliDeleteAndMiss(c, cli); err != nil {
		return err
	}
	return cliHelpIsAccurate(c, cli)
}

// cliDeleteAndMiss covers del, and where a failure's output goes.
func cliDeleteAndMiss(c *runner.Ctx, cli *ctl) error {
	if err := cli.expectLine(c, "1", commandDel, "cli:key"); err != nil {
		return err
	}
	// Deleting what is not there is not a failure; the count is the answer.
	if err := cli.expectLine(c, "0", commandDel, "cli:key"); err != nil {
		return err
	}

	// Errors go to stderr and data to stdout, which is what makes a pipeline
	// work. A miss writes nothing at all to stdout.
	result, err := cli.run(c, commandGet, "cli:key")
	if err != nil {
		return err
	}
	if result.code != ctlFailure {
		return fmt.Errorf("%s: a missing key must exit %d", result, ctlFailure)
	}
	if len(result.stdout) != 0 {
		return fmt.Errorf("a missing key wrote %q to stdout; diagnostics belong on stderr", result.stdout)
	}
	if !strings.Contains(result.stderr, "cli:key") {
		return fmt.Errorf("the error on stderr does not name the key: %q", result.stderr)
	}
	return nil
}

// cliKeyspaceCommands covers keys and scan.
func cliKeyspaceCommands(c *runner.Ctx, cli *ctl) error {
	for i := range 12 {
		if err := cli.expectLine(c, "OK", commandSet, fmt.Sprintf("cli:walk:%02d", i), "v"); err != nil {
			return err
		}
	}

	stdout, err := cli.expectOK(c, commandKeys, "cli:walk:*")
	if err != nil {
		return err
	}
	if lines := nonEmptyLines(string(stdout)); len(lines) != 12 {
		return fmt.Errorf("keys cli:walk:* printed %d lines, want 12: %q", len(lines), stdout)
	}

	// A scan runs to completion whatever the page size, which is the property a
	// cursor-per-invocation CLI could not offer.
	stdout, err = cli.expectOK(c, commandScan, "--match", "cli:walk:*", "--count", "3")
	if err != nil {
		return err
	}
	lines := nonEmptyLines(string(stdout))
	if len(lines) != 12 {
		return fmt.Errorf("scan --count 3 printed %d keys, want all 12: %q", len(lines), stdout)
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "cli:walk:") {
			return fmt.Errorf("scan --match cli:walk:* returned %q", line)
		}
	}

	// A pattern nothing matches is an empty answer, not a failure.
	stdout, err = cli.expectOK(c, commandKeys, "cli:nothing:*")
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(stdout)) != 0 {
		return fmt.Errorf("keys on a pattern nothing matches printed %q", stdout)
	}
	return nil
}

// cliIntrospectionCommands covers stats and info.
func cliIntrospectionCommands(c *runner.Ctx, cli *ctl) error {
	stdout, err := cli.expectOK(c, commandStats)
	if err != nil {
		return err
	}
	for _, want := range []string{"keys:", "commands_processed:"} {
		if !strings.Contains(string(stdout), want) {
			return fmt.Errorf("stats does not report %s: %q", want, stdout)
		}
	}

	stdout, err = cli.expectOK(c, commandInfo)
	if err != nil {
		return err
	}
	for _, want := range []string{"# Server", "atlascache_version", "# Keyspace"} {
		if !strings.Contains(string(stdout), want) {
			return fmt.Errorf("info does not contain %q", want)
		}
	}

	// A named section narrows the report rather than being ignored.
	stdout, err = cli.expectOK(c, commandInfo, "server")
	if err != nil {
		return err
	}
	if !strings.Contains(string(stdout), "# Server") {
		return fmt.Errorf("info server does not contain the Server section: %q", stdout)
	}
	if strings.Contains(string(stdout), "# Keyspace") {
		return fmt.Errorf("info server returned the whole report: %q", stdout)
	}
	return nil
}

// cliHelpIsAccurate checks that --help names every command and exits 0.
//
// Help that is wrong is worse than no help: it is the first thing a new user
// reads and the last thing anyone updates.
func cliHelpIsAccurate(c *runner.Ctx, cli *ctl) error {
	result, err := cli.runWith(c, nil, cli.env, "help")
	if err != nil {
		return err
	}
	if result.code != ctlOK {
		return fmt.Errorf("%s: help must exit 0", result)
	}

	help := string(result.stdout)
	for _, command := range []string{commandPing, commandGet, commandSet, commandDel, commandExists, commandKeys, commandScan, commandStats, commandInfo} {
		if !strings.Contains(help, command) {
			return fmt.Errorf("`atlasctl help` does not mention %q", command)
		}
	}
	for _, flag := range []string{flagAddr, "--auth", "--tls", "--tls-ca", flagJSON, "--timeout"} {
		if !strings.Contains(help, flag) {
			return fmt.Errorf("`atlasctl help` does not mention the %s flag", flag)
		}
	}
	if !strings.Contains(help, "ATLASCACHE_AUTH") {
		return errors.New("`atlasctl help` does not mention ATLASCACHE_AUTH, which is the way to pass a token that `ps` does not show")
	}

	// And per-command help, which is where the arguments are actually
	// documented.
	result, err = cli.runWith(c, nil, cli.env, "help", commandSet)
	if err != nil {
		return err
	}
	if result.code != ctlOK || !strings.Contains(string(result.stdout), "--ttl") {
		return fmt.Errorf("%s: `atlasctl help set` must exit 0 and document --ttl", result)
	}
	return nil
}

// cliValuesRoundTripThroughStdout is the binary-safety assertion.
//
// `atlasctl get k > file` has to reproduce the bytes that were stored, and the
// ways to break it are all things somebody does on purpose: appending a
// newline, escaping a control character, decoding as UTF-8 and re-encoding.
// Each of those is a correct thing to do on a terminal, which is why the
// decision has to be made from what stdout is rather than from a flag — and
// stdout here is a pipe, as it is in every script that will ever run this.
func cliValuesRoundTripThroughStdout(c *runner.Ctx) error {
	cli, err := newCtl(c)
	if err != nil {
		return err
	}

	// A null byte, a two-byte sequence that is not valid UTF-8, a CRLF, and a
	// trailing newline that must not be mistaken for one the CLI added.
	value := []byte{0x00, 'h', 'i', 0xff, 0xfe, '\r', '\n', 0x7f, 'z', '\n'}

	// --stdin, because an argument list cannot carry a null byte at all: the
	// kernel terminates arguments with one. A CLI without a stdin path simply
	// cannot store this value.
	result, err := cli.runWith(c, value, cli.env, commandSet, "cli:binary", "--stdin")
	if err != nil {
		return err
	}
	if result.code != ctlOK {
		return fmt.Errorf("%s: storing a binary value from stdin must succeed", result)
	}

	stdout, err := cli.expectOK(c, commandGet, "cli:binary")
	if err != nil {
		return err
	}
	if !bytes.Equal(stdout, value) {
		return fmt.Errorf("a binary value came back as %q (%d bytes), want %q (%d bytes)",
			stdout, len(stdout), value, len(value))
	}
	c.Logf("%d bytes with a null, invalid UTF-8 and a CRLF round-tripped through stdout unchanged", len(value))

	if err := cliEmptyAndMissing(c, cli); err != nil {
		return err
	}
	return cliBinaryValueThroughJSON(c, cli, value)
}

// cliEmptyAndMissing is the distinction only the exit code can carry.
func cliEmptyAndMissing(c *runner.Ctx, cli *ctl) error {
	// An empty value is a value: it prints nothing and succeeds, where a
	// missing key prints nothing and fails. Only the exit code separates them.
	if err := cli.expectLine(c, "OK", commandSet, "cli:empty", ""); err != nil {
		return err
	}
	result, err := cli.run(c, commandGet, "cli:empty")
	if err != nil {
		return err
	}
	if result.code != ctlOK || len(result.stdout) != 0 {
		return fmt.Errorf("%s: a key holding an empty value must print nothing and exit 0", result)
	}

	result, err = cli.run(c, commandGet, "cli:never-written")
	if err != nil {
		return err
	}
	if result.code != ctlFailure || len(result.stdout) != 0 {
		return fmt.Errorf("%s: a missing key must print nothing and exit %d", result, ctlFailure)
	}
	c.Logf("an empty value and a missing key are distinguishable by exit code alone")
	return nil
}

// cliBinaryValueThroughJSON checks the other way the same bytes come out.
//
// A JSON string cannot hold arbitrary bytes, so the CLI says which encoding it
// used rather than mangling the value into something that parses.
func cliBinaryValueThroughJSON(c *runner.Ctx, cli *ctl, value []byte) error {
	stdout, err := cli.expectOK(c, commandGet, "cli:binary", flagJSON)
	if err != nil {
		return err
	}
	var document struct {
		OK   bool `json:"ok"`
		Data struct {
			Value    string `json:"value"`
			Encoding string `json:"encoding"`
		} `json:"data"`
	}
	if unmarshalErr := json.Unmarshal(stdout, &document); unmarshalErr != nil {
		return fmt.Errorf("--json output is not JSON: %w (%q)", unmarshalErr, stdout)
	}
	if document.Data.Encoding != "base64" {
		return fmt.Errorf("a value that is not valid UTF-8 was reported as %q encoded", document.Data.Encoding)
	}
	decoded, err := base64.StdEncoding.DecodeString(document.Data.Value)
	if err != nil {
		return fmt.Errorf("the base64 in --json output does not decode: %w", err)
	}
	if !bytes.Equal(decoded, value) {
		return fmt.Errorf("--json carried %q, want %q", decoded, value)
	}
	return nil
}

// cliJSONIsStableAndParsable checks the contract a script depends on.
func cliJSONIsStableAndParsable(c *runner.Ctx) error {
	cli, err := newCtl(c)
	if err != nil {
		return err
	}

	if err := cli.expectLine(c, "OK", commandSet, keyJSON, "value"); err != nil {
		return err
	}

	successes := [][]string{
		{commandPing},
		{commandGet, keyJSON},
		{commandSet, keyJSON, "value"},
		{commandExists, keyJSON},
		{commandKeys, keyJSON},
		{commandScan, "--match", keyJSON},
		{commandStats},
		{commandInfo, "server"},
		{commandDel, keyJSON},
	}
	for _, args := range successes {
		result, runErr := cli.run(c, append(args, flagJSON)...)
		if runErr != nil {
			return runErr
		}
		if result.code != ctlOK {
			return fmt.Errorf("%s: want exit 0", result)
		}

		var document map[string]any
		if err := json.Unmarshal(result.stdout, &document); err != nil {
			return fmt.Errorf("--json output of `%s` is not JSON: %w (%q)", strings.Join(args, " "), err, result.stdout)
		}
		if document["ok"] != true {
			return fmt.Errorf("`%s --json` succeeded but reported ok=%v", strings.Join(args, " "), document["ok"])
		}
		if document["command"] != args[0] {
			return fmt.Errorf("`%s --json` reported command=%v", strings.Join(args, " "), document["command"])
		}
		if _, ok := document["data"]; !ok {
			return fmt.Errorf("`%s --json` carried no data object", strings.Join(args, " "))
		}
	}

	return cliJSONOnFailure(c, cli)
}

// cliJSONOnFailure is the half that matters.
//
// A client that prints JSON when things work and a bare error string when they
// do not breaks every script precisely in the case the script was written to
// handle. So each failure is parsed, and its code has to match the process's
// own exit status — the two are the same fact, and a script may read either.
func cliJSONOnFailure(c *runner.Ctx, cli *ctl) error {
	dead, err := deadPort()
	if err != nil {
		return err
	}

	failures := []struct {
		what string
		args []string
		code int
		kind string
	}{
		{"a missing key", []string{commandGet, "cli:never-written", flagJSON}, ctlFailure, "not_found"},
		{"an unknown flag", []string{commandPing, "--nosuchflag", flagJSON}, ctlUsage, kindUsageJSON},
		{"an unknown command", []string{"nosuchcommand", flagJSON}, ctlUsage, kindUsageJSON},
		{"a missing argument", []string{commandGet, flagJSON}, ctlUsage, kindUsageJSON},
		{"an unreachable server", []string{flagAddr, dead, commandPing, flagJSON}, ctlConnection, "connection"},
	}

	for _, failure := range failures {
		result, runErr := cli.run(c, failure.args...)
		if runErr != nil {
			return runErr
		}

		var document struct {
			OK      bool   `json:"ok"`
			Command string `json:"command"`
			Error   *struct {
				Code    int    `json:"code"`
				Kind    string `json:"kind"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if unmarshalErr := json.Unmarshal(result.stdout, &document); unmarshalErr != nil {
			return fmt.Errorf("%s produced %q on stdout, which is not JSON: %w",
				failure.what, result.stdout, unmarshalErr)
		}
		if document.OK {
			return fmt.Errorf("%s reported ok=true", failure.what)
		}
		if document.Error == nil {
			return fmt.Errorf("%s carried no error object: %q", failure.what, result.stdout)
		}
		if document.Error.Kind != failure.kind {
			return fmt.Errorf("%s reported kind %q, want %q", failure.what, document.Error.Kind, failure.kind)
		}
		if document.Error.Code != failure.code {
			return fmt.Errorf("%s reported code %d, want %d", failure.what, document.Error.Code, failure.code)
		}
		if result.code != failure.code {
			return fmt.Errorf("%s exited %d while its JSON said %d; the two must agree",
				failure.what, result.code, document.Error.Code)
		}
		if document.Error.Message == "" {
			return fmt.Errorf("%s carried an empty message", failure.what)
		}
	}
	c.Logf("every failure came back as JSON with a code matching the exit status")
	return nil
}

// cliAuthPrefersTheEnvironment checks that a token can be passed without
// putting it in `ps`, and that the flag which does says so.
func cliAuthPrefersTheEnvironment(c *runner.Ctx) error {
	cli, newErr := newCtl(c)
	if newErr != nil {
		return newErr
	}
	token, tokenErr := specToken(c)
	if tokenErr != nil {
		return tokenErr
	}
	if token == "" {
		return errors.New("this scenario needs a spec with auth.token set")
	}

	// The environment variable, which is what the documentation recommends.
	if err := cli.expectLine(c, "PONG", commandPing); err != nil {
		return fmt.Errorf("with ATLASCACHE_AUTH set: %w", err)
	}
	if err := cli.expectLine(c, "OK", commandSet, "cli:auth", "v"); err != nil {
		return fmt.Errorf("with ATLASCACHE_AUTH set: %w", err)
	}

	if err := cliRefusesBadCredentials(c, cli); err != nil {
		return err
	}

	// The flag works, and warns. The warning goes to stderr, so a script
	// reading stdout is unaffected by it — which is what makes it safe to
	// always print.
	result, err := cli.runWith(c, nil, bareEnvironment(), "--auth", token, commandGet, "cli:auth")
	if err != nil {
		return err
	}
	if result.code != ctlOK || string(result.stdout) != "v" {
		return fmt.Errorf("%s: --auth with the right token must work", result)
	}
	if !strings.Contains(result.stderr, "ATLASCACHE_AUTH") {
		return fmt.Errorf("--auth did not warn about the environment variable it should have used: %q", result.stderr)
	}
	if !strings.Contains(result.stderr, "ps") {
		return fmt.Errorf("the --auth warning does not say why it matters: %q", result.stderr)
	}
	c.Logf("--auth works and warns: %q", strings.TrimSpace(result.stderr))
	return nil
}

// bareEnvironment is an environment with no credential in it.
func bareEnvironment() []string {
	return []string{"PATH=" + os.Getenv("PATH")}
}

// cliRefusesBadCredentials checks that a missing or wrong token is a command
// failure rather than a connection one. A script must be able to tell "my token
// is wrong" from "the server is down", because only one is worth retrying.
func cliRefusesBadCredentials(c *runner.Ctx, cli *ctl) error {
	bare := bareEnvironment()

	result, err := cli.runWith(c, nil, bare, commandGet, "cli:auth")
	if err != nil {
		return err
	}
	if result.code != ctlFailure {
		return fmt.Errorf("%s: an unauthenticated command must exit %d", result, ctlFailure)
	}

	result, err = cli.runWith(c, nil, bare, commandGet, "cli:auth", flagJSON)
	if err != nil {
		return err
	}
	var document struct {
		Error struct {
			Kind string `json:"kind"`
		} `json:"error"`
	}
	if unmarshalErr := json.Unmarshal(result.stdout, &document); unmarshalErr != nil {
		return fmt.Errorf("an unauthenticated command produced %q, which is not JSON: %w",
			result.stdout, unmarshalErr)
	}
	if document.Error.Kind != kindAuthJSON {
		return fmt.Errorf("an unauthenticated command reported kind %q, want auth", document.Error.Kind)
	}

	// A wrong token is the same answer: the socket worked, the credential did
	// not.
	result, err = cli.runWith(c, nil, bare, "--auth", "not-the-token", commandGet, "cli:auth")
	if err != nil {
		return err
	}
	if result.code != ctlFailure {
		return fmt.Errorf("%s: a wrong token must exit %d", result, ctlFailure)
	}
	return nil
}

// cliExitCodesDistinguishOutcomes asserts the four outcomes and, as much as the
// codes themselves, that they are four.
//
// Conflating "the key is missing" with "the server is unreachable" is the
// common failure this table exists to prevent: a script that retries the first
// wastes its time, and a script that gives up on the second loses data.
func cliExitCodesDistinguishOutcomes(c *runner.Ctx) error {
	cli, err := newCtl(c)
	if err != nil {
		return err
	}
	dead, err := deadPort()
	if err != nil {
		return err
	}

	if err := cli.expectLine(c, "OK", commandSet, keyExit, "v"); err != nil {
		return err
	}

	outcomes := []struct {
		what string
		args []string
		want int
	}{
		{"a command that worked", []string{commandGet, keyExit}, ctlOK},
		{"a key that is not there", []string{commandGet, "exit:missing"}, ctlFailure},
		{"a flag that does not exist", []string{commandGet, keyExit, "--nosuchflag"}, ctlUsage},
		{"a server that is not listening", []string{flagAddr, dead, commandGet, keyExit}, ctlConnection},
	}

	seen := map[int]string{}
	for _, outcome := range outcomes {
		result, runErr := cli.run(c, outcome.args...)
		if runErr != nil {
			return runErr
		}
		if result.code != outcome.want {
			return fmt.Errorf("%s exited %d, want %d", outcome.what, result.code, outcome.want)
		}
		if previous, clash := seen[result.code]; clash {
			return fmt.Errorf("%s and %s both exit %d; the codes must distinguish them",
				previous, outcome.what, result.code)
		}
		seen[result.code] = outcome.what
	}
	c.Logf("four outcomes, four codes: %v", seen)

	if err := cliUsageFamilyExitsTwo(c, cli); err != nil {
		return err
	}
	return cliConnectionFailuresExitThree(c, cli, dead)
}

// cliUsageFamilyExitsTwo walks the mistakes a script author makes when they
// edit a command line, which is the code least likely to have been thought
// about and the one they will hit.
func cliUsageFamilyExitsTwo(c *runner.Ctx, cli *ctl) error {
	usageCases := [][]string{
		{"nosuchcommand"},
		{commandGet},
		{commandGet, "too", "many"},
		{commandSet, "only-a-key"},
		{commandDel},
		{commandExists},
		{commandPing, "unexpected-argument"},
		{commandSet, "k", "v", "--ttl", "not-a-duration"},
		{commandScan, "--count", "-1"},
		{commandGet, "k", "--timeout", "-1s"},
	}
	for _, args := range usageCases {
		result, runErr := cli.run(c, args...)
		if runErr != nil {
			return runErr
		}
		if result.code != ctlUsage {
			return fmt.Errorf("`atlasctl %s` exited %d, want the usage code %d",
				strings.Join(args, " "), result.code, ctlUsage)
		}
		if len(result.stdout) != 0 {
			return fmt.Errorf("`atlasctl %s` wrote %q to stdout; a usage error is a diagnostic",
				strings.Join(args, " "), result.stdout)
		}
		if strings.TrimSpace(result.stderr) == "" {
			return fmt.Errorf("`atlasctl %s` failed silently", strings.Join(args, " "))
		}
	}

	return nil
}

// cliConnectionFailuresExitThree checks that the code does not depend on which
// command hit the unreachable server.
func cliConnectionFailuresExitThree(c *runner.Ctx, cli *ctl, dead string) error {
	for _, args := range [][]string{{commandPing}, {commandStats}, {commandInfo}, {commandKeys, "*"}, {commandSet, "k", "v"}} {
		result, runErr := cli.run(c, append([]string{flagAddr, dead}, args...)...)
		if runErr != nil {
			return runErr
		}
		if result.code != ctlConnection {
			return fmt.Errorf("`atlasctl %s` against a dead port exited %d, want %d",
				strings.Join(args, " "), result.code, ctlConnection)
		}
	}
	return nil
}

// nonEmptyLines splits output into the lines that carry something.
func nonEmptyLines(text string) []string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	return lines
}
