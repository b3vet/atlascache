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

// writeTree materializes a fixture dev tree and returns its root.
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

func featureFile(id, title, status, phase string) string {
	return fmt.Sprintf(`---
id: %s
title: %s
status: %s
phase: %s
milestone: v0.1.0
depends_on: []
adr: []
e2e: []
---

## Intent

Fixture.
`, id, title, status, phase)
}

func issueFile(id, title, status, severity, phase string) string {
	return fmt.Sprintf(`---
id: %s
title: %s
status: %s
type: bug
severity: %s
phase: %s
milestone: v0.1.0
found_in: initial audit (abc1234)
e2e: []
---

## Problem

Fixture.
`, id, title, status, severity, phase)
}

func adrFile(id, title, status string) string {
	return fmt.Sprintf(`---
id: %s
title: %s
status: %s
date: 2026-08-04
supersedes: []
superseded_by: []
---

## Context

Fixture.
`, id, title, status)
}

func phaseFile(id, title, milestone, status string, features []string) string {
	return fmt.Sprintf(`---
id: %s
title: %s
milestone: %s
status: %s
depends_on: []
features: [%s]
---

# %s

Fixture.
`, id, title, milestone, status, strings.Join(features, ", "), id)
}

func logFile(date, phase string, touched []string) string {
	return fmt.Sprintf(`---
session: %s
phase: %s
touched: [%s]
---

## Done

Fixture.
`, date, phase, strings.Join(touched, ", "))
}

// standardTree is a small but complete dev tree exercising every section.
func standardTree(t *testing.T) string {
	t.Helper()
	return writeTree(t, map[string]string{
		"features/FEAT-0001.md": featureFile("FEAT-0001", "Doc system", "done", "P0"),
		"features/FEAT-0002.md": featureFile("FEAT-0002", "Repair the build", "planned", "P0"),
		"features/FEAT-0003.md": featureFile("FEAT-0003", "Time wheel", "blocked", "P1"),
		"issues/ISSUE-0001.md":  issueFile("ISSUE-0001", "Clean clone fails", "in-progress", "high", "P0"),
		"issues/ISSUE-0002.md":  issueFile("ISSUE-0002", "Memory grows without bound", "planned", "critical", "P1"),
		"issues/ISSUE-0003.md":  issueFile("ISSUE-0003", "Stats walks twice", "done", "medium", "P1"),
		"adr/ADR-0001.md":       adrFile("ADR-0001", "Planning lives in dev/", "accepted"),
		"adr/ADR-0002.md":       adrFile("ADR-0002", "Items are ID'd markdown files", "superseded"),
		"phases/PHASE-P0-FOUNDATION.md": phaseFile("P0", "Foundation", "v0.1.0", "in-progress",
			[]string{"FEAT-0001", "FEAT-0002"}),
		"phases/PHASE-P1-CORE.md": phaseFile("P1", "Core Engine", "v0.2.0", "planned",
			[]string{"FEAT-0003"}),
		"log/2026-08-04.md": logFile("2026-08-04", "P0", []string{"FEAT-0001", "ISSUE-0001"}),
	})
}

func TestSummaryCounts(t *testing.T) {
	tree, err := loadTree(standardTree(t))
	require.NoError(t, err)

	out := renderIndex(tree)

	assert.Contains(t, out, "| Type | planned | in-progress | blocked | done | dropped | total |")
	assert.Contains(t, out, "| Features | 1 | 0 | 1 | 1 | 0 | 3 |")
	assert.Contains(t, out, "| Issues | 1 | 1 | 0 | 1 | 0 | 3 |")
	assert.Contains(t, out, "**Decisions**: 1 accepted · 1 superseded (2 total)")
}

func TestPhaseTable(t *testing.T) {
	tree, err := loadTree(standardTree(t))
	require.NoError(t, err)

	out := renderIndex(tree)

	assert.Contains(t, out, "| P0 | Foundation | v0.1.0 | in-progress | 2 | `phases/PHASE-P0-FOUNDATION.md` |")
	assert.Contains(t, out, "| P1 | Core Engine | v0.2.0 | planned | 1 | `phases/PHASE-P1-CORE.md` |")
	assert.Less(t, strings.Index(out, "| P0 |"), strings.Index(out, "| P1 |"), "phases must be in numeric order")
}

