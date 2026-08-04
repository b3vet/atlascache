package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeTree materializes a fixture tree and returns its root.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
	}
	return root
}

// featureFile renders a feature the way dev/features/ writes them. verification
// is the body of the ## Verification section; empty means the heading is there
// with nothing under it, which is what the gate has to catch.
func featureFile(id, status, phase string, e2e []string, verification string) string {
	return fmt.Sprintf(`---
id: %s
title: %s
status: %s
phase: %s
milestone: v0.1.0
depends_on: []
adr: []
e2e: [%s]
---

## Intent

Fixture.

## Verification

%s
`, id, id, status, phase, strings.Join(e2e, ", "), verification)
}

const changelogWithContent = `# Changelog

## [Unreleased]

### Added

- A thing that happened.

[Unreleased]: https://example.invalid/commits/main
`

// devTree is a complete, passing fixture: every P0 feature done, every feature
// without E2E specs carrying a real verification section, a worklog naming the
// phase.
func devTree(t *testing.T) string {
	t.Helper()
	return writeTree(t, map[string]string{
		"features/FEAT-0001.md": featureFile("FEAT-0001", "done", "P0", nil, "Unit tests over fixture trees."),
		"features/FEAT-0002.md": featureFile("FEAT-0002", "done", "P0", []string{"ping-basic"}, ""),
		"features/FEAT-0003.md": featureFile("FEAT-0003", "planned", "P1", []string{"ttl-basic"}, ""),
		"log/2026-08-04.md":     "---\nsession: 2026-08-04\nphase: P0\ntouched: [FEAT-0001]\n---\n\n## Done\n\nFixture.\n",
	})
}

// repoTree is the part of the repository the gate reads directly.
func repoTree(t *testing.T) string {
	t.Helper()
	return writeTree(t, map[string]string{"CHANGELOG.md": changelogWithContent})
}

// stubRun answers every subprocess check successfully, with a coverage total
// well clear of any threshold. Tests that care about a specific command
// override it.
func stubRun() commandFunc {
	return func(_, name string, args ...string) commandResult {
		if name == "go" && len(args) > 1 && args[0] == "tool" && args[1] == "cover" {
			return commandResult{Output: "some.go:1:\tf\t100.0%\ntotal:\t(statements)\t91.0%\n"}
		}
		return commandResult{}
	}
}

func testGate(t *testing.T, phase, devDir, repoDir string, run commandFunc) *gate {
	t.Helper()
	g := newGate(phase, devDir, repoDir)
	g.run = run
	g.lookPath = func(name string) (string, error) { return "/fake/bin/" + name, nil }
	return g
}

func TestPhaseWithEveryFeatureDonePasses(t *testing.T) {
	g := testGate(t, "P0", devTree(t), repoTree(t), stubRun())

	var out bytes.Buffer
	results := report(&out, g)

	assert.Zero(t, count(results, fail), "a complete phase must not fail a check:\n%s", out.String())
	assert.Zero(t, count(results, skip), "every dependency is present in this fixture:\n%s", out.String())
	assert.Contains(t, out.String(), "PHASE P0: READY TO CLOSE")
	assert.Contains(t, out.String(), "[x] FEATs in P0 done      2/2 done")
	assert.NotContains(t, out.String(), "Failed:")
}

func TestUnfinishedFeatureFailsAndIsNamed(t *testing.T) {
	dev := devTree(t)
	require.NoError(t, os.WriteFile(filepath.Join(dev, "features", "FEAT-0002.md"),
		[]byte(featureFile("FEAT-0002", "in-progress", "P0", []string{"ping-basic"}, "")), 0o600))

	g := testGate(t, "P0", dev, repoTree(t), stubRun())

	var out bytes.Buffer
	results := report(&out, g)

	require.Equal(t, 1, count(results, fail))
	assert.Contains(t, out.String(), "PHASE P0: NOT READY TO CLOSE (1 failed)")
	assert.Contains(t, out.String(), "1/2 done — FEAT-0002 (in-progress)",
		"the row must name the feature and its status, not merely say FAIL")
	assert.Contains(t, out.String(), "Failed:\n  FEATs in P0 done:")
}

