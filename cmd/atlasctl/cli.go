package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/b3vet/atlascache/pkg/client"
)

// Defaults. The address is the SDK's own, repeated here so that --help states
// it rather than making a reader go and look.
const (
	defaultAddr    = "127.0.0.1:6379"
	defaultTimeout = 5 * time.Second

	// envAuth is the preferred way to pass a token: a flag is visible in `ps`
	// to every user on the host, and an environment variable is not.
	envAuth = "ATLASCACHE_AUTH"

	// The two words that are both a command and a flag, because people write
	// them both ways and neither spelling should be a usage error.
	nameHelp    = "help"
	nameVersion = "version"

	// flagJSON is spelled out here because the pre-parse scan below has to
	// recognize it before any flag set exists.
	flagJSON = "--json"
)

// env is everything the CLI touches outside itself, so that a test can drive
// the whole program without a terminal, a real stdout, or an environment.
type env struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer

	// stdoutIsTTY decides whether a value is escaped before it is printed.
	// Escaping is a courtesy to a human reading a terminal and a corruption of
	// anything else, so it is decided here once and never guessed at again.
	stdoutIsTTY bool

	getenv func(string) string
}

func newEnv() *env {
	return &env{
		stdin:       os.Stdin,
		stdout:      os.Stdout,
		stderr:      os.Stderr,
		stdoutIsTTY: isTerminal(os.Stdout),
		getenv:      os.Getenv,
	}
}

// isTerminal reports whether f is a character device, which is what a terminal
// is and what a pipe, a file and /dev/null are not.
//
// It is done with os.Stat rather than a dependency because the question is this
// coarse: the only thing riding on the answer is whether bytes are escaped for
// a human, and a false negative degrades to the correct, unescaped output.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// invocation is one run of the program, rendered by report once it is over.
type invocation struct {
	command string
	asJSON  bool
	res     result
	err     error
}

// run executes one command line and returns the process exit code.
func run(e *env, args []string) int {
	inv := execute(e, args)
	if inv.err != nil {
		return e.reportError(inv)
	}
	return e.reportSuccess(inv)
}

// execute parses the command line, runs the command, and reports what happened.
// Nothing is printed here: rendering belongs to report, which is the one place
// that knows whether the caller asked for JSON.
func execute(e *env, args []string) invocation {
	// Settled before the flags are parsed, so that a command line too broken to
	// parse is still answered in the format the caller asked for.
	inv := invocation{asJSON: wantsJSON(args)}

	before, name, rest := splitCommand(args)

	switch {
	case name == nameHelp || contains(before, "--"+nameHelp, "-"+nameHelp, "-h"):
		return invocation{command: nameHelp, res: helpResult(e, rest)}
	case name == nameVersion || contains(before, "--"+nameVersion, "-"+nameVersion, "-v"):
		return invocation{command: nameVersion, asJSON: inv.asJSON, res: versionResult()}
	case name == "":
		inv.err = usageErrorf("no command given; run `atlasctl help` for the list")
		return inv
	}

	cmd, ok := lookupCommand(name)
	if !ok {
		inv.command = name
		inv.err = usageErrorf("unknown command %q; run `atlasctl help` for the list", name)
		return inv
	}
	inv.command = cmd.name

	var g globals
	fs := flag.NewFlagSet("atlasctl "+cmd.name, flag.ContinueOnError)
	// The flag package's own error output would go to stderr unprefixed and
	// with a usage dump; this prints one line and an exit code instead.
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	g.register(fs)
	runCommand := cmd.setup(fs)

	if err := fs.Parse(permute(fs, append(before, rest...))); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return invocation{command: cmd.name, res: commandHelpResult(cmd)}
		}
		inv.err = usageErrorf("%v; run `atlasctl help %s`", err, cmd.name)
		return inv
	}
	inv.asJSON = g.asJSON

	c, err := g.newClient(g.resolveToken(e))
	if err != nil {
		inv.err = err
		return inv
	}
	defer func() { _ = c.Close() }()

	ctx, cancel := g.context()
	defer cancel()

	inv.res, inv.err = runCommand(ctx, e, c, fs.Args())
	return inv
}

