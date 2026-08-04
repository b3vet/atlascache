package runner

import (
	"fmt"
	"strconv"
	"strings"
)

// comparison is one expect_field expectation: an operator and its operand.
// An empty operator means exact string equality, which is the default.
type comparison struct {
	op      string
	operand string
}

// comparators are matched longest-first so that >= is never read as >.
var comparators = []string{">=", "<=", "!=", "==", ">", "<"}

func parseComparison(expr string) (comparison, error) {
	trimmed := strings.TrimSpace(expr)
	for _, op := range comparators {
		if !strings.HasPrefix(trimmed, op) {
			continue
		}
		operand := strings.TrimSpace(trimmed[len(op):])
		if operand == "" {
			return comparison{}, fmt.Errorf("%s needs a value to compare against", op)
		}
		return comparison{op: op, operand: operand}, nil
	}
	return comparison{operand: trimmed}, nil
}

// describe renders the comparison the way failure output should show it.
func (c comparison) describe() string {
	if c.op == "" {
		return c.operand
	}
	return c.op + " " + c.operand
}

// match compares an actual field value against the expectation. Ordering
// comparators require both sides to be numeric; == and != fall back to string
// equality when either side is not.
func (c comparison) match(actual string) (bool, error) {
	switch c.op {
	case "":
		return actual == c.operand, nil
	case "==", "!=":
		equal, ok := numericEqual(actual, c.operand)
		if !ok {
			equal = actual == c.operand
		}
		if c.op == "!=" {
			return !equal, nil
		}
		return equal, nil
	}

	got, err := strconv.ParseFloat(actual, 64)
	if err != nil {
		return false, fmt.Errorf("%s needs a number, but the reply held %q", c.op, actual)
	}
	want, err := strconv.ParseFloat(c.operand, 64)
	if err != nil {
		return false, fmt.Errorf("%s needs a number, but the spec wrote %q", c.op, c.operand)
	}

	switch c.op {
	case ">=":
		return got >= want, nil
	case "<=":
		return got <= want, nil
	case ">":
		return got > want, nil
	case "<":
		return got < want, nil
	default:
		return false, fmt.Errorf("unknown comparator %q", c.op)
	}
}

func numericEqual(a, b string) (equal, ok bool) {
	x, err := strconv.ParseFloat(a, 64)
	if err != nil {
		return false, false
	}
	y, err := strconv.ParseFloat(b, 64)
	if err != nil {
		return false, false
	}
	return x == y, true
}

// assertion is the outcome of checking one step's assertion against a reply.
type assertion struct {
	ok       bool
	message  string
	expected string
	actual   string
}

func assertionPassed() assertion { return assertion{ok: true} }

func assertionFailed(message, expected, actual string) assertion {
	return assertion{message: message, expected: expected, actual: actual}
}

// assertReply checks a command step's assertion against the reply it received.
func assertReply(step Step, reply Reply) assertion {
	switch {
	case step.Expect.Set:
		if reply.Kind == KindError {
			return assertionFailed("the server returned an error", step.Expect.Text, "error: "+reply.Text)
		}
		if got := reply.String(); got != step.Expect.Text {
			return assertionFailed("reply did not match expect", step.Expect.Text, got)
		}
		return assertionPassed()

	case step.ExpectError.Set:
		if reply.Kind != KindError {
			return assertionFailed("expected an error reply", "error: "+step.ExpectError.Text,
				reply.Kind.String()+": "+reply.String())
		}
		if reply.Text != step.ExpectError.Text {
			return assertionFailed("error message did not match expect_error", step.ExpectError.Text, reply.Text)
		}
		return assertionPassed()

	case len(step.ExpectField) > 0:
		return assertFields(step.ExpectField, reply)

	default:
		return assertionFailed("step has no assertion", "", reply.String())
	}
}

func assertFields(want map[string]Value, reply Reply) assertion {
	if reply.Kind == KindError {
		return assertionFailed("the server returned an error", describeExpectFields(want), "error: "+reply.Text)
	}
	fields := reply.Fields()
	for _, name := range sortedValueKeys(want) {
		expr := want[name].Text
		cmp, err := parseComparison(expr)
		if err != nil {
			return assertionFailed(fmt.Sprintf("expect_field %s: %s", name, err), expr, "")
		}
		actual, present := fields[name]
		if !present {
			return assertionFailed(
				fmt.Sprintf("field %q is not in the reply (fields present: %s)", name, describeFields(fields)),
				name+" "+cmp.describe(), reply.String())
		}
		matched, err := cmp.match(actual)
		if err != nil {
			return assertionFailed(fmt.Sprintf("expect_field %s: %s", name, err), name+" "+cmp.describe(), name+" = "+actual)
		}
		if !matched {
			return assertionFailed("field did not match expect_field", name+" "+cmp.describe(), name+" = "+actual)
		}
	}
	return assertionPassed()
}

func describeExpectFields(want map[string]Value) string {
	names := sortedValueKeys(want)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		cmp, err := parseComparison(want[name].Text)
		if err != nil {
			parts = append(parts, name+" "+want[name].Text)
			continue
		}
		parts = append(parts, name+" "+cmp.describe())
	}
	return strings.Join(parts, ", ")
}
