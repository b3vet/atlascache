// Command atlas-e2e runs AtlasCache E2E specs against a real server process.
//
//	atlas-e2e --tier smoke               # run one tier
//	atlas-e2e --tier full --parallel 8   # bounded concurrency
//	atlas-e2e --spec ping-basic          # a single spec while iterating
//	atlas-e2e --list                     # enumerate specs and tiers
//
// It exits 0 when every selected spec passed and 1 otherwise. That exit code is
// what CI and phase-check read.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/b3vet/atlascache/test/e2e/harness"
	"github.com/b3vet/atlascache/test/e2e/runner"
	"github.com/b3vet/atlascache/test/e2e/runner/fakeharness"
	_ "github.com/b3vet/atlascache/test/e2e/scenarios"
)

type options struct {
	specsDir string
	tier     string
	spec     string
	list     bool
	parallel int
	asJSON   bool
	binary   string
	harness  string
	timeout  time.Duration
}

const (
	harnessProcess = "process"
	harnessFake    = "fake"

	// tierAll selects every tier, which is what a run with no --tier does.
	tierAll = "all"
)

func main() {
	opts, err := parseFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "atlas-e2e: %v\n", err)
		os.Exit(1)
	}
	if err := run(opts, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "atlas-e2e: %v\n", err)
		os.Exit(1)
	}
}

func parseFlags(args []string) (options, error) {
	var opts options
	fs := flag.NewFlagSet("atlas-e2e", flag.ContinueOnError)
	fs.StringVar(&opts.specsDir, "specs", "specs", "directory holding the spec files")
	fs.StringVar(&opts.tier, "tier", "", "run only this tier: smoke, full, soak, or all")
	fs.StringVar(&opts.spec, "spec", "", "run exactly one spec, by name")
	fs.BoolVar(&opts.list, "list", false, "list the specs instead of running them")
	fs.IntVar(&opts.parallel, "parallel", defaultParallel(), "how many specs may run at once")
	fs.BoolVar(&opts.asJSON, "json", false, "emit a machine-readable report")
	fs.StringVar(&opts.binary, "binary", "bin/atlascache", "path to the server binary under test")
	fs.StringVar(&opts.harness, "harness", harnessProcess,
		"harness to run specs against: process, or fake for runner self-tests")
	fs.DurationVar(&opts.timeout, "timeout", runner.DefaultSpecTimeout, "time limit for one spec")

	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if fs.NArg() > 0 {
		return opts, fmt.Errorf("unexpected argument %q; every option is a flag", fs.Arg(0))
	}
	if opts.tier != "" && opts.spec != "" {
		return opts, errors.New("--tier and --spec select different things; pass one or the other")
	}
	if opts.tier != "" && opts.tier != tierAll && !runner.Tier(opts.tier).Valid() {
		return opts, fmt.Errorf("unknown tier %q; pass smoke, full, soak or all", opts.tier)
	}
	if opts.parallel < 1 {
		return opts, fmt.Errorf("--parallel must be at least 1, got %d", opts.parallel)
	}
	if opts.harness != harnessProcess && opts.harness != harnessFake {
		return opts, fmt.Errorf("unknown harness %q; pass process or fake", opts.harness)
	}
	return opts, nil
}

func defaultParallel() int {
	if n := runtime.NumCPU(); n < 8 {
		return n
	}
	return 8
}

func run(opts options, out, errOut io.Writer) error {
	specs, err := runner.LoadDir(opts.specsDir)
	if err != nil {
		return err
	}
	if len(specs) == 0 {
		return fmt.Errorf("no specs found in %s; a run that asserts nothing must not report success", opts.specsDir)
	}

	if opts.list {
		return list(opts, specs, out)
	}

	selected, err := selectSpecs(opts, specs)
	if err != nil {
		return err
	}
	if len(selected) == 0 {
		// An empty tier is legitimate: soak stays empty until v0.2.
		fmt.Fprintf(out, "0 specs matched tier %s\n", opts.tier)
		return nil
	}

	return execute(opts, selected, out, errOut)
}

func list(opts options, specs []*runner.Spec, out io.Writer) error {
	if opts.tier != "" && opts.tier != tierAll {
		specs = runner.FilterTier(specs, runner.Tier(opts.tier))
	}
	rows := runner.Listing(specs)
	if opts.asJSON {
		return runner.WriteListingJSON(out, rows)
	}
	return runner.WriteListing(out, rows)
}

func selectSpecs(opts options, specs []*runner.Spec) ([]*runner.Spec, error) {
	if opts.spec != "" {
		spec, err := runner.FindSpec(specs, opts.spec)
		if err != nil {
			return nil, err
		}
		return []*runner.Spec{spec}, nil
	}
	if opts.tier == "" || opts.tier == tierAll {
		return specs, nil
	}
	return runner.FilterTier(specs, runner.Tier(opts.tier)), nil
}

func execute(opts options, specs []*runner.Spec, out, errOut io.Writer) error {
	factory, err := harnessFactory(opts, errOut)
	if err != nil {
		return err
	}

	executor := &runner.Executor{
		Factory:     factory,
		Binary:      opts.binary,
		SpecTimeout: opts.timeout,
	}

	var reporter runner.Reporter
	if opts.asJSON {
		reporter = &runner.JSONReporter{Out: out}
	} else {
		reporter = runner.NewTextReporter(out, specs)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	started := time.Now()
	results := executor.RunAll(ctx, specs, opts.parallel, reporter.SpecFinished)
	elapsed := time.Since(started)

	summary := runner.Summarize(results)
	report := runner.Report{
		Harness:    opts.harness,
		Tier:       opts.tier,
		Parallel:   opts.parallel,
		DurationMS: elapsed.Milliseconds(),
		Summary:    summary,
		Specs:      results,
	}
	if err := reporter.Finish(report); err != nil {
		return err
	}
	if summary.Failed > 0 {
		return fmt.Errorf("%d of %d specs failed", summary.Failed, summary.Total)
	}
	return nil
}

// harnessFactory picks the harness the run drives.
//
// The process harness is the real one and the default: it launches the binary
// under test, on its own ports, against its own data directory. The fake starts
// no server at all and is only for the runner's own tests, so it announces
// itself loudly and is recorded in the report — a green run against the fake
// proves something about the runner and nothing about AtlasCache.
func harnessFactory(opts options, errOut io.Writer) (runner.HarnessFactory, error) {
	switch opts.harness {
	case harnessProcess:
		return harness.Factory(harness.Settings{}), nil
	case harnessFake:
		fmt.Fprintln(errOut, "atlas-e2e: WARNING running against the fake harness; no server is started and no server behavior is proven")
		return fakeFactory, nil
	default:
		return nil, fmt.Errorf("unknown harness %q; pass process or fake", opts.harness)
	}
}

// fakeFactory answers a fixed handful of commands, which is enough to drive
// every step form and every assertion form through the runner without a server.
func fakeFactory(runner.HarnessOptions) (runner.Harness, error) {
	h := fakeharness.New(map[string]runner.Reply{
		"PING":  runner.StatusReply("PONG"),
		"QUIT":  runner.StatusReply("OK"),
		"STATS": runner.MapReply(map[string]runner.Reply{"keys": runner.IntegerReply(2), "expirations": runner.IntegerReply(3)}),
	})
	h.Fallback = runner.ErrorReply("ERR unknown command")
	return h, nil
}
