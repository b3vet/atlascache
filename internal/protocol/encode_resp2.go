package protocol

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// The RESP2 encoder. It is the only encoder in v0.1.0 (ADR-0028): every logical
// reply type has a rendering here, including the types RESP3 will render
// differently, so FEAT-0048 adds a second file beside this one rather than
// adding reply types.

// appendReply renders one reply in RESP2 and appends it to dst.
//
// The aggregates are handled here; everything that fits on one line is handled
// by appendScalar.
func appendReply(dst []byte, reply Reply) ([]byte, error) {
	switch v := reply.(type) {
	case Array:
		return appendItems(dst, v)

	case Set:
		// RESP2 has no set frame. The order is whatever the handler built, and
		// no client may read meaning into it.
		return appendItems(dst, v)

	case Push:
		// RESP2 has no push frame either: a pub/sub message is delivered as an
		// ordinary array, which is how RESP2 pub/sub has always worked.
		return appendItems(dst, v)

	case Map:
		return appendPairs(dst, v)

	case Attribute:
		// RESP2 has no attribute frame, so the metadata is dropped and the
		// value is sent alone — what Redis sends a RESP2 client.
		if v.Value == nil {
			return appendReply(dst, Nil)
		}
		return appendReply(dst, v.Value)

	default:
		return appendScalar(dst, reply)
	}
}

// appendScalar renders the replies that carry their whole value on one line, or
// in one length-prefixed string.
func appendScalar(dst []byte, reply Reply) ([]byte, error) {
	switch v := reply.(type) {
	case SimpleString:
		dst = append(dst, '+')
		dst = appendSingleLine(dst, string(v))
		return appendCRLF(dst), nil

	case Error:
		dst = append(dst, '-')
		dst = appendSingleLine(dst, v.Error())
		return appendCRLF(dst), nil

	case BulkError:
		// RESP2 has no blob error, so it degrades to a simple error. That is
		// why the message is folded onto one line rather than truncated: the
		// whole text still reaches the client.
		dst = append(dst, '-')
		dst = appendSingleLine(dst, v.Error())
		return appendCRLF(dst), nil

	case BulkString:
		return appendBulk(dst, v), nil

	case Verbatim:
		// The three-byte format hint is a RESP3 frame detail; a RESP2 client
		// gets the text.
		return appendBulk(dst, []byte(v.Text)), nil

	case BigNumber:
		return appendBulk(dst, []byte(v)), nil

	case Double:
		return appendBulk(dst, []byte(formatDouble(float64(v)))), nil

	case Integer:
		dst = append(dst, ':')
		dst = strconv.AppendInt(dst, int64(v), 10)
		return appendCRLF(dst), nil

	case Boolean:
		dst = append(dst, ':')
		if v {
			dst = append(dst, '1')
		} else {
			dst = append(dst, '0')
		}
		return appendCRLF(dst), nil

	case nilReply:
		return append(dst, "$-1\r\n"...), nil

	default:
		return nil, fmt.Errorf("protocol: unsupported reply type %T", reply)
	}
}

// appendItems renders an array header and its elements.
func appendItems(dst []byte, items []Reply) ([]byte, error) {
	dst = appendArrayHeader(dst, len(items))
	for _, item := range items {
		var err error
		if dst, err = appendReply(dst, item); err != nil {
			return nil, err
		}
	}
	return dst, nil
}

// appendPairs flattens a map into an array of 2n elements, key then value,
// which is the shape a RESP2 client has always read a map-like reply in.
func appendPairs(dst []byte, pairs []KV) ([]byte, error) {
	dst = appendArrayHeader(dst, 2*len(pairs))
	for _, pair := range pairs {
		var err error
		if dst, err = appendReply(dst, pair.Key); err != nil {
			return nil, err
		}
		if dst, err = appendReply(dst, pair.Value); err != nil {
			return nil, err
		}
	}
	return dst, nil
}

func appendArrayHeader(dst []byte, n int) []byte {
	dst = append(dst, '*')
	dst = strconv.AppendInt(dst, int64(n), 10)
	return appendCRLF(dst)
}

func appendBulk(dst, payload []byte) []byte {
	dst = append(dst, '$')
	dst = strconv.AppendInt(dst, int64(len(payload)), 10)
	dst = appendCRLF(dst)
	dst = append(dst, payload...)
	return appendCRLF(dst)
}

// appendSingleLine writes text that a CRLF would otherwise frame out of the
// reply. A status or error carrying a newline would be read as the end of that
// reply and the start of another, so the two bytes are folded to spaces rather
// than trusted — the same thing Redis does to an error it formats.
func appendSingleLine(dst []byte, text string) []byte {
	if !strings.ContainsAny(text, "\r\n") {
		return append(dst, text...)
	}
	return append(dst, strings.NewReplacer("\r", " ", "\n", " ").Replace(text)...)
}

// formatDouble renders a double the way a RESP2 client reads one: the shortest
// text that parses back to the same value, with the infinities spelled as Redis
// spells them.
func formatDouble(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	case math.IsNaN(f):
		return "nan"
	default:
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
}

func appendCRLF(dst []byte) []byte {
	return append(dst, '\r', '\n')
}
