// Package runner parses AtlasCache E2E specs and executes them against a
// server harness.
//
// A spec is one YAML file describing a sequence of steps against one server
// process. Parsing is strict: an unknown field, an unknown tier, or a step
// naming an unregistered scenario is rejected before any server is started.
package runner

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// nullTag is the YAML tag of an empty value, which is never a valid scalar in
// a spec: a field written with no value is a mistake, not a zero.
const nullTag = "!!null"

// SpecVersion is the spec format version this runner understands. A spec that
// omits `version` is read as version 1, since no earlier format exists; a spec
// declaring a newer version is rejected rather than misinterpreted.
const SpecVersion = 1

// Tier selects when a spec runs: smoke on every commit, full on every PR and
// at phase close, soak nightly.
type Tier string

// The three tiers. Any other value is a parse error.
const (
	TierSmoke Tier = "smoke"
	TierFull  Tier = "full"
	TierSoak  Tier = "soak"
)

// Tiers lists every valid tier, in increasing cost order.
var Tiers = []Tier{TierSmoke, TierFull, TierSoak}

// Valid reports whether t is one of the three defined tiers.
func (t Tier) Valid() bool {
	for _, known := range Tiers {
		if t == known {
			return true
		}
	}
	return false
}

// Spec is one parsed spec file.
type Spec struct {
	Version     int            `yaml:"version"`
	Name        string         `yaml:"name"`
	Tier        Tier           `yaml:"tier"`
	Feature     string         `yaml:"feature"`
	Issue       string         `yaml:"issue"`
	Description string         `yaml:"description"`
	Config      map[string]any `yaml:"config"`
	Steps       []Step         `yaml:"steps"`

	// Path is the file the spec was read from, used in failure output.
	Path string `yaml:"-"`
}

// StepKind is which of the step forms a step uses.
type StepKind string

// The step forms. Exactly one may be present per step.
const (
	StepCommand  StepKind = "cmd"
	StepSleep    StepKind = "sleep"
	StepScenario StepKind = "scenario"
	StepRestart  StepKind = "restart"
	StepKill     StepKind = "kill"
)

// Step is one entry in a spec's `steps:` list.
type Step struct {
	Cmd         string           `yaml:"cmd"`
	Expect      Value            `yaml:"expect"`
	ExpectError Value            `yaml:"expect_error"`
	ExpectField map[string]Value `yaml:"expect_field"`
	ExpectValue Value            `yaml:"expect_value"`
	Sleep       Duration         `yaml:"sleep"`
	Scenario    string           `yaml:"scenario"`
	Restart     *bool            `yaml:"restart"`
	Kill        *bool            `yaml:"kill"`

	// Line is the step's first source line, used in failure output.
	Line int `yaml:"-"`
}

// Kind returns the step's form. It is meaningful only for a validated step,
// where exactly one form is present.
func (s Step) Kind() StepKind {
	switch {
	case s.Cmd != "":
		return StepCommand
	case s.Sleep.Set:
		return StepSleep
	case s.Scenario != "":
		return StepScenario
	case s.Restart != nil:
		return StepRestart
	case s.Kill != nil:
		return StepKill
	default:
		return ""
	}
}

// String describes the step the way failure output names it.
func (s Step) String() string {
	switch s.Kind() {
	case StepCommand:
		return "cmd: " + s.Cmd
	case StepSleep:
		return "sleep: " + s.Sleep.D.String()
	case StepScenario:
		return "scenario: " + s.Scenario
	case StepRestart:
		return "restart"
	case StepKill:
		return "kill"
	default:
		return "<empty step>"
	}
}

// Value is a YAML scalar kept in its source text form, so that `expect: 1` and
// `expect: "1"` are the same assertion and `expect: NIL` stays a string.
type Value struct {
	Text string
	Set  bool
}

// UnmarshalYAML decodes any scalar into its literal text.
func (v *Value) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: expected a single value, got %s", node.Line, nodeKind(node))
	}
	if node.Tag == nullTag {
		return fmt.Errorf("line %d: expected a value, got an empty one", node.Line)
	}
	v.Text = node.Value
	v.Set = true
	return nil
}

// Duration is a step duration, written with an explicit unit such as 1200ms.
type Duration struct {
	D   time.Duration
	Set bool
}

// UnmarshalYAML parses a Go duration string and rejects unitless numbers,
// which would otherwise silently mean nanoseconds.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || node.Tag == nullTag {
		return fmt.Errorf("line %d: expected a duration such as 1200ms, got %s", node.Line, nodeKind(node))
	}
	parsed, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("line %d: %q is not a duration; write a unit, such as 1200ms or 2s", node.Line, node.Value)
	}
	d.D = parsed
	d.Set = true
	return nil
}

