#!/usr/bin/env bash
#
# Runs every atlasctl command against a live server and checks the two things
# scripts depend on: the exit code, and the keys in the --json object.
#
#   ./examples/atlasctl/tour.sh --addr 127.0.0.1:6379 [--auth-addr host:port]
#
# docs/atlasctl.md documents both as an interface. This is what keeps that
# document honest: every exit code and every JSON field named there is asserted
# below, so a change to either fails here before it reaches anyone's script.
#
# ATLASCACHE_AUTH, when set, is the token used against --auth-addr.

set -uo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
addr="127.0.0.1:6379"
auth_addr=""
ctl=""

while [ $# -gt 0 ]; do
    case "$1" in
        --addr)      addr="$2"; shift 2 ;;
        --auth-addr) auth_addr="$2"; shift 2 ;;
        --binary)    ctl="$2"; shift 2 ;;
        -h|--help)
            sed -n '3,12p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
            exit 0 ;;
        *) echo "tour.sh: unknown argument $1" >&2; exit 2 ;;
    esac
done

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# The token is taken once and then removed from the environment, so that the
# checks against the plaintext server run as an unauthenticated client would —
# a leaked ATLASCACHE_AUTH would otherwise make every one of them fail against
# a server that has no password set.
token="${ATLASCACHE_AUTH:-}"
unset ATLASCACHE_AUTH

if [ -z "$ctl" ]; then
    ctl="$work/atlasctl"
    (cd "$root" && go build -o "$ctl" ./cmd/atlasctl) || exit 1
fi

failures=0
note() { printf '  %s\n' "$*"; }
fail() { printf '  FAIL: %s\n' "$*" >&2; failures=$((failures + 1)); }

# check runs atlasctl, compares the exit code, and checks stdout for each of the
# remaining arguments as a fixed string.
#
#   check <expected exit> <description> -- <atlasctl args...> -- <substrings...>
check() {
    local want="$1" what="$2"; shift 2
    [ "$1" = "--" ] && shift

    local args=()
    while [ $# -gt 0 ] && [ "$1" != "--" ]; do args+=("$1"); shift; done
    [ $# -gt 0 ] && shift

    local out status
    out="$("$ctl" --addr "$addr" "${args[@]}" 2>"$work/stderr")"
    status=$?

    if [ "$status" -ne "$want" ]; then
        fail "$what: exit $status, want $want"
        sed 's/^/    /' "$work/stderr" >&2
        return
    fi
    local wanted
    for wanted in "$@"; do
        case "$out" in
            *"$wanted"*) ;;
            *) fail "$what: output does not contain $wanted"; printf '    %s\n' "$out" >&2; return ;;
        esac
    done
    note "$what"
}

key="example:ctl:$RANDOM"

echo "atlasctl tour against $addr"

# ---- exit code 0, and the JSON envelope ---------------------------------------

check 0 "ping"        -- ping                          -- "PONG"
check 0 "ping --json" -- --json ping                   -- '"ok":true' '"command":"ping"' '"latency_ms":'

check 0 "set"         -- set "$key" hello              -- "OK"
check 0 "set --json"  -- --json set "$key" hello --ttl 60s \
    -- '"ok":true' '"command":"set"' '"key":' '"ttl_seconds":60' '"bytes":5'
check 0 "set without a ttl reports null" -- --json set "$key" hello \
    -- '"ttl_seconds":null'

check 0 "get"         -- get "$key"                    -- "hello"
check 0 "get --json"  -- --json get "$key" \
    -- '"ok":true' '"found":true' '"value":"hello"' '"encoding":"utf8"'

check 0 "exists"        -- exists "$key"               -- "1"
check 0 "exists --json" -- --json exists "$key" "$key:absent" \
    -- '"keys":[' '"count":1'

check 0 "keys --json" -- --json keys "$key*" -- '"pattern":' '"keys":[' '"count":' '"encoding":'
check 0 "scan --json" -- --json scan --match "$key*" --count 10 \
    -- '"match":' '"keys":[' '"count":' '"cursor":"0"' '"complete":true'

check 0 "stats --json" -- --json stats -- '"stats":{' '"keys":'
check 0 "info --json"  -- --json info server -- '"sections":["server"]' '"fields":{' '"text":'

check 0 "version --json" -- --json version -- '"version":' '"commit":' '"built":'
check 0 "help"           -- help -- "Usage:" "Exit codes:"
check 0 "help <command>" -- help scan -- "atlasctl scan"

