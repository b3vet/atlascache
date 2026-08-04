package client

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ParseCommand splits a spec's command line into the arguments of a RESP array.
//
// The grammar is redis-cli's, so a spec author can paste a line they tried by
// hand and get the same command on the wire:
//
//	SET k1 hello                    → ["SET", "k1", "hello"]
//	SET k1 "hello world"            → ["SET", "k1", "hello world"]
//	SET k1 "line\r\n"               → ["SET", "k1", "line\r\n"]
//	SET k1 "\x41"                   → ["SET", "k1", "A"]
//	SET k1 'it''s literal'          → ["SET", "k1", "it's literal"]  (single quotes escape only \')
//	SET k1 ""                       → ["SET", "k1", ""]
//
// Quoting matters more than it looks: without it a spec cannot write an empty
// argument or a value containing a space, and both appear the moment real data
// commands arrive in P1.
func ParseCommand(line string) ([]string, error) {
	args, err := scanArgs(line)
	if err != nil {
		return nil, fmt.Errorf("command %q: %w", line, err)
	}
	if len(args) == 0 {
		return nil, fmt.Errorf("command %q is empty", line)
	}
	return args, nil
}

// scanArgs walks the line once, one argument at a time.
func scanArgs(line string) ([]string, error) {
	var args []string
	for i := 0; i < len(line); {
		if isSpace(line[i]) {
			i++
			continue
		}
		arg, next, err := scanArg(line, i)
		if err != nil {
			return nil, err
		}
		args = append(args, arg)
		i = next
	}
	return args, nil
}

// scanArg reads the argument starting at i and returns it with the index just
// past it.
func scanArg(line string, i int) (string, int, error) {
	var arg strings.Builder
	switch line[i] {
	case '"':
		return scanQuoted(line, i+1, &arg)
	case '\'':
		return scanSingleQuoted(line, i+1, &arg)
	}

	for i < len(line) && !isSpace(line[i]) {
		if line[i] == '"' || line[i] == '\'' {
			return "", 0, fmt.Errorf("a quote must open an argument, not appear inside one, at byte %d", i)
		}
		arg.WriteByte(line[i])
		i++
	}
	return arg.String(), i, nil
}

// scanQuoted reads a double-quoted argument, honoring the escapes redis-cli
// honors, including \xHH.
func scanQuoted(line string, i int, arg *strings.Builder) (string, int, error) {
	for i < len(line) {
		switch line[i] {
		case '\\':
			decoded, next, err := unescape(line, i+1)
			if err != nil {
				return "", 0, err
			}
			arg.WriteString(decoded)
			i = next

		case '"':
			if i+1 < len(line) && !isSpace(line[i+1]) {
				return "", 0, fmt.Errorf("a closing quote must be followed by a space, at byte %d", i)
			}
			return arg.String(), i + 1, nil

		default:
			arg.WriteByte(line[i])
			i++
		}
	}
	return "", 0, errors.New("a double quote is never closed")
}

// scanSingleQuoted reads a single-quoted argument. Only \' is an escape, so a
// value full of backslashes can be written without doubling them.
func scanSingleQuoted(line string, i int, arg *strings.Builder) (string, int, error) {
	for i < len(line) {
		switch {
		case line[i] == '\\' && i+1 < len(line) && line[i+1] == '\'':
			arg.WriteByte('\'')
			i += 2

		case line[i] == '\'':
			if i+1 < len(line) && !isSpace(line[i+1]) {
				return "", 0, fmt.Errorf("a closing quote must be followed by a space, at byte %d", i)
			}
			return arg.String(), i + 1, nil

		default:
			arg.WriteByte(line[i])
			i++
		}
	}
	return "", 0, errors.New("a single quote is never closed")
}

// unescape decodes the escape sequence whose body starts at i.
func unescape(line string, i int) (string, int, error) {
	if i >= len(line) {
		return "", 0, errors.New("a backslash ends the line with nothing to escape")
	}
	switch line[i] {
	case 'n':
		return "\n", i + 1, nil
	case 'r':
		return "\r", i + 1, nil
	case 't':
		return "\t", i + 1, nil
	case 'a':
		return "\a", i + 1, nil
	case 'b':
		return "\b", i + 1, nil
	case 'x':
		if i+2 >= len(line) {
			return "", 0, errors.New(`\x needs two hex digits`)
		}
		value, err := strconv.ParseUint(line[i+1:i+3], 16, 8)
		if err != nil {
			return "", 0, fmt.Errorf(`\x%s is not two hex digits`, line[i+1:i+3])
		}
		return string([]byte{byte(value)}), i + 3, nil
	default:
		// \\ and \" land here, as does any other escaped byte: it stands for
		// itself, which is what redis-cli does.
		return string(line[i]), i + 1, nil
	}
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}
