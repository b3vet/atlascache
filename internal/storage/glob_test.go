package storage

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Every `want` below was read off redis:7-alpine (Redis 7.4.10) rather than
// reasoned out, using the harness in glob_redis_test.go. They are a regression
// net over the constructs the differential run covers in bulk: if an edit to
// the matcher breaks one of these, it broke compatibility, and this table says
// so without a Redis running.
func TestMatchGlobAgreesWithRedis(t *testing.T) {
	cases := []struct {
		pattern string
		key     string
		want    bool
	}{
		// The wildcard. Note the '/' cases: filepath.Match refused to let '*'
		// cross a separator, which silently hid most of a real keyspace.
		{pattern: "*", key: "anything", want: true},
		{pattern: "*", key: "a/b/c", want: true},
		{pattern: "*", key: "a/b", want: true},
		{pattern: "**", key: "abc", want: true},
		{pattern: "a**c", key: "abbc", want: true},
		{pattern: "a*c", key: "abc", want: true},
		{pattern: "a*c", key: "ac", want: true},
		{pattern: "a*c", key: "abbbc", want: true},
		{pattern: "a*c", key: "abcd", want: false},
		{pattern: "*a*", key: "bab", want: true},
		{pattern: "*a*", key: "bbb", want: false},
		{pattern: "*a", key: "a", want: true},
		{pattern: "a*", key: "a", want: true},
		{pattern: "cdn/*", key: "cdn/eu/asset.png", want: true},
		{pattern: "*/*", key: "a/b", want: true},

		// The empty pattern is not the match-all pattern. MatchAllPattern is,
		// and only because KEYS and SCAN short circuit it.
		{pattern: "", key: "", want: true},
		{pattern: "", key: "a", want: false},

		// '?' is one byte, not one rune: "é" is two bytes and needs two.
		{pattern: "?", key: "a", want: true},
		{pattern: "?", key: "", want: false},
		{pattern: "?", key: "ab", want: false},
		{pattern: "??", key: "ab", want: true},
		{pattern: "?", key: "é", want: false},
		{pattern: "??", key: "é", want: true},
		{pattern: "user:?", key: "user:12", want: false},
		{pattern: "user:*", key: "user:1", want: true},
		{pattern: "user:*", key: "session:1", want: false},

		// Character classes. Negation is spelled '^' and only '^' — Redis has
		// no '[!abc]', which is the spelling filepath.Match accepts.
		{pattern: "[abc]", key: "a", want: true},
		{pattern: "[abc]", key: "d", want: false},
		{pattern: "[a-c]", key: "b", want: true},
		{pattern: "[a-c]", key: "d", want: false},
		{pattern: "[c-a]", key: "b", want: true},
		{pattern: "[^abc]", key: "d", want: true},
		{pattern: "[^abc]", key: "a", want: false},
		{pattern: "[^a-c]", key: "z", want: true},
		{pattern: "[0-9]*", key: "0abc", want: true},
		{pattern: "[0-9]*", key: "abc", want: false},
		{pattern: "[-a]", key: "-", want: true},

		// '*' still crosses whatever a preceding class excluded, so an
		// anchored-looking pattern is not anchored.
		{pattern: "[^/]*", key: "abc", want: true},
		{pattern: "[^/]*", key: "a/c", want: true},

		// Malformed classes. Redis matches what it read and treats the rest as
		// a length mismatch; it never reports an error, which is what
		// filepath.Match did and what the old fallback was papering over.
		{pattern: "[]", key: "a", want: false},
		{pattern: "[]", key: "]", want: false},
		{pattern: "[^]", key: "a", want: true},
		{pattern: "[abc", key: "a", want: true},
		{pattern: "[abc", key: "d", want: false},
		{pattern: "a[b", key: "a[b", want: false},
		{pattern: "*a[b", key: "a[b", want: false},
		{pattern: "[a-]", key: "-", want: false},

		// Escapes, inside a class and out, including the trailing backslash
		// that has nothing to escape and so stands for itself.
		{pattern: `[a\]b]`, key: "]", want: true},
		{pattern: `[a\]b]`, key: "a", want: true},
		{pattern: `[\\]`, key: `\`, want: true},
		{pattern: `\*`, key: "*", want: true},
		{pattern: `\*`, key: "a", want: false},
		{pattern: `\?`, key: "?", want: true},
		{pattern: `\[`, key: "[", want: true},
		{pattern: `\\`, key: `\`, want: true},
		{pattern: `a\`, key: `a\`, want: true},
		{pattern: `\`, key: `\`, want: true},
		{pattern: `a\*b`, key: "a*b", want: true},
		{pattern: `a\*b`, key: "axb", want: false},
	}

	for _, tc := range cases {
		assert.Equalf(t, tc.want, MatchGlob(tc.pattern, tc.key),
			"MatchGlob(%q, %q)", tc.pattern, tc.key)
	}
}

// TestMatchGlobEmptyKeyNeedsTheShortCircuit is the one place the port is not
// self-sufficient. Redis's matcher answers "no" for `*` against the empty key;
// `KEYS *` returns it anyway, because KEYS never asks the matcher about a bare
// star. A caller that forgets the short circuit loses the empty key — which
// would be an odd way to reintroduce ISSUE-0013 a week after closing it.
func TestMatchGlobEmptyKeyNeedsTheShortCircuit(t *testing.T) {
	assert.False(t, MatchGlob("*", ""), "the matcher alone does not match an empty subject")
	assert.True(t, MatchesEveryKey("*"))
	assert.False(t, MatchesEveryKey(""), "an empty pattern is not the match-all pattern")
	assert.False(t, MatchesEveryKey("**"), "only a bare star is short circuited, as in Redis")
}

// TestMatchGlobTerminates guards the recursion. Alternating stars against a
// long subject is the shape that makes a naive backtracking matcher take
// exponential time; Redis's skipLongerMatches is what stops it, and this fails
// by timing out rather than by asserting if the port drops it.
func TestMatchGlobTerminates(t *testing.T) {
	pattern := strings.Repeat("a*", 24) + "b"
	key := strings.Repeat("a", 200)

	assert.False(t, MatchGlob(pattern, key))
}

func BenchmarkMatchGlob(b *testing.B) {
	benchmarks := map[string]struct{ pattern, key string }{
		"prefix":  {"user:*", "user:1234:session"},
		"suffix":  {"*.png", "cdn/eu/assets/logo.png"},
		"class":   {"[a-z]*:[0-9]*", "user:1234"},
		"literal": {"user:1234:session", "user:1234:session"},
	}

	for name, bm := range benchmarks {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = MatchGlob(bm.pattern, bm.key)
			}
		})
	}
}
