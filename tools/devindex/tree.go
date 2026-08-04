package main

import (
	"fmt"
	"sort"
	"strings"
)

// knownIDs returns every ID that a [[reference]] may resolve to.
func (t *devTree) knownIDs() map[string]bool {
	ids := make(map[string]bool, len(t.Features)+len(t.Issues)+len(t.ADRs))
	for _, group := range [][]item{t.Features, t.Issues, t.ADRs} {
		for _, it := range group {
			ids[it.ID] = true
		}
	}
	return ids
}

// featuresByPhase groups features by their phase field, preserving ID order.
func (t *devTree) featuresByPhase() map[string][]item {
	byPhase := map[string][]item{}
	for _, f := range t.Features {
		byPhase[f.Phase] = append(byPhase[f.Phase], f)
	}
	return byPhase
}

// phaseOrder lists every phase that has features or a plan file: the planned
// phases in numeric order, then any phase referenced only by a feature.
func (t *devTree) phaseOrder() []string {
	var order []string
	seen := map[string]bool{}
	for _, p := range t.Phases {
		order = append(order, p.ID)
		seen[p.ID] = true
	}
	var orphans []string
	for id := range t.featuresByPhase() {
		if !seen[id] {
			orphans = append(orphans, id)
		}
	}
	sort.Slice(orphans, func(i, j int) bool { return phaseNumber(orphans[i]) < phaseNumber(orphans[j]) })
	return append(order, orphans...)
}

// warnings reports inconsistencies that are worth knowing but do not make the
// index wrong: a feature pointing at a phase with no plan, or a plan whose
// feature list disagrees with the features themselves.
func (t *devTree) warnings() []string {
	var out []string
	byPhase := t.featuresByPhase()
	planned := map[string]bool{}
	for _, p := range t.Phases {
		planned[p.ID] = true
	}

	var orphanPhases []string
	for id := range byPhase {
		if !planned[id] {
			orphanPhases = append(orphanPhases, id)
		}
	}
	sort.Strings(orphanPhases)
	for _, id := range orphanPhases {
		out = append(out, fmt.Sprintf("phase %s has no plan file in phases/ but %d feature(s) claim it",
			id, len(byPhase[id])))
	}

	for _, p := range t.Phases {
		listed := map[string]bool{}
		for _, id := range p.Features {
			listed[id] = true
		}
		actual := map[string]bool{}
		for _, f := range byPhase[p.ID] {
			actual[f.ID] = true
		}
		if missing := diffIDs(listed, actual); len(missing) > 0 {
			out = append(out, fmt.Sprintf("%s lists %s, which do not carry phase: %s",
				p.Path, strings.Join(missing, ", "), p.ID))
		}
		if extra := diffIDs(actual, listed); len(extra) > 0 {
			out = append(out, fmt.Sprintf("%s omits %s, which carry phase: %s",
				p.Path, strings.Join(extra, ", "), p.ID))
		}
	}
	return out
}

// diffIDs returns the sorted IDs present in a but not in b.
func diffIDs(a, b map[string]bool) []string {
	var out []string
	for id := range a {
		if !b[id] {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return lessByID(out[i], out[j]) })
	return out
}

// tally is the one-line summary printed after a successful write.
func (t *devTree) tally() string {
	return strings.Join([]string{
		plural(len(t.Features), "feature", "features"),
		plural(len(t.Issues), "issue", "issues"),
		plural(len(t.ADRs), "decision", "decisions"),
		plural(len(t.Phases), "phase plan", "phase plans"),
		plural(len(t.Logs), "worklog", "worklogs"),
	}, ", ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