func TestBlockedAndPlannedFeaturesAreAllNamed(t *testing.T) {
	dev := writeTree(t, map[string]string{
		"features/FEAT-0001.md": featureFile("FEAT-0001", "done", "P1", []string{"ttl-basic"}, ""),
		"features/FEAT-0002.md": featureFile("FEAT-0002", "planned", "P1", []string{"ttl-long"}, ""),
		"features/FEAT-0003.md": featureFile("FEAT-0003", "blocked", "P1", []string{"ttl-update"}, ""),
		"features/FEAT-0004.md": featureFile("FEAT-0004", "dropped", "P1", []string{"ttl-drop"}, ""),
		"log/2026-08-04.md":     "---\nsession: 2026-08-04\nphase: P1\n---\n\nFixture.\n",
	})
	g := testGate(t, "P1", dev, repoTree(t), stubRun())

	out, detail := g.checkFeatures()

	assert.Equal(t, fail, out)
	assert.Contains(t, detail, "FEAT-0002 (planned)")
	assert.Contains(t, detail, "FEAT-0003 (blocked)")
	assert.Contains(t, detail, "1/3 done, 1 dropped",
		"a dropped feature is not outstanding work and must not be counted as such")
	assert.NotContains(t, detail, "FEAT-0004")
}

func TestFeatureWithNoSpecsNeedsAVerificationSection(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"empty section", featureFile("FEAT-0002", "done", "P0", nil, "")},
		{"whitespace only", featureFile("FEAT-0002", "done", "P0", nil, "   \n\n")},
		{"html comment only", featureFile("FEAT-0002", "done", "P0", nil, "<!-- TODO -->")},
		{
			name: "no section at all",
			content: "---\nid: FEAT-0002\ntitle: T\nstatus: done\nphase: P0\ne2e: []\n---\n\n" +
				"## Intent\n\nFixture.\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dev := devTree(t)
			require.NoError(t, os.WriteFile(filepath.Join(dev, "features", "FEAT-0002.md"),
				[]byte(tc.content), 0o600))

			g := testGate(t, "P0", dev, repoTree(t), stubRun())
			out, detail := g.checkVerification()

			assert.Equal(t, fail, out, "e2e: [] must not be a way to skip testing")
			assert.Contains(t, detail, "FEAT-0002")
			assert.Contains(t, detail, "## Verification")
		})
	}
}

func TestVerificationCheckIgnoresFeaturesThatNameSpecs(t *testing.T) {
	dev := writeTree(t, map[string]string{
		// No verification section, but it names a spec, so the E2E suite is its proof.
		"features/FEAT-0001.md": "---\nid: FEAT-0001\ntitle: T\nstatus: done\nphase: P0\ne2e: [ping-basic]\n---\n\n## Intent\n\nFixture.\n",
	})
	g := testGate(t, "P0", dev, repoTree(t), stubRun())

	out, detail := g.checkVerification()

	assert.Equal(t, pass, out)
	assert.Contains(t, detail, "no feature in P0 declares e2e: []")
}

func TestAbsentDevTreeSkipsRatherThanFails(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "dev")
	g := testGate(t, "P0", missing, repoTree(t), stubRun())

	var out bytes.Buffer
	results := report(&out, g)

	require.Zero(t, count(results, fail), "a public clone has no dev/ and must still pass:\n%s", out.String())
	assert.Contains(t, out.String(), "PHASE P0: READY TO CLOSE (4 skipped)")

	skipped := map[string]bool{}
	for _, r := range results {
		if r.Outcome == skip {
			skipped[r.Name] = true
			assert.Contains(t, r.Detail, "not present", "a skip must name the missing dependency")
		}
	}
	assert.Equal(t, map[string]bool{
		"FEATs in P0 done":     true,
		"verification present": true,
		"worklog entry":        true,
		"dev/INDEX.md current": true,
	}, skipped, "only the dev-reading checks may skip; the repository checks still run")
}

func TestMalformedDevTreeFailsRatherThanSkips(t *testing.T) {
	dev := writeTree(t, map[string]string{
		"features/FEAT-0001.md": "# No front-matter here\n",
	})
	g := testGate(t, "P0", dev, repoTree(t), stubRun())

	out, detail := g.checkFeatures()

	assert.Equal(t, fail, out, "a tree that exists but cannot be read must not be waved through")
	assert.Contains(t, detail, "FEAT-0001.md")
	assert.Contains(t, detail, "must start with a --- line")
}

func TestFeatureCheckFailsWhenThePhaseHasNoFeatures(t *testing.T) {
	g := testGate(t, "P9", devTree(t), repoTree(t), stubRun())

	out, detail := g.checkFeatures()

	assert.Equal(t, fail, out, "an unknown phase is a typo, not a finished phase")
	assert.Contains(t, detail, "carries phase: P9")
}

