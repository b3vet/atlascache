package config

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Strict decoding (ISSUE-0024).
//
// A key the schema does not define used to be discarded in silence and the
// option kept its default, which turns `auth.enbaled: true` into a server with
// authentication off that reports itself perfectly healthy. Every option here
// is security- or availability-relevant, so a configuration the server cannot
// fully honor is refused rather than half-applied — and the refusal names the
// key, its section, its line, and the near match, because "invalid config"
// sends an operator back to guessing.
//
// The check is written against the decoded document rather than handed to
// yaml.v3's Decoder.KnownFields, because the loader decodes through viper and
// mapstructure: the schema lives in `mapstructure` tags, and a second set of
// `yaml` tags would be a parallel copy of it that drifts the first time a field
// is renamed. Deriving the schema from the struct by reflection keeps one
// source for the vocabulary the file, the admin API (see Effective) and the
// loader all speak.

// UnknownKey is one key a configuration file carries that the schema does not
// define.
type UnknownKey struct {
	// Key is the offending key exactly as the file writes it.
	Key string
	// Section is the dotted path of the mapping it appeared in, empty at the
	// top level.
	Section string
	// Line is its 1-based line in the file, 0 when the parser gave none.
	Line int
	// Suggestion is the near match, empty when nothing was close enough to be
	// useful. A wrong suggestion is worse than none.
	Suggestion string
}

// String renders the key the way the error reports it.
func (k UnknownKey) String() string {
	where := "at the top level"
	if k.Section != "" {
		where = "in " + k.Section
	}
	if k.Suggestion == "" {
		return fmt.Sprintf("unknown field %q %s", k.Key, where)
	}
	return fmt.Sprintf("unknown field %q %s (did you mean %q?)", k.Key, where, k.Suggestion)
}

// UnknownKeysError is what a configuration file with keys outside the schema
// fails with. Every unknown key is reported at once: an operator fixing a
// config one error per restart is the experience this check exists to avoid.
type UnknownKeysError struct {
	Path string
	Keys []UnknownKey
}

func (e *UnknownKeysError) Error() string {
	if len(e.Keys) == 1 {
		return fmt.Sprintf("%s: %s", e.location(e.Keys[0]), e.Keys[0])
	}

	lines := make([]string, 0, len(e.Keys))
	for _, key := range e.Keys {
		lines = append(lines, fmt.Sprintf("%s: %s", lineLabel(key), key))
	}
	return fmt.Sprintf("%s: %d unknown fields:\n  - %s",
		e.Path, len(e.Keys), strings.Join(lines, "\n  - "))
}

// location renders one key as file:line, so an editor can jump to it.
func (e *UnknownKeysError) location(key UnknownKey) string {
	if key.Line <= 0 {
		return e.Path
	}
	return fmt.Sprintf("%s:%d", e.Path, key.Line)
}

func lineLabel(key UnknownKey) string {
	if key.Line <= 0 {
		return "somewhere in the file"
	}
	return fmt.Sprintf("line %d", key.Line)
}

// CheckUnknownKeys reports every key in a YAML configuration document that the
// schema does not define.
//
// A document that does not parse is not this check's business and passes it:
// the loader reports the syntax error, with the line the parser found it on.
func CheckUnknownKeys(path string, data []byte) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil
	}

	root := documentRoot(&doc)
	if root == nil {
		return nil
	}

	keys := unknownKeysIn(root, configSchema(), "")
	if len(keys) == 0 {
		return nil
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Line != keys[j].Line {
			return keys[i].Line < keys[j].Line
		}
		return keys[i].Key < keys[j].Key
	})
	return &UnknownKeysError{Path: path, Keys: keys}
}

// documentRoot returns the mapping a configuration file is, or nil for anything
// else — an empty file, a list, a bare scalar. Those are the loader's to
// report; this check only knows about keys.
func documentRoot(doc *yaml.Node) *yaml.Node {
	node := doc
	if node.Kind == yaml.DocumentNode && len(node.Content) == 1 {
		node = node.Content[0]
	}
	if node.Kind != yaml.MappingNode {
		return nil
	}
	return node
}

