package storage

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/b3vet/atlascache/internal/glob"
)

// The table of Redis-verified patterns lives with the port, in
// internal/glob/glob_test.go. What is left to check here is that the keyspace's
// own names still reach it — a wrapper that silently stopped delegating would
// pass every test in that package and break every KEYS in this one.
//
// The differential run in glob_redis_test.go goes through these same wrappers,
// so the agreement with a real Redis is asserted on the path KEYS and SCAN
// actually take.
func TestKeyspaceGlobDelegatesToThePort(t *testing.T) {
	for _, tc := range []struct{ pattern, key string }{
		{"user:*", "user:1234"},
		{"user:*", "session:1"},
		{"[a-c]?", "b1"},
		{`a\*b`, "a*b"},
		{"[abc", "a"},
		{"*", ""},
	} {
		assert.Equalf(t, glob.Match(tc.pattern, tc.key), MatchGlob(tc.pattern, tc.key),
			"MatchGlob(%q, %q) must answer what the port answers", tc.pattern, tc.key)
	}

	assert.Equal(t, glob.MatchAllPattern, MatchAllPattern)
	assert.True(t, MatchesEveryKey("*"))
	assert.False(t, MatchesEveryKey(""), "an empty pattern is not the match-all pattern")
	assert.False(t, MatchesEveryKey("**"), "only a bare star is short circuited, as in Redis")
}
