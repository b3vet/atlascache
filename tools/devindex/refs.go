package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// refPattern matches a cross-reference such as [[FEAT-0004]]. Template
// placeholders like [[FEAT-XXXX]] deliberately do not match.
var refPattern = regexp.MustCompile(`\[\[((?:FEAT|ISSUE|ADR)-\d{4})\]\]`)

// danglingRefs scans every markdown file under root and reports references that
// resolve to no tracked item. These are warnings, not errors: the check is cheap
// and catches typos that would otherwise sit unnoticed for months.
func danglingRefs(root string, known map[string]bool) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".md") || strings.HasPrefix(name, ".") {
			return nil
		}
		refs, err := scanRefs(path, known)
		if err != nil {
			return err
		}
		for _, r := range refs {
			out = append(out, relPath(root, path)+r)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// scanRefs returns the dangling references in one file, formatted as a suffix of
// ":line: dangling reference [[ID]]" so the caller can prefix the path.
func scanRefs(path string, known map[string]bool) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for i, line := range strings.Split(string(data), "\n") {
		reported := map[string]bool{}
		for _, m := range refPattern.FindAllStringSubmatch(line, -1) {
			id := m[1]
			if known[id] || reported[id] {
				continue
			}
			reported[id] = true
			out = append(out, ":"+strconv.Itoa(i+1)+": dangling reference [["+id+"]]")
		}
	}
	return out, nil
}
