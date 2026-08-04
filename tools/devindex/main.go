// Command devindex regenerates dev/INDEX.md from the YAML front-matter of the
// items tracked under dev/.
//
//	devindex               regenerate dev/INDEX.md
//	devindex --check       fail if dev/INDEX.md is out of date
//	devindex --dir path    operate on a dev tree somewhere other than ./dev
//
// The dev tree is private and gitignored, so it may be absent entirely. That is
// not an error: the command says so and exits zero.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// indexFile is the generated file, relative to the dev tree root.
const indexFile = "INDEX.md"

// filePerm keeps the generated index owner-writable only; it is regenerated,
// never edited, so nothing needs group or world write.
const filePerm = 0o600

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "devindex: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("devindex", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dir := flags.String("dir", "dev", "path to the dev tree")
	check := flags.Bool("check", false, "verify the index is current instead of writing it")
	if err := flags.Parse(args); err != nil {
		return err
	}

	root := *dir
	info, err := os.Stat(root)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		fmt.Fprintf(stdout, "devindex: %s/ not present, nothing to do\n", filepath.Clean(root))
		return nil
	case err != nil:
		return err
	case !info.IsDir():
		return fmt.Errorf("%s is not a directory", root)
	}

	tree, err := loadTree(root)
	if err != nil {
		return err
	}

	for _, w := range tree.warnings() {
		fmt.Fprintf(stderr, "devindex: warning: %s\n", w)
	}
	refs, err := danglingRefs(root, tree.knownIDs())
	if err != nil {
		return err
	}
	for _, r := range refs {
		fmt.Fprintf(stderr, "devindex: warning: %s\n", r)
	}

	generated := renderIndex(tree)
	path := filepath.Join(root, indexFile)

	if *check {
		return checkIndex(path, generated, stdout)
	}

	if err := os.WriteFile(path, []byte(generated), filePerm); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "devindex: wrote %s (%s)\n", path, tree.tally())
	return nil
}

// checkIndex compares the on-disk index with what would be generated now and
// reports the first difference, so a stale index is actionable rather than a
// bare exit code.
func checkIndex(path, generated string, stdout io.Writer) error {
	current, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%s does not exist; run `make dev-index`", path)
	case err != nil:
		return err
	}
	if string(current) == generated {
		fmt.Fprintf(stdout, "devindex: %s is up to date\n", path)
		return nil
	}
	line, want, got := firstDifference(string(current), generated)
	return fmt.Errorf("%s is out of date; run `make dev-index`\n  line %d\n  have: %s\n  want: %s",
		path, line, quoteLine(got), quoteLine(want))
}

// firstDifference returns the 1-based line number where current and generated
// diverge, along with the generated ("want") and current ("got") lines.
func firstDifference(current, generated string) (line int, want, got string) {
	cur := strings.Split(current, "\n")
	gen := strings.Split(generated, "\n")
	for i := 0; i < len(cur) || i < len(gen); i++ {
		got, want = "", ""
		if i < len(cur) {
			got = cur[i]
		}
		if i < len(gen) {
			want = gen[i]
		}
		if got != want {
			return i + 1, want, got
		}
	}
	return 0, "", ""
}

func quoteLine(s string) string {
	if s == "" {
		return "(end of file)"
	}
	return fmt.Sprintf("%q", s)
}
