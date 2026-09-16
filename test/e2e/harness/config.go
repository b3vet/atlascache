package harness

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// loopback is what every harnessed server binds. A spec server that answered on
// a routable interface would be reachable from off the machine during a test
// run, and on macOS it would also raise a firewall prompt on every run.
const loopback = "127.0.0.1"

// The config blocks and keys the harness writes itself. Named once because each
// appears in three places — the defaults, the paths forced back over the spec,
// and the check that those paths can be written — and a key spelled two ways
// across them is a harness that quietly stops forcing it.
const (
	nodeBlock    = "node"
	serverBlock  = "server"
	adminBlock   = "admin"
	loggingBlock = "logging"
	tlsBlock     = "tls"

	bindAddrKey   = "bind_addr"
	clientPortKey = "client_port"
	adminPortKey  = "port"
	dataDirKey    = "data_dir"
	certFileKey   = "cert_file"
	keyFileKey    = "key_file"
)

// writeConfig renders the server's config file and returns its path.
//
// The spec's `config:` block is merged over the harness defaults, so a spec can
// tune anything it needs. Three keys are then forced back: the two ports and
// the data directory. They belong to the harness, and a spec that changed them
// would break the isolation every other spec depends on — parallel runs would
// collide on a port, and one spec's data would leak into another's.
func writeConfig(path, dataDir string, specName string, clientPort, adminPort int, overrides map[string]any) error {
	config := map[string]any{
		nodeBlock: map[string]any{
			"id":       "e2e-" + specName,
			"name":     specName,
			dataDirKey: dataDir,
		},
		serverBlock: map[string]any{
			bindAddrKey:   loopback,
			clientPortKey: clientPort,
		},
		adminBlock: map[string]any{
			bindAddrKey:  loopback,
			adminPortKey: adminPort,
		},
		loggingBlock: map[string]any{
			// debug, because the log is only ever read when something failed.
			"level":  "debug",
			"format": "json",
		},
	}

	config = mergeConfig(config, overrides)
	// Every path is attempted rather than the first failure returned, so a spec
	// that got two blocks wrong is told about both in one run.
	if err := errors.Join(
		setPath(config, clientPort, serverBlock, clientPortKey),
		setPath(config, adminPort, adminBlock, adminPortKey),
		setPath(config, dataDir, nodeBlock, dataDirKey),
	); err != nil {
		return err
	}

	data, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("rendering the server config: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("writing the server config: %w", err)
	}
	return nil
}

// tlsRequested reports whether the spec's config block turns TLS on.
//
// It is read before the server is launched, because the certificate has to
// exist before the config naming it is written.
func tlsRequested(config map[string]any) bool {
	section, ok := asMap(config[tlsBlock])
	if !ok {
		return false
	}
	enabled, ok := section["enabled"].(bool)
	return ok && enabled
}

// withTLSPaths returns the spec's config with the certificate paths forced to
// the harness's own.
//
// They belong to the harness for the same reason the ports and the data
// directory do: they live inside the directory it creates and removes, and a
// spec that pointed the server somewhere else would leave files behind and
// break the isolation every other spec depends on.
func withTLSPaths(config map[string]any, certFile, keyFile string) (map[string]any, error) {
	// Copied down to the block being written, because the map belongs to the
	// spec: the runner parsed it, and a harness that edited it in place would
	// be changing what the spec says it wants.
	updated := mergeConfig(map[string]any{}, config)
	if section, ok := asMap(updated[tlsBlock]); ok {
		updated[tlsBlock] = mergeConfig(map[string]any{}, section)
	}

	if err := errors.Join(
		setPath(updated, certFile, tlsBlock, certFileKey),
		setPath(updated, keyFile, tlsBlock, keyFileKey),
	); err != nil {
		return nil, err
	}
	return updated, nil
}

// forcedPaths are the config keys the harness writes over whatever the spec
// said: the two ports and the data directory that keep parallel specs from
// colliding, and — when the spec asks for TLS — the certificate the harness
// generated into the directory it cleans up. Each of them has to be written
// into a mapping, so each of them is a path a spec can make impossible.
func forcedPaths(config map[string]any) [][]string {
	paths := [][]string{
		{serverBlock, clientPortKey},
		{adminBlock, adminPortKey},
		{nodeBlock, dataDirKey},
	}
	if tlsRequested(config) {
		paths = append(paths, []string{tlsBlock, certFileKey}, []string{tlsBlock, keyFileKey})
	}
	return paths
}

// checkConfig reports a spec `config:` block the harness cannot write its own
// keys into, naming every path at fault.
//
// It runs when the harness is built — before a directory is made, a certificate
// is generated or a server is launched — so a spec asking for something
// impossible is told when it is read rather than diagnosed later from a server
// that started with a config nobody wrote.
func checkConfig(config map[string]any) error {
	var problems []error
	for _, path := range forcedPaths(config) {
		if err := checkPath(config, path...); err != nil {
			problems = append(problems, err)
		}
	}
	return errors.Join(problems...)
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
//
// A block on the way that is not a mapping is an error naming it, never an
// overwrite: a spec writing `node: "somewhere"` would otherwise have it
// replaced by `node: {data_dir: ...}`, taking node.id and node.name with it and
// saying nothing. A spec that asked for something impossible is told so instead.
// Nothing is written when the path is refused.
func setPath(config map[string]any, value any, keys ...string) error {
	if err := checkPath(config, keys...); err != nil {
		return err
	}

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
	return nil
}

// checkPath reports whether setPath could write at keys without destroying
// something the spec wrote, and changes nothing itself. Separating the question
// from the write is what lets the check run when the spec is read, against the
// spec's own map, rather than only when the config is rendered.
//
// An absent block is not a mismatch: setPath creates it, and there is nothing
// underneath it to lose. A block written with no value at all decodes to nil,
// which is the same thing.
func checkPath(config map[string]any, keys ...string) error {
	node := config
	for depth, key := range keys[:len(keys)-1] {
		value := node[key]
		if value == nil {
			return nil
		}
		child, ok := asMap(value)
		if !ok {
			at := strings.Join(keys[:depth+1], ".")
			return fmt.Errorf(
				"spec config: %s is %T, but the harness has to set %s; write %s as a mapping so the keys under it survive",
				at, value, strings.Join(keys, "."), at)
		}
		node = child
	}
	return nil
}