func TestFeaturesGroupedByPhaseInIDOrder(t *testing.T) {
	tree, err := loadTree(standardTree(t))
	require.NoError(t, err)

	out := renderIndex(tree)

	assert.Contains(t, out, "### P0 — Foundation")
	assert.Contains(t, out, "### P1 — Core Engine")
	assert.Contains(t, out, "| [[FEAT-0001]] | Doc system | **done** |")
	assert.Contains(t, out, "| [[FEAT-0003]] | Time wheel | blocked |")
	assert.Less(t, strings.Index(out, "### P0 —"), strings.Index(out, "### P1 —"))
	assert.Less(t, strings.Index(out, "[[FEAT-0001]]"), strings.Index(out, "[[FEAT-0002]]"))
}

func TestIssuesSortedBySeverityThenID(t *testing.T) {
	root := writeTree(t, map[string]string{
		"issues/ISSUE-0001.md": issueFile("ISSUE-0001", "Low one", "planned", "low", "P0"),
		"issues/ISSUE-0002.md": issueFile("ISSUE-0002", "High two", "planned", "high", "P0"),
		"issues/ISSUE-0003.md": issueFile("ISSUE-0003", "Critical three", "planned", "critical", "P0"),
		"issues/ISSUE-0004.md": issueFile("ISSUE-0004", "High four", "done", "high", "P0"),
		"issues/ISSUE-0010.md": issueFile("ISSUE-0010", "Medium ten", "planned", "medium", "P0"),
		"issues/ISSUE-0009.md": issueFile("ISSUE-0009", "Critical nine", "planned", "critical", "P0"),
	})
	tree, err := loadTree(root)
	require.NoError(t, err)

	var got []string
	for _, line := range strings.Split(renderIndex(tree), "\n") {
		if strings.HasPrefix(line, "| [[ISSUE-") {
			got = append(got, strings.Fields(line)[1])
		}
	}

	// Severity descending, then ID ascending — a done issue keeps its place.
	assert.Equal(t, []string{
		"[[ISSUE-0003]]", "[[ISSUE-0009]]",
		"[[ISSUE-0002]]", "[[ISSUE-0004]]",
		"[[ISSUE-0010]]",
		"[[ISSUE-0001]]",
	}, got)
}

func TestDecisionsAndWorklogs(t *testing.T) {
	tree, err := loadTree(standardTree(t))
	require.NoError(t, err)

	out := renderIndex(tree)

	assert.Contains(t, out, "| [[ADR-0001]] | Planning lives in dev/ | accepted |")
	assert.Contains(t, out, "| [[ADR-0002]] | Items are ID'd markdown files | superseded |")
	assert.Contains(t, out, "| [2026-08-04](log/2026-08-04.md) | P0 | 2 items |")
}

func TestMalformedFrontMatterIsAHardError(t *testing.T) {
	cases := []struct {
		name    string
		file    string
		content string
		reason  string
	}{
		{
			name:    "no front-matter",
			file:    "features/FEAT-0001.md",
			content: "# Just a heading\n",
			reason:  "must start with a --- line",
		},
		{
			name:    "unterminated front-matter",
			file:    "features/FEAT-0001.md",
			content: "---\nid: FEAT-0001\ntitle: No closing delimiter\n",
			reason:  "unterminated",
		},
		{
			name:    "invalid yaml",
			file:    "features/FEAT-0001.md",
			content: "---\nid: FEAT-0001\ntitle: [unclosed\n---\n",
			reason:  "yaml",
		},
		{
			name:    "missing title",
			file:    "features/FEAT-0001.md",
			content: "---\nid: FEAT-0001\nstatus: planned\nphase: P0\n---\n",
			reason:  "missing required field: title",
		},
		{
			name:    "missing phase",
			file:    "features/FEAT-0001.md",
			content: "---\nid: FEAT-0001\ntitle: T\nstatus: planned\n---\n",
			reason:  "missing required field: phase",
		},
		{
			name:    "unknown status",
			file:    "features/FEAT-0001.md",
			content: "---\nid: FEAT-0001\ntitle: T\nstatus: wip\nphase: P0\n---\n",
			reason:  `unknown status "wip"`,
		},
		{
			name:    "id does not match filename",
			file:    "features/FEAT-0001.md",
			content: featureFile("FEAT-0002", "Wrong id", "planned", "P0"),
			reason:  `id "FEAT-0002" does not match the filename`,
		},
		{
			name:    "unknown severity",
			file:    "issues/ISSUE-0001.md",
			content: issueFile("ISSUE-0001", "T", "planned", "urgent", "P0"),
			reason:  `unknown severity "urgent"`,
		},
		{
			name:    "unknown decision status",
			file:    "adr/ADR-0001.md",
			content: adrFile("ADR-0001", "T", "agreed"),
			reason:  `unknown status "agreed"`,
		},
		{
			name:    "worklog session disagrees with filename",
			file:    "log/2026-08-04.md",
			content: logFile("2026-08-03", "P0", nil),
			reason:  "does not match the filename",
		},
		{
			name:    "phase with a bad id",
			file:    "phases/PHASE-BAD.md",
			content: phaseFile("Phase Zero", "T", "v0.1.0", "planned", nil),
			reason:  "is not a phase id",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := writeTree(t, map[string]string{tc.file: tc.content})

			_, err := loadTree(root)

			require.Error(t, err, "malformed front-matter must not be skipped silently")
			assert.Contains(t, err.Error(), tc.file, "the error must name the file")
			assert.Contains(t, err.Error(), tc.reason)
		})
	}
}

