package storage

import "github.com/b3vet/atlascache/internal/glob"

// Glob matching, as Redis does it (FEAT-0020)
//
// The port of `stringmatchlen` itself lives in internal/glob. It moved there
// when `CONFIG GET` needed the same semantics for parameter names
// (ISSUE-0021): KEYS, SCAN MATCH and CONFIG GET all ask "does this glob select
// this name?", and two ports of the same Redis function in one binary would
// eventually disagree about a pattern — which is exactly what the differential
// run against a real Redis exists to catch, and it would then be catching it in
// only one of them.
//
// The keyspace keeps its own names for the operation because its callers read
// better for them, and because "does this key match" is part of the vocabulary
// the engine speaks.

// MatchGlob reports whether key matches pattern under Redis's glob semantics.
//
// Note the deliberate asymmetry callers rely on: MatchGlob("*", "") is false,
// because Redis's matcher requires the pattern and the subject to run out
// together. Redis's KEYS and SCAN never ask it that question — they short
// circuit a bare "*" to "every key" before calling the matcher, which is why an
// empty key is returned by `KEYS *` and not by `KEYS *x`. MatchAllPattern is
// that short circuit, and callers must apply it to agree with Redis.
func MatchGlob(pattern, key string) bool { return glob.Match(pattern, key) }

// MatchAllPattern is the pattern Redis answers without consulting the matcher
// at all. Every key matches it, the empty key included.
const MatchAllPattern = glob.MatchAllPattern

// MatchesEveryKey reports whether the pattern is the one that selects the whole
// keyspace. It exists so that KEYS and SCAN MATCH share one answer to the
// question rather than each spelling the comparison out.
func MatchesEveryKey(pattern string) bool { return glob.MatchesEverything(pattern) }
