package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoadRefusesUnknownKeys is ISSUE-0024 itself: the key the operator wrote
// is the one the message has to name.
// testConfigName is the file name the checks below report against.
const testConfigName = "config.yaml"

func TestLoadRefusesUnknownKeys(t *testing.T) {
	tests := []struct {
		name    string
		content string
		// mentions is what the message must contain to be actionable.
		mentions []string
		// absent is what it must not contain — a suggestion that is not close
		// enough to be right sends an operator further from the fix, not
		// nearer it.
		absent []string
	}{
		{
			name:     "the key that started the issue",
			content:  "server:\n  port: 7911\n",
			mentions: []string{`unknown field "port"`, "in server", `did you mean "client_port"`, "config.yaml:2"},
		},
		{
			name:     "the production version",
			content:  "auth:\n  enbaled: true\n  token: \"a-token\"\n",
			mentions: []string{`unknown field "enbaled"`, "in auth", `did you mean "enabled"`},
		},
		{
			name:     "an unknown section",
			content:  "lgging:\n  level: debug\n",
			mentions: []string{`unknown field "lgging"`, "at the top level", `did you mean "logging"`},
		},
		{
			name:     "a key under the wrong parent",
			content:  "max_connections: 50000\n",
			mentions: []string{`unknown field "max_connections"`, `did you mean "server.max_connections"`},
		},
		{
			name:     "a key that resembles nothing",
			content:  "storage:\n  banana: 3\n",
			mentions: []string{`unknown field "banana"`, "in storage"},
			absent:   []string{"did you mean"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), testConfigName)
			require.NoError(t, os.WriteFile(path, []byte(tc.content), 0o600))

			cfg, err := NewLoader().LoadFromFile(path)

			require.Error(t, err)
			assert.Nil(t, cfg, "a rejected config must not be half-applied")
			for _, want := range tc.mentions {
				assert.Contains(t, err.Error(), want)
			}
			for _, unwanted := range tc.absent {
				assert.NotContains(t, err.Error(), unwanted)
			}
		})
	}
}

// TestLoadReportsEveryUnknownKeyAtOnce covers the other half of the fix. An
// operator fixing a config one error per restart is the experience this is
// meant to replace, not reproduce.
func TestLoadReportsEveryUnknownKeyAtOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), testConfigName)
	content := "" +
		"server:\n" +
		"  max_conections: 50000\n" +
		"auth:\n" +
		"  enbaled: true\n" +
		"lgging:\n" +
		"  level: debug\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	_, err := NewLoader().LoadFromFile(path)

	require.Error(t, err)
	message := err.Error()
	assert.Contains(t, message, "3 unknown fields")
	assert.Contains(t, message, `line 2: unknown field "max_conections" in server (did you mean "max_connections"?)`)
	assert.Contains(t, message, `line 4: unknown field "enbaled" in auth (did you mean "enabled"?)`)
	assert.Contains(t, message, `line 5: unknown field "lgging" at the top level (did you mean "logging"?)`)

	var unknown *UnknownKeysError
	require.ErrorAs(t, err, &unknown)
	require.Len(t, unknown.Keys, 3)
	// Reported in file order, so the list reads down the file being fixed.
	assert.Equal(t, []int{2, 4, 5}, []int{unknown.Keys[0].Line, unknown.Keys[1].Line, unknown.Keys[2].Line})
}

// TestLoadAcceptsEveryDocumentedKey is the drift guard on the example config.
// It is the file most users copy, so a key that stops existing has to fail
// here rather than in somebody's first run.
func TestLoadAcceptsEveryDocumentedKey(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	require.NoError(t, err)

	assert.NoError(t, CheckUnknownKeys("config.example.yaml", data))
}

func TestCheckUnknownKeys(t *testing.T) {
	t.Run("a valid config passes", func(t *testing.T) {
		content := "" +
			"server:\n  bind_addr: \"127.0.0.1\"\n  client_port: 6380\n" +
			"auth:\n  enabled: true\n  token: \"a-token\"\n"

		assert.NoError(t, CheckUnknownKeys(testConfigName, []byte(content)))
	})

	t.Run("a file that does not parse is left to the loader", func(t *testing.T) {
		// Reporting "unknown field" for a syntax error would name the wrong
		// problem; viper reports the parse error with the line it failed on.
		assert.NoError(t, CheckUnknownKeys(testConfigName, []byte("server: [this: is not\n  valid yaml\n")))
	})

	t.Run("an empty file passes", func(t *testing.T) {
		assert.NoError(t, CheckUnknownKeys(testConfigName, nil))
	})

	t.Run("a document that is not a mapping passes", func(t *testing.T) {
		assert.NoError(t, CheckUnknownKeys(testConfigName, []byte("- one\n- two\n")))
	})

	t.Run("a section written as a scalar is left to the decoder", func(t *testing.T) {
		// mapstructure reports it, naming the type it wanted. Walking into it
		// here would report nothing an operator could act on.
		assert.NoError(t, CheckUnknownKeys(testConfigName, []byte("server: 6379\n")))
	})
}

