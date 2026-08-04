package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// kind is a tracked item type. Each kind lives in its own subdirectory and has
// its own required fields.
type kind int

const (
	kindFeature kind = iota
	kindIssue
	kindADR
	kindPhase
	kindLog
)

func (k kind) dir() string {
	switch k {
	case kindFeature:
		return "features"
	case kindIssue:
		return "issues"
	case kindADR:
		return "adr"
	case kindPhase:
		return "phases"
	case kindLog:
		return "log"
	}
	return ""
}

// statusDone is named because the index renders it differently from the rest.
const statusDone = "done"

// Status vocabularies. dev/README.md section 3 defines the item statuses; the
// ADR and phase templates define their own. A status outside its vocabulary is
// a hard error: it would otherwise land in no column of the summary table and
// the item would silently stop being counted.
var (
	itemStatuses  = []string{"planned", "in-progress", "blocked", statusDone, "dropped"}
	adrStatuses   = []string{"proposed", "accepted", "superseded", "reversed"}
	phaseStatuses = []string{"planned", "in-progress", statusDone}
	severities    = []string{"critical", "high", "medium", "low"}
)

var (
	// idPattern matches a tracked item ID: FEAT-0001, ISSUE-0002, ADR-0003.
	idPattern = regexp.MustCompile(`^(FEAT|ISSUE|ADR)-\d{4}$`)
	// datePattern matches a worklog filename stem, YYYY-MM-DD.
	datePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	// phaseIDPattern matches a phase ID, P0 through P99.
	phaseIDPattern = regexp.MustCompile(`^P\d{1,2}$`)
)

// frontMatter is the union of every front-matter schema in dev/. Unknown keys
// are ignored so item-specific fields (blocked_by, reason, found_in, e2e) do not
// need to be modeled here; required keys are checked per kind after decoding.
type frontMatter struct {
	ID        string   `yaml:"id"`
	Title     string   `yaml:"title"`
	Status    string   `yaml:"status"`
	Phase     string   `yaml:"phase"`
	Milestone string   `yaml:"milestone"`
	Severity  string   `yaml:"severity"`
	Features  []string `yaml:"features"`
	Session   string   `yaml:"session"`
	Touched   []string `yaml:"touched"`
}

// item is a feature, issue, or decision.
type item struct {
	ID       string
	Title    string
	Status   string
	Phase    string
	Severity string
	Path     string // relative to the dev tree root
}

// phase is one entry in phases/.
type phase struct {
	ID        string
	Title     string
	Milestone string
	Status    string
	Features  []string
	Path      string
}

// worklog is one entry in log/.
type worklog struct {
	Date    string
	Phase   string
	Touched []string
	Path    string
}

// devTree is everything parsed out of a dev tree.
type devTree struct {
	Features []item
	Issues   []item
	ADRs     []item
	Phases   []phase
	Logs     []worklog
}

func loadTree(root string) (*devTree, error) {
	t := &devTree{}
	var err error
	if t.Features, err = loadItems(root, kindFeature); err != nil {
		return nil, err
	}
	if t.Issues, err = loadItems(root, kindIssue); err != nil {
		return nil, err
	}
	if t.ADRs, err = loadItems(root, kindADR); err != nil {
		return nil, err
	}
	if t.Phases, err = loadPhases(root); err != nil {
		return nil, err
	}
	if t.Logs, err = loadLogs(root); err != nil {
		return nil, err
	}
	return t, nil
}

// markdownFiles lists the .md files in a subdirectory of the dev tree. A missing
// subdirectory is tolerated — early on there are no features, and later there
// may be no open issues.
func markdownFiles(root string, k kind) ([]string, error) {
	dir := filepath.Join(root, k.dir())
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
	return paths, nil
}

