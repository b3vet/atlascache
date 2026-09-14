package client

import (
	"errors"
	"strconv"
	"time"
)

// errUnsupportedArg is what Do answers for an argument it will not guess at.
var errUnsupportedArg = errors.New("unsupported argument type")

// encodeArg renders one Do argument as wire bytes.
//
// The accepted types are the ones a command line is actually built from.
// Anything else is refused rather than rendered with %v: a struct that reached
// the wire as its Go formatting would be stored, retrieved, and only noticed to
// be wrong much later.
//
// []byte is passed through without copying, which is what makes Do binary safe.
// A string is converted for the same reason: Go strings hold arbitrary bytes,
// so a key with a null in it survives the trip.
func encodeArg(arg any) ([]byte, error) {
	switch v := arg.(type) {
	case nil:
		return nil, errors.New("nil is not a command argument")

	case []byte:
		return v, nil
	case string:
		return []byte(v), nil

	case bool:
		// 1 and 0, because that is what the server's integer replies use and
		// what a caller comparing them will expect — "true" is a string no
		// command reads.
		if v {
			return []byte("1"), nil
		}
		return []byte("0"), nil

	case time.Duration:
		// Whole seconds, the unit EX and EXPIRE take. A caller who needs
		// milliseconds writes PX and the number themselves.
		return strconv.AppendInt(nil, int64(v/time.Second), 10), nil
	}

	if value, ok := encodeSigned(arg); ok {
		return value, nil
	}
	if value, ok := encodeUnsigned(arg); ok {
		return value, nil
	}
	if value, ok := encodeFloat(arg); ok {
		return value, nil
	}
	return nil, errUnsupportedArg
}

func encodeSigned(arg any) ([]byte, bool) {
	var n int64
	switch v := arg.(type) {
	case int:
		n = int64(v)
	case int8:
		n = int64(v)
	case int16:
		n = int64(v)
	case int32:
		n = int64(v)
	case int64:
		n = v
	default:
		return nil, false
	}
	return strconv.AppendInt(nil, n, 10), true
}

func encodeUnsigned(arg any) ([]byte, bool) {
	var n uint64
	switch v := arg.(type) {
	case uint:
		n = uint64(v)
	case uint8:
		n = uint64(v)
	case uint16:
		n = uint64(v)
	case uint32:
		n = uint64(v)
	case uint64:
		n = v
	default:
		return nil, false
	}
	return strconv.AppendUint(nil, n, 10), true
}

// encodeFloat renders with the shortest representation that round-trips, and
// at the precision the value was given in: a float32 rendered through float64
// would grow digits it never had.
func encodeFloat(arg any) ([]byte, bool) {
	switch v := arg.(type) {
	case float32:
		return strconv.AppendFloat(nil, float64(v), 'f', -1, 32), true
	case float64:
		return strconv.AppendFloat(nil, v, 'f', -1, 64), true
	default:
		return nil, false
	}
}
