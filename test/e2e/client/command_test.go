package client_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/b3vet/atlascache/test/e2e/client"
)

func TestParseCommand(t *testing.T) {
	tests := []struct {
		name string
		line string
		want []string
	}{
		{"one word", "PING", []string{"PING"}},
		{"several words", "SET k1 hello", []string{"SET", "k1", "hello"}},
		{"extra spacing", "  SET   k1\thello  ", []string{"SET", "k1", "hello"}},
		{"quoted value", `SET k1 "hello world"`, []string{"SET", "k1", "hello world"}},
		{"empty argument", `SET k1 ""`, []string{"SET", "k1", ""}},
		{"escapes", `SET k1 "a\r\nb\tc"`, []string{"SET", "k1", "a\r\nb\tc"}},
		{"hex escape", `SET k1 "\x41\x00"`, []string{"SET", "k1", "A\x00"}},
		{"escaped quote", `SET k1 "say \"hi\""`, []string{"SET", "k1", `say "hi"`}},
		{"escaped backslash", `SET k1 "a\\b"`, []string{"SET", "k1", `a\b`}},
		{"single quotes keep backslashes", `SET k1 'a\b'`, []string{"SET", "k1", `a\b`}},
		{"single quotes escape the quote", `SET k1 'it\'s'`, []string{"SET", "k1", "it's"}},
		{"quoted command name", `"PING"`, []string{"PING"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := client.ParseCommand(tc.line)
			if err != nil {
				t.Fatalf("ParseCommand(%q): %v", tc.line, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseCommand(%q) = %q, want %q", tc.line, got, tc.want)
			}
		})
	}
}

func TestParseCommandRejects(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
	}{
		{"empty", "   ", "is empty"},
		{"unclosed double quote", `SET k1 "hello`, "never closed"},
		{"unclosed single quote", `SET k1 'hello`, "never closed"},
		{"quote inside a word", `SET k1 he"llo"`, "must open an argument"},
		{"no space after the closing quote", `SET "k1"x`, "followed by a space"},
		{"dangling backslash", `SET k1 "a\`, "nothing to escape"},
		{"short hex escape", `SET k1 "\x4"`, "two hex digits"},
		{"bad hex escape", `SET k1 "\xzz"`, "hex digits"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.ParseCommand(tc.line)
			if err == nil {
				t.Fatalf("ParseCommand(%q) was accepted", tc.line)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}
