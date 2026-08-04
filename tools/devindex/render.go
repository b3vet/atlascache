package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const indexHeader = "# Index\n" +
	"\n" +
	"> Generated from front-matter by `tools/devindex`. Do not hand-edit — run\n" +
	"> `make dev-index`. `devindex --check` fails when this file is stale, and\n" +
	"> `phase-check` runs it.\n"

const emptySection = "_None._\n"

// severityRank orders the issue table: highest severity first.
var severityRank = map[string]int{"critical": 0, "high": 1, "medium": 2, "low": 3}

// renderIndex produces the complete INDEX.md content. It is a pure function of
// the parsed tree — no timestamps, no host state — so `--check` compares content
// and nothing else.
func renderIndex(t *devTree) string {
	var b strings.Builder
	b.WriteString(indexHeader)
	renderSummary(&b, t)
	renderPhases(&b, t)
	renderFeatures(&b, t)
	renderIssues(&b, t)
	renderDecisions(&b, t)
	renderLogs(&b, t)
	return b.String()
}

func section(b *strings.Builder, name string, first bool) {
	if !first {
		b.WriteString("\n---\n")
	}
	fmt.Fprintf(b, "\n## %s\n\n", name)
}

// tableHeader writes a header row and its separator, sizing the separator to the
// column labels the way the rest of dev/ writes tables by hand.
func tableHeader(b *strings.Builder, columns ...string) {
	fmt.Fprintf(b, "| %s |\n", strings.Join(columns, " | "))
	b.WriteString("|")
	for _, c := range columns {
		b.WriteString(strings.Repeat("-", utf8.RuneCountInString(c)+2) + "|")
	}
	b.WriteString("\n")
}

func row(b *strings.Builder, cells ...string) {
	fmt.Fprintf(b, "| %s |\n", strings.Join(cells, " | "))
}

func renderSummary(b *strings.Builder, t *devTree) {
	section(b, "Summary", true)

	tableHeader(b, append(append([]string{"Type"}, itemStatuses...), "total")...)
	writeCountRow(b, "Features", t.Features)
	writeCountRow(b, "Issues", t.Issues)

	fmt.Fprintf(b, "\n**Decisions**: %s\n", statusSummary(t.ADRs, adrStatuses))
}

func writeCountRow(b *strings.Builder, label string, items []item) {
	counts := countByStatus(items)
	cells := make([]string, 0, len(itemStatuses)+2)
	cells = append(cells, label)
	for _, s := range itemStatuses {
		cells = append(cells, strconv.Itoa(counts[s]))
	}
	cells = append(cells, strconv.Itoa(len(items)))
	row(b, cells...)
}

// statusSummary renders "25 accepted · 2 superseded (27 total)", omitting the
// total when a single status accounts for everything.
func statusSummary(items []item, vocabulary []string) string {
	if len(items) == 0 {
		return "none"
	}
	counts := countByStatus(items)
	var parts []string
	for _, s := range vocabulary {
		if counts[s] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[s], s))
		}
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return fmt.Sprintf("%s (%d total)", strings.Join(parts, " · "), len(items))
}

func countByStatus(items []item) map[string]int {
	counts := map[string]int{}
	for _, it := range items {
		counts[it.Status]++
	}
	return counts
}

func renderPhases(b *strings.Builder, t *devTree) {
	section(b, "Phases", false)
	if len(t.Phases) == 0 {
		b.WriteString(emptySection)
		return
	}
	byPhase := t.featuresByPhase()
	tableHeader(b, "ID", "Title", "Version", "Status", "Features", "Plan")
	for _, p := range t.Phases {
		row(b, p.ID, cell(p.Title), cell(p.Milestone), p.Status,
			strconv.Itoa(len(byPhase[p.ID])), "`"+p.Path+"`")
	}
	b.WriteString("\nA phase with no plan file under `phases/` is not planned yet; `SOW.md`\n" +
		"carries the full release arc.\n")
}

func renderFeatures(b *strings.Builder, t *devTree) {
	section(b, "Features", false)
	if len(t.Features) == 0 {
		b.WriteString(emptySection)
		return
	}
	titles := map[string]string{}
	for _, p := range t.Phases {
		titles[p.ID] = p.Title
	}
	byPhase := t.featuresByPhase()
	first := true
	for _, id := range t.phaseOrder() {
		features := byPhase[id]
		if len(features) == 0 {
			continue
		}
		if !first {
			b.WriteString("\n")
		}
		first = false
		title, ok := titles[id]
		if !ok {
			title = "(no plan file)"
		}
		fmt.Fprintf(b, "### %s — %s\n\n", id, cell(title))
		tableHeader(b, "ID", "Title", "Status")
		for _, f := range features {
			row(b, ref(f.ID), cell(f.Title), status(f.Status))
		}
	}
}

func renderIssues(b *strings.Builder, t *devTree) {
	section(b, "Issues", false)
	if len(t.Issues) == 0 {
		b.WriteString(emptySection)
		return
	}
	// Severity descending, then ID: the table opens with what matters.
	issues := make([]item, len(t.Issues))
	copy(issues, t.Issues)
	sort.SliceStable(issues, func(i, j int) bool {
		if severityRank[issues[i].Severity] != severityRank[issues[j].Severity] {
			return severityRank[issues[i].Severity] < severityRank[issues[j].Severity]
		}
		return lessByID(issues[i].ID, issues[j].ID)
	})

	tableHeader(b, "ID", "Severity", "Title", "Phase", "Status")
	for _, is := range issues {
		row(b, ref(is.ID), is.Severity, cell(is.Title), cell(is.Phase), status(is.Status))
	}
}

func renderDecisions(b *strings.Builder, t *devTree) {
	section(b, "Decisions", false)
	if len(t.ADRs) == 0 {
		b.WriteString(emptySection)
		return
	}
	tableHeader(b, "ID", "Title", "Status")
	for _, a := range t.ADRs {
		row(b, ref(a.ID), cell(a.Title), a.Status)
	}
}

func renderLogs(b *strings.Builder, t *devTree) {
	section(b, "Work logs", false)
	if len(t.Logs) == 0 {
		b.WriteString(emptySection)
		return
	}
	tableHeader(b, "Date", "Phase", "Touched")
	for _, l := range t.Logs {
		row(b, fmt.Sprintf("[%s](%s)", l.Date, l.Path), cell(l.Phase),
			plural(len(l.Touched), "item", "items"))
	}
}

// ref renders an ID as the [[FEAT-0001]] cross-reference form used throughout dev/.
func ref(id string) string {
	return "[[" + id + "]]"
}

// status emphasizes done so a finished item is visible when scanning.
func status(s string) string {
	if s == statusDone {
		return "**done**"
	}
	return s
}

// cell makes a front-matter value safe to drop into a markdown table.
func cell(s string) string {
	if s == "" {
		return "—"
	}
	return strings.ReplaceAll(s, "|", `\|`)
}
