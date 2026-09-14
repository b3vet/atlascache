package client

import (
	"errors"
	"testing"
)

func TestReplyBytesAndText(t *testing.T) {
	t.Run("bytes are handed out without a copy", func(t *testing.T) {
		payload := []byte{0x00, 0xff, 'x'}
		got, err := Reply{Type: TypeBulk, Str: payload}.Bytes()
		if err != nil {
			t.Fatalf("Bytes: %v", err)
		}
		if string(got) != string(payload) {
			t.Fatalf("Bytes gave %q, want %q", got, payload)
		}
	})

	t.Run("a nil reply is not an error", func(t *testing.T) {
		got, err := Reply{Type: TypeNil}.Bytes()
		if err != nil || got != nil {
			t.Fatalf("Bytes on a nil reply gave (%q, %v)", got, err)
		}
	})

	t.Run("integers render as text", func(t *testing.T) {
		text, err := Reply{Type: TypeInteger, Int: -12}.Text()
		if err != nil || text != "-12" {
			t.Fatalf("Text gave (%q, %v)", text, err)
		}
	})

	t.Run("doubles keep the text the server sent", func(t *testing.T) {
		text, err := Reply{Type: TypeDouble, Float: 1.5, Str: []byte("1.5")}.Text()
		if err != nil || text != "1.5" {
			t.Fatalf("Text gave (%q, %v)", text, err)
		}
	})
}

func TestReplyNumbers(t *testing.T) {
	t.Run("a numeric bulk string reads as an integer", func(t *testing.T) {
		n, err := Reply{Type: TypeBulk, Str: []byte("42")}.Int64()
		if err != nil || n != 42 {
			t.Fatalf("Int64 gave (%d, %v)", n, err)
		}
	})

	t.Run("a non-numeric bulk string does not", func(t *testing.T) {
		_, err := Reply{Type: TypeBulk, Str: []byte("nope")}.Int64()
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("Int64 gave %v, want ErrProtocol", err)
		}
	})

	t.Run("doubles read as floats", func(t *testing.T) {
		f, err := Reply{Type: TypeDouble, Float: 1.5, Str: []byte("1.5")}.Float64()
		if err != nil || f != 1.5 {
			t.Fatalf("Float64 gave (%v, %v)", f, err)
		}
	})

	t.Run("integers read as floats", func(t *testing.T) {
		f, err := Reply{Type: TypeInteger, Int: 3}.Float64()
		if err != nil || f != 3 {
			t.Fatalf("Float64 gave (%v, %v)", f, err)
		}
	})

	t.Run("a non-numeric string is not a float", func(t *testing.T) {
		_, err := Reply{Type: TypeBulk, Str: []byte("nope")}.Float64()
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("Float64 gave %v, want ErrProtocol", err)
		}
	})
}

func TestReplyBooleans(t *testing.T) {
	t.Run("OK is true", func(t *testing.T) {
		ok, err := Reply{Type: TypeStatus, Str: []byte(statusOK)}.Bool()
		if err != nil || !ok {
			t.Fatalf("Bool gave (%v, %v)", ok, err)
		}
	})

	t.Run("a nil reply is false", func(t *testing.T) {
		ok, err := Reply{Type: TypeNil}.Bool()
		if err != nil || ok {
			t.Fatalf("Bool gave (%v, %v)", ok, err)
		}
	})

	t.Run("a non-zero integer is true", func(t *testing.T) {
		ok, err := Reply{Type: TypeInteger, Int: 1}.Bool()
		if err != nil || !ok {
			t.Fatalf("Bool gave (%v, %v)", ok, err)
		}
	})
}

