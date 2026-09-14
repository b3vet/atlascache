package client

import (
	"strconv"
)

// opReply is the Op an error from a conversion helper carries. The failure did
// not happen at a command or a socket, it happened while reading what one of
// them returned.
const opReply = "reply"

// ReplyType is the shape of a decoded reply.
//
// The RESP3 types are decoded even though v0.1.0 servers only speak RESP2
// (ADR-0028), and the ones with a RESP2 equivalent are normalized onto it: a
// verbatim string reads as a bulk string, a set reads as an array. A caller
// that wrote its type switch against a RESP2 server keeps working when the
// server it talks to starts answering HELLO 3.
type ReplyType uint8

// The reply shapes a caller may see.
const (
	// TypeNil is the null bulk string or null array. It is not an error: a
	// cache miss is the most ordinary outcome there is.
	TypeNil ReplyType = iota
	TypeStatus
	TypeError
	TypeInteger
	TypeBulk
	TypeArray
	TypeMap
	TypeBool
	TypeDouble
	TypeBigNumber

	// TypePush is a RESP3 out-of-band message. Nothing in v0.1.0 produces one,
	// and one arriving unsolicited would mean the stream is not where the SDK
	// thinks it is — so conn.exchange refuses it rather than returning it.
	TypePush
)

// replyTypeNames is what a type is called in an error message. It is a table
// rather than a switch so that adding a type without naming it is a compile
// error's worth of obvious rather than a silent "unknown(7)".
var replyTypeNames = map[ReplyType]string{
	TypeNil:       "nil",
	TypeStatus:    "status",
	TypeError:     "error",
	TypeInteger:   "integer",
	TypeBulk:      "bulk string",
	TypeArray:     "array",
	TypeMap:       "map",
	TypeBool:      "boolean",
	TypeDouble:    "double",
	TypeBigNumber: "big number",
	TypePush:      "push",
}

func (t ReplyType) String() string {
	if name, ok := replyTypeNames[t]; ok {
		return name
	}
	return "unknown(" + strconv.Itoa(int(t)) + ")"
}

// Reply is one decoded server reply, and what Do hands back.
//
// The payload is []byte rather than string throughout, because RESP is binary
// safe and a value containing a null byte or invalid UTF-8 is a value like any
// other. The conversion helpers below are where a caller opts into a string.
type Reply struct {
	Type ReplyType

	// Str carries the bytes of a bulk string, the text of a status or error
	// reply, and the digits of a big number.
	Str []byte

	// Int carries an integer reply, and a boolean as 1 or 0.
	Int int64

	// Float carries a RESP3 double. Str holds the same value as the server
	// wrote it, for callers that must not round-trip through float64.
	Float float64

	// Kind is the error kind of an error reply — the first word, "ERR" or
	// "WRONGPASS" or "OOM" — with Str holding the whole line.
	Kind string

	// Arr holds the elements of an array, or the keys and values of a map
	// flattened into alternating pairs. Flattening is deliberate: RESP2 has no
	// map frame and renders one as exactly that array, so a caller reading
	// STATS sees the same thing from a RESP2 and a RESP3 server.
	Arr []Reply
}

// IsNil reports whether the server answered with a null — a missing key, or an
// operation that produced nothing.
func (r Reply) IsNil() bool { return r.Type == TypeNil }

// Bytes returns the reply as bytes, without copying.
//
// A nil reply returns (nil, nil), because "no value" is not an error. A caller
// that must tell a missing key from an empty one — both are legal, and the
// server distinguishes them — checks IsNil rather than the length.
func (r Reply) Bytes() ([]byte, error) {
	switch r.Type {
	case TypeNil:
		return nil, nil
	case TypeBulk, TypeStatus, TypeBigNumber:
		return r.Str, nil
	case TypeInteger:
		return strconv.AppendInt(nil, r.Int, 10), nil
	case TypeDouble:
		return r.Str, nil
	case TypeBool:
		return strconv.AppendInt(nil, r.Int, 10), nil
	case TypeError:
		return nil, replyError(opReply, "", r)
	default:
		return nil, r.mismatch("bytes")
	}
}