// TestSuggestionsOnlyFireWhenTheyHelp is the judgement call in the fix: a
// wrong suggestion is worse than none, so ambiguity produces silence.
func TestSuggestionsOnlyFireWhenTheyHelp(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "a transposition",
			content: "auth:\n  enbaled: true\n",
			want:    "enabled",
		},
		{
			name:    "separators and case are never meaningful",
			content: "server:\n  clientPort: 6380\n",
			want:    "client_port",
		},
		{
			name:    "a whole word inside a longer key",
			content: "server:\n  port: 6380\n",
			want:    "client_port",
		},
		{
			name:    "a key that lives in another section",
			content: "ttl:\n  policy: lru\n",
			want:    "eviction.policy",
		},
		{
			name: "nothing when two sections could have been meant",
			// enabled exists under both tls and auth, and guessing which one
			// an operator meant is how a suggestion sends them somewhere
			// wrong.
			content: "admin:\n  enabled: true\n",
			want:    "",
		},
		{
			name:    "nothing when the key resembles no key at all",
			content: "storage:\n  banana: 3\n",
			want:    "",
		},
		{
			name:    "nothing for a word that is far from every sibling",
			content: "eviction:\n  prot: 1\n",
			want:    "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckUnknownKeys(testConfigName, []byte(tc.content))

			var unknown *UnknownKeysError
			require.ErrorAs(t, err, &unknown)
			require.Len(t, unknown.Keys, 1)
			assert.Equal(t, tc.want, unknown.Keys[0].Suggestion)
		})
	}
}

// TestUnknownKeysErrorMessage pins the shape of the message, which is the whole
// value of the fix: an error that says only "invalid config" sends people back
// to guessing.
func TestUnknownKeysErrorMessage(t *testing.T) {
	t.Run("one key names the file and the line", func(t *testing.T) {
		err := &UnknownKeysError{
			Path: "/etc/atlascache/config.yaml",
			Keys: []UnknownKey{{Key: "enbaled", Section: "auth", Line: 29, Suggestion: "enabled"}},
		}

		assert.Equal(t,
			`/etc/atlascache/config.yaml:29: unknown field "enbaled" in auth (did you mean "enabled"?)`,
			err.Error())
	})

	t.Run("a key with no line still names the file", func(t *testing.T) {
		err := &UnknownKeysError{Path: testConfigName, Keys: []UnknownKey{{Key: "wat"}}}

		assert.Equal(t, `config.yaml: unknown field "wat" at the top level`, err.Error())
	})

	t.Run("several keys are listed under one heading", func(t *testing.T) {
		err := &UnknownKeysError{
			Path: testConfigName,
			Keys: []UnknownKey{
				{Key: "prot", Section: "server", Line: 2},
				{Key: "enbaled", Section: "auth", Line: 9, Suggestion: "enabled"},
			},
		}

		assert.Equal(t, "config.yaml: 2 unknown fields:\n"+
			`  - line 2: unknown field "prot" in server`+"\n"+
			`  - line 9: unknown field "enbaled" in auth (did you mean "enabled"?)`,
			err.Error())
	})
}