func TestWorklogMustMentionThePhase(t *testing.T) {
	t.Run("front-matter phase counts", func(t *testing.T) {
		g := testGate(t, "P0", devTree(t), repoTree(t), stubRun())

		out, detail := g.checkWorklog()

		assert.Equal(t, pass, out)
		assert.Equal(t, "log/2026-08-04.md", detail)
	})

	t.Run("a mention in the body counts", func(t *testing.T) {
		dev := writeTree(t, map[string]string{
			"log/2026-08-05.md": "---\nsession: 2026-08-05\nphase: P1\n---\n\nClosed out P0 as well.\n",
		})
		g := testGate(t, "P0", dev, repoTree(t), stubRun())

		out, _ := g.checkWorklog()
		assert.Equal(t, pass, out)
	})

	t.Run("a different phase does not count", func(t *testing.T) {
		dev := writeTree(t, map[string]string{
			"log/2026-08-05.md": "---\nsession: 2026-08-05\nphase: P1\n---\n\nWorked on P10 and P11.\n",
		})
		g := testGate(t, "P0", dev, repoTree(t), stubRun())

		out, detail := g.checkWorklog()

		assert.Equal(t, fail, out)
		assert.Contains(t, detail, "no file in")
		assert.Contains(t, detail, "P0")
	})

	t.Run("the newest of several entries is named", func(t *testing.T) {
		dev := writeTree(t, map[string]string{
			"log/2026-08-04.md": "---\nsession: 2026-08-04\nphase: P0\n---\n\nFixture.\n",
			"log/2026-08-06.md": "---\nsession: 2026-08-06\nphase: P0\n---\n\nFixture.\n",
		})
		g := testGate(t, "P0", dev, repoTree(t), stubRun())

		out, detail := g.checkWorklog()

		assert.Equal(t, pass, out)
		assert.Equal(t, "2 entries, newest log/2026-08-06.md", detail)
	})
}

func TestChangelogUnreleasedSection(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    outcome
		detail  string
	}{
		{"has entries", changelogWithContent, pass, "`[Unreleased]` has 1 entry"},
		{
			name:    "empty section",
			content: "# Changelog\n\n## [Unreleased]\n\n## [0.1.0]\n\n- Released.\n",
			want:    fail,
			detail:  "empty",
		},
		{
			name:    "only its own link definition",
			content: "# Changelog\n\n## [Unreleased]\n\n[Unreleased]: https://example.invalid/commits/main\n",
			want:    fail,
			detail:  "empty",
		},
		{
			name:    "no unreleased section",
			content: "# Changelog\n\n## [0.1.0]\n\n- Released.\n",
			want:    fail,
			detail:  "no `## [Unreleased]` section",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := writeTree(t, map[string]string{"CHANGELOG.md": tc.content})
			g := testGate(t, "P0", devTree(t), repo, stubRun())

			out, detail := g.checkChangelog()

			assert.Equal(t, tc.want, out)
			assert.Contains(t, detail, tc.detail)
		})
	}

	t.Run("an absent changelog skips", func(t *testing.T) {
		g := testGate(t, "P0", devTree(t), t.TempDir(), stubRun())

		out, detail := g.checkChangelog()

		assert.Equal(t, skip, out)
		assert.Contains(t, detail, "CHANGELOG.md not present")
	})
}

func TestCoverageThresholdIsPerPhase(t *testing.T) {
	assert.InDelta(t, 60.0, thresholdFor("P0"), 0.001, "P0 is scaffolding")
	for _, phase := range []string{"P1", "P2", "P5", "P10"} {
		assert.InDelta(t, 80.0, thresholdFor(phase), 0.001, phase)
	}
}