func TestDuplicateIDIsAHardError(t *testing.T) {
	root := writeTree(t, map[string]string{
		"features/FEAT-0001.md": featureFile("FEAT-0001", "One", "planned", "P0"),
		"features/FEAT-0002.md": featureFile("FEAT-0002", "Two", "planned", "P0"),
	})
	// Same id, different file: only reachable by editing one after the fact.
	require.NoError(t, os.WriteFile(filepath.Join(root, "features", "FEAT-0002.md"),
		[]byte(featureFile("FEAT-0001", "Copy", "planned", "P0")), 0o600))

	_, err := loadTree(root)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "FEAT-0002.md")
}

func TestMissingSubdirectoriesAreTolerated(t *testing.T) {
	t.Run("only decisions exist", func(t *testing.T) {
		root := writeTree(t, map[string]string{
			"adr/ADR-0001.md": adrFile("ADR-0001", "The first decision", "accepted"),
		})

		tree, err := loadTree(root)
		require.NoError(t, err)

		out := renderIndex(tree)
		assert.Contains(t, out, "| Features | 0 | 0 | 0 | 0 | 0 | 0 |")
		assert.Contains(t, out, "**Decisions**: 1 accepted")
		assert.Contains(t, out, "## Features\n\n_None._")
		assert.Contains(t, out, "## Issues\n\n_None._")
		assert.Contains(t, out, "## Phases\n\n_None._")
		assert.Contains(t, out, "## Work logs\n\n_None._")
	})

	t.Run("no open issues", func(t *testing.T) {
		root := writeTree(t, map[string]string{
			"features/FEAT-0001.md":         featureFile("FEAT-0001", "Only feature", "planned", "P0"),
			"phases/PHASE-P0-FOUNDATION.md": phaseFile("P0", "Foundation", "v0.1.0", "planned", []string{"FEAT-0001"}),
		})

		tree, err := loadTree(root)
		require.NoError(t, err)
		assert.Contains(t, renderIndex(tree), "## Issues\n\n_None._")
	})

	t.Run("an empty tree still renders", func(t *testing.T) {
		root := writeTree(t, map[string]string{})

		tree, err := loadTree(root)
		require.NoError(t, err)
		assert.Contains(t, renderIndex(tree), "**Decisions**: none")
	})
}

func TestDanglingReferencesAreReported(t *testing.T) {
	root := writeTree(t, map[string]string{
		"features/FEAT-0001.md": featureFile("FEAT-0001", "One", "planned", "P0") +
			"\nSee [[FEAT-0099]] and [[ADR-0001]] and [[FEAT-XXXX]].\n",
		"adr/ADR-0001.md": adrFile("ADR-0001", "A decision", "accepted"),
		"SOW.md":          "Refers to [[ISSUE-0042]].\n",
	})
	tree, err := loadTree(root)
	require.NoError(t, err)

	refs, err := danglingRefs(root, tree.knownIDs())
	require.NoError(t, err)

	require.Len(t, refs, 2, "resolvable refs and XXXX placeholders must not be reported")
	assert.Contains(t, refs[0], "SOW.md:1: dangling reference [[ISSUE-0042]]")
	assert.Contains(t, refs[1], "features/FEAT-0001.md")
	assert.Contains(t, refs[1], "dangling reference [[FEAT-0099]]")
}