// unknownKeysIn walks one mapping against its schema, descending into the
// sections it defines.
func unknownKeysIn(node *yaml.Node, schema *schemaNode, path string) []UnknownKey {
	var found []UnknownKey

	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if key.Kind != yaml.ScalarNode {
			continue
		}

		child, known := schema.children[key.Value]
		if !known {
			found = append(found, UnknownKey{
				Key:        key.Value,
				Section:    path,
				Line:       key.Line,
				Suggestion: suggestionFor(key.Value, schema, path),
			})
			continue
		}

		// A section written as something other than a mapping is a type error,
		// and mapstructure reports it with the type it wanted. Descending into
		// it here would report nothing useful.
		if child != nil && value.Kind == yaml.MappingNode {
			found = append(found, unknownKeysIn(value, child, join(path, key.Value))...)
		}
	}

	return found
}

// schemaNode is one level of the configuration schema: the keys a mapping may
// hold, with a nil child marking a leaf — a value, not a section.
type schemaNode struct {
	children map[string]*schemaNode
}

// keys returns the node's keys, sorted, which is the candidate set a suggestion
// is drawn from.
func (n *schemaNode) keys() []string {
	names := make([]string, 0, len(n.children))
	for name := range n.children {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// paths appends every dotted path at or under this node, sections included.
func (n *schemaNode) paths(prefix string, out *[]string) {
	for _, name := range n.keys() {
		path := join(prefix, name)
		*out = append(*out, path)
		if child := n.children[name]; child != nil {
			child.paths(path, out)
		}
	}
}

// configPkgPath is the package the configuration sections are declared in. Only
// a struct from here is descended into: a struct from elsewhere — time.Time,
// say — is a value with its own parser, not a section of the file.
var configPkgPath = reflect.TypeOf(Config{}).PkgPath()

// configSchema is the schema of Config, derived from the struct rather than
// written out. A field added to Config is accepted without anyone registering
// it here, and — the half that matters — a field renamed in Config stops being
// accepted under its old name in the same commit.
var configSchema = sync.OnceValue(func() *schemaNode {
	return schemaOf(reflect.TypeOf(Config{}))
})

// knownPaths is every dotted path the schema defines, sorted. It is what a
// key written under the wrong parent is recognized against.
var knownPaths = sync.OnceValue(func() []string {
	var paths []string
	configSchema().paths("", &paths)
	sort.Strings(paths)
	return paths
})

func schemaOf(t reflect.Type) *schemaNode {
	node := &schemaNode{children: make(map[string]*schemaNode, t.NumField())}

	for i := range t.NumField() {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		name := fieldName(field)
		if name == "" {
			continue
		}
		node.children[name] = sectionSchema(field.Type)
	}

	return node
}

// sectionSchema returns the schema of a section, or nil when the field is a
// leaf value.
func sectionSchema(t reflect.Type) *schemaNode {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || t.PkgPath() != configPkgPath {
		return nil
	}
	return schemaOf(t)
}

// suggestionFor picks the near match to offer for an unknown key, or returns
// empty when nothing is close enough. The siblings at the key's own level are
// tried first, then the rest of the schema — a key written under the wrong
// parent is a common enough mistake to be worth naming its real home.
func suggestionFor(unknown string, schema *schemaNode, path string) string {
	if match := nearest(unknown, schema.keys()); match != "" {
		return match
	}
	return elsewhere(unknown, path)
}

// nearest returns the one candidate close enough to unknown to suggest.
//
// Three rules, in order of how certain each is. A candidate that differs only
// in case or separators is what the operator meant. A candidate holding the
// unknown key as a whole word is the `server.port` case that opened
// ISSUE-0024: the real key is `client_port`, which no edit-distance threshold
// would ever reach. Only then is edit distance consulted, and only when one
// candidate is closest — a tie is an ambiguity, and guessing at one produces
// the wrong suggestion, which is worse than none.
func nearest(unknown string, candidates []string) string {
	if match, ok := onlyMatch(candidates, func(candidate string) bool {
		return normalizeKey(candidate) == normalizeKey(unknown)
	}); ok {
		return match
	}

	if match, ok := onlyMatch(candidates, func(candidate string) bool {
		return hasWord(candidate, unknown)
	}); ok {
		return match
	}

	return closest(unknown, candidates)
}

// elsewhere returns the dotted path of a key that exists under a different
// parent, when exactly one does.
func elsewhere(unknown, path string) string {
	var found []string
	for _, candidate := range knownPaths() {
		if candidate == unknown {
			// The key names a top-level section and was written inside one.
			// Suggesting it back verbatim says nothing.
			continue
		}
		if normalizeKey(lastSegment(candidate)) == normalizeKey(unknown) {
			found = append(found, candidate)
		}
	}
	if len(found) == 1 && found[0] != join(path, unknown) {
		return found[0]
	}
	return ""
}

// closest returns the candidate within the edit-distance budget, when exactly
// one is closest.
func closest(unknown string, candidates []string) string {
	best, match, ties := -1, "", 0
	budget := distanceBudget(len(unknown))

	for _, candidate := range candidates {
		distance := editDistance(normalizeKey(unknown), normalizeKey(candidate))
		if distance > budget {
			continue
		}
		switch {
		case best < 0 || distance < best:
			best, match, ties = distance, candidate, 1
		case distance == best:
			ties++
		}
	}

	if ties != 1 {
		return ""
	}
	return match
}

// distanceBudget is how far a candidate may be from a key and still be offered.
//
// It scales with length because one edit in a four-character key is a different
// kind of accident from one edit in a twenty-character one, and a fixed
// threshold either misses `enbaled` or suggests `node` for `port`.
func distanceBudget(length int) int {
	switch {
	case length <= 3:
		return 1
	case length <= 9:
		return 2
	default:
		return 3
	}
}

// onlyMatch returns the single candidate satisfying match, if there is exactly
// one.
func onlyMatch(candidates []string, match func(string) bool) (string, bool) {
	found := ""
	count := 0
	for _, candidate := range candidates {
		if match(candidate) {
			found, count = candidate, count+1
		}
	}
	return found, count == 1
}

// hasWord reports whether candidate contains word as a whole underscore- or
// dash-delimited segment, so that `port` finds `client_port`.
func hasWord(candidate, word string) bool {
	normalized := normalizeKey(word)
	if normalized == "" {
		return false
	}
	for _, segment := range strings.FieldsFunc(candidate, func(r rune) bool {
		return r == '_' || r == '-' || r == '.'
	}) {
		if normalizeKey(segment) == normalized {
			return true
		}
	}
	return false
}

// normalizeKey folds the differences that are never meaningful in a key: case,
// and the separators between words.
func normalizeKey(key string) string {
	var b strings.Builder
	b.Grow(len(key))
	for _, r := range strings.ToLower(key) {
		if r == '_' || r == '-' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func lastSegment(path string) string {
	if idx := strings.LastIndex(path, "."); idx >= 0 {
		return path[idx+1:]
	}
	return path
}

// editDistance is the Levenshtein distance between two keys. Keys are short
// enough that the two-row form is all the optimization this needs.
func editDistance(a, b string) int {
	if a == b {
		return 0
	}
	if len(a) == 0 {
		return len([]rune(b))
	}
	if len(b) == 0 {
		return len([]rune(a))
	}

	left, right := []rune(a), []rune(b)
	previous := make([]int, len(right)+1)
	current := make([]int, len(right)+1)
	for j := range previous {
		previous[j] = j
	}

	for i := 1; i <= len(left); i++ {
		current[0] = i
		for j := 1; j <= len(right); j++ {
			cost := 1
			if left[i-1] == right[j-1] {
				cost = 0
			}
			current[j] = min(previous[j]+1, min(current[j-1]+1, previous[j-1]+cost))
		}
		previous, current = current, previous
	}

	return previous[len(right)]
}
