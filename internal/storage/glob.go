package storage

// Glob matching, as Redis does it (FEAT-0020)
//
// KEYS and SCAN MATCH are judged against Redis's own matcher, so this is a
// direct port of `stringmatchlen` from util.c rather than an approximation
// built on a Go standard library equivalent.
//
// The approximation that was here before used filepath.Match, which differs in
// ways that matter for a cache:
//
//   - filepath.Match refuses to let '*' cross a path separator. Cache keys are
//     full of separators — "user:1/session", "cdn/eu/asset" — so `KEYS *` style
//     patterns silently missed most of the keyspace on the platforms where '/'
//     is the separator.
//   - filepath.Match rejects a malformed pattern with ErrBadPattern, where
//     Redis matches what it can and treats the rest literally. An unterminated
//     '[' is the common case, and erroring on it meant falling into an ad-hoc
//     prefix/suffix fallback that agreed with Redis on nothing.
//   - filepath.Match reads '[' classes over runes and supports the `[!abc]`
//     spelling of negation; Redis works in bytes and spells negation `[^abc]`
//     only.
//
// The supported syntax is exactly Redis's: '*', '?', '[abc]', '[a-c]',
// '[^abc]', and '\' escaping anywhere, including inside a class.
//
// Matching is byte-oriented, which is the point rather than a limitation: keys
// are arbitrary byte strings, and '?' means one byte here exactly as it does in
// Redis.

// MatchGlob reports whether key matches pattern under Redis's glob semantics.
//
// Note the deliberate asymmetry callers rely on: MatchGlob("*", "") is false,
// because Redis's matcher requires the pattern and the subject to run out
// together. Redis's KEYS and SCAN never ask it that question — they short
// circuit a bare "*" to "every key" before calling the matcher, which is why an
// empty key is returned by `KEYS *` and not by `KEYS *x`. MatchAllPattern is
// that short circuit, and callers must apply it to agree with Redis.
func MatchGlob(pattern, key string) bool {
	skipLongerMatches := false
	return globMatch(pattern, key, &skipLongerMatches)
}

// MatchAllPattern is the pattern Redis answers without consulting the matcher
// at all. Every key matches it, the empty key included.
const MatchAllPattern = "*"

// MatchesEveryKey reports whether the pattern is the one that selects the whole
// keyspace. It exists so that KEYS and SCAN MATCH share one answer to the
// question rather than each spelling the comparison out.
func MatchesEveryKey(pattern string) bool { return pattern == MatchAllPattern }

// globMatch is the port of stringmatchlen_impl. skipLongerMatches is shared
// with every recursive call, exactly as Redis shares it through a pointer: once
// a '*' has proved that no suffix of the subject matches the rest of the
// pattern, no longer match of an earlier '*' can help, and the search is cut
// short rather than retried for every remaining position.
func globMatch(pattern, s string, skipLongerMatches *bool) bool {
	pi, si := 0, 0

	for pi < len(pattern) && si < len(s) {
		switch pattern[pi] {
		case '*':
			return matchStar(pattern[pi:], s[si:], skipLongerMatches)

		case '?':
			si++

		case '[':
			next, matched := matchClass(pattern, pi, s[si])
			pi = next
			if !matched {
				return false
			}
			si++

		case '\\':
			// An escape stands for the byte after it. A trailing backslash has
			// nothing to escape and stands for itself.
			if len(pattern)-pi >= 2 {
				pi++
			}
			fallthrough

		default:
			if pattern[pi] != s[si] {
				return false
			}
			si++
		}

		pi++

		// The subject is spent. Trailing stars still match nothing, so they are
		// consumed before the lengths are compared.
		if si == len(s) {
			for pi < len(pattern) && pattern[pi] == '*' {
				pi++
			}
			break
		}
	}

	return pi == len(pattern) && si == len(s)
}

// matchStar matches a pattern whose first byte is '*' against s, which has at
// least one byte left.
//
// The wildcard is the only construct that backtracks, and skipLongerMatches is
// what keeps the backtracking linear in practice. Once some suffix of s has
// failed against the rest of the pattern, no earlier star growing longer can
// help — the rest of the pattern would only start matching later still — so the
// flag propagates that "no" back out through every enclosing star instead of
// each of them retrying every position.
func matchStar(pattern, s string, skipLongerMatches *bool) bool {
	// A run of stars is one star. Treating "**" as two independent wildcards is
	// quadratic and means nothing different.
	for len(pattern) >= 2 && pattern[1] == '*' {
		pattern = pattern[1:]
	}

	// A trailing star matches whatever is left, including nothing.
	if len(pattern) == 1 {
		return true
	}

	for i := 0; i < len(s); i++ {
		if globMatch(pattern[1:], s[i:], skipLongerMatches) {
			return true
		}
		if *skipLongerMatches {
			return false
		}
	}

	*skipLongerMatches = true
	return false
}

// matchClass reads the bracket expression starting at the '[' in pattern[pi]
// and reports whether c is in it, together with the index of the class's last
// byte — the caller advances past it.
//
// An unterminated class is not an error. Redis stops at the end of the pattern
// and steps back one byte, which leaves the class matching what it had read so
// far and the overall match hinging on the lengths agreeing. Reproducing that
// is the difference between `KEYS [abc` answering as Redis does and answering
// with an error Redis never sends.
func matchClass(pattern string, pi int, c byte) (next int, matched bool) {
	pi++ // past '['

	negate := pi < len(pattern) && pattern[pi] == '^'
	if negate {
		pi++
	}

	for {
		switch {
		case pi >= len(pattern):
			// Unterminated. Step back so the caller's advance lands exactly at
			// the end of the pattern rather than past it. Whatever the class
			// read before running out still counts.
			return pi - 1, matched != negate

		case pattern[pi] == '\\' && len(pattern)-pi >= 2:
			pi++
			if pattern[pi] == c {
				matched = true
			}

		case pattern[pi] == ']':
			return pi, matched != negate

		case len(pattern)-pi >= 3 && pattern[pi+1] == '-':
			// A range. Redis swaps the ends rather than rejecting an inverted
			// one, so "[c-a]" and "[a-c]" mean the same thing.
			low, high := pattern[pi], pattern[pi+2]
			if low > high {
				low, high = high, low
			}
			pi += 2
			if c >= low && c <= high {
				matched = true
			}

		default:
			if pattern[pi] == c {
				matched = true
			}
		}

		pi++
	}
}