func TestPhaseFeatureListMismatchWarns(t *testing.T) {
	root := writeTree(t, map[string]string{
		"features/FEAT-0001.md": featureFile("FEAT-0001", "Listed", "planned", "P0"),
		"features/FEAT-0002.md": featureFile("FEAT-0002", "Not listed", "planned", "P0"),
		"features/FEAT-0003.md": featureFile("FEAT-0003", "Phase has no plan", "planned", "P9"),
		"phases/PHASE-P0-FOUNDATION.md": phaseFile("P0", "Foundation", "v0.1.0", "planned",
			[]string{"FEAT-0001", "FEAT-0004"}),
	})
	tree, err := loadTree(root)
	require.NoError(t, err)

	warnings := strings.Join(tree.warnings(), "\n")

	assert.Contains(t, warnings, "phase P9 has no plan file")
	assert.Contains(t, warnings, "lists FEAT-0004, which do not carry phase: P0")
	assert.Contains(t, warnings, "omits FEAT-0002, which carry phase: P0")

	// A feature whose phase has no plan file still appears in the index.
	assert.Contains(t, renderIndex(tree), "### P9 — (no plan file)")
}

func TestRunWithAbsentDevTreeSucceeds(t *testing.T) {
	var stdout, stderr bytes.Buffer
	missing := filepath.Join(t.TempDir(), "dev")

	err := run([]string{"--dir", missing}, &stdout, &stderr)

	require.NoError(t, err, "a public clone has no dev/ and must not fail")
	assert.Contains(t, stdout.String(), "not present, nothing to do")
	assert.Empty(t, stderr.String())
}

func TestRunWritesAndChecksTheIndex(t *testing.T) {
	root := standardTree(t)
	path := filepath.Join(root, indexFile)

	var stdout, stderr bytes.Buffer
	require.NoError(t, run([]string{"--dir", root}, &stdout, &stderr))
	assert.Contains(t, stdout.String(), "3 features, 3 issues, 2 decisions, 2 phase plans, 1 worklog")

	written, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "# Index\n", string(written)[:len("# Index\n")])

	t.Run("check passes on a current index", func(t *testing.T) {
		var out, errOut bytes.Buffer
		require.NoError(t, run([]string{"--dir", root, "--check"}, &out, &errOut))
		assert.Contains(t, out.String(), "is up to date")
	})

	t.Run("check fails on a hand-edited index", func(t *testing.T) {
		edited := strings.Replace(string(written), "Doc system", "Doc system (edited)", 1)
		require.NoError(t, os.WriteFile(path, []byte(edited), 0o600))

		var out, errOut bytes.Buffer
		err := run([]string{"--dir", root, "--check"}, &out, &errOut)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "out of date")
		assert.Contains(t, err.Error(), "Doc system (edited)")
	})

	t.Run("check fails when an item changes status", func(t *testing.T) {
		require.NoError(t, run([]string{"--dir", root}, &bytes.Buffer{}, &bytes.Buffer{}))
		require.NoError(t, os.WriteFile(filepath.Join(root, "features", "FEAT-0002.md"),
			[]byte(featureFile("FEAT-0002", "Repair the build", "done", "P0")), 0o600))

		err := run([]string{"--dir", root, "--check"}, &bytes.Buffer{}, &bytes.Buffer{})

		require.Error(t, err)
		assert.Contains(t, err.Error(), "out of date")
	})

	t.Run("check fails when the index is missing", func(t *testing.T) {
		require.NoError(t, os.Remove(path))

		err := run([]string{"--dir", root, "--check"}, &bytes.Buffer{}, &bytes.Buffer{})

		require.Error(t, err)
		assert.Contains(t, err.Error(), "does not exist")
	})
}

func TestRunReportsWarningsOnStderr(t *testing.T) {
	root := writeTree(t, map[string]string{
		"features/FEAT-0001.md": featureFile("FEAT-0001", "One", "planned", "P0") +
			"\nSee [[ISSUE-0404]].\n",
	})

	var stdout, stderr bytes.Buffer
	require.NoError(t, run([]string{"--dir", root}, &stdout, &stderr))

	assert.Contains(t, stderr.String(), "warning: features/FEAT-0001.md")
	assert.Contains(t, stderr.String(), "dangling reference [[ISSUE-0404]]")
	assert.Contains(t, stderr.String(), "phase P0 has no plan file")
}

func TestRenderIsStableAcrossRuns(t *testing.T) {
	root := standardTree(t)
	tree, err := loadTree(root)
	require.NoError(t, err)

	first := renderIndex(tree)
	reloaded, err := loadTree(root)
	require.NoError(t, err)

	assert.Equal(t, first, renderIndex(reloaded), "output must not depend on time or map order")
	assert.NotContains(t, first, "Last generated", "a timestamp would make --check fail on an unchanged tree")
}
