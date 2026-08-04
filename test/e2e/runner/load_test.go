package runner

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLoadDir(t *testing.T) {
	specs, err := LoadDir(filepath.Join("testdata", "specs"))
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}

	want := []string{"lifecycle", "ping-basic", "soak-idle", "stats-counters"}
	if len(specs) != len(want) {
		t.Fatalf("loaded %d specs, want %d", len(specs), len(want))
	}
	for i, name := range want {
		if specs[i].Name != name {
			t.Errorf("spec %d = %q, want %q (specs must come back sorted)", i, specs[i].Name, name)
		}
	}
	if specs[0].Path == "" {
		t.Error("a spec must remember the file it came from")
	}
}

func TestLoadDirReportsEveryBadSpec(t *testing.T) {
	_, err := LoadDir(filepath.Join("testdata", "invalid"))
	if err == nil {
		t.Fatal("LoadDir accepted a directory of broken specs")
	}
	for _, want := range []string{"bad-tier.yaml", "unknown-field.yaml", "missing-name.yaml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %s, got:\n%s", want, err)
		}
	}
}

func TestLoadDirRejectsRepeatedNames(t *testing.T) {
	_, err := LoadDir(filepath.Join("testdata", "duplicates"))
	if err == nil {
		t.Fatal("two specs may not share a name")
	}
	if !strings.Contains(err.Error(), `spec name "repeated-name" is already used by`) {
		t.Errorf("error = %v", err)
	}
}

func TestLoadDirRejectsAMissingDirectory(t *testing.T) {
	if _, err := LoadDir(filepath.Join("testdata", "nope")); err == nil {
		t.Fatal("LoadDir accepted a directory that does not exist")
	}
}

func TestFilterTier(t *testing.T) {
	specs, err := LoadDir(filepath.Join("testdata", "specs"))
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}

	// Tiers are cumulative: each selects its own specs and everything below it.
	if got := FilterTier(specs, TierSmoke); len(got) != 1 || got[0].Name != "ping-basic" {
		t.Errorf("smoke tier = %v", specNames(got))
	}

	full := FilterTier(specs, TierFull)
	if len(full) != 3 {
		t.Errorf("full tier should include the smoke specs, got %v", specNames(full))
	}
	if !slices.ContainsFunc(full, func(s *Spec) bool { return s.Name == "ping-basic" }) {
		t.Errorf("full tier must include smoke spec ping-basic, got %v", specNames(full))
	}
	if slices.ContainsFunc(full, func(s *Spec) bool { return s.Tier == TierSoak }) {
		t.Errorf("full tier must not include soak specs, got %v", specNames(full))
	}

	if got := FilterTier(specs, TierSoak); len(got) != len(specs) {
		t.Errorf("soak tier should include every spec, got %v", specNames(got))
	}
	if got := FilterTier(specs, ""); len(got) != len(specs) {
		t.Errorf("an empty tier should select every spec, got %v", specNames(got))
	}
}

func TestFindSpec(t *testing.T) {
	specs, err := LoadDir(filepath.Join("testdata", "specs"))
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}

	spec, err := FindSpec(specs, "ping-basic")
	if err != nil {
		t.Fatalf("FindSpec: %v", err)
	}
	if spec.Name != "ping-basic" {
		t.Errorf("found %q", spec.Name)
	}

	_, err = FindSpec(specs, "ping-bsaic")
	if err == nil {
		t.Fatal("FindSpec accepted a name that does not exist")
	}
	if !strings.Contains(err.Error(), "known specs: lifecycle, ping-basic") {
		t.Errorf("error should list the known specs, got %v", err)
	}
}

func specNames(specs []*Spec) []string {
	names := make([]string, len(specs))
	for i, spec := range specs {
		names[i] = spec.Name
	}
	return names
}
