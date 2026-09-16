package scenarios

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/b3vet/atlascache/test/e2e/runner"
	"github.com/b3vet/atlascache/test/e2e/runner/fakeharness"
)

// The scenarios here need a real server — they send it SIGHUP and read its log
// — so the specs are what exercise their bodies. The helpers underneath are
// ordinary functions, and one of them is load-bearing in a way that is easy to
// get quietly wrong: withUnknownKey has to produce a config that is broken in
// exactly one way. If it produced a file that was invalid for some *other*
// reason, the scenario would still see a refused startup and still pass, while
// testing nothing.

func TestWithUnknownKeyBreaksTheConfigInExactlyOneWay(t *testing.T) {
	t.Parallel()

	original := []byte("server:\n  client_port: 6379\nauth:\n  enabled: false\n  token: \"\"\neviction:\n  policy: lru\n")

	broken, err := withUnknownKey(original)
	if err != nil {
		t.Fatalf("withUnknownKey: %v", err)
	}

	// It must still be YAML. A file the parser rejects would make the server
	// refuse for a reason that has nothing to do with unknown keys.
	var document map[string]any
	if err := yaml.Unmarshal(broken, &document); err != nil {
		t.Fatalf("the broken config is not parseable YAML, so it tests the wrong failure: %v", err)
	}

	auth, ok := document["auth"].(map[string]any)
	if !ok {
		t.Fatalf("auth block = %T, want a mapping", document["auth"])
	}
	if _, present := auth["enbaled"]; !present {
		t.Error("the typo the scenario is built around is missing")
	}
	if _, present := auth["enabled"]; present {
		t.Error("both spellings are present; the original auth block must be dropped, " +
			"or the file is ambiguous rather than wrong")
	}

	// The visible change is what proves a server did not quietly apply the
	// rejected file. Without it, "nothing changed" and "the reload was rejected"
	// look identical.
	eviction, ok := document["eviction"].(map[string]any)
	if !ok {
		t.Fatalf("eviction block = %T, want a mapping", document["eviction"])
	}
	if eviction["policy"] != "fifo" {
		t.Errorf("eviction.policy = %v, want fifo — a change visible through the admin API", eviction["policy"])
	}

	// Untouched settings must survive, or the scenario would be testing a
	// server started from a different configuration than it thinks.
	server, ok := document["server"].(map[string]any)
	if !ok || server["client_port"] != 6379 {
		t.Errorf("server block = %v, want client_port preserved", document["server"])
	}
}

func TestWithUnknownKeyRefusesInputItCannotParse(t *testing.T) {
	t.Parallel()

	if _, err := withUnknownKey([]byte("server:\n\tclient_port: [unclosed\n")); err == nil {
		t.Error("withUnknownKey accepted a config it cannot have parsed")
	}
}

func TestConfigFileRoundTrip(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	original := []byte("server:\n  client_port: 6379\n")

	if err := writeConfigFile(path, original); err != nil {
		t.Fatalf("writeConfigFile: %v", err)
	}

	// The file holds a token in the scenarios that use it, so its mode matters.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %o, want 600: this file carries a token", mode)
	}

	read, err := readConfigFile(path)
	if err != nil {
		t.Fatalf("readConfigFile: %v", err)
	}
	if string(read) != string(original) {
		t.Errorf("read back %q, want %q", read, original)
	}
}

func TestReadConfigFileNamesTheFileItCouldNotRead(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "absent.yaml")
	_, err := readConfigFile(missing)
	if err == nil {
		t.Fatal("reading a file that does not exist succeeded")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error = %v, want it to name %s", err, missing)
	}
}

// restoreConfigFile runs in a defer, after the verdict is decided, so it must
// report a failure rather than return one — and must not panic when the
// directory has already gone.
func TestRestoreConfigFileReportsRatherThanFailing(t *testing.T) {
	t.Parallel()

	h := fakeharness.New(nil)
	c := &runner.Ctx{Spec: &runner.Spec{Name: "unit"}, Harness: h}

	gone := filepath.Join(t.TempDir(), "removed", "config.yaml")
	restoreConfigFile(c, gone, []byte("server:\n"))

	log := strings.Join(c.Log(), "\n")
	if !strings.Contains(log, "restoring the original config failed") {
		t.Errorf("log = %q, want the restore failure recorded", log)
	}
}

func TestRestoreConfigFilePutsTheOriginalBack(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	original := []byte("server:\n  client_port: 6379\n")
	if err := writeConfigFile(path, original); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if err := writeConfigFile(path, []byte("broken\n")); err != nil {
		t.Fatalf("overwriting: %v", err)
	}

	c := &runner.Ctx{Spec: &runner.Spec{Name: "unit"}, Harness: fakeharness.New(nil)}
	restoreConfigFile(c, path, original)

	read, err := readConfigFile(path)
	if err != nil {
		t.Fatalf("readConfigFile: %v", err)
	}
	if string(read) != string(original) {
		t.Errorf("restored %q, want %q", read, original)
	}
	if log := strings.Join(c.Log(), "\n"); strings.Contains(log, "failed") {
		t.Errorf("a successful restore logged a failure: %q", log)
	}
}
