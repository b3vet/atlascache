#!/bin/sh
#
# Tests for .githooks/commit-msg. Run it directly:
#
#   ./.githooks/commit-msg.test.sh
#
# Named *.test.sh so git never mistakes it for a hook: hooks are resolved by
# exact filename.

set -u

HOOK=$(dirname "$0")/commit-msg
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

# check <expect-accept|expect-reject> <name> <message...>
check() {
	expect=$1
	name=$2
	shift 2
	printf '%s\n' "$@" >"$TMP/msg"

	out=$("$HOOK" "$TMP/msg" 2>&1)
	code=$?

	case "$expect" in
	accept) want=0 ;;
	reject) want=1 ;;
	esac

	if [ "$code" -eq "$want" ]; then
		pass=$((pass + 1))
		printf 'ok    %-8s %s\n' "$expect" "$name"
	else
		fail=$((fail + 1))
		printf 'FAIL  %-8s %s (exit %d, wanted %d)\n%s\n' "$expect" "$name" "$code" "$want" "$out"
	fi
}

check accept "type and scope"        "feat(ttl): add hierarchical time wheel"
check accept "no scope"              "fix: reject empty keys"
check accept "breaking change"       "feat(resp)!: drop RESP2 fallback"
check accept "every allowed type"    "chore: tidy modules"
check accept "scope with a dash"     "ci(go-test): cache module downloads"
check accept "body and refs trailer" "feat(ttl): add hierarchical time wheel" "" "Four-level wheel." "" "refs FEAT-0011"
check accept "leading comments"      "# please enter a commit message" "docs(readme): describe the E2E tiers"
check accept "subject exactly 72"    "feat(storage): aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
check accept "merge commit"          "Merge branch 'main' into feat/ttl"
check accept "merge remote"          "Merge remote-tracking branch 'origin/main'"
check accept "revert commit"         "Revert \"feat(ttl): add hierarchical time wheel\"" "" "This reverts commit 35f2767."
check accept "fixup commit"          "fixup! feat(ttl): add hierarchical time wheel"
check accept "empty message"         ""

check reject "no type prefix"        "add a time wheel"
check reject "unknown type"          "feature(ttl): add hierarchical time wheel"
check reject "uppercase type"        "Feat(ttl): add hierarchical time wheel"
check reject "missing space"         "feat(ttl):add hierarchical time wheel"
check reject "empty subject"         "feat(ttl): "
check reject "trailing period"       "feat(ttl): add hierarchical time wheel."
check reject "past tense"            "feat(ttl): added hierarchical time wheel"
check reject "third person"          "fix(storage): fixes the buffer aliasing bug"
check reject "subject of 73"         "feat(storage): aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