// TestReloadRejectsUnknownKeysAndKeepsRunning is the constraint the fix must
// not break: strict decoding at startup is right, and the same strictness
// applied to a hot reload must never be a way to kill a running server.
func TestReloadRejectsUnknownKeysAndKeepsRunning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, testConfigName)
	require.NoError(t, os.WriteFile(path, []byte("eviction:\n  policy: lru\n"), 0o600))

	watcher, err := NewWatcher(NewLoader(), path)
	require.NoError(t, err)
	defer func() { assert.NoError(t, watcher.Stop()) }()

	// The callbacks run in their own goroutines, so what they applied is read
	// through a channel rather than a slice.
	applied := make(chan string, 4)
	watcher.OnChange(func(cfg *Config) { applied <- cfg.Eviction.Policy })

	require.Equal(t, "lru", watcher.Config().Eviction.Policy)

	// The typo comes with a change that would be visible if it were applied,
	// so this proves the running config was kept rather than merely that
	// nothing crashed.
	require.NoError(t, os.WriteFile(path, []byte("eviction:\n  policy: fifo\nauth:\n  enbaled: true\n"), 0o600))
	watcher.reload()

	assert.Equal(t, "lru", watcher.Config().Eviction.Policy, "a rejected reload must leave the running config alone")
	select {
	case policy := <-applied:
		t.Fatalf("a rejected reload reached the callbacks with policy %q", policy)
	case <-time.After(100 * time.Millisecond):
	}

	// And the watcher is not wedged: the next good config still applies.
	require.NoError(t, os.WriteFile(path, []byte("eviction:\n  policy: fifo\n"), 0o600))
	watcher.reload()

	assert.Equal(t, "fifo", watcher.Config().Eviction.Policy)
	select {
	case policy := <-applied:
		assert.Equal(t, "fifo", policy)
	case <-time.After(5 * time.Second):
		t.Fatal("a valid reload after a rejected one never reached the callbacks")
	}
}

func TestEnvWarnings(t *testing.T) {
	tests := []struct {
		name string
		env  []string
		want []string
	}{
		{
			name: "a key the loader reads is not warned about",
			env:  []string{"ATLAS_EVICTION_POLICY=fifo", "ATLAS_SERVER_CLIENT_PORT=6380"},
		},
		{
			name: "a variable with no ATLAS_ prefix is none of our business",
			env:  []string{"PATH=/usr/bin", "SERVER_CLIENT_PORT=6380"},
		},
		{
			name: "the variables an orchestrator injects are left alone",
			// A Kubernetes Service named atlas puts all of these into every
			// container. Warning about them would be noise; refusing to start
			// on them would be an outage.
			env: []string{
				"ATLAS_PORT=tcp://10.0.0.1:6379",
				"ATLAS_SERVICE_HOST=10.0.0.1",
				"ATLAS_SERVICE_PORT=6379",
				"ATLAS_PORT_6379_TCP_ADDR=10.0.0.1",
			},
		},
		{
			name: "a misspelled key is named, with the variable that works",
			env:  []string{"ATLAS_AUTH_ENBALED=true"},
			want: []string{"ATLAS_AUTH_ENBALED is not a configuration key and was ignored (did you mean ATLAS_AUTH_ENABLED?)"},
		},
		{
			name: "a key that does not exist under a real section",
			env:  []string{"ATLAS_SERVER_PORT=7911"},
			want: []string{"ATLAS_SERVER_PORT is not a configuration key and was ignored (did you mean ATLAS_SERVER_CLIENT_PORT?)"},
		},
		{
			name: "a misspelled section is still recognizable",
			env:  []string{"ATLAS_AUHT_ENABLED=true"},
			want: []string{"ATLAS_AUHT_ENABLED is not a configuration key and was ignored (did you mean ATLAS_AUTH_ENABLED?)"},
		},
		{
			name: "a real section with nothing close under it",
			env:  []string{"ATLAS_STORAGE_BANANA=3"},
			want: []string{"ATLAS_STORAGE_BANANA is not a configuration key and was ignored"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, envWarnings(tc.env))
		})
	}
}

// TestEnvWarningsReadsTheProcessEnvironment covers the exported entry point,
// which is what the server calls once logging is configured.
func TestEnvWarningsReadsTheProcessEnvironment(t *testing.T) {
	t.Setenv("ATLAS_AUTH_ENBALED", "true")

	warnings := EnvWarnings()

	require.NotEmpty(t, warnings)
	assert.Contains(t, strings.Join(warnings, "\n"), "ATLAS_AUTH_ENBALED")
}

// TestSchemaMatchesTheStruct checks the derivation rather than a copy of it: the
// schema is reflected out of Config so that a renamed field cannot leave its
// old name accepted.
func TestSchemaMatchesTheStruct(t *testing.T) {
	schema := configSchema()

	assert.Equal(t,
		[]string{"admin", "auth", "eviction", "logging", "node", "server", "storage", "tls", "ttl"},
		schema.keys())

	require.Contains(t, schema.children, "auth")
	require.NotNil(t, schema.children["auth"], "auth is a section, not a value")
	assert.Equal(t, []string{"enabled", "token"}, schema.children["auth"].keys())

	// Secret and time.Duration are values with their own parsers, not sections
	// to walk into.
	assert.Nil(t, schema.children["auth"].children["token"])
	assert.Nil(t, schema.children["server"].children["client_idle_timeout"])

	assert.Contains(t, knownPaths(), "server.client_port")
	assert.Contains(t, knownPaths(), "auth")
}