// splitCommand finds the command name in a line whose global flags may come
// before it, after it, or both.
//
// `atlasctl --addr host:port get k` and `atlasctl get k --addr host:port` are
// the same command, because a user should not have to know where a flag goes.
// Telling the two apart needs only the global flag set, which is known before
// the command is: a token that is one of those flags consumes its value, and
// the first token left over is the command.
func splitCommand(args []string) (before []string, name string, rest []string) {
	var g globals
	fs := flag.NewFlagSet("atlasctl", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	g.register(fs)

	for i := 0; i < len(args); i++ {
		arg := args[i]

		if arg == "--" {
			if i+1 < len(args) {
				return before, args[i+1], args[i+2:]
			}
			return before, "", nil
		}
		if len(arg) > 1 && arg[0] == '-' {
			before = append(before, arg)
			flagName, hasValue := cutFlagName(arg)
			known := fs.Lookup(flagName)
			if known != nil && !hasValue && !isBoolFlag(known) && i+1 < len(args) {
				i++
				before = append(before, args[i])
			}
			continue
		}
		return before, arg, args[i+1:]
	}
	return before, "", nil
}

func contains(args []string, wanted ...string) bool {
	for _, arg := range args {
		for _, want := range wanted {
			if arg == want {
				return true
			}
		}
	}
	return false
}

// globals are the flags every command takes.
type globals struct {
	addr    string
	auth    string
	useTLS  bool
	tlsCA   string
	asJSON  bool
	timeout time.Duration
}

// register binds the global flags to fs.
//
// They are registered on each command's own flag set rather than on a separate
// one parsed first, so that `atlasctl get k --json` and `atlasctl --json get k`
// both work. A user should not have to know where a flag goes.
func (g *globals) register(fs *flag.FlagSet) {
	fs.StringVar(&g.addr, "addr", defaultAddr, "server address, as host:port")
	fs.StringVar(&g.auth, "auth", "", "authentication `token`; prefer "+envAuth+", which a process listing does not show")
	fs.BoolVar(&g.useTLS, "tls", false, "connect with TLS")
	fs.StringVar(&g.tlsCA, "tls-ca", "", "PEM file of certificate authorities to verify the server against (implies --tls)")
	fs.BoolVar(&g.asJSON, "json", false, "print one JSON object instead of text")
	fs.DurationVar(&g.timeout, "timeout", defaultTimeout, "time limit for the whole command; 0 means none")
}

// resolveToken picks the token to authenticate with, and warns about the way
// that leaks it.
func (g *globals) resolveToken(e *env) string {
	if g.auth != "" {
		// Not suppressible, and deliberately so: the whole point is that the
		// person who typed it may not know that `ps` shows it to everyone with
		// an account on this host, or that it is now in their shell history.
		fmt.Fprintf(e.stderr, "atlasctl: warning: --auth is visible to other users in `ps` output; prefer the %s environment variable\n", envAuth)
		return g.auth
	}
	return e.getenv(envAuth)
}

// context bounds the whole command, including the dial.
func (g *globals) context() (context.Context, context.CancelFunc) {
	if g.timeout <= 0 {
		return context.WithCancel(context.Background())
	}
	return context.WithTimeout(context.Background(), g.timeout)
}

// newClient builds the SDK client this command runs on.
//
// Every option failure is a usage error: the only way to reach one is to have
// typed something the SDK rejects, and reporting that as a command failure
// would tell a script to retry a command line that will never work.
func (g *globals) newClient(token string) (client.Client, error) {
	if g.timeout < 0 {
		return nil, usageErrorf("--timeout must not be negative, got %s", g.timeout)
	}

	opts := []client.Option{
		client.WithAddr(g.addr),
		// One connection, because a CLI runs one command. It also keeps a
		// SCAN's cursor on the connection that issued it (ADR-0017), which a
		// larger pool would not guarantee across the pages of one iteration.
		client.WithPoolSize(1),
		client.WithDialTimeout(g.timeout),
		client.WithReadTimeout(g.timeout),
		client.WithWriteTimeout(g.timeout),
		// No reconnect window. The SDK's default spends five seconds retrying a
		// dial, which is right for a long-running process riding out a restart
		// and wrong for a command whose caller is waiting: a script that wants
		// to retry can, and one that wants to know now is told now.
		client.WithReconnectWindow(0),
	}
	if token != "" {
		opts = append(opts, client.WithAuth(token))
	}

	tlsConfig, err := g.tlsConfig()
	if err != nil {
		return nil, err
	}
	if tlsConfig != nil {
		opts = append(opts, client.WithTLS(tlsConfig))
	}

	c, err := client.New(opts...)
	if err != nil {
		return nil, usageErrorf("%v", err)
	}
	return c, nil
}

// lookupCommand finds a command by name.
func lookupCommand(name string) (command, bool) {
	for _, cmd := range commands() {
		if cmd.name == name {
			return cmd, true
		}
	}
	return command{}, false
}

// wantsJSON reports whether the command line asks for JSON, without parsing it.
//
// It exists for the failures that happen before parsing can succeed — an
// unknown flag, a missing argument — which are exactly the ones a script most
// needs to be able to read. Everything after a bare `--` is an operand rather
// than a flag, so the scan stops there.
func wantsJSON(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		switch arg {
		case flagJSON, "-json", flagJSON + "=true", "-json=true":
			return true
		}
	}
	return false
}

