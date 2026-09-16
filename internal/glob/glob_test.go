package glob

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Every `want` below was read off redis:7-alpine rather than reasoned out,
// using the harness in internal/storage/glob_redis_test.go. They are a
// regression net over the constructs the differential run covers in bulk: if an
// edit to the matcher breaks one of these, it broke compatibility, and this
// table says so without a Redis running.
func TestMatchAgreesWithRedis(t *testing.T) {
	cases := []struct {
		pattern string
		subject string
		want    bool
	}{
		// The wildcard. Note the '/' cases: filepath.Match refused to let '*'
		// cross a separator, which silently hid most of a real keyspace.
		{pattern: "*", subject: "anything", want: true},
		{pattern: "*", subject: "a/b/c", want: true},
		{pattern: "*", subject: "a/b", want: true},
		{pattern: "**", subject: "abc", want: true},
		{pattern: "a**c", subject: "abbc", want: true},
		{pattern: "a*c", subject: "abc", want: true},
		{pattern: "a*c", subject: "ac", want: true},
		{pattern: "a*c", subject: "abbbc", want: true},
		{pattern: "a*c", subject: "abcd", want: false},
		{pattern: "*a*", subject: "bab", want: true},
		{pattern: "*a*", subject: "bbb", want: false},
		{pattern: "*a", subject: "a", want: true},
		{pattern: "a*", subject: "a", want: true},
		{pattern: "cdn/*", subject: "cdn/eu/asset.png", want: true},
		{pattern: "*/*", subject: "a/b", want: true},

		// The empty pattern is not the match-all pattern. MatchAllPattern is,
		// and only because KEYS and SCAN short circuit it.
		{pattern: "", subject: "", want: true},
		{pattern: "", subject: "a", want: false},

		// '?' is one byte, not one rune: "é" is two bytes and needs two.
		{pattern: "?", subject: "a", want: true},
		{pattern: "?", subject: "", want: false},
		{pattern: "?", subject: "ab", want: false},
		{pattern: "??", subject: "ab", want: true},
		{pattern: "?", subject: "é", want: false},
		{pattern: "??", subject: "é", want: true},
		{pattern: "user:?", subject: "user:12", want: false},
		{pattern: "user:*", subject: "user:1", want: true},
		{pattern: "user:*", subject: "session:1", want: false},

		// Character classes. Negation is spelled '^' and only '^' — Redis has
		// no '[!abc]', which is the spelling filepath.Match accepts.
		{pattern: "[abc]", subject: "a", want: true},
		{pattern: "[abc]", subject: "d", want: false},
		{pattern: "[a-c]", subject: "b", want: true},
		{pattern: "[a-c]", subject: "d", want: false},
		{pattern: "[c-a]", subject: "b", want: true},
		{pattern: "[^abc]", subject: "d", want: true},
		{pattern: "[^abc]", subject: "a", want: false},
		{pattern: "[^a-c]", subject: "z", want: true},
		{pattern: "[0-9]*", subject: "0abc", want: true},
		{pattern: "[0-9]*", subject: "abc", want: false},
		{pattern: "[-a]", subject: "-", want: true},

		// '*' still crosses whatever a preceding class excluded, so an
		// anchored-looking pattern is not anchored.
		{pattern: "[^/]*", subject: "abc", want: true},
		{pattern: "[^/]*", subject: "a/c", want: true},

		// Malformed classes. Redis matches what it read and treats the rest as
		// a length mismatch; it never reports an error, which is what
		// filepath.Match did and what the old fallback was papering over.
		{pattern: "[]", subject: "a", want: false},
		{pattern: "[]", subject: "]", want: false},
		{pattern: "[^]", subject: "a", want: true},
		{pattern: "[abc", subject: "a", want: true},
		{pattern: "[abc", subject: "d", want: false},
		{pattern: "a[b", subject: "a[b", want: false},
		{pattern: "*a[b", subject: "a[b", want: false},
		{pattern: "[a-]", subject: "-", want: false},

		// Escapes, inside a class and out, including the trailing backslash
		// that has nothing to escape and so stands for itself.
		{pattern: `[a\]b]`, subject: "]", want: true},
		{pattern: `[a\]b]`, subject: "a", want: true},
		{pattern: `[\\]`, subject: `\`, want: true},
		{pattern: `\*`, subject: "*", want: true},
		{pattern: `\*`, subject: "a", want: false},
		{pattern: `\?`, subject: "?", want: true},
		{pattern: `\[`, subject: "[", want: true},
		{pattern: `\\`, subject: `\`, want: true},
		{pattern: `a\`, subject: `a\`, want: true},
		{pattern: `\`, subject: `\`, want: true},
		{pattern: `a\*b`, subject: "a*b", want: true},
		{pattern: `a\*b`, subject: "axb", want: false},
	}

	for _, tc := range cases {
		assert.Equalf(t, tc.want, Match(tc.pattern, tc.subject),
			"Match(%q, %q)", tc.pattern, tc.subject)
	}
}

// TestMatchEmptySubjectNeedsTheShortCircuit is the one place the port is not
// self-sufficient. Redis's matcher answers "no" for `*` against the empty key;
// `KEYS *` returns it anyway, because KEYS never asks the matcher about a bare
// star. A caller that forgets the short circuit loses the empty key — which
// would be an odd way to reintroduce ISSUE-0013 a week after closing it.
func TestMatchEmptySubjectNeedsTheShortCircuit(t *testing.T) {
	assert.False(t, Match("*", ""), "the matcher alone does not match an empty subject")
	assert.True(t, MatchesEverything("*"))
	assert.False(t, MatchesEverything(""), "an empty pattern is not the match-all pattern")
	assert.False(t, MatchesEverything("**"), "only a bare star is short circuited, as in Redis")
}

// TestMatchTerminates guards the recursion. Alternating stars against a
// long subject is the shape that makes a naive backtracking matcher take
// exponential time; Redis's skipLongerMatches is what stops it, and this fails
// by timing out rather than by asserting if the port drops it.
func TestMatchTerminates(t *testing.T) {
	pattern := strings.Repeat("a*", 24) + "b"
	key := strings.Repeat("a", 200)

	assert.False(t, Match(pattern, key))
}

func BenchmarkMatch(b *testing.B) {
	benchmarks := map[string]struct{ pattern, subject string }{
		"prefix":  {"user:*", "user:1234:session"},
		"suffix":  {"*.png", "cdn/eu/assets/logo.png"},
		"class":   {"[a-z]*:[0-9]*", "user:1234"},
		"literal": {"user:1234:session", "user:1234:session"},
	}

	for name, bm := range benchmarks {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = Match(bm.pattern, bm.subject)
			}
		})
	}
}