// Text returns the reply as a string, copying the bytes.
func (r Reply) Text() (string, error) {
	value, err := r.Bytes()
	if err != nil {
		return "", err
	}
	return string(value), nil
}

// Int64 returns the reply as an integer, parsing a bulk string if that is what
// arrived — the server answers some numeric questions with one.
func (r Reply) Int64() (int64, error) {
	switch r.Type {
	case TypeInteger, TypeBool:
		return r.Int, nil
	case TypeBulk, TypeStatus, TypeBigNumber:
		n, err := strconv.ParseInt(string(r.Str), 10, 64)
		if err != nil {
			return 0, protocolError(opReply, "", "reply "+strconv.Quote(string(r.Str))+" is not an integer")
		}
		return n, nil
	case TypeError:
		return 0, replyError(opReply, "", r)
	default:
		return 0, r.mismatch("integer")
	}
}

// Bool reports the reply as a truth value: a non-zero integer, a RESP3 boolean,
// or the +OK every write answers with.
func (r Reply) Bool() (bool, error) {
	switch r.Type {
	case TypeInteger, TypeBool:
		return r.Int != 0, nil
	case TypeNil:
		return false, nil
	case TypeStatus:
		return string(r.Str) == statusOK, nil
	case TypeError:
		return false, replyError(opReply, "", r)
	default:
		return false, r.mismatch("boolean")
	}
}

// Float64 returns the reply as a floating point number.
func (r Reply) Float64() (float64, error) {
	switch r.Type {
	case TypeDouble:
		return r.Float, nil
	case TypeInteger:
		return float64(r.Int), nil
	case TypeBulk, TypeStatus:
		f, err := strconv.ParseFloat(string(r.Str), 64)
		if err != nil {
			return 0, protocolError(opReply, "", "reply "+strconv.Quote(string(r.Str))+" is not a number")
		}
		return f, nil
	case TypeError:
		return 0, replyError(opReply, "", r)
	default:
		return 0, r.mismatch("number")
	}
}

// Slice returns the elements of an array reply. A nil reply is an empty slice:
// RESP2 renders an empty array and a null array differently, and no caller
// iterating one cares which it got.
func (r Reply) Slice() ([]Reply, error) {
	switch r.Type {
	case TypeArray, TypeMap:
		return r.Arr, nil
	case TypeNil:
		return nil, nil
	case TypeError:
		return nil, replyError(opReply, "", r)
	default:
		return nil, r.mismatch("array")
	}
}

// ByteSlices returns an array reply as its elements' bytes.
func (r Reply) ByteSlices() ([][]byte, error) {
	items, err := r.Slice()
	if err != nil {
		return nil, err
	}
	out := make([][]byte, 0, len(items))
	for _, item := range items {
		value, err := item.Bytes()
		if err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, nil
}

// Strings returns an array reply as strings.
func (r Reply) Strings() ([]string, error) {
	items, err := r.Slice()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		text, err := item.Text()
		if err != nil {
			return nil, err
		}
		out = append(out, text)
	}
	return out, nil
}

// Map reads a reply as key/value pairs.
//
// It accepts a RESP3 map and the flat RESP2 array a v0.1.0 server sends for the
// same reply, so a caller reading STATS or HELLO need not know which encoding
// the connection negotiated.
func (r Reply) Map() (map[string]Reply, error) {
	items, err := r.Slice()
	if err != nil {
		return nil, err
	}
	if len(items)%2 != 0 {
		return nil, protocolError(opReply, "", "map has "+strconv.Itoa(len(items))+" elements, which is not an even number of pairs")
	}

	out := make(map[string]Reply, len(items)/2)
	for i := 0; i < len(items); i += 2 {
		key, err := items[i].Text()
		if err != nil {
			return nil, err
		}
		out[key] = items[i+1]
	}
	return out, nil
}

// mismatch is the error a conversion helper returns when the reply was a
// perfectly good one of some other shape. It is ErrProtocol because that is the
// category for "the reply was not what this call can use", and naming both
// shapes is what makes it debuggable.
func (r Reply) mismatch(want string) *Error {
	return protocolError(opReply, "", "cannot read a "+r.Type.String()+" reply as "+want)
}
