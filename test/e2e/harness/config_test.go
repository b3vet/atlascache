package harness

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// The config keys these tests reach into. They are named once so the assertions
// read as prose rather than as string soup — and so repeating them here does not
// push the package's literal count over the linter's duplication threshold.
const (
	nodeBlock     = "node"
	serverBlock   = "server"
	adminBlock    = "admin"
	bindAddrKey   = "bind_addr"
	clientPortKey = "client_port"
)

// render writes a config with the given overrides and reads it back as YAML, so
// the assertions below are made against what the server would actually be given
// rather than against the map on the way in.
func render(t *testing.T, dataDir, specName string, clientPort, adminPort int, overrides map[string]any) map[string]any {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := writeConfig(path, dataDir, specName, clientPort, adminPort, overrides); err != nil {
		t.Fatalf("writeConfig: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the rendered config: %v", err)
	}
	var config map[string]any
	if err := yaml.Unmarshal(data, &config); err != nil {
		t.Fatalf("the rendered config is not YAML: %v\n%s", err, data)
	}
	return config
}

// block reads one top-level section of a rendered config.
func block(t *testing.T, config map[string]any, name string) map[string]any {
	t.Helper()
	section, ok := config[name].(map[string]any)
	if !ok {
		t.Fatalf("config.%s = %#v, want a map", name, config[name])
	}
	return section
}

// TestSpecOverridesMergeIntoTheBlockRatherThanReplacingIt. A spec tuning one key
// under `server:` must not delete the rest of the block: the harness's bind
// address lives there, and losing it would put the server under test on a
// routable interface.
func TestSpecOverridesMergeIntoTheBlockRatherThanReplacingIt(t *testing.T) {
	t.Parallel()

	config := render(t, t.TempDir(), "merge-spec", 11111, 22222, map[string]any{
		serverBlock: map[string]any{"max_clients": 10},
	})

	server := block(t, config, serverBlock)
	if got := server["max_clients"]; got != 10 {
		t.Errorf("server.max_clients = %#v, want the spec's 10", got)
	}
	if got := server[bindAddrKey]; got != loopback {
		t.Errorf("server.bind_addr = %#v, want the harness default %q to survive the merge", got, loopback)
	}
	if got := block(t, config, "logging")["level"]; got != "debug" {
		t.Errorf("logging.level = %#v; an untouched block must survive a merge into another one", got)
	}
}

// TestMergeUnderstandsAMappingWithNonStringKeys. yaml.v3 decodes a mapping into
// map[string]any only when every key is a string, and into map[any]any
// otherwise. A merge that recognized just the first shape would treat the second
// as an opaque value and replace the whole block with it — quietly dropping the
// bind address and the data directory the harness put there.
func TestMergeUnderstandsAMappingWithNonStringKeys(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	config := render(t, dataDir, "any-keys", 11111, 22222, map[string]any{
		serverBlock: map[any]any{"max_clients": 10, 128: "legacy"},
	})

	server := block(t, config, serverBlock)
	if got := server["max_clients"]; got != 10 {
		t.Errorf("server.max_clients = %#v, want the spec's 10", got)
	}
	if got := server["128"]; got != "legacy" {
		t.Errorf("server.128 = %#v, want a non-string key carried through as text", got)
	}
	if got := server[bindAddrKey]; got != loopback {
		t.Errorf("server.bind_addr = %#v; the whole block was replaced instead of merged", got)
	}
	if got := server[clientPortKey]; got != 11111 {
		t.Errorf("server.client_port = %#v, want the harness's port %d", got, 11111)
	}
}

// TestTheHarnessKeysAreForcedBackHoweverASpecWritesThem. The two ports and the
// data directory are what keeps parallel specs from colliding on a port and
// from reading each other's data. A spec must not be able to take them, and it
// must not be able to take them by replacing the whole block with something that
// is not a map either.
func TestTheHarnessKeysAreForcedBackHoweverASpecWritesThem(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	config := render(t, dataDir, "greedy", 11111, 22222, map[string]any{
		nodeBlock:   "not-a-map-at-all",
		serverBlock: map[string]any{clientPortKey: 6379, bindAddrKey: "0.0.0.0"},
		adminBlock:  map[string]any{"port": 6380},
	})

	if got := block(t, config, nodeBlock)["data_dir"]; got != dataDir {
		t.Errorf("node.data_dir = %#v, want the harness's %q even though the spec replaced the block", got, dataDir)
	}
	if got := block(t, config, serverBlock)[clientPortKey]; got != 11111 {
		t.Errorf("server.client_port = %#v, want the harness's 11111", got)
	}
	if got := block(t, config, adminBlock)["port"]; got != 22222 {
		t.Errorf("admin.port = %#v, want the harness's 22222", got)
	}
	// bind_addr is not one of the forced keys, so the spec keeps it; this is the
	// line that would have to change if that ever stopped being true.
	if got := block(t, config, serverBlock)[bindAddrKey]; got != "0.0.0.0" {
		t.Errorf("server.bind_addr = %#v, want the spec's own value", got)
	}
}

// TestWriteConfigReportsAPathItCannotWrite. A config that was never written
// would leave the server reading whatever was there before, or nothing at all,
// and the spec would fail somewhere far from the cause.
func TestWriteConfigReportsAPathItCannotWrite(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "absent-directory", "config.yaml")
	err := writeConfig(path, t.TempDir(), "unwritable", 1, 2, nil)
	if err == nil {
		t.Fatal("writeConfig reported success for a path it cannot write")
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("the config file exists after a failed write")
	}
}
