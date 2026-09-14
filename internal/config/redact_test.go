package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEffectiveRendersTheWholeConfiguration(t *testing.T) {
	cfg := Defaults()
	effective := cfg.Effective()

	for _, section := range []string{"node", "server", "admin", "storage", "ttl", "eviction", "logging", "tls", "auth"} {
		assert.Contains(t, effective, section)
	}

	server, ok := effective["server"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "0.0.0.0", server["bind_addr"])
	assert.Equal(t, 6379, server["client_port"])

	// Durations render the way the configuration file writes them, so a
	// response can be diffed against a file without a conversion step. A raw
	// nanosecond count would be a different document from the one an operator
	// wrote.
	assert.Equal(t, "30s", server["client_idle_timeout"])

	ttl, ok := effective["ttl"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "100ms", ttl["check_interval"])
}

// TestEffectiveContainsNoSecretAnywhere is the full-body scan, run against the
// rendered document rather than against named fields.
//
// Checking auth.token and admin.token individually would pass against a
// renderer that leaked the same string through a section added later or a
// nested copy. Marshaling the whole document and searching the bytes is the
// check that cannot be satisfied by looking in the wrong place.
func TestEffectiveContainsNoSecretAnywhere(t *testing.T) {
	const (
		authSecret  = "CLIENT-TOKEN-unique-9e4f21c7"
		adminSecret = "ADMIN-TOKEN-unique-16b0d83a"
	)

	cfg := Defaults()
	cfg.Auth.Enabled = true
	cfg.Auth.Token = authSecret
	cfg.Admin.Token = adminSecret

	// Encoded the way the admin API encodes it, HTML escaping and all, because
	// a scan of a body that was escaped differently is a scan of a different
	// document.
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	require.NoError(t, encoder.Encode(cfg.Effective()))
	body := buf.String()

	assert.NotContains(t, body, authSecret)
	assert.NotContains(t, body, adminSecret)

	// Present and marked, not dropped: an operator has to be able to tell a
	// configured token from an absent one.
	assert.Contains(t, body, RedactedValue)
}

// TestEverySecretFieldIsRedacted is the guard against the next secret.
//
// It walks the configuration struct and fails on any field that reads like a
// credential but is neither Secret-typed nor registered as a secret path. Name
// matching is unfit to *perform* redaction — it redacts auth.token_file and
// misses admin.api_key — but it is exactly right for *detecting* a field that
// somebody forgot to protect, because the cost of a false positive here is a
// one-line fix and the cost of a false negative in production is a disclosed
// credential.
func TestEverySecretFieldIsRedacted(t *testing.T) {
	rendered := Defaults().Effective()

	for _, path := range secretLookingPaths(t, reflect.TypeOf(Config{}), "") {
		assert.True(t, isRedacted(t, rendered, path),
			"%s reads like a credential but is neither a config.Secret nor a registered secret path; "+
				"declare it as Secret, or add it to secretPaths", path)
	}
}

// TestRegisteredSecretPathsExist catches a typo in secretPaths, which would
// otherwise look exactly like a working registration.
func TestRegisteredSecretPathsExist(t *testing.T) {
	rendered := Defaults().Effective()

	for _, path := range SecretPaths() {
		_, found := lookup(rendered, path)
		assert.True(t, found, "secretPaths registers %q, which is not a field of Config", path)
	}
	assert.Equal(t, []string{"admin.token", "auth.token"}, SecretPaths())
	assert.True(t, IsSecretPath("auth.token"))
	assert.False(t, IsSecretPath("auth.enabled"))
}

// TestAPathRegisteredButNotTypedIsStillRedacted proves the two rules are
// independent. A string field at a registered path is redacted even though its
// type says nothing, which is what covers a secret somebody declared as a plain
// string.
func TestAPathRegisteredButNotTypedIsStillRedacted(t *testing.T) {
	type section struct {
		Token string `mapstructure:"token"`
		Other string `mapstructure:"other"`
	}
	type document struct {
		Auth section `mapstructure:"auth"`
	}

	rendered, ok := renderValue(
		reflect.ValueOf(document{Auth: section{Token: "plain-string-secret", Other: "not-a-secret"}}),
		"",
	).(map[string]any)
	require.True(t, ok)

	auth, ok := rendered["auth"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, RedactedValue, auth["token"], "a registered path is redacted whatever its type")
	assert.Equal(t, "not-a-secret", auth["other"])
}

