package runner

import (
	"strings"
	"testing"
)

func expectStep(text string) Step { return Step{Cmd: "X", Expect: Value{Text: text, Set: true}} }

func expectErrorStep(text string) Step {
	return Step{Cmd: "X", ExpectError: Value{Text: text, Set: true}}
}

func expectFieldStep(fields map[string]string) Step {
	values := make(map[string]Value, len(fields))
	for name, expr := range fields {
		values[name] = Value{Text: expr, Set: true}
	}
	return Step{Cmd: "X", ExpectField: values}
}

func TestAssertExpect(t *testing.T) {
	tests := []struct {
		name  string
		want  string
		reply Reply
		ok    bool
	}{
		{"status matches", "PONG", StatusReply("PONG"), true},
		{"status differs", "PONG", StatusReply("PANG"), false},
		{"bulk matches", "hello", BulkReply("hello"), true},
		{"nil reads as NIL", "NIL", NilReply(), true},
		{"nil against a value fails", "hello", NilReply(), false},
		{"integer compares as text", "7", IntegerReply(7), true},
		{"array renders bracketed", "[a b]", ArrayReply(BulkReply("a"), BulkReply("b")), true},
		{"error reply fails expect", "PONG", ErrorReply("ERR nope"), false},
		{"empty string is a real expectation", "", BulkReply(""), true},
		{"invalid reply never matches", "", Reply{}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := assertReply(expectStep(tc.want), tc.reply)
			if got.ok != tc.ok {
				t.Fatalf("ok = %v, want %v (message %q, actual %q)", got.ok, tc.ok, got.message, got.actual)
			}
			if !got.ok && got.expected != tc.want {
				t.Errorf("expected = %q, want %q", got.expected, tc.want)
			}
		})
	}
}

func TestAssertExpectReportsErrorReplies(t *testing.T) {
	got := assertReply(expectStep("PONG"), ErrorReply("ERR unknown command"))
	if got.ok {
		t.Fatal("an error reply must not satisfy expect")
	}
	if !strings.Contains(got.actual, "ERR unknown command") {
		t.Errorf("actual = %q, should carry the error text", got.actual)
	}
}

func TestAssertExpectError(t *testing.T) {
	tests := []struct {
		name  string
		want  string
		reply Reply
		ok    bool
	}{
		{"matches", "ERR no such key", ErrorReply("ERR no such key"), true},
		{"differs", "ERR no such key", ErrorReply("ERR wrong type"), false},
		{"exact, not substring", "ERR no such key", ErrorReply("ERR no such key 'k1'"), false},
		{"non-error reply fails", "ERR no such key", StatusReply("OK"), false},
		{"nil reply fails", "ERR no such key", NilReply(), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := assertReply(expectErrorStep(tc.want), tc.reply); got.ok != tc.ok {
				t.Fatalf("ok = %v, want %v (message %q)", got.ok, tc.ok, got.message)
			}
		})
	}
}

func TestComparators(t *testing.T) {
	tests := []struct {
		expr   string
		actual string
		ok     bool
	}{
		{"3", "3", true},
		{"3", "4", false},
		{"== 3", "3", true},
		{"== 3", "3.0", true},
		{"== ok", "ok", true},
		{"== ok", "nope", false},
		{"!= 0", "1", true},
		{"!= 0", "0", false},
		{"!= none", "lru", true},
		{">= 1", "1", true},
		{">= 1", "2", true},
		{">= 1", "0", false},
		{"<= 10", "10", true},
		{"<= 10", "11", false},
		{"> 1", "2", true},
		{"> 1", "1", false},
		{"< 5", "4", true},
		{"< 5", "5", false},
		{">=1", "1", true},
		{"  >=   1  ", "1", true},
	}

	for _, tc := range tests {
		t.Run(tc.expr+" vs "+tc.actual, func(t *testing.T) {
			cmp, err := parseComparison(tc.expr)
			if err != nil {
				t.Fatalf("parseComparison(%q): %v", tc.expr, err)
			}
			got, err := cmp.match(tc.actual)
			if err != nil {
				t.Fatalf("match: %v", err)
			}
			if got != tc.ok {
				t.Errorf("%q vs %q = %v, want %v", tc.expr, tc.actual, got, tc.ok)
			}
		})
	}
}

func TestComparatorsAreMatchedLongestFirst(t *testing.T) {
	cmp, err := parseComparison(">= 1")
	if err != nil {
		t.Fatal(err)
	}
	if cmp.op != ">=" || cmp.operand != "1" {
		t.Fatalf("parsed %q %q, want >= and 1", cmp.op, cmp.operand)
	}
}

func TestOrderingComparatorsNeedNumbers(t *testing.T) {
	cmp, err := parseComparison(">= 1")
	if err != nil {
		t.Fatal(err)
	}
	if _, matchErr := cmp.match("lots"); matchErr == nil {
		t.Fatal("comparing a non-numeric reply value should fail")
	}

	cmp, err = parseComparison(">= many")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cmp.match("3"); err == nil {
		t.Fatal("comparing against a non-numeric operand should fail")
	}
}

