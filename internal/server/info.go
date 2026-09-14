package server

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"

	"github.com/b3vet/atlascache/internal/protocol"
)

// INFO, in Redis's format (FEAT-0021)
//
// Every monitoring tool that has ever pointed at a Redis parses INFO as
// `key:value` lines grouped under `# Section` headers, with CRLF endings and a
// blank line between sections. That is not a convention this server may improve
// on: JSON, or YAML, or anything else, means no existing exporter works, which
// defeats the reason for speaking RESP at all (ADR-0006).
//
// Field naming follows one rule. Where Redis has a direct equivalent, the
// field carries Redis's spelling exactly — used_memory, keyspace_hits,
// keyspace_misses, expired_keys, evicted_keys, connected_clients,
// total_connections_received, total_commands_processed — so a dashboard built
// for Redis reads them without being told. Everything else carries an
// atlascache_ prefix, so that a field Redis adds later cannot collide with one
// invented here and quietly change meaning.
//
// Deliberately absent: redis_version. Claiming one would make every client's
// version check pass against a server that implements a fraction of the command
// set, and a fraction is not a version. atlascache_version says what this is.

// The section names INFO understands, in the order it renders them.
const (
	sectionServer   = "server"
	sectionClients  = "clients"
	sectionMemory   = "memory"
	sectionStats    = "stats"
	sectionKeyspace = "keyspace"
)

// infoSections is the render order. Redis's own order is Server first and
// Keyspace last, and tools that display INFO verbatim look wrong if it differs.
var infoSections = []string{sectionServer, sectionClients, sectionMemory, sectionStats, sectionKeyspace}

// infoSectionAliases are the arguments that mean "all of them". Redis treats
// default, all and everything as progressively larger sets; with no optional
// sections here they are the same set, and answering all three keeps
// `INFO everything` from returning nothing to a tool that asks for it.
var infoSectionAliases = map[string]bool{"default": true, "all": true, "everything": true}

// crlf is INFO's line ending. Not "\n": Redis writes CRLF, and a parser that
// splits on it strictly would read the whole payload as one line otherwise.
const crlf = "\r\n"

// info renders the server's state as Redis-format text.
//
// The reply is a protocol.Verbatim, which RESP2 renders as a plain bulk string —
// what Redis sends a RESP2 client — and RESP3 will render as a verbatim string
// with its "txt" hint, which is what Redis sends a RESP3 one. The handler does
// not choose between them (ADR-0028).
func (s *session) info(cmd protocol.Command) protocol.Reply {
	wanted := infoSections
	if len(cmd.Args) > 0 {
		wanted = requestedSections(cmd.Args)
	}

	// Redis answers an unrecognized section with an empty string rather than an
	// error, and tools probe for sections they are not sure exist.
	if len(wanted) == 0 {
		return protocol.Verbatim{Format: protocol.VerbatimText, Text: ""}
	}

	stats := s.store().Stats()

	blocks := make([]string, 0, len(wanted))
	for _, name := range wanted {
		blocks = append(blocks, s.infoSection(name, stats))
	}

	return protocol.Verbatim{Format: protocol.VerbatimText, Text: strings.Join(blocks, crlf)}
}

// requestedSections resolves the section arguments to the subset this server
// renders, in render order and without repeats — `INFO memory memory` is one
// Memory block, as it is in Redis.
func requestedSections(args [][]byte) []string {
	known := make(map[string]bool, len(infoSections))
	for _, name := range infoSections {
		known[name] = true
	}

	selected := make(map[string]bool, len(args))
	for _, arg := range args {
		name := strings.ToLower(string(arg))
		switch {
		case infoSectionAliases[name]:
			return infoSections
		case known[name]:
			selected[name] = true
		}
	}

	wanted := make([]string, 0, len(selected))
	for _, name := range infoSections {
		if selected[name] {
			wanted = append(wanted, name)
		}
	}
	return wanted
}

// infoSection renders one section, header included and trailing CRLF included,
// so the caller joins blocks with a single blank line between them.
func (s *session) infoSection(name string, stats Stats) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# %s%s", strings.ToUpper(name[:1])+name[1:], crlf)
	for _, line := range s.infoFields(name, stats) {
		b.WriteString(line)
		b.WriteString(crlf)
	}
	return b.String()
}

// infoFields is the content of one section as `key:value` strings.
func (s *session) infoFields(name string, stats Stats) []string {
	switch name {
	case sectionServer:
		return s.infoServer()
	case sectionClients:
		return s.infoClients()
	case sectionMemory:
		return infoMemory(stats)
	case sectionStats:
		return s.infoStats(stats)
	case sectionKeyspace:
		return infoKeyspace(stats)
	default:
		return nil
	}
}

func (s *session) infoServer() []string {
	return []string{
		field("atlascache_version", Version),
		field("atlascache_mode", serverMode),
		field("atlascache_role", serverRole),
		field("go_version", runtime.Version()),
		field("os", runtime.GOOS+" "+runtime.GOARCH),
		field("arch_bits", 64),
		field("process_id", os.Getpid()),
		field("tcp_port", listenPort(s.srv.addr)),
		field("uptime_in_seconds", int64(uptime().Seconds())),
		field("uptime_in_days", int64(uptime().Hours()/24)),
	}
}

