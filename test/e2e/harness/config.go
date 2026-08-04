package harness

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// loopback is what every harnessed server binds. A spec server that answered on
// a routable interface would be reachable from off the machine during a test
// run, and on macOS it would also raise a firewall prompt on every run.
const loopback = "127.0.0.1"

// writeConfig renders the server's config file and returns its path.
//
// The spec's `config:` block is merged over the harness defaults, so a spec can
// tune anything it needs. Three keys are then forced back: the two ports and
// the data directory. They belong to the harness, and a spec that changed them
// would break the isolation every other spec depends on — parallel runs would
// collide on a port, and one spec's data would leak into another's.
func writeConfig(path, dataDir string, specName string, clientPort, adminPort int, overrides map[string]any) error {
	config := map[string]any{
		"node": map[string]any{
			"id":       "e2e-" + specName,
			"name":     specName,
			"data_dir": dataDir,
		},
		"server": map[string]any{
			"bind_addr":   loopback,
			"client_port": clientPort,
		},
		"admin": map[string]any{
			"bind_addr": loopback,
			"port":      adminPort,
		},
		"logging": map[string]any{
			// debug, because the log is only ever read when something failed.
			"level":  "debug",
			"format": "json",
		},
	}

	config = mergeConfig(config, overrides)
	setPath(config, clientPort, "server", "client_port")
	setPath(config, adminPort, "admin", "port")
	setPath(config, dataDir, "node", "data_dir")

	data, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("rendering the server config: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("writing the server config: %w", err)
	}
	return nil
}

// mergeConfig overlays override onto base, recursing into nested maps so that a
// spec setting one key under `storage:` does not delete the rest of the block.
func mergeConfig(base, override map[string]any) map[string]any {
	merged := make(map[string]any, len(base)+len(override))
	for key, value := range base {
		merged[key] = value
	}
	for key, value := range override {
		existing, inBase := asMap(merged[key])
		incoming, isMap := asMap(value)
		if inBase && isMap {
			merged[key] = mergeConfig(existing, incoming)
			continue
		}
		merged[key] = value
	}
	return merged
}

// asMap reads a YAML mapping as map[string]any. yaml.v3 decodes a mapping with
// string keys into map[string]any and anything else into map[any]any, and a
// spec could legitimately produce either.
func asMap(value any) (map[string]any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		return typed, true
	case map[any]any:
		converted := make(map[string]any, len(typed))
		for key, item := range typed {
			converted[fmt.Sprint(key)] = item
		}
		return converted, true
	default:
		return nil, false
	}
}

// setPath writes value at the given nested key, creating maps as needed.
func setPath(config map[string]any, value any, keys ...string) {
	node := config
	for _, key := range keys[:len(keys)-1] {
		child, ok := asMap(node[key])
		if !ok {
			child = map[string]any{}
		}
		node[key] = child
		node = child
	}
	node[keys[len(keys)-1]] = value
}
