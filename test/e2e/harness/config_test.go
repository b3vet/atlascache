package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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
// from reading each other's data. A spec must not be able to take them, however
// plausibly it asks.
func TestTheHarnessKeysAreForcedBackHoweverASpecWritesThem(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	config := render(t, dataDir, "greedy", 11111, 22222, map[string]any{
		nodeBlock:   map[string]any{"data_dir": "/tmp/somewhere-of-its-own", "id": "chosen-by-the-spec"},
		serverBlock: map[string]any{clientPortKey: 6379, bindAddrKey: "0.0.0.0"},
		adminBlock:  map[string]any{"port": 6380},
	})

	if got := block(t, config, nodeBlock)["data_dir"]; got != dataDir {
		t.Errorf("node.data_dir = %#v, want the harness's %q even though the spec asked for its own", got, dataDir)
	}
	// id is not one of the forced keys, so the spec keeps it — and keeping it is
	// the proof that forcing data_dir back did not take the rest of the block.
	if got := block(t, config, nodeBlock)["id"]; got != "chosen-by-the-spec" {
		t.Errorf("node.id = %#v, want the spec's own value", got)
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

// TestASpecThatReplacesABlockWithAScalarIsRefused. The harness writes its own
// keys into `node:`, `server:` and `admin:`. A spec that made one of them
// something other than a mapping used to have it silently replaced — `node:
// "x"` became `node: {data_dir: ...}`, and node.id and node.name went with it.
// The spec meant something, it cannot be honored, and being told beats running
// a server configured by nobody.
func TestASpecThatReplacesABlockWithAScalarIsRefused(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		config map[string]any
		want   []string
	}{
		{
			name:   "a block replaced by a string",
			config: map[string]any{nodeBlock: "not-a-map-at-all"},
			want:   []string{"node is string", "node.data_dir"},
		},
		{
			name:   "a block replaced by a number",
			config: map[string]any{adminBlock: 6380},
			want:   []string{"admin is int", "admin.port"},
		},
		{
			name:   "a block replaced by a list",
			config: map[string]any{serverBlock: []any{clientPortKey}},
			want:   []string{"server is []interface {}", "server.client_port"},
		},
		{
			name: "the tls block, which the harness owns too once a spec asks for TLS",
			// `tls: true` is the plausible mistake: it reads like turning TLS on,
			// and there is then nowhere to put the certificate paths.
			config: map[string]any{"tls": map[string]any{"enabled": true}, adminBlock: "6380"},
			want:   []string{"admin is string", "admin.port"},
		},
		{
			name:   "two blocks wrong at once are both reported",
			config: map[string]any{nodeBlock: "x", serverBlock: 1},
			want:   []string{"node.data_dir", "server.client_port"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := checkConfig(tc.config)
			if err == nil {
				t.Fatalf("checkConfig(%#v) = nil; the spec asked for something impossible", tc.config)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %v, want it to mention %q", err, want)
				}
			}

			// And the same mistake must not get through by another door: the
			// render that would have dropped the sibling keys refuses as well.
			path := filepath.Join(t.TempDir(), "config.yaml")
			if writeErr := writeConfig(path, t.TempDir(), "refused", 11111, 22222, tc.config); writeErr == nil {
				t.Error("writeConfig rendered a config the harness cannot write its own keys into")
			}
			if _, statErr := os.Stat(path); statErr == nil {
				t.Error("a refused config was written to disk anyway")
			}
		})
	}
}

// TestAConfigTheHarnessCanWriteIntoIsAccepted. The refusal above must be narrow.
// A spec that leaves a block out, writes it empty, or writes it as a mapping
// with non-string keys is asking for nothing impossible, and a check that
// rejected those would fail specs for no reason at all.
func TestAConfigTheHarnessCanWriteIntoIsAccepted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		config map[string]any
	}{
		{name: "no config at all"},
		{name: "an unrelated block", config: map[string]any{"storage": map[string]any{"shard_count": 1}}},
		// `node:` with nothing under it decodes to nil, which is a block the
		// spec left empty rather than a block it replaced.
		{name: "a block written with no value", config: map[string]any{nodeBlock: nil}},
		{name: "a mapping with non-string keys", config: map[string]any{serverBlock: map[any]any{128: "legacy"}}},
		{name: "tls off, so the certificate paths are not forced", config: map[string]any{"tls": map[string]any{"enabled": false}}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := checkConfig(tc.config); err != nil {
				t.Errorf("checkConfig(%#v) = %v, want nil", tc.config, err)
			}
		})
	}

	// And the block a spec never mentioned is still created rather than
	// refused: every harness key is set this way, so a setPath that only ever
	// wrote into blocks that already existed would set none of them.
	t.Run("a block nobody wrote is created", func(t *testing.T) {
		t.Parallel()

		config := map[string]any{}
		if err := setPath(config, 6379, "server", clientPortKey); err != nil {
			t.Fatalf("setPath into an empty config: %v", err)
		}
		section, ok := asMap(config[serverBlock])
		if !ok {
			t.Fatalf("config.server = %#v, want the block to have been created", config[serverBlock])
		}
		if got := section[clientPortKey]; got != 6379 {
			t.Errorf("server.client_port = %#v, want 6379", got)
		}
	})
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