// infoClients reports the connection accounting and the bounds it is measured
// against.
//
// The limits are here rather than only in the log banner because a limit an
// operator cannot read back is one they have to guess at: "connected_clients is
// 9998" means nothing without maxclients beside it, and a client disconnected by
// the idle timeout is only diagnosable if the timeout is visible. Redis does the
// same with maxclients, which is why that one field carries Redis's spelling.
func (s *session) infoClients() []string {
	return []string{
		field("connected_clients", s.srv.conns.connected.Load()),
		field("blocked_clients", 0),
		field("maxclients", s.srv.limits.MaxConnections),
		field("atlascache_client_idle_timeout_ms", s.srv.limits.IdleTimeout.Milliseconds()),
		field("atlascache_max_request_size", s.srv.limits.MaxRequestBytes),
		field("atlascache_max_request_elements", s.srv.codecLimits.MaxMultiBulkLength),
		field("atlascache_max_pipeline_commands", s.srv.limits.MaxPipelineCommands),
		field("atlascache_max_output_buffer", s.srv.limits.MaxOutputBytes),
	}
}

// infoMemory reports the cache's accounting first and the process's second, and
// keeps them apart.
//
// used_memory is the cache's own figure — the bytes the entries occupy, tracked
// incrementally — because that is the number max_memory is enforced against and
// the number an operator is deciding capacity on. The Go heap is a different
// quantity and carries the atlascache_ prefix so nobody plots one thinking it
// is the other.
//
// Every process figure comes from a timer-driven sample, never from a read
// taken here: runtime.ReadMemStats stops the world, and INFO is polled by the
// second (ISSUE-0015).
func infoMemory(stats Stats) []string {
	fields := make([]string, 0, 15)
	fields = append(fields,
		field("used_memory", stats.MemoryUsed),
		field("used_memory_human", humanBytes(stats.MemoryUsed)),
		field("maxmemory", stats.MemoryMax),
		field("maxmemory_human", humanBytes(stats.MemoryMax)),
		field("atlascache_scan_cursors", stats.ScanCursors),
		field("atlascache_scan_snapshot_bytes", stats.ScanSnapshotBytes),
	)

	if stats.Process.SampledAt.IsZero() {
		return fields
	}

	return append(fields,
		field("atlascache_heap_alloc", stats.Process.HeapAlloc),
		field("atlascache_heap_sys", stats.Process.HeapSys),
		field("atlascache_heap_inuse", stats.Process.HeapInuse),
		field("atlascache_heap_objects", stats.Process.HeapObjects),
		field("atlascache_stack_inuse", stats.Process.StackInuse),
		field("atlascache_process_memory", stats.Process.Sys),
		field("atlascache_num_gc", stats.Process.NumGC),
		field("atlascache_goroutines", stats.Process.Goroutines),
		field("atlascache_memory_sampled_at", stats.Process.SampledAt.UTC().Unix()),
	)
}

func (s *session) infoStats(stats Stats) []string {
	return []string{
		field("total_connections_received", s.srv.conns.connectionsReceived.Load()),
		field("total_commands_processed", s.srv.conns.commandsProcessed.Load()),
		// Redis's spelling again: connections turned away at max_connections.
		field("rejected_connections", s.srv.conns.rejected.Load()),
		// The rest have no Redis equivalent, so they carry the prefix. Each
		// names one reason a connection ended that is not the client hanging
		// up, because the four are four different things to go and fix
		// (FEAT-0024).
		field("atlascache_idle_closed", s.srv.conns.idleClosed.Load()),
		field("atlascache_output_limit_closed", s.srv.conns.outputClosed.Load()),
		field("atlascache_stalled_closed", s.srv.conns.stalledClosed.Load()),
		field("atlascache_request_limit_closed", s.srv.conns.requestClosed.Load()),
		field("atlascache_handler_panics", s.srv.conns.panics.Load()),
		field("keyspace_hits", stats.Hits),
		field("keyspace_misses", stats.Misses),
		field("expired_keys", stats.Expirations),
		field("evicted_keys", stats.Evictions),
		field("atlascache_gets", stats.Gets),
		field("atlascache_sets", stats.Sets),
		field("atlascache_deletes", stats.Deletes),
		field("atlascache_oom_rejected", stats.OOMRejected),
	}
}

// infoKeyspace renders the db0 line, and omits it entirely when the keyspace is
// empty — which is Redis's behavior, and which several exporters depend on to
// tell an empty database from a missing one.
func infoKeyspace(stats Stats) []string {
	if stats.Keys == 0 {
		return nil
	}

	// avg_ttl is reported as 0 rather than computed. Redis reports 0 too unless
	// its own sampling has run, and a parser that reads the line needs the key
	// present more than it needs the number to be interesting.
	return []string{
		fmt.Sprintf("db0:keys=%d,expires=%d,avg_ttl=0", stats.Keys, stats.KeysWithTTL),
	}
}

// field renders one `key:value` line.
func field(name string, value any) string {
	return fmt.Sprintf("%s:%v", name, value)
}

// humanBytes renders a byte count the way Redis's bytesToHuman does, because
// used_memory_human is displayed verbatim by redis-cli and by every dashboard
// that shows it, and "1.00M" beside "1048576" is how operators read the pair.
func humanBytes(n uint64) string {
	const unit = 1024

	if n < unit {
		return fmt.Sprintf("%dB", n)
	}

	value := float64(n)
	for _, suffix := range []string{"K", "M", "G", "T", "P"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.2f%s", value, suffix)
		}
	}
	return fmt.Sprintf("%.2fE", value/unit)
}

// ParseInfo reads INFO output back into a flat field map, the way a monitoring
// client does: section headers and blank lines are skipped, and each remaining
// line is split at the first colon.
//
// It is exported because the assertion worth making about INFO is that a
// standard parser can read it, and a parser written to the same rules as the
// renderer proves nothing. This one is written from the format's rules; the E2E
// suite and the differential run check the output against real parsers.
func ParseInfo(text string) map[string]string {
	fields := map[string]string{}

	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		fields[name] = value
	}

	return fields
}

// InfoSections lists the sections INFO renders, for callers that want to probe
// them one at a time.
func InfoSections() []string {
	names := append([]string(nil), infoSections...)
	sort.Strings(names)
	return names
}