func TestCoverageFailureNamesTheModuleAndTheNumbers(t *testing.T) {
	run := func(dir, name string, args ...string) commandResult {
		if name == "go" && len(args) > 1 && args[0] == "tool" && args[1] == "cover" {
			total := "91.0"
			if !strings.HasSuffix(filepath.Clean(dir), filepath.Join("test", "e2e")) {
				total = "74.1"
			}
			return commandResult{Output: "total:\t(statements)\t" + total + "%\n"}
		}
		return commandResult{}
	}

	t.Run("below the P1 threshold", func(t *testing.T) {
		g := testGate(t, "P1", devTree(t), repoTree(t), run)

		out, detail := g.checkCoverage()

		assert.Equal(t, fail, out)
		assert.Equal(t, "root 74.1% < 80% (measured: root 74.1%, test/e2e 91.0%)", detail,
			"the row must state the shortfall and keep the passing module's figure visible")
	})

	t.Run("every module short states each shortfall once", func(t *testing.T) {
		low := func(dir, name string, args ...string) commandResult {
			if name == "go" && len(args) > 1 && args[0] == "tool" && args[1] == "cover" {
				return commandResult{Output: "total:\t(statements)\t12.5%\n"}
			}
			return commandResult{}
		}
		g := testGate(t, "P1", devTree(t), repoTree(t), low)

		out, detail := g.checkCoverage()

		assert.Equal(t, fail, out)
		assert.Equal(t, "root 12.5% < 80%, test/e2e 12.5% < 80%", detail)
	})

	t.Run("the same numbers clear the P0 threshold", func(t *testing.T) {
		g := testGate(t, "P0", devTree(t), repoTree(t), run)

		out, detail := g.checkCoverage()

		assert.Equal(t, pass, out)
		assert.Equal(t, "root 74.1%, test/e2e 91.0%", detail)
	})
}

func TestBothModulesAreTested(t *testing.T) {
	var dirs []string
	run := func(dir, name string, args ...string) commandResult {
		if name == "go" && args[0] == "test" {
			dirs = append(dirs, filepath.ToSlash(dir))
			require.Contains(t, args, "-race")
			require.Contains(t, args, "./...")
		}
		return stubRun()(dir, name, args...)
	}
	g := testGate(t, "P0", devTree(t), "/repo", run)

	out, detail := g.checkRace()

	assert.Equal(t, pass, out)
	assert.Equal(t, "root and test/e2e pass", detail)
	assert.Equal(t, []string{"/repo", "/repo/test/e2e"}, dirs,
		"`go test ./...` in the root module does not reach the e2e module")
}

func TestFailingTestsFailTheRaceCheckAndVoidCoverage(t *testing.T) {
	run := func(dir, name string, args ...string) commandResult {
		if name == "go" && args[0] == "test" && strings.Contains(filepath.ToSlash(dir), "test/e2e") {
			return commandResult{
				Output: "--- FAIL: TestHarness (0.01s)\n    harness_test.go:12: boom\nFAIL\tgithub.com/b3vet/atlascache/test/e2e/runner\t0.2s\nFAIL\n",
				Err:    fmt.Errorf("exit status 1"),
			}
		}
		return stubRun()(dir, name, args...)
	}
	g := testGate(t, "P0", devTree(t), "/repo", run)

	raceOut, raceDetail := g.checkRace()
	coverOut, coverDetail := g.checkCoverage()

	assert.Equal(t, fail, raceOut)
	assert.Contains(t, raceDetail, "test/e2e: --- FAIL: TestHarness")
	assert.Equal(t, fail, coverOut)
	assert.Contains(t, coverDetail, "not measured", "a partial profile is not a coverage figure")
	require.Len(t, g.transcripts, 1, "the failing command's output is kept for the tail")
	assert.Contains(t, g.transcripts[0].Output, "harness_test.go:12: boom")
}

func TestMissingLinterSkipsLoudly(t *testing.T) {
	g := testGate(t, "P0", devTree(t), repoTree(t), stubRun())
	g.lookPath = func(name string) (string, error) {
		return "", fmt.Errorf("%s not found on PATH or in /go/bin", name)
	}

	var out bytes.Buffer
	results := report(&out, g)

	assert.Zero(t, count(results, fail))
	assert.Contains(t, out.String(), "[-] golangci-lint")
	assert.Contains(t, out.String(), "skipped (golangci-lint not found on PATH or in /go/bin; run `make tools`)")
	assert.Contains(t, out.String(), "1 skipped")
}

func TestFailingSubprocessesAreExplained(t *testing.T) {
	run := func(dir, name string, args ...string) commandResult {
		if name == "make" {
			return commandResult{
				Output: "go build ./...\nrunning full tier\nFAIL ttl-basic: expected OK, got ERR\nmake: *** [e2e-full] Error 1\n",
				Err:    fmt.Errorf("exit status 2"),
			}
		}
		return stubRun()(dir, name, args...)
	}
	g := testGate(t, "P0", devTree(t), repoTree(t), run)

	var out bytes.Buffer
	results := report(&out, g)

	require.Equal(t, 1, count(results, fail))
	assert.Contains(t, out.String(), "[!] e2e --tier full       FAIL ttl-basic: expected OK, got ERR")
	assert.Contains(t, out.String(), "--- make e2e-full: last 4 lines of output ---")
	assert.Contains(t, out.String(), "make: *** [e2e-full] Error 1")
}

