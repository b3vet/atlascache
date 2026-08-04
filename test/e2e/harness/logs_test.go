package harness

import (
	"strings"
	"testing"
)

// TestLogBufferKeepsTheTailWhenAServerFloodsIt. A crash-looping server can print
// without bound, and the failure report only ever shows the tail — so the
// buffer must discard from the front. Discarding from the back instead, or not
// discarding at all, would either throw away the output that explains the
// failure or let one spec's log exhaust the run.
func TestLogBufferKeepsTheTailWhenAServerFloodsIt(t *testing.T) {
	t.Parallel()

	var buf logBuffer
	buf.Printf("the oldest annotation")

	chunk := []byte(strings.Repeat("x", 64<<10) + "\n")
	for written := 0; written <= maxLogBytes; written += len(chunk) {
		n, err := buf.Write(chunk)
		if n != len(chunk) || err != nil {
			t.Fatalf("Write = %d, %v; a log write must never fail or short-write", n, err)
		}
	}
	buf.Printf("the newest annotation")

	kept := buf.String()
	if len(kept) > maxLogBytes {
		t.Errorf("the buffer kept %d bytes, over the %d byte cap", len(kept), maxLogBytes)
	}
	if strings.Contains(kept, "the oldest annotation") {
		t.Error("the oldest output survived the flood; the buffer must discard from the front")
	}
	if !strings.Contains(kept, "the newest annotation") {
		t.Error("the newest output was discarded; the tail is the part that explains a failure")
	}
}

// TestSinceReadsOnlyOneStartAttemptEvenAfterADiscard. Start marks the log,
// launches, and reads back only what that attempt produced — which is how it
// tells "this launch lost the race for its port" from a message an earlier
// attempt left behind. Offsets therefore have to survive the buffer discarding
// its front, and an offset that has been discarded must widen to what is left
// rather than slice out of range.
func TestSinceReadsOnlyOneStartAttemptEvenAfterADiscard(t *testing.T) {
	t.Parallel()

	var buf logBuffer
	buf.Printf("first attempt: address already in use")
	mark := buf.Offset()
	buf.Printf("second attempt: listening")

	since := buf.Since(mark)
	if strings.Contains(since, "first attempt") {
		t.Errorf("Since(mark) reached back before the mark:\n%s", since)
	}
	if !strings.Contains(since, "second attempt") {
		t.Errorf("Since(mark) lost what was written after it:\n%s", since)
	}

	// Push the mark off the front of the buffer.
	chunk := []byte(strings.Repeat("y", 64<<10) + "\n")
	for written := 0; written <= maxLogBytes; written += len(chunk) {
		if _, err := buf.Write(chunk); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if got, want := len(buf.Since(mark)), len(buf.String()); got != want {
		t.Errorf("Since a discarded offset returned %d bytes, want everything still held (%d)", got, want)
	}
}

// TestSinceAnOffsetPastTheEndIsEmpty. Nothing written since the mark must read
// as nothing, not as a panic: this is the ordinary case for a launch that died
// without printing anything at all.
func TestSinceAnOffsetPastTheEndIsEmpty(t *testing.T) {
	t.Parallel()

	var buf logBuffer
	buf.Printf("launching")

	if got := buf.Since(buf.Offset()); got != "" {
		t.Errorf("Since(Offset()) = %q, want nothing written since the mark", got)
	}
	if got := buf.Since(buf.Offset() + 4096); got != "" {
		t.Errorf("Since an offset past the end = %q, want empty", got)
	}
}
