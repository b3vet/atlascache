package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// linkDefinitionPattern matches a markdown link-reference definition, such as
// the `[Unreleased]: https://…` line that sits under the changelog's own
// heading and says nothing about what changed.
var linkDefinitionPattern = regexp.MustCompile(`^\[[^\]]+\]:\s`)

// Statuses this tool has to reason about. devindex owns the full vocabulary and
// rejects anything outside it; the gate only needs to know which statuses stop
// a phase from closing.
const (
	statusDone    = "done"
	statusDropped = "dropped"
)

// changelogFile is the public changelog, relative to the repository root.
const changelogFile = "CHANGELOG.md"

// feature is the part of a dev/features/ item the gate reads: enough front
// matter to decide whether the phase is finished, plus the body, because the
// `## Verification` section is part of the contract for a feature with no E2E
// specs.
type feature struct {
	ID     string
	Status string
	Phase  string
	E2E    []string
	Path   string // relative to the dev tree root
	Body   string
}

// frontMatter mirrors devindex's decoder, narrowed to the fields this tool
// reads. Unknown keys are ignored, so a feature can grow fields without the
// gate needing to hear about them.
type frontMatter struct {
	ID     string   `yaml:"id"`
	Status string   `yaml:"status"`
	Phase  string   `yaml:"phase"`
	E2E    []string `yaml:"e2e"`
}

// loadFeatures parses dev/features/. A missing dev tree returns fs.ErrNotExist
// so the caller can tell "public clone" from "broken front-matter": the first
// skips checks, the second must fail them.
func loadFeatures(devDir string) ([]feature, error) {
	info, err := os.Stat(devDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("stat %s: %w", devDir, fs.ErrNotExist)
	case err != nil:
		return nil, err
	case !info.IsDir():
		return nil, fmt.Errorf("%s is not a directory", devDir)
	}

	dir := filepath.Join(devDir, "features")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var paths []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") || strings.HasPrefix(name, ".") {
			continue
		}
		paths = append(paths, filepath.Join(dir, name))
	}
	sort.Strings(paths)

	features := make([]feature, 0, len(paths))
	for _, path := range paths {
		f, err := readFeature(devDir, path)
		if err != nil {
			return nil, err
		}
		features = append(features, f)
	}
	return features, nil
}

func readFeature(devDir, path string) (feature, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return feature{}, err
	}
	rel := relPath(devDir, path)

	block, body, err := splitFrontMatter(data)
	if err != nil {
		return feature{}, fmt.Errorf("%s: %w", rel, err)
	}
	var fm frontMatter
	if err := yaml.Unmarshal(block, &fm); err != nil {
		return feature{}, fmt.Errorf("%s: %w", rel, err)
	}
	switch {
	case fm.ID == "":
		return feature{}, fmt.Errorf("%s: missing required field: id", rel)
	case fm.Status == "":
		return feature{}, fmt.Errorf("%s: missing required field: status", rel)
	case fm.Phase == "":
		return feature{}, fmt.Errorf("%s: missing required field: phase", rel)
	}

	return feature{
		ID:     fm.ID,
		Status: fm.Status,
		Phase:  fm.Phase,
		E2E:    fm.E2E,
		Path:   rel,
		Body:   body,
	}, nil
}

// splitFrontMatter returns the YAML block delimited by --- at the top of a file
// and everything after it.
func splitFrontMatter(data []byte) (block []byte, body string, err error) {
	const delim = "---"
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != delim {
		return nil, "", errors.New("no YAML front-matter: the file must start with a --- line")
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == delim {
			return []byte(strings.Join(lines[1:i], "\n")), strings.Join(lines[i+1:], "\n"), nil
		}
	}
	return nil, "", errors.New("unterminated YAML front-matter: no closing --- line")
}

// inPhase returns the features carrying this phase, in file order.
func (g *gate) inPhase() []feature {
	var out []feature
	for _, f := range g.features {
		if f.Phase == g.Phase {
			out = append(out, f)
		}
	}
	return out
}

// checkFeatures requires every feature in the phase to be done. Dropped
// features are excluded rather than counted against the phase: a dropped ID
// stays burned so references do not dangle, and it is not outstanding work.
func (g *gate) checkFeatures() (outcome, string) {
	if out, detail, unavailable := g.devUnavailable(); unavailable {
		return out, detail
	}

	features := g.inPhase()
	if len(features) == 0 {
		return fail, fmt.Sprintf("no feature in %s/features/ carries phase: %s", g.DevDir, g.Phase)
	}

	var done, dropped, outstanding []string
	for _, f := range features {
		switch f.Status {
		case statusDone:
			done = append(done, f.ID)
		case statusDropped:
			dropped = append(dropped, f.ID)
		default:
			outstanding = append(outstanding, fmt.Sprintf("%s (%s)", f.ID, f.Status))
		}
	}

	total := len(done) + len(outstanding)
	detail := fmt.Sprintf("%d/%d done", len(done), total)
	if len(dropped) > 0 {
		detail += fmt.Sprintf(", %d dropped", len(dropped))
	}
	if len(outstanding) > 0 {
		return fail, fmt.Sprintf("%s — %s", detail, strings.Join(outstanding, ", "))
	}
	return pass, detail
}