func nodeKind(node *yaml.Node) string {
	switch node.Kind {
	case yaml.SequenceNode:
		return "a list"
	case yaml.MappingNode:
		return "a map"
	case yaml.AliasNode:
		return "an alias"
	case yaml.DocumentNode:
		return "a document"
	case yaml.ScalarNode:
		if node.Tag == nullTag {
			return "an empty value"
		}
		return "a value"
	default:
		return "an unexpected node"
	}
}

var (
	featureRE = regexp.MustCompile(`^FEAT-\d{4}$`)
	issueRE   = regexp.MustCompile(`^ISSUE-\d{4}$`)
)

// ParseSpec decodes and validates one spec. Errors are reported in
// file:line: message form, and every problem found is reported at once so a
// broken spec is fixed in one pass.
//
// Scenario names are checked against the registry here, so a typo fails before
// a server is started.
func ParseSpec(path string, data []byte) (*Spec, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("%s: spec file is empty", path)
	}

	var spec Spec
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&spec); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s: spec file has no document", path)
		}
		return nil, decodeError(path, err)
	}
	if err := dec.Decode(new(Spec)); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: a spec file must hold exactly one YAML document", path)
	}

	spec.Path = path
	lines := readSourceLines(data)
	for i := range spec.Steps {
		if i < len(lines.steps) {
			spec.Steps[i].Line = lines.steps[i]
		}
	}

	if err := spec.validate(path, lines); err != nil {
		return nil, err
	}
	return &spec, nil
}

// unknownFieldRE matches the yaml.v3 strict-decoding message so it can be
// rewritten into the file:line form the rest of the runner uses.
var unknownFieldRE = regexp.MustCompile(`^line (\d+): field (\S+) not found in type runner\.(\w+)$`)

func decodeError(path string, err error) error {
	var typeErr *yaml.TypeError
	if !errors.As(err, &typeErr) {
		return fmt.Errorf("%s", locate(path, strings.TrimPrefix(err.Error(), "yaml: ")))
	}
	parts := make([]error, 0, len(typeErr.Errors))
	for _, msg := range typeErr.Errors {
		if m := unknownFieldRE.FindStringSubmatch(msg); m != nil {
			parts = append(parts, fmt.Errorf("%s:%s: unknown field %q in %s", path, m[1], m[2], strings.ToLower(m[3])))
			continue
		}
		parts = append(parts, errors.New(locate(path, msg)))
	}
	return errors.Join(parts...)
}

// locate rewrites a yaml.v3 "line N: message" into the file:line: message form
// the rest of the runner uses, so an editor can jump straight to it.
func locate(path, msg string) string {
	if prefix, rest, ok := strings.Cut(msg, ": "); ok && strings.HasPrefix(prefix, "line ") {
		return fmt.Sprintf("%s:%s: %s", path, strings.TrimPrefix(prefix, "line "), rest)
	}
	return path + ": " + msg
}

// sourceLines is where things sit in the file. The strict decoder cannot supply
// line numbers, and an error without one costs a hunt through the file.
type sourceLines struct {
	fields map[string]int
	steps  []int
}

func readSourceLines(data []byte) sourceLines {
	lines := sourceLines{fields: map[string]int{}}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return lines
	}
	root := &doc
	if root.Kind == yaml.DocumentNode && len(root.Content) == 1 {
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return lines
	}

	for i := 0; i+1 < len(root.Content); i += 2 {
		key, value := root.Content[i], root.Content[i+1]
		lines.fields[key.Value] = key.Line
		if key.Value == "steps" && value.Kind == yaml.SequenceNode {
			for _, item := range value.Content {
				lines.steps = append(lines.steps, item.Line)
			}
		}
	}
	return lines
}

// at names a spec field's position, falling back to the file when the field is
// absent — as it is for anything reported as missing.
func (l sourceLines) at(path, field string) string {
	if line, ok := l.fields[field]; ok {
		return fmt.Sprintf("%s:%d", path, line)
	}
	return path
}

