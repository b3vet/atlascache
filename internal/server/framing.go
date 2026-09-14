package server

import "bufio"

// Knowing when to stop draining a pipeline.
//
// Pipelining here is buffering, not concurrency (ADR-0021): a batch of commands
// is decoded from the read buffer, executed in order, and answered with one
// flush. The whole design turns on one question asked between commands — is
// there another complete request already in the buffer? — and on what happens
// when the answer is guessed wrong.
//
// Guessing "no" when the answer was yes costs a flush that could have waited:
// the next read blocks, the data is already there, and it returns at once. That
// is a wasted syscall and nothing else.
//
// Guessing "yes" when the answer was no is a stall. The decoder would block
// waiting for the rest of a partial frame while the replies to the commands
// before it sit unflushed in a buffer the client is waiting on, and a client
// that pipelines and then waits deadlocks against a server that is waiting for
// it.
//
// So this scanner is one-sided by construction: it reports a complete request
// only when decoding one cannot block, and treats everything it does not
// understand — including anything the decoder might skip — as incomplete. It
// never consumes, so a frame split across two packets stays in the read buffer
// to be completed by the next read rather than being misparsed.
//
// It is not a second parser, and it must not become one. It answers "can the
// decoder proceed without reading" and nothing about what a request means: the
// codec remains the only thing that decides whether a request is valid, what it
// costs, and which limit it breaks.

// requestBuffered reports whether a complete request is already in the read
// buffer, so that decoding one cannot block.
func requestBuffered(r *bufio.Reader) bool {
	buffered := r.Buffered()
	if buffered == 0 {
		return false
	}

	// Peeking at what is already buffered never reads: the count came from the
	// buffer itself, so the request is always satisfied from it.
	buf, err := r.Peek(buffered)
	if err != nil {
		return false
	}

	return scanRequest(buf) > 0
}

// scanRequest returns the length of the first complete request in buf, or 0 if
// buf does not hold one.
//
// A malformed request counts as complete, because the decoder refuses it
// without reading another byte — and the question here is whether decoding can
// block, not whether it will succeed.
func scanRequest(buf []byte) int {
	pos := 0
	for {
		line, next, ok := scanLine(buf[pos:])
		if !ok {
			return 0
		}

		switch {
		case len(line) == 0:
			// An empty line, which the decoder skips before reading the next.
			pos += next

		case line[0] == '*':
			count, countOK := scanInt(line[1:])
			switch {
			case !countOK || count < 0:
				// A header the decoder refuses on sight.
				return pos + next
			case count == 0:
				// The empty request Redis skips; the decoder reads on.
				pos += next
			case count > len(buf):
				// There are not enough bytes left for one element each, so
				// this request cannot be complete however it is shaped.
				return 0
			default:
				return scanElements(buf, pos+next, count)
			}

		case commandBytes(line):
			// An inline request, complete the moment its line ends.
			return pos + next

		default:
			// A line with nothing in it the decoder would read as a command
			// name. It skips such a line and reads the next one, and rather
			// than model which bytes count as spaces, the scan gives up: the
			// caller flushes and blocks, which is correct for any line.
			return 0
		}
	}
}

// scanElements measures the bulk strings of a multibulk request whose header
// ended at start.
func scanElements(buf []byte, start, count int) int {
	at := start
	for range count {
		line, next, ok := scanLine(buf[at:])
		if !ok {
			return 0
		}
		if len(line) == 0 || line[0] != '$' {
			// Not a bulk string: the decoder refuses it having read this line
			// and nothing after it.
			return at + next
		}

		length, lengthOK := scanInt(line[1:])
		if !lengthOK || length < 0 {
			return at + next
		}

		at += next
		// The payload, and the CRLF behind it.
		if len(buf)-at < length+2 {
			return 0
		}
		at += length + 2
	}

	return at
}

// scanLine returns the content of the line starting at buf[0], without its
// terminator, and how many bytes the line occupied. It mirrors the decoder's
// terminator rule: a line ends at a LF, and a CR in front of the LF belongs to
// the terminator (ISSUE-0019).
func scanLine(buf []byte) (line []byte, size int, ok bool) {
	for i, b := range buf {
		if b != '\n' {
			continue
		}
		end := i
		if end > 0 && buf[end-1] == '\r' {
			end--
		}
		return buf[:end], i + 1, true
	}
	return nil, 0, false
}

// commandBytes reports whether an inline line certainly yields a command.
//
// The decoder splits an inline line on whitespace and skips the line entirely
// when nothing is left, so a line of spaces is not a request and claiming it
// was one would stall the connection. Rather than reproduce the decoder's idea
// of whitespace — which is Unicode's, and spans bytes above 0x7f — anything
// outside plain ASCII is treated as possibly-blank. A real command name is
// ASCII, so this says yes to every request a client actually sends and no to
// the cases it cannot be sure about.
func commandBytes(line []byte) bool {
	for _, b := range line {
		if b < 0x80 && !asciiSpace(b) {
			return true
		}
	}
	return false
}

func asciiSpace(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\v', '\f', '\r':
		return true
	default:
		return false
	}
}

// scanInt parses a decimal integer without allocating, which strconv.Atoi over
// a converted string would do once per element on the pipelining hot path.
// Anything that is not a plain decimal number of a workable size is refused,
// and the decoder is left to produce the error for it.
func scanInt(text []byte) (int, bool) {
	// Eighteen digits is the most that cannot overflow an int64, and a length
	// nobody sends legitimately; the decoder refuses anything longer too.
	const maxDigits = 18

	if len(text) == 0 {
		return 0, false
	}

	negative := text[0] == '-'
	if negative {
		text = text[1:]
	}
	if len(text) == 0 || len(text) > maxDigits {
		return 0, false
	}

	value := 0
	for _, b := range text {
		if b < '0' || b > '9' {
			return 0, false
		}
		value = value*10 + int(b-'0')
	}
	if negative {
		return -value, true
	}
	return value, true
}
