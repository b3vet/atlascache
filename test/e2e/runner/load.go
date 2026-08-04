package runner

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// LoadDir parses every spec under dir, recursively. Directories whose name
// begins with a dot and directories named testdata are skipped.
//
// Every parse error is reported, not just the first, so one pass fixes the
// whole tree. Spec names must be unique across the tree, since --spec and the
// reports address a spec by name.
func LoadDir(dir string) ([]*Spec, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("spec directory %s: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("spec directory %s is not a directory", dir)
	}

	var (
		specs    []*Spec
		problems []error
	)
	walkErr := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != dir && skipDir(entry.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if ext := filepath.Ext(entry.Name()); ext != ".yaml" && ext != ".yml" {
			return nil
		}
		data, err := os.ReadFile(path) //nolint:gosec // paths come from the spec directory being scanned
		if err != nil {
			problems = append(problems, err)
			return nil
		}
		spec, err := ParseSpec(path, data)
		if err != nil {
			problems = append(problems, err)
			return nil
		}
		specs = append(specs, spec)
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("scanning %s: %w", dir, walkErr)
	}

	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	problems = append(problems, duplicateNames(specs)...)

	if err := errors.Join(problems...); err != nil {
		return nil, err
	}
	return specs, nil
}

func skipDir(name string) bool {
	return strings.HasPrefix(name, ".") || name == "testdata"
}

func duplicateNames(specs []*Spec) []error {
	seen := map[string]string{}
	var problems []error
	for _, spec := range specs {
		if first, exists := seen[spec.Name]; exists {
			problems = append(problems, fmt.Errorf("%s: spec name %q is already used by %s", spec.Path, spec.Name, first))
			continue
		}
		seen[spec.Name] = spec.Path
	}
	return problems
}

// FilterTier returns the specs at or below the given tier. An empty tier
// returns all of them, which is what a run without --tier does.
//
// Tiers are cumulative: full includes smoke, and soak includes both. A gate
// that ran only its own tier would let `--tier full` — the PR and phase-close
// gate — skip every smoke spec, so the fastest and most fundamental checks
// would be the ones never run before merging.
func FilterTier(specs []*Spec, tier Tier) []*Spec {
	if tier == "" {
		return specs
	}
	max := tierRank(tier)
	filtered := make([]*Spec, 0, len(specs))
	for _, spec := range specs {
		if tierRank(spec.Tier) <= max {
			filtered = append(filtered, spec)
		}
	}
	return filtered
}

// tierRank orders the tiers so a run can include everything below it.
func tierRank(t Tier) int {
	switch t {
	case TierSmoke:
		return 0
	case TierFull:
		return 1
	case TierSoak:
		return 2
	default:
		return -1
	}
}

// FindSpec returns the single spec with the given name.
func FindSpec(specs []*Spec, name string) (*Spec, error) {
	for _, spec := range specs {
		if spec.Name == name {
			return spec, nil
		}
	}
	names := make([]string, len(specs))
	for i, spec := range specs {
		names[i] = spec.Name
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no spec named %q; no specs were found at all", name)
	}
	return nil, fmt.Errorf("no spec named %q; known specs: %s", name, strings.Join(names, ", "))
}
