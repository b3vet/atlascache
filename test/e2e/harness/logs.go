package harness

import (
	"bytes"
	"fmt"
	"sync"
	"time"
)

// maxLogBytes caps what one harness keeps. A crash-looping server can produce
// output without bound, and a failure report only ever shows the tail.
const maxLogBytes = 4 << 20

// logBuffer collects the server's stdout and stderr, interleaved with the
// harness's own annotations, so a failure report reads as one story: what the
// harness did, and what the server said about it.
//
// It is written from the goroutine os/exec uses to drain the pipe and read from
// whichever goroutine reports a failure, so every access is locked.
type logBuffer struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	dropped int // bytes discarded from the front, to keep offsets meaningful
}

// Write appends server output. It never fails: losing the log to an error would
// leave a failure undiagnosable.
func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Write(p)
	b.trim()
	return len(p), nil
}

// Printf appends a harness annotation. The prefix distinguishes it from server
// output, so nobody hunts the server source for a line the harness wrote.
func (b *logBuffer) Printf(format string, args ...any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	fmt.Fprintf(&b.buf, "--- harness %s: %s\n", time.Now().Format("15:04:05.000"), fmt.Sprintf(format, args...))
	b.trim()
}

// String returns everything kept so far.
func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Offset marks the current end of the log, for a later Since.
func (b *logBuffer) Offset() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropped + b.buf.Len()
}

// Since returns everything written after the given Offset. It is how a failed
// start reads only its own attempt's output rather than the whole history.
func (b *logBuffer) Since(offset int) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	start := offset - b.dropped
	if start < 0 {
		start = 0
	}
	content := b.buf.Bytes()
	if start > len(content) {
		return ""
	}
	return string(content[start:])
}

// trim discards from the front once the buffer outgrows its cap. The caller
// holds the lock.
func (b *logBuffer) trim() {
	if b.buf.Len() <= maxLogBytes {
		return
	}
	excess := b.buf.Len() - maxLogBytes
	discarded := b.buf.Next(excess)
	b.dropped += len(discarded)
}
