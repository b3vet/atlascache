package config

import (
	"encoding/json"
	"strconv"
)

// RedactedValue is what stands in for a secret everywhere a configuration value
// is rendered: the admin API's /config, a log line, a debug dump.
//
// It is a fixed string rather than a masked prefix or a length: "tok…" narrows
// a brute force, and a length narrows it too. What an operator needs from a
// redacted field is whether it is set, and the empty case answers that without
// disclosing anything.
const RedactedValue = "<redacted>"

// Secret is a configuration value that must never be rendered.
//
// The type is the mechanism, not a convention. Redaction driven by field names
// fails the moment somebody adds `admin.api_key`; redaction driven by an
// explicit path list fails the moment somebody adds a secret and forgets the
// list. A value of this type is redacted by the config renderer wherever it
// appears, at whatever path, including one nobody has registered — which is
// what "default to redacting anything unrecognized" means in practice
// (FEAT-0030).
//
// String, GoString, MarshalJSON and MarshalYAML are all overridden, because a
// leak needs only one of them: fmt's %v and %s reach String, %#v reaches
// GoString, encoding/json reaches MarshalJSON, and a YAML config dump reaches
// MarshalYAML. zerolog's Str() takes a string and so cannot be handed one of
// these without a deliberate conversion, which is the point.
//
// Reading the actual secret is an explicit conversion — string(s) — so every
// use of the real value is visible at the call site and greppable.
type Secret string

// String renders the secret as its redaction, or as empty when there is nothing
// to redact. An operator reading a config dump needs to know whether a token is
// configured; that is all this discloses.
func (s Secret) String() string {
	if s == "" {
		return ""
	}
	return RedactedValue
}

// GoString covers %#v, which would otherwise print the underlying string.
func (s Secret) GoString() string {
	return strconv.Quote(s.String())
}

// MarshalJSON covers encoding/json, so a struct carrying a Secret cannot leak
// it by being marshaled whole — including by a handler that never knew the
// field was there.
func (s Secret) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.String())
}

// MarshalYAML covers a YAML config dump for the same reason.
func (s Secret) MarshalYAML() (any, error) {
	return s.String(), nil
}

// IsSet reports whether a secret has a value, without disclosing it.
func (s Secret) IsSet() bool { return s != "" }
