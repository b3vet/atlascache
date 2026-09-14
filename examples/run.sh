#!/usr/bin/env bash
#
# Builds and runs every program under examples/ against a real atlascache
# server, and fails if any of them does.
#
# This is what makes the examples documentation rather than decoration. An
# example that has never been executed is a guess, and a guess in a README
# outlives every refactor that invalidated it.
#
#   ./examples/run.sh              # build a server from this checkout and use it
#   ./examples/run.sh --binary bin/atlascache
#   ./examples/run.sh --keep       # leave the temporary directory behind
#
# Three servers are started, because the examples need three shapes of one:
# plaintext with no auth, plaintext with auth (for the ErrAuth case), and TLS
# with auth. All of them listen on loopback, on ports chosen at run time, and
# all of them are stopped on the way out.

set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
binary=""
keep=0

while [ $# -gt 0 ]; do
    case "$1" in
        --binary) binary="$2"; shift 2 ;;
        --keep)   keep=1; shift ;;
        -h|--help)
            sed -n '3,20p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
            exit 0 ;;
        *)
            echo "run.sh: unknown argument $1" >&2
            exit 2 ;;
    esac
done

work="$(mktemp -d)"
pids=()

cleanup() {
    for pid in "${pids[@]:-}"; do
        [ -n "$pid" ] && kill "$pid" 2>/dev/null || true
    done
    for pid in "${pids[@]:-}"; do
        [ -n "$pid" ] && wait "$pid" 2>/dev/null || true
    done
    if [ "$keep" -eq 1 ]; then
        echo "run.sh: left $work in place"
    else
        rm -rf "$work"
    fi
}
trap cleanup EXIT

# free_port finds a loopback port nothing is listening on. Bash's /dev/tcp is
# enough to ask, and it keeps this script free of a helper binary that would
# itself need building.
free_port() {
    local port
    for _ in $(seq 1 100); do
        port=$(( 20000 + RANDOM % 20000 ))
        if ! port_open "$port"; then
            echo "$port"
            return 0
        fi
    done
    echo "run.sh: could not find a free port" >&2
    return 1
}

# port_open reports whether something is listening. The connection is made in a
# subshell so the descriptor it opens dies with it — a bare `exec 3<&-` in this
# shell would take the script's own stderr with it, which is the sort of bug
# that only shows up when something finally has an error to report.
port_open() {
    (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null
}

# start_server writes a config, launches a server on it, and waits until the
# port answers. It takes the client port, the admin port, and any extra YAML.
start_server() {
    local name="$1" port="$2" admin="$3" extra="$4"
    local dir="$work/$name"
    mkdir -p "$dir/data"

    cat > "$dir/config.yaml" <<YAML
node:
  data_dir: "$dir/data"
server:
  bind_addr: "127.0.0.1"
  client_port: $port
admin:
  enabled: true
  bind_addr: "127.0.0.1"
  port: $admin
logging:
  level: "error"
$extra
YAML

    "$binary" --config "$dir/config.yaml" > "$dir/server.log" 2>&1 &
    pids+=("$!")

    for _ in $(seq 1 100); do
        if port_open "$port"; then
            return 0
        fi
        sleep 0.1
    done

    echo "run.sh: the $name server never came up on port $port" >&2
    cat "$dir/server.log" >&2
    return 1
}

step() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }

cd "$root"

if [ -z "$binary" ]; then
    step "building the server"
    binary="$work/atlascache"
    go build -o "$binary" ./cmd/atlascache
else
    case "$binary" in
        /*) ;;
        *) binary="$root/$binary" ;;
    esac
fi

step "building every example"
# Building and vetting them all first is itself a check: an example that does
# not compile is a documentation bug, and this is where it is caught. They are
# named by file because they carry the `ignore` build tag — see the comment at
# the top of any of them — so a `./examples/...` pattern would match nothing and
# silently check nothing.
#
# The directory listing is what finds them, rather than a list kept here, so a
# new example cannot be added without being built.
built=""
for dir in "$root"/examples/*/; do
    name="$(basename "$dir")"
    [ -f "$dir/main.go" ] || continue
    go vet "$dir/main.go"
    go build -o "$work/bin/$name" "$dir/main.go"
    built="$built $name"
    echo "  $name"
done

step "generating a self-signed certificate for the TLS example"
# Generated per run rather than committed. A committed test certificate gets
# copied into production with depressing regularity, and it expires.
openssl req -x509 -newkey rsa:2048 -sha256 -days 1 -nodes \
    -keyout "$work/server.key" -out "$work/server.crt" \
    -subj "/CN=localhost/O=AtlasCache Examples" \
    -addext "subjectAltName=DNS:localhost,IP:127.0.0.1" >/dev/null 2>&1

token="examples-$RANDOM-$RANDOM"
plain_port="$(free_port)";  plain_admin="$(free_port)"
auth_port="$(free_port)";   auth_admin="$(free_port)"
tls_port="$(free_port)";    tls_admin="$(free_port)"

step "starting servers"
start_server plain "$plain_port" "$plain_admin" ""
start_server auth  "$auth_port"  "$auth_admin"  "auth:
  enabled: true
  token: \"$token\""
start_server tls   "$tls_port"   "$tls_admin"   "auth:
  enabled: true
  token: \"$token\"
tls:
  enabled: true
  cert_file: \"$work/server.crt\"
  key_file: \"$work/server.key\""
echo "plaintext 127.0.0.1:$plain_port, with auth 127.0.0.1:$auth_port, TLS localhost:$tls_port"

# A plain string rather than an array: `${#arr[@]}` on an empty array is an
# unbound variable under `set -u` in the bash macOS still ships.
failed=""
ran=""
run_example() {
    local name="$1"; shift
    step "examples/$name"
    ran="$ran $name"
    if "$work/bin/$name" "$@"; then
        return 0
    fi
    failed="$failed $name"
    return 0
}

run_example quickstart -addr "127.0.0.1:$plain_port"
run_example ttl        -addr "127.0.0.1:$plain_port"
run_example errors     -addr "127.0.0.1:$plain_port" -auth-addr "127.0.0.1:$auth_port"
run_example retries    -addr "127.0.0.1:$plain_port"
run_example pool       -addr "127.0.0.1:$plain_port"
run_example scan       -addr "127.0.0.1:$plain_port"
run_example do         -addr "127.0.0.1:$plain_port"
ATLASCACHE_AUTH="$token" run_example tls -addr "localhost:$tls_port" -ca "$work/server.crt"

step "examples/atlasctl (the CLI tour)"
if ! ATLASCACHE_AUTH="$token" "$root/examples/atlasctl/tour.sh" \
        --addr "127.0.0.1:$plain_port" --auth-addr "127.0.0.1:$auth_port"; then
    failed="$failed atlasctl"
fi

# An example that was built and never run is one somebody added without adding
# it here, which would make it documentation nothing checks — the exact failure
# this script exists to prevent.
step "checking that every example was run"
for name in $built; do
    case " $ran " in
        *" $name "*) ;;
        *) echo "  examples/$name was built but never run: add it to run.sh" >&2
           failed="$failed $name" ;;
    esac
done
[ -n "$failed" ] || echo "  all of:$built"

echo
if [ -n "$failed" ]; then
    echo "run.sh: FAILED:$failed" >&2
    exit 1
fi
echo "run.sh: every example ran green against a live server"
