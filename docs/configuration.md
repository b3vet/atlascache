# Configuration

AtlasCache reads a YAML file given with `--config`. Every option has a default,
so the shortest working configuration is an empty file — but see
[Production hardening](#production-hardening) before running one anywhere real.

`config.example.yaml` in the repository root is a complete, commented copy of
everything below.

## How configuration is loaded

Three things are worth knowing before the option tables.

**The startup line reports every effective setting.** The server logs one line
at boot naming the port, shard count, memory limit, eviction policy, and whether
TLS and auth are on. It is the fastest way to confirm the server is running the
configuration you think it is, and it is more trustworthy than reading the file
back, because it reports what was *applied*.

**Unknown keys are refused.** A key that does not match the schema stops
startup, naming the key, its line, and the key you probably meant:

```
$ atlascache --config atlascache.yaml
atlascache: atlascache.yaml:2: unknown field "port" in server (did you mean "client_port"?)
```

All of them are reported at once, so fixing a config does not take one restart
per mistake:

```
atlascache: atlascache.yaml: 3 unknown fields:
  - line 2: unknown field "max_conections" in server (did you mean "max_connections"?)
  - line 4: unknown field "enbaled" in auth (did you mean "enabled"?)
  - line 5: unknown field "lgging" at the top level (did you mean "logging"?)
```

This matters more than it sounds. Without it, `enbaled: true` under `auth`
gives you a server with authentication **off**, reachable by anyone who can open
a socket, reporting itself perfectly healthy. A configuration the server cannot
fully honour is one you have misunderstood, and every option here is security-
or availability-relevant.

**Most of the file reloads on `SIGHUP`**, without dropping connections. A reload
carrying an unknown key is rejected and logged, and the running configuration
keeps serving — a typo cannot take down a live server. Options that cannot be
changed on a running server are marked *restart* below.

**The file is the only way to change a setting.** There is no `CONFIG SET`: a
second mutation path would let the file and the running server disagree, and the
next reload would silently undo whatever was set. `CONFIG SET` says so rather
than answering "unknown subcommand". What is running can be *read* back, from
`CONFIG GET` (a handful of parameters under Redis's names — see
[protocol.md](protocol.md)), from `INFO`, and in full from the admin API's
`/config`, which redacts the secrets.

## `node`

| Option | Default | Meaning |
|--------|---------|---------|
| `node.id` | generated | Unique node identifier. Generated per boot if empty. *restart* |
| `node.name` | `""` | Human-readable name, for logs and `INFO`. |
| `node.data_dir` | `/var/lib/atlascache` | Where persistence will write. Unused in v0.1.0, which keeps nothing on disk. |

## `server`

| Option | Default | Meaning |
|--------|---------|---------|
| `server.bind_addr` | `0.0.0.0` | Interface for the client listener. *restart* |
| `server.client_port` | `6379` | RESP client port. *restart* |
| `server.max_connections` | `10000` | Clients served at once. |
| `server.client_idle_timeout` | `30s` | Close a connection idle this long. `0` disables. |
| `server.max_request_size` | `0` (derived) | Wire bytes one request may consume. |
| `server.max_pipeline_commands` | `1024` | Pipelined commands executed before replies are flushed. |
| `server.max_output_buffer` | `64MB` | Reply bytes buffered for one connection before it is closed. |

Four of these bound what a **single client** can cost the server, and with auth
disabled every one of them is reachable by anyone who can open a socket. They
are not tuning knobs so much as blast-radius limits.

- **`max_connections`** — the client past the limit is accepted, told `-ERR max
  number of clients reached`, and closed, rather than refused at accept, which
  reaches the client as an opaque connection-refused. Each connection costs a
  goroutine plus read and write buffers, roughly 40KB, so the default ceiling is
  about 400MB of connection overhead.
- **`client_idle_timeout`** is refreshed per *command*, not per byte, so a
  client dribbling bytes without ever completing a request is still reaped.
- **`max_request_size`** exists because the per-field protocol limits each bound
  one field and nothing bounds their product: the largest request those limits
  accept is a million one-byte elements, 7MB on the wire and 154MB decoded. The
  default `0` derives this from `storage.max_value_size` plus 1MB of framing, so
  the largest value the engine accepts always fits.
- **`max_output_buffer`** is for `KEYS *` on a large keyspace from a client that
  stops reading. `0` leaves output uncapped, which is not recommended.

## `admin`

| Option | Default | Meaning |
|--------|---------|---------|
| `admin.bind_addr` | `127.0.0.1` | Interface for the admin HTTP API. *restart* |
| `admin.port` | `8080` | Admin HTTP port. Must differ from `server.client_port`. *restart* |
| `admin.token` | `""` | Credential for the protected endpoints. |

The API has two halves. `/health`, `/health/live` and `/health/ready` need no
credential, because a Kubernetes probe cannot present one; they report liveness
and readiness and nothing else. `/stats`, `/stats/memory` and `/config` require
`admin.token`.

**`admin.token` is deliberately not `auth.token`.** A client holding a data
token must not thereby gain administrative access, and the two must be rotatable
separately. Setting both to the same value is refused.

An empty `admin.token` is allowed only while `admin.bind_addr` is loopback.
Binding the admin API to a routable interface without a token is refused at
startup rather than warned about — it is the one misconfiguration that hands a
stranger your statistics and effective configuration.

## `storage`

| Option | Default | Meaning |
|--------|---------|---------|
| `storage.shard_count` | `0` (derived) | Independent shards. `0` derives it as CPU count × 4, clamped to 16–1024. *restart* |
| `storage.max_memory` | `0` | Memory ceiling for stored data. `0` means unlimited. |
| `storage.max_value_size` | `1MB` | Largest single value accepted. |

**What `max_memory` actually bounds** is the keys and values the engine holds,
plus their accounting overhead. It does **not** bound connection buffers, Go
runtime overhead, or the process as a whole — so a process limit set equal to
`max_memory` will be exceeded. Leave headroom: `max_connections` × 40KB for
connections, plus room for the runtime.

`0` means unlimited, and unlimited plus an eviction policy means the policy
never fires, because nothing ever declares pressure. If you want eviction, set
`max_memory`.

## `ttl`

| Option | Default | Meaning |
|--------|---------|---------|
| `ttl.lazy_expiration` | `true` | Expired keys are dropped when touched. |
| `ttl.active_expiration` | `true` | A background wheel reclaims expired keys. |
| `ttl.check_interval` | `100ms` | How often the wheel runs. |
| `ttl.batch_size` | `10000` | Keys examined per sweep. |

Turning **both** off means expired keys are never reclaimed: they stop being
readable — a client never sees an expired value — but they keep occupying
memory until overwritten or evicted. That is a legitimate configuration for a
cache under a memory limit where eviction does the reclaiming, and a memory leak
in any other setup.

## `eviction`

| Option | Default | Meaning |
|--------|---------|---------|
| `eviction.policy` | `lru` | `lru`, `lfu`, `fifo`, or `none`. |
| `eviction.sample_size` | `5` | Keys sampled when choosing a victim. |

Eviction is **approximate by sampling**, as Redis's is: rather than maintaining
a global ordering, the engine samples `sample_size` keys and evicts the best
candidate among them. A larger sample is a better choice and more work per
eviction. 5 is the value Redis uses and a good default.

`none` means a write that would exceed `max_memory` is refused with an error
rather than making room. That is the right policy when losing data silently is
worse than failing loudly.

## `tls`

| Option | Default | Meaning |
|--------|---------|---------|
| `tls.enabled` | `false` | Whether the client port speaks TLS. |
| `tls.cert_file` | `""` | PEM certificate chain, leaf first. |
| `tls.key_file` | `""` | PEM private key for `cert_file`. |

**The protocol floor is TLS 1.3 and is deliberately not configurable**, because
an adjustable floor is one somebody lowers.

Certificate and key are re-read when the files change, so a renewal needs no
restart. A replacement that does not parse is rejected and the running
certificate keeps serving — a bad cert cannot take the server down.

`make dev-certs` writes a self-signed localhost pair into `certs/` for local use.

## `auth`

| Option | Default | Meaning |
|--------|---------|---------|
| `auth.enabled` | `false` | Whether clients must authenticate. |
| `auth.token` | `""` | The shared secret. |

One shared token, compared in constant time. Clients send either `AUTH <token>`
or `AUTH default <token>` — `default` is the only username there is.

The token is re-read when the config file changes, and a rotation leaves
connections that have **already** authenticated alone, so rotating does not
disconnect live clients.

**Keep the config file readable only by the server user.** A token in a
world-readable file is not a token.

## `logging`

| Option | Default | Meaning |
|--------|---------|---------|
| `logging.level` | `info` | `debug`, `info`, `warn`, `error`. |
| `logging.format` | `json` | `json` or `console`. |

## Production hardening

TLS and authentication ship **disabled**, so that the first run needs no setup,
and the server warns about both at startup. This is the configuration those
warnings are pointing at:

```yaml
server:
  # Bind to the interface the application tier reaches, not to everything.
  bind_addr: "10.0.0.5"
  client_port: 6379
  max_connections: 5000
  client_idle_timeout: "30s"
  max_output_buffer: "64MB"

admin:
  # Loopback, and a token as well. Either alone is weaker than it looks: a
  # token on a public interface is one credential away, and loopback alone
  # trusts every process on the host.
  bind_addr: "127.0.0.1"
  port: 8080
  # Set via ATLAS_ADMIN_TOKEN rather than written here.
  token: ""

storage:
  # Set it. Without a ceiling the eviction policy below never fires, because
  # nothing ever declares pressure.
  max_memory: "4GB"
  max_value_size: "1MB"

eviction:
  policy: "lru"
  sample_size: 5

tls:
  enabled: true
  cert_file: "/etc/atlascache/tls/server.crt"
  key_file: "/etc/atlascache/tls/server.key"

auth:
  enabled: true
  # Set via ATLAS_AUTH_TOKEN rather than written here.
  token: ""

logging:
  level: "info"
  format: "json"
```

Alongside the file:

- **File permissions.** `chmod 600` the config and own it by the server user. It
  holds two secrets.
- **Memory headroom.** `max_memory` bounds stored data, not the process. Give
  the container or cgroup limit room for connection buffers and the Go runtime
  on top.
- **Do not expose the admin port.** It reports statistics and the effective
  configuration. The token redacts secrets in `/config`, but everything else it
  tells a reader is a head start.
- **v0.1.0 has no persistence.** A restart is an empty cache, and there is no
  encryption at rest because there is no rest. Size your backing store for the
  thundering herd that follows a restart.

## Environment overrides

Any option can be set by an environment variable named `ATLAS_`, then the
option's path upper-cased with dots written as underscores:

```bash
ATLAS_SERVER_CLIENT_PORT=6380
ATLAS_AUTH_TOKEN=...
ATLAS_EVICTION_POLICY=lfu
ATLAS_LOGGING_LEVEL=debug
```

Prefer this for the two tokens. It keeps credentials out of the file you check
into version control, and out of a file whose permissions someone may get wrong.

Overrides are applied after the file is read, so they win. The startup line
reports the result, which is the value to trust.

**An unrecognised `ATLAS_` variable warns; it does not stop the server** — the
opposite of an unknown key in the config file, and deliberately so. A config
file is an artifact you wrote, so every key in it is a claim about intent, and
one the server cannot honour means something has been misunderstood. The
environment is not yours alone: Kubernetes and Docker inject a block of
variables for every linked service, so a Service named `atlas` puts `ATLAS_PORT`,
`ATLAS_SERVICE_HOST` and half a dozen others into the container before the
process starts. Refusing to boot on those would turn this check into an outage
in exactly the deployment it exists to protect.

The warning is aimed accordingly: a variable is reported only when its first
segment names a configuration section or is a near miss for one, which is what
separates `ATLAS_SERVER_PORT` — your mistake, and it suggests
`ATLAS_SERVER_CLIENT_PORT` — from `ATLAS_SERVICE_HOST`, which is the
orchestrator's bookkeeping and none of your business.