func TestReplyArrays(t *testing.T) {
	t.Run("arrays convert to strings and to bytes", func(t *testing.T) {
		reply := Reply{Type: TypeArray, Arr: []Reply{
			{Type: TypeBulk, Str: []byte("a")},
			{Type: TypeBulk, Str: []byte("b")},
		}}

		strs, err := reply.Strings()
		if err != nil || len(strs) != 2 || strs[1] != "b" {
			t.Fatalf("Strings gave (%v, %v)", strs, err)
		}
		raw, err := reply.ByteSlices()
		if err != nil || len(raw) != 2 || string(raw[0]) != "a" {
			t.Fatalf("ByteSlices gave (%v, %v)", raw, err)
		}
	})

	t.Run("a nil reply is an empty slice", func(t *testing.T) {
		items, err := Reply{Type: TypeNil}.Slice()
		if err != nil || items != nil {
			t.Fatalf("Slice gave (%v, %v)", items, err)
		}
	})

	t.Run("an element of the wrong shape is refused", func(t *testing.T) {
		reply := Reply{Type: TypeArray, Arr: []Reply{{Type: TypeArray}}}
		if _, err := reply.Strings(); !errors.Is(err, ErrProtocol) {
			t.Fatalf("Strings gave %v, want ErrProtocol", err)
		}
		if _, err := reply.ByteSlices(); !errors.Is(err, ErrProtocol) {
			t.Fatalf("ByteSlices gave %v, want ErrProtocol", err)
		}
	})
}

// A RESP2 server flattens a map into an array and a RESP3 server sends a map.
// Map has to read both the same way, or every caller of STATS breaks the day
// the server starts accepting HELLO 3.
func TestReplyMapReadsBothEncodings(t *testing.T) {
	pairs := []Reply{
		{Type: TypeBulk, Str: []byte("keys")},
		{Type: TypeInteger, Int: 4},
	}

	for _, kind := range []ReplyType{TypeArray, TypeMap} {
		t.Run(kind.String(), func(t *testing.T) {
			got, err := Reply{Type: kind, Arr: pairs}.Map()
			if err != nil {
				t.Fatalf("Map: %v", err)
			}
			if got["keys"].Int != 4 {
				t.Fatalf("Map gave %+v", got)
			}
		})
	}
}

func TestReplyMapRejectsAnOddNumberOfElements(t *testing.T) {
	_, err := Reply{Type: TypeArray, Arr: []Reply{{Type: TypeBulk, Str: []byte("lonely")}}}.Map()
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("Map gave %v, want ErrProtocol", err)
	}
}

// An error reply converted rather than inspected must still surface as an
// error, and as the right category: a caller who calls Text on a -WRONGPASS
// should not be told it is a type mismatch.
func TestReplyConversionsSurfaceServerErrors(t *testing.T) {
	reply := errorReply("WRONGPASS invalid username-password pair")

	for name, convert := range map[string]func() error{
		"Bytes":      func() error { _, err := reply.Bytes(); return err },
		"Int64":      func() error { _, err := reply.Int64(); return err },
		"Bool":       func() error { _, err := reply.Bool(); return err },
		"Float64":    func() error { _, err := reply.Float64(); return err },
		"Slice":      func() error { _, err := reply.Slice(); return err },
		"ByteSlices": func() error { _, err := reply.ByteSlices(); return err },
	} {
		t.Run(name, func(t *testing.T) {
			err := convert()
			if !errors.Is(err, ErrAuth) {
				t.Fatalf("%s gave %v, want ErrAuth", name, err)
			}
		})
	}
}

func TestReplyConversionsRejectTheWrongShape(t *testing.T) {
	array := Reply{Type: TypeArray}

	for name, convert := range map[string]func() error{
		"Bytes":   func() error { _, err := array.Bytes(); return err },
		"Int64":   func() error { _, err := array.Int64(); return err },
		"Bool":    func() error { _, err := array.Bool(); return err },
		"Float64": func() error { _, err := array.Float64(); return err },
		"Slice":   func() error { _, err := Reply{Type: TypeInteger}.Slice(); return err },
	} {
		t.Run(name, func(t *testing.T) {
			err := convert()
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("%s gave %v, want ErrProtocol", name, err)
			}
		})
	}
}

func TestReplyTypeNames(t *testing.T) {
	for kind, want := range map[ReplyType]string{
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
	} {
		if got := kind.String(); got != want {
			t.Errorf("ReplyType(%d).String() = %q, want %q", kind, got, want)
		}
	}
	if got := ReplyType(200).String(); got != "unknown(200)" {
		t.Errorf("an unnamed type rendered as %q", got)
	}
}