func (s *Spec) validate(path string, lines sourceLines) error {
	var problems []error
	add := func(field, format string, args ...any) {
		problems = append(problems, fmt.Errorf("%s: %s", lines.at(path, field), fmt.Sprintf(format, args...)))
	}

	switch {
	case s.Version == 0:
		s.Version = SpecVersion
	case s.Version < 0 || s.Version > SpecVersion:
		add("version", "version %d is not supported; this runner reads spec version %d", s.Version, SpecVersion)
	}

	switch {
	case s.Name == "":
		add("name", "name is required")
	case path != "":
		stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		if s.Name != stem {
			add("name", "name %q must match the file name %q", s.Name, stem)
		}
	}

	if !s.Tier.Valid() {
		add("tier", "tier %q is not one of %s", s.Tier, tierList())
	}
	if s.Feature == "" {
		add("feature", "feature is required, for traceability back to the plan")
	} else if !featureRE.MatchString(s.Feature) {
		add("feature", "feature %q must look like FEAT-0001", s.Feature)
	}
	if s.Issue != "" && !issueRE.MatchString(s.Issue) {
		add("issue", "issue %q must look like ISSUE-0001", s.Issue)
	}
	if len(s.Steps) == 0 {
		add("steps", "steps is required and must not be empty")
	}

	for i, step := range s.Steps {
		for _, err := range step.validate() {
			problems = append(problems, fmt.Errorf("%s:%d: step %d: %s", path, step.Line, i+1, err))
		}
	}

	return errors.Join(problems...)
}

func (s Step) validate() []string {
	forms := s.presentForms()
	assertions := s.presentAssertions()

	if len(forms) == 0 {
		if len(assertions) > 0 {
			return []string{fmt.Sprintf("%s needs a cmd to assert against", strings.Join(assertions, " and "))}
		}
		return []string{fmt.Sprintf("no step form given; write one of %s", strings.Join(allForms(), ", "))}
	}
	if len(forms) > 1 {
		return []string{fmt.Sprintf("step forms %s cannot be combined; write one per step", strings.Join(forms, " and "))}
	}

	var problems []string
	if forms[0] != string(StepCommand) && len(assertions) > 0 {
		problems = append(problems, fmt.Sprintf("%s does not take %s", forms[0], strings.Join(assertions, " or ")))
	}

	switch StepKind(forms[0]) {
	case StepCommand:
		problems = append(problems, s.validateCommand(assertions)...)
	case StepSleep:
		if s.Sleep.D <= 0 {
			problems = append(problems, fmt.Sprintf("sleep %s must be positive", s.Sleep.D))
		}
	case StepScenario:
		if _, ok := LookupScenario(s.Scenario); !ok {
			problems = append(problems, fmt.Sprintf("scenario %q is not registered; known scenarios: %s",
				s.Scenario, describeScenarios()))
		}
	case StepRestart:
		if !*s.Restart {
			problems = append(problems, "restart must be true; drop the step instead of disabling it")
		}
	case StepKill:
		if !*s.Kill {
			problems = append(problems, "kill must be true; drop the step instead of disabling it")
		}
	}

	return problems
}

func (s Step) validateCommand(assertions []string) []string {
	var problems []string
	switch len(assertions) {
	case 1:
	case 0:
		problems = append(problems, "cmd needs one of expect, expect_value, expect_error or expect_field; a command with no assertion asserts nothing")
	default:
		problems = append(problems, fmt.Sprintf("cmd takes one assertion, got %s", strings.Join(assertions, " and ")))
	}
	for _, name := range sortedValueKeys(s.ExpectField) {
		if _, err := parseComparison(s.ExpectField[name].Text); err != nil {
			problems = append(problems, fmt.Sprintf("expect_field %s: %s", name, err))
		}
	}
	if s.ExpectValue.Set {
		if _, err := parseComparison(s.ExpectValue.Text); err != nil {
			problems = append(problems, fmt.Sprintf("expect_value: %s", err))
		}
	}
	return problems
}

func (s Step) presentForms() []string {
	var forms []string
	if s.Cmd != "" {
		forms = append(forms, string(StepCommand))
	}
	if s.Sleep.Set {
		forms = append(forms, string(StepSleep))
	}
	if s.Scenario != "" {
		forms = append(forms, string(StepScenario))
	}
	if s.Restart != nil {
		forms = append(forms, string(StepRestart))
	}
	if s.Kill != nil {
		forms = append(forms, string(StepKill))
	}
	return forms
}

func (s Step) presentAssertions() []string {
	var found []string
	if s.Expect.Set {
		found = append(found, "expect")
	}
	if s.ExpectError.Set {
		found = append(found, "expect_error")
	}
	if len(s.ExpectField) > 0 {
		found = append(found, "expect_field")
	}
	if s.ExpectValue.Set {
		found = append(found, "expect_value")
	}
	return found
}

func allForms() []string {
	return []string{
		string(StepCommand), string(StepSleep), string(StepScenario),
		string(StepRestart), string(StepKill),
	}
}

func tierList() string {
	names := make([]string, len(Tiers))
	for i, tier := range Tiers {
		names[i] = string(tier)
	}
	return strings.Join(names, ", ")
}

func sortedValueKeys(m map[string]Value) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