func TestParseComparisonRejectsEmptyOperand(t *testing.T) {
	for _, expr := range []string{">=", "<=", "!=", "==", ">", "<", ">=   "} {
		if _, err := parseComparison(expr); err == nil {
			t.Errorf("parseComparison(%q) should fail", expr)
		}
	}
}

func TestAssertFields(t *testing.T) {
	reply := MapReply(map[string]Reply{
		"expirations": IntegerReply(3),
		"keys":        IntegerReply(2),
		"policy":      BulkReply("lru"),
	})

	if got := assertReply(expectFieldStep(map[string]string{
		"expirations": ">= 1", "keys": "== 2", "policy": "lru",
	}), reply); !got.ok {
		t.Fatalf("expected a pass, got %q (expected %q, actual %q)", got.message, got.expected, got.actual)
	}

	got := assertReply(expectFieldStep(map[string]string{"expirations": ">= 99"}), reply)
	if got.ok {
		t.Fatal("expirations >= 99 should fail against 3")
	}
	if got.expected != "expirations >= 99" || got.actual != "expirations = 3" {
		t.Errorf("expected = %q, actual = %q", got.expected, got.actual)
	}
}

func TestAssertFieldsReportsMissingField(t *testing.T) {
	reply := MapReply(map[string]Reply{"keys": IntegerReply(2)})
	got := assertReply(expectFieldStep(map[string]string{"expirations": ">= 1"}), reply)
	if got.ok {
		t.Fatal("a missing field must fail")
	}
	if !strings.Contains(got.message, `field "expirations" is not in the reply`) {
		t.Errorf("message = %q", got.message)
	}
	if !strings.Contains(got.message, "fields present: keys") {
		t.Errorf("message should list the fields that are present, got %q", got.message)
	}
}

func TestAssertFieldsOnErrorReply(t *testing.T) {
	got := assertReply(expectFieldStep(map[string]string{"keys": ">= 1"}), ErrorReply("ERR nope"))
	if got.ok {
		t.Fatal("an error reply must fail expect_field")
	}
	if !strings.Contains(got.actual, "ERR nope") {
		t.Errorf("actual = %q", got.actual)
	}
}

func TestReplyFields(t *testing.T) {
	tests := []struct {
		name  string
		reply Reply
		want  map[string]string
	}{
		{
			"map reply",
			MapReply(map[string]Reply{"keys": IntegerReply(2)}),
			map[string]string{"keys": "2"},
		},
		{
			"array of pairs",
			ArrayReply(BulkReply("keys"), IntegerReply(2), BulkReply("hits"), IntegerReply(9)),
			map[string]string{"keys": "2", "hits": "9"},
		},
		{
			"odd array yields nothing",
			ArrayReply(BulkReply("keys")),
			map[string]string{},
		},
		{
			"colon separated lines",
			BulkReply("# server\nkeys:2\nhits: 9\n\n"),
			map[string]string{"keys": "2", "hits": "9"},
		},
		{
			"equals separated lines",
			BulkReply("keys=2"),
			map[string]string{"keys": "2"},
		},
		{
			"integer reply has no fields",
			IntegerReply(3),
			map[string]string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.reply.Fields()
			if len(got) != len(tc.want) {
				t.Fatalf("fields = %#v, want %#v", got, tc.want)
			}
			for key, want := range tc.want {
				if got[key] != want {
					t.Errorf("field %q = %q, want %q", key, got[key], want)
				}
			}
		})
	}
}

func TestReplyString(t *testing.T) {
	tests := []struct {
		reply Reply
		want  string
	}{
		{StatusReply("OK"), "OK"},
		{BulkReply("hello"), "hello"},
		{IntegerReply(-3), "-3"},
		{NilReply(), "NIL"},
		{ErrorReply("ERR nope"), "ERR nope"},
		{ArrayReply(BulkReply("a"), IntegerReply(1)), "[a 1]"},
		{MapReply(map[string]Reply{"b": IntegerReply(2), "a": IntegerReply(1)}), "{a=1 b=2}"},
		{Reply{}, "<invalid reply>"},
	}
	for _, tc := range tests {
		if got := tc.reply.String(); got != tc.want {
			t.Errorf("String() = %q, want %q", got, tc.want)
		}
	}
}

func TestTailLines(t *testing.T) {
	log := "one\ntwo\nthree\nfour\n"
	if got := tailLines(log, 2); got != "three\nfour" {
		t.Errorf("tailLines = %q", got)
	}
	if got := tailLines(log, 99); got != "one\ntwo\nthree\nfour" {
		t.Errorf("tailLines = %q", got)
	}
	if got := tailLines(log, 0); got != "" {
		t.Errorf("tailLines = %q, want empty", got)
	}
	if got := tailLines("", 5); got != "" {
		t.Errorf("tailLines = %q, want empty", got)
	}
}
