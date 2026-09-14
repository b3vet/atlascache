package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The two spellings a value can take in JSON. A JSON string cannot hold
// arbitrary bytes, so one of these has to be said out loud rather than guessed
// at by the reader.
const (
	encodingUTF8   = "utf8"
	encodingBase64 = "base64"
)

// result is what a command produced: the JSON payload, and how to write the
// same thing for a human or a pipe.
//
// Both are always built, whichever the caller asked for, because a command that
// rendered its own output would have to know about --json — and then adding a
// command would mean remembering to handle it twice.
type result struct {
	data any
	text func(*output) error
}

// output writes a command's data to stdout.
type output struct {
	w   io.Writer
	tty bool
}

// write puts s on stdout exactly as given.
func (o *output) write(s string) error {
	_, err := io.WriteString(o.w, s)
	return err
}

// line writes one line of text, with the newline a line has.
func (o *output) line(format string, args ...any) error {
	_, err := fmt.Fprintf(o.w, format+"\n", args...)
	return err
}

// value writes a stored value, and is the reason this type exists.
//
// Redirected, the bytes go out exactly as the server holds them: no trailing
// newline, no escaping, nothing added. `atlasctl get k > file` has to
// round-trip a value holding null bytes or invalid UTF-8, and a newline
// appended "for readability" would corrupt every such value silently.
//
// On a terminal the value is followed by a newline so the shell prompt lands in
// the right place, and a value that would disturb the terminal — a control
// character, a byte sequence that is not UTF-8 — is printed in Go's quoted
// form instead. That is a courtesy to a human and a corruption of anything
// else, which is why it is decided by what stdout is rather than by a flag.
func (o *output) value(b []byte) error {
	if !o.tty {
		_, err := o.w.Write(b)
		return err
	}
	if needsEscaping(b) {
		return o.line("%s", strconv.Quote(string(b)))
	}
	return o.line("%s", b)
}

// needsEscaping reports whether printing these bytes raw would disturb a
// terminal: anything that is not valid UTF-8, and any control character other
// than the whitespace a text value legitimately holds.
func needsEscaping(b []byte) bool {
	if !utf8.Valid(b) {
		return true
	}
	for _, r := range string(b) {
		if r == '\n' || r == '\t' || r == '\r' {
			continue
		}
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// encodeValue renders bytes for JSON, and says which way it did it.
func encodeValue(b []byte) (text, encoding string) {
	if utf8.Valid(b) {
		return string(b), encodingUTF8
	}
	return base64.StdEncoding.EncodeToString(b), encodingBase64
}

// encodeKeys renders a list of keys for JSON under one encoding.
//
// One encoding for the whole list, rather than one per element, so that the
// shape of the array does not change with its contents: a caller reading
// data.keys[0] gets a string either way, and data.encoding says how to read it.
func encodeKeys(keys []string) ([]string, string) {
	encoding := encodingUTF8
	for _, key := range keys {
		if !utf8.Valid([]byte(key)) {
			encoding = encodingBase64
			break
		}
	}
	if encoding == encodingUTF8 {
		return keys, encodingUTF8
	}

	encoded := make([]string, len(keys))
	for i, key := range keys {
		encoded[i] = base64.StdEncoding.EncodeToString([]byte(key))
	}
	return encoded, encodingBase64
}

// envelope is the --json document, and is a published interface: people will
// parse it, so its shape is stable and documented in this package's doc
// comment (FEAT-0029).
type envelope struct {
	OK      bool       `json:"ok"`
	Command string     `json:"command"`
	Data    any        `json:"data,omitempty"`
	Error   *jsonError `json:"error,omitempty"`
}

// jsonError is the failure half of the envelope. Code is the process exit code,
// repeated inside the document so that a caller reading the JSON off a pipe
// need not also have kept the exit status.
type jsonError struct {
	Code    int    `json:"code"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// reportSuccess writes a command's output and returns the exit code.
func (e *env) reportSuccess(inv invocation) int {
	if inv.asJSON {
		return e.writeJSON(envelope{OK: true, Command: inv.command, Data: inv.res.data})
	}
	if inv.res.text == nil {
		return exitOK
	}
	if err := inv.res.text(&output{w: e.stdout, tty: e.stdoutIsTTY}); err != nil {
		fmt.Fprintf(e.stderr, "atlasctl: writing the result: %v\n", err)
		return exitFailure
	}
	return exitOK
}

// reportError writes a failure and returns the exit code that describes it.
//
// The data a failing command did produce is kept: a `get` that found no key
// still says which key, and a script reading the JSON should not have to parse
// the message to find out.
func (e *env) reportError(inv invocation) int {
	code, kind := classify(inv.err)

	if inv.asJSON {
		return e.writeJSON(envelope{
			OK:      false,
			Command: inv.command,
			Data:    inv.res.data,
			Error:   &jsonError{Code: code, Kind: kind, Message: inv.err.Error()},
		})
	}

	fmt.Fprintf(e.stderr, "atlasctl: %v\n", inv.err)
	return code
}

// writeJSON prints the envelope, and only the envelope: one object on stdout,
// nothing else, whatever happened.
func (e *env) writeJSON(env envelope) int {
	encoder := json.NewEncoder(e.stdout)
	// Off, because a value is not HTML and turning its < into < would make
	// a round trip through this CLI change the bytes.
	encoder.SetEscapeHTML(false)

	if err := encoder.Encode(env); err != nil {
		fmt.Fprintf(e.stderr, "atlasctl: writing JSON: %v\n", err)
		return exitFailure
	}
	if env.Error != nil {
		return env.Error.Code
	}
	return exitOK
}

// indentedLines renders a block of text with a trailing newline, which is what
// every multi-line command output here wants.
func indentedLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}