// permute moves flags ahead of operands so that `atlasctl get k --json` parses.
//
// Go's flag package stops at the first operand, which would make the flag above
// a third argument to get rather than a flag — a rule nobody expects and one
// that produces a confusing error rather than a hint. The flag set itself says
// which names exist and which of them take a value, so this needs no table of
// its own; a name it does not recognize is left where it is, for Parse to
// report.
func permute(fs *flag.FlagSet, args []string) []string {
	flags := make([]string, 0, len(args))
	operands := make([]string, 0, len(args))

	for i := 0; i < len(args); i++ {
		arg := args[i]

		if arg == "--" {
			// Everything after it is an operand, by definition, including the
			// values that look like flags.
			operands = append(operands, args[i+1:]...)
			break
		}
		if len(arg) < 2 || arg[0] != '-' {
			operands = append(operands, arg)
			continue
		}

		name, hasValue := cutFlagName(arg)
		found := fs.Lookup(name)
		switch {
		case found == nil:
			// Unknown, or an operand that merely looks like a flag. Either way
			// it is not this function's business to decide.
			flags = append(flags, arg)
		case hasValue || isBoolFlag(found):
			flags = append(flags, arg)
		case i+1 < len(args):
			// A value-taking flag written as two words.
			flags = append(flags, arg, args[i+1])
			i++
		default:
			// Missing its value; Parse says so better than this could.
			flags = append(flags, arg)
		}
	}

	return append(flags, operands...)
}

// cutFlagName splits "--count=10" into its name and whether a value came with
// it. Leading dashes are stripped, so the one- and two-dash spellings the flag
// package accepts are the same flag here too.
func cutFlagName(arg string) (name string, hasValue bool) {
	name, _, hasValue = strings.Cut(strings.TrimLeft(arg, "-"), "=")
	return name, hasValue
}

// isBoolFlag reports whether a flag may be written without a value, which is
// the question that decides whether the next argument belongs to it.
func isBoolFlag(f *flag.Flag) bool {
	boolFlag, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && boolFlag.IsBoolFlag()
}

// helpResult renders `atlasctl help` or `atlasctl help <command>`.
func helpResult(e *env, args []string) result {
	if len(args) > 0 {
		if cmd, ok := lookupCommand(args[0]); ok {
			return commandHelpResult(cmd)
		}
		fmt.Fprintf(e.stderr, "atlasctl: unknown command %q\n", args[0])
	}
	return result{
		data: map[string]any{"commands": commandNames()},
		text: func(o *output) error { return o.write(usageText()) },
	}
}

func commandHelpResult(cmd command) result {
	return result{
		data: map[string]any{"command": cmd.name, "usage": cmd.synopsis},
		text: func(o *output) error { return o.write(cmd.helpText()) },
	}
}

func versionResult() result {
	return result{
		data: map[string]any{"version": version, "commit": commit, "built": date},
		text: func(o *output) error {
			return o.write(fmt.Sprintf("atlasctl %s\ncommit: %s\nbuilt:  %s\n", version, commit, date))
		},
	}
}

func commandNames() []string {
	names := make([]string, 0, len(commands()))
	for _, cmd := range commands() {
		names = append(names, cmd.name)
	}
	sort.Strings(names)
	return names
}

// usageText is what `atlasctl help` prints. It is written out rather than
// generated from the flag set because a generated one lists flags in an order
// nobody chose and explains nothing.
func usageText() string {
	var b strings.Builder
	b.WriteString("atlasctl is the AtlasCache command-line client.\n\n")
	b.WriteString("Usage:\n  atlasctl [flags] <command> [arguments]\n\nCommands:\n")

	width := 0
	for _, cmd := range commands() {
		if len(cmd.name) > width {
			width = len(cmd.name)
		}
	}
	for _, cmd := range commands() {
		fmt.Fprintf(&b, "  %-*s  %s\n", width, cmd.name, cmd.summary)
	}

	b.WriteString("\nGlobal flags:\n")
	b.WriteString(globalFlagHelp())
	fmt.Fprintf(&b, "\nEnvironment:\n  %s  the authentication token; preferred over --auth, which `ps` shows to every user\n", envAuth)
	b.WriteString("\nExit codes:\n")
	b.WriteString("  0  success\n")
	b.WriteString("  1  the command failed: a missing key, a server error, a refused token\n")
	b.WriteString("  2  usage: an unknown flag, a missing argument\n")
	b.WriteString("  3  the server could not be reached, or did not answer in time\n")
	b.WriteString("\nRun `atlasctl help <command>` for one command.\n")
	return b.String()
}

// globalFlagHelp renders the global flags, which every command shares.
func globalFlagHelp() string {
	var g globals
	fs := flag.NewFlagSet("atlasctl", flag.ContinueOnError)
	g.register(fs)
	return flagHelp(fs)
}

// flagHelp renders a flag set the way `atlasctl help` lays it out.
func flagHelp(fs *flag.FlagSet) string {
	var b strings.Builder
	fs.VisitAll(func(f *flag.Flag) {
		name, usage := flag.UnquoteUsage(f)
		line := "  --" + f.Name
		if name != "" {
			line += " " + name
		}
		fmt.Fprintf(&b, "%-24s  %s", line, usage)
		if f.DefValue != "" && f.DefValue != "false" && f.DefValue != "0s" {
			fmt.Fprintf(&b, " (default %s)", f.DefValue)
		}
		b.WriteString("\n")
	})
	return b.String()
}