func loadItems(root string, k kind) ([]item, error) {
	paths, err := markdownFiles(root, k)
	if err != nil {
		return nil, err
	}
	items := make([]item, 0, len(paths))
	for _, path := range paths {
		fm, err := readFrontMatter(path)
		if err != nil {
			return nil, err
		}
		it, err := newItem(root, path, k, fm)
		if err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	if err := checkDuplicateIDs(items); err != nil {
		return nil, err
	}
	sortItemsByID(items)
	return items, nil
}

func newItem(root, path string, k kind, fm *frontMatter) (item, error) {
	rel := relPath(root, path)
	stem := strings.TrimSuffix(filepath.Base(path), ".md")

	if err := requireID(rel, fm.ID, stem); err != nil {
		return item{}, err
	}
	if fm.Title == "" {
		return item{}, malformed(rel, "missing required field: title")
	}

	statuses := itemStatuses
	if k == kindADR {
		statuses = adrStatuses
	}
	if err := requireOneOf(rel, "status", fm.Status, statuses); err != nil {
		return item{}, err
	}
	if k == kindFeature && fm.Phase == "" {
		return item{}, malformed(rel, "missing required field: phase")
	}
	if k == kindIssue {
		if err := requireOneOf(rel, "severity", fm.Severity, severities); err != nil {
			return item{}, err
		}
	}

	return item{
		ID:       fm.ID,
		Title:    fm.Title,
		Status:   fm.Status,
		Phase:    fm.Phase,
		Severity: fm.Severity,
		Path:     rel,
	}, nil
}

func loadPhases(root string) ([]phase, error) {
	paths, err := markdownFiles(root, kindPhase)
	if err != nil {
		return nil, err
	}
	phases := make([]phase, 0, len(paths))
	seen := map[string]string{}
	for _, path := range paths {
		fm, err := readFrontMatter(path)
		if err != nil {
			return nil, err
		}
		rel := relPath(root, path)
		if !phaseIDPattern.MatchString(fm.ID) {
			return nil, malformed(rel, fmt.Sprintf("id %q is not a phase id such as P0", fm.ID))
		}
		if fm.Title == "" {
			return nil, malformed(rel, "missing required field: title")
		}
		if err := requireOneOf(rel, "status", fm.Status, phaseStatuses); err != nil {
			return nil, err
		}
		if prev, dup := seen[fm.ID]; dup {
			return nil, malformed(rel, fmt.Sprintf("duplicate phase id %s, already declared by %s", fm.ID, prev))
		}
		seen[fm.ID] = rel
		phases = append(phases, phase{
			ID:        fm.ID,
			Title:     fm.Title,
			Milestone: fm.Milestone,
			Status:    fm.Status,
			Features:  fm.Features,
			Path:      rel,
		})
	}
	sort.Slice(phases, func(i, j int) bool {
		return phaseNumber(phases[i].ID) < phaseNumber(phases[j].ID)
	})
	return phases, nil
}

func loadLogs(root string) ([]worklog, error) {
	paths, err := markdownFiles(root, kindLog)
	if err != nil {
		return nil, err
	}
	logs := make([]worklog, 0, len(paths))
	for _, path := range paths {
		fm, err := readFrontMatter(path)
		if err != nil {
			return nil, err
		}
		rel := relPath(root, path)
		stem := strings.TrimSuffix(filepath.Base(path), ".md")
		if !datePattern.MatchString(stem) {
			return nil, malformed(rel, "worklog filename must be YYYY-MM-DD.md")
		}
		if fm.Session != stem {
			return nil, malformed(rel, fmt.Sprintf("session %q does not match the filename", fm.Session))
		}
		logs = append(logs, worklog{
			Date:    fm.Session,
			Phase:   fm.Phase,
			Touched: fm.Touched,
			Path:    rel,
		})
	}
	// Newest first: dev/README.md tells a cold reader to open the newest log.
	sort.Slice(logs, func(i, j int) bool { return logs[i].Date > logs[j].Date })
	return logs, nil
}

// readFrontMatter parses the YAML block delimited by --- at the top of a file.
// Every failure names the file: silently skipping an unparseable file makes the
// item vanish from the index while its file still exists.
func readFrontMatter(path string) (*frontMatter, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, err := splitFrontMatter(data)
	if err != nil {
		return nil, malformed(path, err.Error())
	}
	var fm frontMatter
	if err := yaml.Unmarshal(block, &fm); err != nil {
		return nil, malformed(path, err.Error())
	}
	return &fm, nil
}

func splitFrontMatter(data []byte) ([]byte, error) {
	const delim = "---"
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != delim {
		return nil, errors.New("no YAML front-matter: the file must start with a --- line")
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == delim {
			return []byte(strings.Join(lines[1:i], "\n")), nil
		}
	}
	return nil, errors.New("unterminated YAML front-matter: no closing --- line")
}

func requireID(rel, id, stem string) error {
	if id == "" {
		return malformed(rel, "missing required field: id")
	}
	if !idPattern.MatchString(id) {
		return malformed(rel, fmt.Sprintf("id %q is not of the form FEAT-0001", id))
	}
	if id != stem {
		return malformed(rel, fmt.Sprintf("id %q does not match the filename", id))
	}
	return nil
}

func requireOneOf(rel, field, value string, allowed []string) error {
	if value == "" {
		return malformed(rel, "missing required field: "+field)
	}
	for _, a := range allowed {
		if value == a {
			return nil
		}
	}
	return malformed(rel, fmt.Sprintf("unknown %s %q, expected one of %s",
		field, value, strings.Join(allowed, ", ")))
}

func checkDuplicateIDs(items []item) error {
	seen := map[string]string{}
	for _, it := range items {
		if prev, dup := seen[it.ID]; dup {
			return malformed(it.Path, fmt.Sprintf("duplicate id %s, already declared by %s", it.ID, prev))
		}
		seen[it.ID] = it.Path
	}
	return nil
}

func malformed(path, reason string) error {
	return fmt.Errorf("%s: %s", path, reason)
}

func relPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}

// sortItemsByID orders items by numeric ID, so FEAT-0009 precedes FEAT-0010.
func sortItemsByID(items []item) {
	sort.Slice(items, func(i, j int) bool { return lessByID(items[i].ID, items[j].ID) })
}

func lessByID(a, b string) bool {
	na, nb := idNumber(a), idNumber(b)
	if na != nb {
		return na < nb
	}
	return a < b
}

func idNumber(id string) int {
	i := strings.LastIndex(id, "-")
	if i < 0 {
		return 0
	}
	n, err := strconv.Atoi(id[i+1:])
	if err != nil {
		return 0
	}
	return n
}

func phaseNumber(id string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(id, "P"))
	if err != nil {
		return 0
	}
	return n
}