// TestATypedSecretAtAnUnregisteredPathIsRedacted proves the other direction,
// which is the one that covers a secret added at a path nobody remembered.
func TestATypedSecretAtAnUnregisteredPathIsRedacted(t *testing.T) {
	type document struct {
		Cluster struct {
			Gossip Secret `mapstructure:"gossip_key"`
		} `mapstructure:"cluster"`
	}

	var doc document
	doc.Cluster.Gossip = "a-secret-nobody-registered"
	require.False(t, IsSecretPath("cluster.gossip_key"), "the point of the test is that it is unregistered")

	rendered, ok := renderValue(reflect.ValueOf(doc), "").(map[string]any)
	require.True(t, ok)
	cluster, ok := rendered["cluster"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, RedactedValue, cluster["gossip_key"])
}

func TestEffectiveOnNilConfig(t *testing.T) {
	var cfg *Config
	assert.Empty(t, cfg.Effective())
}

// TestSecretCannotBePrinted covers the ways a value escapes into a log or a
// response without anybody meaning it to. Each of the four is a real path: %v
// and %s through a log line, %#v through a debug dump, and encoding/json
// through a handler marshaling a struct whole.
func TestSecretCannotBePrinted(t *testing.T) {
	const value = "the-actual-secret-c71fa9"
	secret := Secret(value)

	assert.Equal(t, RedactedValue, fmt.Sprintf("%v", secret))
	//nolint:staticcheck // the assertion is precisely that %s reaches String rather than the raw value
	assert.Equal(t, RedactedValue, fmt.Sprintf("%s", secret))
	assert.NotContains(t, fmt.Sprintf("%#v", secret), value)

	encoded, err := json.Marshal(struct {
		Token Secret `json:"token"`
	}{Token: secret})
	require.NoError(t, err)
	assert.JSONEq(t, `{"token":"`+RedactedValue+`"}`, string(encoded))

	yaml, err := secret.MarshalYAML()
	require.NoError(t, err)
	assert.Equal(t, RedactedValue, yaml)

	// An empty secret stays empty: "<redacted>" against a token nobody set
	// would tell an operator they had configured one.
	assert.Empty(t, Secret("").String())
	assert.False(t, Secret("").IsSet())
	assert.True(t, secret.IsSet())

	// And the real value is still reachable, by an explicit conversion that is
	// visible at the call site and greppable.
	assert.Equal(t, value, string(secret))
}

// secretLookingPaths returns every leaf path whose final segment reads like a
// credential.
func secretLookingPaths(t *testing.T, structType reflect.Type, prefix string) []string {
	t.Helper()

	var found []string
	for i := range structType.NumField() {
		field := structType.Field(i)
		if !field.IsExported() {
			continue
		}
		name := fieldName(field)
		if name == "" {
			continue
		}

		path := join(prefix, name)
		if field.Type.Kind() == reflect.Struct && field.Type != secretType {
			found = append(found, secretLookingPaths(t, field.Type, path)...)
			continue
		}
		if looksLikeASecret(name) {
			found = append(found, path)
		}
	}

	sort.Strings(found)
	return found
}

// looksLikeASecret is the detection heuristic, and only the detection
// heuristic. "key_file" and "cert_file" are paths on disk rather than secrets,
// which is why the match is on whole trailing words rather than on substrings.
func looksLikeASecret(name string) bool {
	for _, word := range []string{"token", "secret", "password", "passwd", "credential", "api_key", "private_key"} {
		if name == word || strings.HasSuffix(name, "_"+word) {
			return true
		}
	}
	return false
}

func isRedacted(t *testing.T, rendered map[string]any, path string) bool {
	t.Helper()

	// Rendered against Defaults, where the secrets are empty, so the visible
	// proof is that a set value does not survive the render.
	value, found := lookup(rendered, path)
	if !found {
		return false
	}
	if text, ok := value.(string); ok && text != "" && text != RedactedValue {
		return false
	}
	return IsSecretPath(path) || fieldTypeAt(reflect.TypeOf(Config{}), path) == secretType
}

func lookup(rendered map[string]any, path string) (any, bool) {
	segments := strings.Split(path, ".")
	node := rendered

	for i, segment := range segments {
		value, ok := node[segment]
		if !ok {
			return nil, false
		}
		if i == len(segments)-1 {
			return value, true
		}
		node, ok = value.(map[string]any)
		if !ok {
			return nil, false
		}
	}
	return nil, false
}

func fieldTypeAt(structType reflect.Type, path string) reflect.Type {
	segment, rest, nested := strings.Cut(path, ".")

	for i := range structType.NumField() {
		field := structType.Field(i)
		if fieldName(field) != segment {
			continue
		}
		if !nested {
			return field.Type
		}
		if field.Type.Kind() != reflect.Struct {
			return nil
		}
		return fieldTypeAt(field.Type, rest)
	}
	return nil
}
