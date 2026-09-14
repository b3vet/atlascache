package config

import (
	"reflect"
	"sort"
	"strings"
	"time"
)

// secretPaths is the set of dotted configuration paths whose values are
// secrets, written the way the configuration file writes them.
//
// This is the path-based half of redaction (FEAT-0030). It is deliberately not
// name matching: a rule like "redact any field whose name contains token"
// redacts `auth.token_file` — a path, not a secret — and misses `admin.api_key`
// entirely, and neither failure is visible until somebody reads a response
// closely. A path is exact, it is reviewable, and adding one is a one-line diff
// that shows up in a security review.
//
// The type-based half is Secret: a value of that type is redacted wherever it
// appears, at any path, registered here or not. The two overlap on purpose.
// Registering a path costs nothing and covers a field that holds a secret but
// was declared a plain string by mistake; the type covers a secret added at a
// path nobody remembered to register. A secret has to escape both to leak, and
// TestEverySecretFieldIsRedacted fails the build if one does.
var secretPaths = map[string]bool{
	fieldAuthToken:  true,
	fieldAdminToken: true,
}

// SecretPaths returns the registered secret paths, sorted. It exists so that a
// test, or a future `atlasctl config` renderer, can state the set rather than
// rediscovering it.
func SecretPaths() []string {
	paths := make([]string, 0, len(secretPaths))
	for path := range secretPaths {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// IsSecretPath reports whether a dotted configuration path holds a secret.
func IsSecretPath(path string) bool { return secretPaths[path] }

var (
	secretType   = reflect.TypeOf(Secret(""))
	durationType = reflect.TypeOf(time.Duration(0))
)

// Effective renders the configuration the way the admin API returns it: every
// field under its configuration-file name, nested exactly as the file nests it,
// with every secret replaced by its redaction.
//
// It is built by walking the struct rather than by listing fields, so a field
// added to Config appears here without anyone remembering to add it — and,
// more to the point, so a *secret* added to Config is redacted here without
// anyone remembering to add it. A hand-written renderer gets the first of those
// wrong quietly and the second of those wrong dangerously.
//
// Durations are rendered in their string form ("30s"), which is what the
// configuration file holds and what viper reads back, so a response can be
// diffed against a config file without a conversion step.
func (c *Config) Effective() map[string]any {
	if c == nil {
		return map[string]any{}
	}

	rendered, ok := renderValue(reflect.ValueOf(*c), "").(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return rendered
}

// renderValue renders one configuration value, redacting it if either half of
// the rule applies: its path is registered, or its type is Secret.
func renderValue(v reflect.Value, path string) any {
	if isSecret(v.Type(), path) {
		return redact(v)
	}

	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return nil
		}
		return renderValue(v.Elem(), path)
	case reflect.Struct:
		return renderStruct(v, path)
	case reflect.Slice, reflect.Array:
		return renderList(v, path)
	case reflect.Map:
		return renderMap(v, path)
	default:
		return renderLeaf(v)
	}
}

// renderStruct renders a configuration section, keyed by the mapstructure tags
// the loader reads, so the response and the config file use one vocabulary.
func renderStruct(v reflect.Value, path string) any {
	structType := v.Type()
	out := make(map[string]any, structType.NumField())

	for i := range structType.NumField() {
		field := structType.Field(i)
		if !field.IsExported() {
			continue
		}

		name := fieldName(field)
		if name == "" {
			continue
		}
		out[name] = renderValue(v.Field(i), join(path, name))
	}
	return out
}

func renderList(v reflect.Value, path string) any {
	out := make([]any, 0, v.Len())
	for i := range v.Len() {
		out = append(out, renderValue(v.Index(i), path))
	}
	return out
}

func renderMap(v reflect.Value, path string) any {
	out := make(map[string]any, v.Len())
	for _, key := range v.MapKeys() {
		name := keyText(key)
		out[name] = renderValue(v.MapIndex(key), join(path, name))
	}
	return out
}

// renderLeaf renders a scalar. Durations become their string form; everything
// else is handed over as the Go value, which encoding/json renders natively.
func renderLeaf(v reflect.Value) any {
	if v.Type() == durationType {
		return time.Duration(v.Int()).String()
	}
	if !v.CanInterface() {
		return nil
	}
	return v.Interface()
}

// isSecret is the whole redaction rule, in one place so that neither half can
// be applied without the other.
func isSecret(t reflect.Type, path string) bool {
	return t == secretType || (path != "" && secretPaths[path])
}

// redact replaces a secret with its redaction, preserving only whether it was
// set. A field that was never configured renders empty, because "no token is
// configured" is something an operator has to be able to see and is not itself
// a disclosure.
func redact(v reflect.Value) any {
	if v.IsZero() {
		return ""
	}
	return RedactedValue
}

// fieldName is the configuration-file name of a struct field: its mapstructure
// tag, or its lower-cased Go name when it carries none. A field tagged "-" is
// not part of the configuration and is skipped.
func fieldName(field reflect.StructField) string {
	tag, ok := field.Tag.Lookup("mapstructure")
	if !ok {
		return strings.ToLower(field.Name)
	}

	name, _, _ := strings.Cut(tag, ",")
	if name == "-" {
		return ""
	}
	if name == "" {
		return strings.ToLower(field.Name)
	}
	return name
}

// keyText renders a map key as a path segment. Configuration maps are
// string-keyed today; anything else is rendered through its String method
// rather than dropped, so an added map cannot silently lose entries.
func keyText(key reflect.Value) string {
	if key.Kind() == reflect.String {
		return key.String()
	}
	if stringer, ok := key.Interface().(interface{ String() string }); ok {
		return stringer.String()
	}
	return ""
}

func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}
