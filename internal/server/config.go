package server

import (
	"strconv"
	"strings"

	"github.com/b3vet/atlascache/internal/glob"
	"github.com/b3vet/atlascache/internal/protocol"
)

// CONFIG, read-only (ISSUE-0021)
//
// `redis-benchmark` asks for `save` and `appendonly` before it runs a single
// command, and prints "WARNING: Could not fetch server CONFIG" when the probe
// fails. That warning is the first line an evaluator sees, and it reads as
// "this server is incomplete" — an expensive impression for a command that is
// this small. Several monitoring agents probe `maxmemory` for the same reason.
//
// Two decisions shape what is below.
//
// **The table holds what AtlasCache actually has.** `save` and `appendonly`
// are answered with an empty value rather than a fabricated one: persistence is
// a later phase, and a server that reports `appendonly:no` is making a claim
// about durability it has no implementation behind. They are present rather
// than absent because absence is what produces the warning — redis-benchmark
// reads a reply of fewer than two elements as a failed probe — and an empty
// value is the honest way to be present.
//
// **There is no CONFIG SET.** Configuration is hot-reloaded from the file
// (ADR-0009's pattern), and a second mutation path would let the two disagree:
// the file would say one thing, the running server another, and the next
// reload would silently undo whatever CONFIG SET had done. The error says so
// rather than answering "unknown subcommand", because an operator who reaches
// for CONFIG SET needs to be told where the knob actually is.

// The CONFIG subcommands this server answers to, upper-cased as the codec
// delivers them.
const (
	configGet  = "GET"
	configSet  = "SET"
	configHelp = "HELP"
)

// paramMaxMemory is the one parameter name spelled in more than one place: the
// table below and the tests that assert its format. The rest are written where
// they are read.
const paramMaxMemory = "maxmemory"

// configParameter is one row of the table CONFIG GET answers from.
//
// The value is a function rather than a string because every parameter here is
// read from the running server: `maxmemory` follows a config reload, and a
// snapshot taken at startup would report the value the server no longer uses.
type configParameter struct {
	name  string
	value func(*session) string
}

// configParameters is every parameter CONFIG GET reports, in the order it
// reports them.
//
// Alphabetical, because the order has to come from somewhere and a stable one
// is testable. Redis's own order is its config dictionary's, which is to say
// arbitrary, so nothing can be relying on a particular one.
//
// Each name is Redis's spelling for the same quantity, in Redis's units, so a
// tool written against Redis reads them unchanged: `maxmemory` in bytes,
// `timeout` in seconds, `maxclients` as a count. A setting with no Redis
// equivalent is deliberately absent rather than invented under an
// atlascache_ prefix — INFO is where AtlasCache's own figures are reported,
// and CONFIG GET is read by tools that only know Redis's names.
var configParameters = []configParameter{
	{name: "appendonly", value: notPersisted},
	{name: "maxclients", value: func(s *session) string {
		return strconv.Itoa(s.srv.limits.MaxConnections)
	}},
	{name: paramMaxMemory, value: func(s *session) string {
		// Bytes, as a decimal string, which is how Redis reports it — "0"
		// means unlimited there and here.
		return strconv.FormatUint(s.store().Stats().MemoryMax, 10)
	}},
	{name: "save", value: notPersisted},
	{name: "timeout", value: func(s *session) string {
		// Seconds, as Redis's `timeout` is. Note the default differs: Redis
		// leaves it 0, AtlasCache closes an idle connection after 30s
		// (FEAT-0024), and this is where an operator can read that back.
		return strconv.FormatInt(int64(s.srv.limits.IdleTimeout.Seconds()), 10)
	}},
}

// notPersisted is the value of a persistence parameter on a server that does
// not persist. See the file comment: empty, never fabricated.
func notPersisted(*session) string { return "" }

// config dispatches the CONFIG subcommands.
//
// Everything outside GET is refused, and the refusals are worth as much as the
// one implementation: an operator who sends CONFIG SET learns where
// configuration is actually changed, and a tool that sends RESETSTAT or
// REWRITE gets the error Redis gives for a subcommand it does not know rather
// than a dropped connection.
func (s *session) config(cmd protocol.Command) protocol.Reply {
	switch strings.ToUpper(string(cmd.Args[0])) {
	case configGet:
		if len(cmd.Args) < 2 {
			// Redis names the subcommand in the arity error, so a client
			// logging it can tell CONFIG GET from CONFIG SET.
			return protocol.Errorf("wrong number of arguments for 'config|get' command")
		}
		return s.configGet(cmd.Args[1:])

	case configSet:
		return errConfigSetUnsupported()

	case configHelp:
		return configHelpReply()

	default:
		return protocol.Errorf("unknown subcommand '%s'. Try CONFIG HELP.", cmd.Args[0])
	}
}