// checkVerification enforces the other half of the e2e: [] bargain. A feature
// may declare no specs only when it changes no runtime behavior, and it then
// owes a concrete `## Verification` section — otherwise e2e: [] becomes the way
// to skip testing.
func (g *gate) checkVerification() (outcome, string) {
	if out, detail, unavailable := g.devUnavailable(); unavailable {
		return out, detail
	}

	var documented, missing []string
	for _, f := range g.inPhase() {
		if len(f.E2E) > 0 || f.Status == statusDropped {
			continue
		}
		if hasContent(sectionBody(f.Body, "Verification")) {
			documented = append(documented, f.ID)
			continue
		}
		missing = append(missing, f.ID)
	}

	switch {
	case len(missing) > 0:
		return fail, fmt.Sprintf("%s declare e2e: [] with no ## Verification section",
			strings.Join(missing, ", "))
	case len(documented) == 0:
		return pass, "no feature in " + g.Phase + " declares e2e: []"
	}
	return pass, fmt.Sprintf("%s with e2e: [] documented", plural(len(documented), "feature", "features"))
}

// checkWorklog requires the phase to appear in a session log. dev/README.md
// makes the log the answer to "where did I leave off?", so a phase that closes
// without one closes without a record.
func (g *gate) checkWorklog() (outcome, string) {
	if out, detail, unavailable := g.devUnavailable(); unavailable {
		return out, detail
	}

	dir := filepath.Join(g.DevDir, "log")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return fail, fmt.Sprintf("%s/ does not exist", filepath.ToSlash(dir))
	}
	if err != nil {
		return fail, err.Error()
	}

	// One scan covers both the `phase:` field and any mention in the body: the
	// front-matter is part of the file's text.
	mentions := regexp.MustCompile(`\b` + regexp.QuoteMeta(g.Phase) + `\b`)

	var matches []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") || strings.HasPrefix(name, ".") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return fail, err.Error()
		}
		if mentions.Match(data) {
			matches = append(matches, "log/"+name)
		}
	}
	if len(matches) == 0 {
		return fail, fmt.Sprintf("no file in %s/log/ mentions %s", g.DevDir, g.Phase)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(matches)))
	if len(matches) == 1 {
		return pass, matches[0]
	}
	return pass, fmt.Sprintf("%s, newest %s", plural(len(matches), "entry", "entries"), matches[0])
}

// checkChangelog requires the public changelog to carry unreleased content. It
// is keyed on the file rather than on dev/, because the changelog is tracked and
// a public clone does have it.
func (g *gate) checkChangelog() (outcome, string) {
	path := filepath.Join(g.RepoDir, changelogFile)
	if !exists(path) {
		return skip, changelogFile + " not present"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fail, err.Error()
	}

	section, found := findSection(string(data), "[Unreleased]")
	if !found {
		return fail, "no `## [Unreleased]` section"
	}
	entries := 0
	for _, line := range strings.Split(section, "\n") {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
			entries++
		}
	}
	if !hasContent(section) {
		return fail, "the `## [Unreleased]` section is empty"
	}
	if entries == 0 {
		return pass, "`[Unreleased]` has content"
	}
	return pass, fmt.Sprintf("`[Unreleased]` has %s", plural(entries, "entry", "entries"))
}

// sectionBody returns the lines under a `## Name` heading, up to the next
// heading of the same or higher level.
func sectionBody(body, name string) string {
	section, _ := findSection(body, name)
	return section
}

func findSection(body, name string) (string, bool) {
	want := strings.ToLower("## " + name)
	var out []string
	found := false
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if found {
			if strings.HasPrefix(trimmed, "## ") || strings.HasPrefix(trimmed, "# ") {
				break
			}
			out = append(out, line)
			continue
		}
		if strings.ToLower(trimmed) == want {
			found = true
		}
	}
	return strings.Join(out, "\n"), found
}

// hasContent reports whether a section says anything. Blank lines, HTML
// comments, and link-reference definitions do not count: a changelog whose
// [Unreleased] section holds only its own link target is an empty one.
func hasContent(section string) bool {
	for _, line := range strings.Split(section, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
		case strings.HasPrefix(trimmed, "<!--"):
		case linkDefinitionPattern.MatchString(trimmed):
		default:
			return true
		}
	}
	return false
}

func relPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}