# --stdin is the only way to store a value an argument list cannot carry.
printf 'line one\nline two\n' | "$ctl" --addr "$addr" set "$key:stdin" --stdin >/dev/null
if [ "$("$ctl" --addr "$addr" get "$key:stdin")" = "$(printf 'line one\nline two')" ]; then
    note "set --stdin round-trips"
else
    fail "set --stdin did not round-trip"
fi

# A redirected value is written byte for byte, with nothing appended: that is
# what makes `atlasctl get k > file` safe for arbitrary bytes.
"$ctl" --addr "$addr" get "$key" > "$work/value"
if [ "$(wc -c < "$work/value" | tr -d ' ')" = "5" ]; then
    note "get adds no trailing newline when redirected"
else
    fail "get appended something to a redirected value"
fi

check 0 "del --json" -- --json del "$key" "$key:stdin" -- '"removed":2'
check 0 "del of an absent key is not a failure" -- --json del "$key" -- '"removed":0'

# ---- exit code 1: the command failed ------------------------------------------

check 1 "get of a missing key exits 1" -- get "$key:absent"
check 1 "get --json of a missing key" -- --json get "$key:absent" \
    -- '"ok":false' '"found":false' '"code":1' '"kind":"not_found"'

# An empty value is not a missing key, and the exit code is the only channel
# that can carry the difference.
"$ctl" --addr "$addr" set "$key:empty" "" >/dev/null
check 0 "get of an empty value exits 0" -- get "$key:empty"
"$ctl" --addr "$addr" del "$key:empty" >/dev/null

# ---- exit code 2: usage -------------------------------------------------------

check 2 "an unknown command"  -- nosuchcommand
check 2 "a missing argument"  -- get
check 2 "an unknown flag"     -- --nosuchflag ping
check 2 "a bad flag value"    -- scan --count -1
check 2 "usage --json"        -- --json get -- '"ok":false' '"code":2' '"kind":"usage"'

# A usage error prints nothing to stdout, so a script parsing output gets
# nothing to parse.
if [ -z "$("$ctl" --addr "$addr" get 2>/dev/null)" ]; then
    note "a usage error is silent on stdout"
else
    fail "a usage error wrote to stdout"
fi

# ---- exit code 3: the server could not be reached ------------------------------

if "$ctl" --addr "127.0.0.1:1" --timeout 2s ping >/dev/null 2>&1; then
    fail "something answered on 127.0.0.1:1"
else
    status=$?
    if [ "$status" -eq 3 ]; then
        note "an unreachable server exits 3"
    else
        fail "an unreachable server exited $status, want 3"
    fi
fi

out="$("$ctl" --addr "127.0.0.1:1" --timeout 2s --json ping 2>/dev/null)"
case "$out" in
    *'"ok":false'*'"code":3'*'"kind":"connection"'*) note "an unreachable server is still a JSON object" ;;
    *) fail "unreachable --json gave: $out" ;;
esac

# ---- authentication ------------------------------------------------------------

if [ -n "$auth_addr" ] && [ -n "$token" ]; then
    if ATLASCACHE_AUTH="$token" "$ctl" --addr "$auth_addr" ping >/dev/null 2>&1; then
        note "ATLASCACHE_AUTH authenticates without putting the token in \`ps\`"
    else
        fail "ATLASCACHE_AUTH did not authenticate against $auth_addr"
    fi

    # `ping` is on the server's pre-auth allowlist, so it answers with or
    # without a token and proves nothing about being authenticated. A data
    # command is what actually asks the question.
    if "$ctl" --addr "$auth_addr" get "$key" >/dev/null 2>&1; then
        fail "a server with auth enabled served an unauthenticated GET"
    else
        note "an unauthenticated data command against an auth-enabled server fails"
    fi
    if "$ctl" --addr "$auth_addr" ping >/dev/null 2>&1; then
        note "ping answers before authentication, so it is not an auth check"
    else
        fail "ping did not answer on the pre-auth allowlist"
    fi

    # --auth works and warns, every time, on stderr. The warning is not
    # suppressible: the person who typed it may not know `ps` shows it to every
    # other user on the host.
    "$ctl" --addr "$auth_addr" --auth "$token" ping >/dev/null 2>"$work/stderr"
    if grep -q "ps" "$work/stderr"; then
        note "--auth warns about the process listing"
    else
        fail "--auth did not warn"
    fi
fi

echo
if [ "$failures" -gt 0 ]; then
    echo "tour.sh: $failures check(s) failed" >&2
    exit 1
fi
echo "tour.sh: every documented exit code and JSON field is as documented"