// configGet answers with the parameters the patterns select, as a map of name
// to value.
//
// It is a protocol.Map for the same reason STATS is: RESP2 has no map frame, so
// the codec flattens it into the flat array of alternating names and values
// that Redis sends a RESP2 client, and the RESP3 encoder in P6 will render the
// same handler's reply as the map Redis sends a RESP3 client (ADR-0028).
//
// A pattern that selects nothing is not an error. `CONFIG GET nosuchparam`
// answers with an empty reply, exactly as Redis does, because tools probe for
// parameters they are not sure the server has.
func (s *session) configGet(patterns [][]byte) protocol.Reply {
	selected := s.configMatches(patterns)

	reply := make(protocol.Map, 0, len(selected))
	for _, match := range selected {
		reply = append(reply, protocol.KV{
			Key:   protocol.BulkString(match.reportAs),
			Value: protocol.BulkString(match.parameter.value(s)),
		})
	}
	return reply
}

// configMatch is one parameter a CONFIG GET selected, and the spelling to
// report it under.
type configMatch struct {
	parameter configParameter
	reportAs  string
}

// configMatches resolves CONFIG GET's patterns to the parameters they select,
// in table order and without repeats.
//
// Two Redis behaviors are reproduced here because tools depend on them.
//
// A parameter name is matched case-insensitively, and a **name** — as opposed
// to a pattern — is echoed back in the spelling the client used: `CONFIG GET
// MAXMEMORY` answers with the key `MAXMEMORY`, because Redis keys its match
// set by what was asked for. A client that looks the reply up by the string it
// sent therefore finds it, which is the whole point of the quirk.
//
// A **pattern** is matched against the canonical names and reports them, since
// there is no single spelling a glob could be echoed as. The case-insensitivity
// comes from lower-casing the pattern, which is exact for the lower-case names
// in the table above.
func (s *session) configMatches(patterns [][]byte) []configMatch {
	// Canonical name -> the spelling to report it under. First pattern to
	// select a parameter names it, as Redis's match dictionary does.
	reportAs := make(map[string]string, len(configParameters))

	for _, raw := range patterns {
		pattern := string(raw)
		lowered := strings.ToLower(pattern)

		if !strings.ContainsAny(pattern, "*?[") {
			if _, known := lookupConfigParameter(lowered); known {
				if _, seen := reportAs[lowered]; !seen {
					reportAs[lowered] = pattern
				}
			}
			continue
		}

		for _, parameter := range configParameters {
			if _, seen := reportAs[parameter.name]; seen {
				continue
			}
			// No MatchesEverything short circuit: that exists for the empty
			// subject, and a parameter name is never empty.
			if glob.Match(lowered, parameter.name) {
				reportAs[parameter.name] = parameter.name
			}
		}
	}

	matches := make([]configMatch, 0, len(reportAs))
	for _, parameter := range configParameters {
		if spelling, selected := reportAs[parameter.name]; selected {
			matches = append(matches, configMatch{parameter: parameter, reportAs: spelling})
		}
	}
	return matches
}

// lookupConfigParameter finds a parameter by its canonical (lower-case) name.
func lookupConfigParameter(name string) (configParameter, bool) {
	for _, parameter := range configParameters {
		if parameter.name == name {
			return parameter, true
		}
	}
	return configParameter{}, false
}

// errConfigSetUnsupported names the path that does work.
//
// "Unknown subcommand" would be true and useless: the operator would go looking
// for a version of AtlasCache that has it. Configuration is a file, it is
// re-read when it changes, and SIGHUP forces the re-read — that is the sentence
// they need.
func errConfigSetUnsupported() protocol.Reply {
	return protocol.Errorf("CONFIG SET is not supported; AtlasCache reads its configuration " +
		"from the config file and reloads it when the file changes or on SIGHUP")
}

// configHelpReply is Redis's shape for CONFIG HELP: an array of status lines.
//
// It exists so the "Try CONFIG HELP." in the unknown-subcommand error above is
// advice that leads somewhere, and so an operator who reaches for CONFIG SET
// interactively can find out what is supported without leaving the session.
func configHelpReply() protocol.Reply {
	return protocol.Array{
		protocol.SimpleString("CONFIG <subcommand> [<arg> [value] [opt] ...]. Subcommands are:"),
		protocol.SimpleString("GET <pattern>"),
		protocol.SimpleString("    Return parameters matching the glob-like <pattern> and their values."),
		protocol.SimpleString("SET <directive> <value>"),
		protocol.SimpleString("    Not supported. AtlasCache reloads its configuration from the config"),
		protocol.SimpleString("    file when the file changes, or on SIGHUP."),
		protocol.SimpleString("HELP"),
		protocol.SimpleString("    Print this help."),
	}
}