func TestIndexCheckDelegatesToDevindex(t *testing.T) {
	t.Run("stale index fails with devindex's own diagnosis", func(t *testing.T) {
		run := func(dir, name string, args ...string) commandResult {
			if name == "go" && args[0] == "run" {
				return commandResult{
					Output: "devindex: warning: phase P9 has no plan file\n" +
						"devindex: dev/INDEX.md is out of date; run `make dev-index`\n" +
						"  line 12\n  have: \"a\"\n  want: \"b\"\n",
					Err: fmt.Errorf("exit status 1"),
				}
			}
			return stubRun()(dir, name, args...)
		}
		g := testGate(t, "P0", devTree(t), repoTree(t), run)

		out, detail := g.checkIndex()

		assert.Equal(t, fail, out)
		assert.Equal(t, "dev/INDEX.md is out of date; run `make dev-index`", detail,
			"warnings printed before the error must not be mistaken for it")
	})

	t.Run("current index passes", func(t *testing.T) {
		g := testGate(t, "P0", devTree(t), repoTree(t), stubRun())

		out, detail := g.checkIndex()

		assert.Equal(t, pass, out)
		assert.Equal(t, "up to date", detail)
	})
}

func TestSummaryIgnoresMakesOwnFailureLine(t *testing.T) {
	res := commandResult{
		Output: "cd test/e2e && go build -o ../../bin/atlas-e2e ./cmd/atlas-e2e\n" +
			"./bin/atlas-e2e --tier full --binary ./bin/atlascache --specs test/e2e/specs\n" +
			"atlas-e2e: no specs found in test/e2e/specs\n" +
			"make[1]: *** [e2e-full] Error 1\n",
		Err: fmt.Errorf("exit status 2"),
	}

	assert.Equal(t, "atlas-e2e: no specs found in test/e2e/specs", summarizeFailure(res),
		"`make: *** [target] Error 1` reports that a recipe failed, never why")
}

func TestLintSummaryQuotesTheFirstIssue(t *testing.T) {
	res := commandResult{
		Output: "runner/execute.go:245:2: (*Executor).runScenario - index is unused (unparam)\n" +
			"runner/load.go:12:9: Error return value is not checked (errcheck)\n" +
			"2 issues:\n* unparam: 1\n* errcheck: 1\n",
		Err: fmt.Errorf("exit status 1"),
	}

	assert.Equal(t, "2 issues, first: runner/execute.go:245:2: (*Executor).runScenario - index is unused (unparam)",
		summarizeLint(res), "the per-linter tally at the end names no file and no line")

	single := commandResult{
		Output: "runner/load.go:12:9: Error return value is not checked (errcheck)\n1 issues:\n* errcheck: 1\n",
		Err:    fmt.Errorf("exit status 1"),
	}
	assert.Equal(t, "runner/load.go:12:9: Error return value is not checked (errcheck)", summarizeLint(single))

	// A linter that failed to start reports no issues at all.
	broken := commandResult{Output: "level=error msg=\"can't load config\"\n", Err: fmt.Errorf("exit status 3")}
	assert.Contains(t, summarizeLint(broken), "can't load config")
}

func TestParseCoverageTotal(t *testing.T) {
	total, ok := parseCoverageTotal("a.go:1:\tf\t50.0%\ntotal:\t\t\t\t(statements)\t\t70.7%\n")
	require.True(t, ok)
	assert.InDelta(t, 70.7, total, 0.001)

	_, ok = parseCoverageTotal("a.go:1:\tf\t50.0%\n")
	assert.False(t, ok, "a profile with no total line is not a coverage figure")
}

func TestRunRejectsABadPhase(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		reason string
	}{
		{"no phase", nil, "missing required flag: --phase"},
		{"empty phase", []string{"--phase", ""}, "missing required flag: --phase"},
		{"not a phase id", []string{"--phase", "phase-one"}, "is not a phase id"},
		{"a feature id", []string{"--phase", "FEAT-0006"}, "is not a phase id"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			err := run(tc.args, &stdout, &stderr)

			require.Error(t, err, "an unusable phase must not run a six-minute gate")
			assert.Contains(t, err.Error(), tc.reason)
			assert.Empty(t, stdout.String())
		})
	}
}
