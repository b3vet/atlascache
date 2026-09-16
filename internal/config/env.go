package config

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
)

// EnvPrefix is what an environment override is named with: the prefix, then
// the dotted key upper-cased with its dots written as underscores, so that
// ATLAS_EVICTION_POLICY sets eviction.policy.
const EnvPrefix = "ATLAS_"

// EnvWarnings reports ATLAS_-prefixed environment variables that look like an
// attempt to set a configuration key and set none.
//
// These warn; they do not refuse the start, and the asymmetry with an unknown
// key in the config file is deliberate (ISSUE-0024). A config file is an
// artifact the operator wrote, so every key in it is a claim about intent and
// one the server cannot honor means they have misunderstood something. The
// environment is not theirs alone: Kubernetes and Docker inject a block of
// variables per linked service, so a Service named `atlas` puts ATLAS_PORT,
// ATLAS_SERVICE_HOST and half a dozen more into every container before the
// process starts. Refusing to boot on those would make this check an outage in
// exactly the deployment it is meant to protect.
//
// The same reasoning bounds what is warned about. A variable is reported only
// when its first segment names a configuration section, or is a near miss for
// one — which is what tells ATLAS_SERVER_PORT, an operator's mistake, from
// ATLAS_SERVICE_HOST, an orchestrator's bookkeeping.
func EnvWarnings() []string { return envWarnings(os.Environ()) }

func envWarnings(environ []string) []string {
	known := knownEnvNames()

	var warnings []string
	for _, entry := range environ {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || !strings.HasPrefix(name, EnvPrefix) {
			continue
		}
		if known[name] {
			continue
		}
		if warning := unknownEnvWarning(name); warning != "" {
			warnings = append(warnings, warning)
		}
	}

	sort.Strings(warnings)
	return warnings
}

// unknownEnvWarning describes one variable, or returns empty when it is not an
// attempt at a configuration key.
func unknownEnvWarning(name string) string {
	section, remainder, _ := strings.Cut(strings.ToLower(strings.TrimPrefix(name, EnvPrefix)), "_")

	target := nearest(section, sectionNames())
	if target == "" {
		return ""
	}

	message := fmt.Sprintf("%s is not a configuration key and was ignored", name)
	if key := nearest(remainder, configSchema().children[target].keys()); key != "" {
		message += fmt.Sprintf(" (did you mean %s?)", envNameFor(join(target, key)))
	}
	return message
}

// sectionNames lists the top-level sections, which are what the first segment
// of an override names.
var sectionNames = sync.OnceValue(func() []string {
	schema := configSchema()
	names := make([]string, 0, len(schema.children))
	for _, name := range schema.keys() {
		if schema.children[name] != nil {
			names = append(names, name)
		}
	}
	return names
})

// knownEnvNames is the set of variables the loader actually reads, one per leaf
// key in the schema.
var knownEnvNames = sync.OnceValue(func() map[string]bool {
	var paths []string
	configSchema().paths("", &paths)

	sections := make(map[string]bool, len(paths))
	for _, path := range paths {
		if idx := strings.LastIndex(path, "."); idx >= 0 {
			sections[path[:idx]] = true
		}
	}

	names := make(map[string]bool, len(paths))
	for _, path := range paths {
		if sections[path] {
			continue
		}
		names[envNameFor(path)] = true
	}
	return names
})

// envNameFor renders the variable a dotted key is overridden by.
func envNameFor(path string) string {
	return EnvPrefix + strings.ToUpper(strings.ReplaceAll(path, ".", "_"))
}
