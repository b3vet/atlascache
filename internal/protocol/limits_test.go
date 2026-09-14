package protocol

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ISSUE-0016: three allocation paths sized themselves from a number an
// unauthenticated client declared. Each test below fails on the shipped parser,
// and each asserts on what was allocated rather than on the error reply — an
// error returned after a 512MB allocation is still a 512MB allocation.

// connBufferSize mirrors the read buffer internal/server gives a connection, so
// the bounds these tests assert are the ones a real client meets.
const connBufferSize = 16 * 1024

// TestReadLineIsBoundedWhileItAccumulates. The shipped parser called
// ReadBytes('\n') and compared the result against the limit afterwards, so a
// client that sent no newline grew the buffer until the process died. The
// connection must be refused at the limit, having read about that much and
// allocated about that much, however many bytes the client is willing to send.
func TestReadLineIsBoundedWhileItAccumulates(t *testing.T) {
	const (
		// Far more than the limit, and enough that an unbounded parser is
		// unmistakable in both numbers below.
		flood      = 64 << 20
		iterations = 10
	)

	codec := NewRESP()
	limit := codec.Limits().MaxInlineLength

	var errs []error
	var read int
	allocated := measureAlloc(func() {
		for range iterations {
			source := &floodReader{fill: 'x', cap: flood}
			_, err := codec.Decode(bufio.NewReaderSize(source, connBufferSize))
			errs = append(errs, err)
			read = max(read, source.read)
		}
	})

	t.Logf("read at most %d bytes and allocated %d over %d floods of %d bytes each",
		read, allocated, iterations, flood)
	require.LessOrEqualf(t, read, limit+2*connBufferSize,
		"the parser read %d bytes of a line it may not accept past %d", read, limit)
	for _, err := range errs {
		var protoErr *ProtocolError
		require.True(t, errors.As(err, &protoErr), "expected a protocol error, got %v", err)
		require.Contains(t, protoErr.Error(), "too big inline request")
	}

	// The line is the only thing that may grow, and it stops at the limit.
	// Accumulating it doubles the buffer as it goes, so the bytes handed out
	// over one decode are at most twice the limit plus the read buffer; the
	// bound below leaves room and is still three orders of magnitude under the
	// flood a regression would read.
	perDecode := 4*limit + connBufferSize
	assert.LessOrEqualf(t, allocated, uint64(iterations*perDecode),
		"decoding %d unterminated lines allocated %d bytes, want under %d",
		iterations, allocated, iterations*perDecode)
}

// TestDeclaredBulkLengthAllocatesNothingWhenItIsRefused. maxBulkLength was
// 512MB while the engine refuses anything over max_value_size, so a 15-byte
// request bought a half-gigabyte allocation and then an error. The rejection
// must cost nothing.
func TestDeclaredBulkLengthAllocatesNothingWhenItIsRefused(t *testing.T) {
	const iterations = 100

	codec := NewRESP()
	request := fmt.Sprintf("*1\r\n$%d\r\n", 512*1024*1024)

	var errs []error
	allocated := measureAlloc(func() {
		for range iterations {
			_, err := codec.Decode(decoderFor(request))
			errs = append(errs, err)
		}
	})

	// Parsing the header allocates a few small strings and the bufio buffer.
	// The allocation this test exists to catch is 512MB each time round, which
	// the shipped parser made before deciding the length was too large.
	t.Logf("rejecting %d headers that each declared 512MB allocated %d bytes", iterations, allocated)
	assert.Lessf(t, allocated, uint64(iterations*32*1024),
		"rejecting %d oversized bulk headers allocated %d bytes", iterations, allocated)

	for _, err := range errs {
		var protoErr *ProtocolError
		require.True(t, errors.As(err, &protoErr), "expected a protocol error, got %v", err)
		require.Contains(t, protoErr.Error(), "invalid bulk length")
	}
}

// TestBulkLengthIsTiedToTheValueCeiling. The parser's ceiling has to track the
// largest value the engine will accept, or the two drift and the parser reads
// megabytes to reach a rejection the engine was always going to make.
func TestBulkLengthIsTiedToTheValueCeiling(t *testing.T) {
	assert.Equal(t, maxValueSizeCeiling+bulkMargin, DefaultLimits().MaxBulkLength,
		"the default ceiling is config's max_value_size ceiling plus protocol margin")

	configured := LimitsForValueSize(1024 * 1024)
	assert.Equal(t, 1024*1024+bulkMargin, configured.MaxBulkLength,
		"a configured max_value_size tightens the parser with it")

	assert.Equal(t, DefaultLimits(), LimitsForValueSize(0), "an unset value size falls back")
	assert.Equal(t, DefaultLimits(), LimitsForValueSize(-1), "a nonsensical value size falls back")
	assert.Equal(t, DefaultLimits(), LimitsForValueSize(1<<30), "a value size above the ceiling falls back")
}

// TestMultiBulkHeaderDoesNotPreallocateForItsDeclaredCount. make([][]byte, 0,
// count) turned a 12-byte header into ~24MB of slice headers before a single
// element had arrived.
func TestMultiBulkHeaderDoesNotPreallocateForItsDeclaredCount(t *testing.T) {
	const iterations = 50

	codec := NewRESP()
	header := fmt.Sprintf("*%d\r\n", codec.Limits().MaxMultiBulkLength)

	var errs []error
	allocated := measureAlloc(func() {
		for range iterations {
			// The header is accepted; the elements never arrive, which is the
			// cheapest way for a client to ask for the reservation.
			_, err := codec.Decode(decoderFor(header))
			errs = append(errs, err)
		}
	})

	// One clamped reservation is multiBulkPrealloc pointers, not a million —
	// 8MB of slice headers per request header before the fix.
	t.Logf("accepting %d million-element headers allocated %d bytes", iterations, allocated)
	assert.Lessf(t, allocated, uint64(iterations*32*1024),
		"accepting %d million-element headers allocated %d bytes", iterations, allocated)

	for _, err := range errs {
		require.ErrorIs(t, err, io.ErrUnexpectedEOF, "a header with no elements behind it is a truncated frame")
	}
}

// TestElementsAreReadIntoAGrowingSlice. Clamping the reservation must not cap
// what a well-behaved client may send: a request with more elements than the
// clamp still decodes, the slice having grown as the elements arrived.
func TestElementsAreReadIntoAGrowingSlice(t *testing.T) {
	const count = multiBulkPrealloc * 4

	var request strings.Builder
	fmt.Fprintf(&request, "*%d\r\n$3\r\nDEL\r\n", count)
	for i := 1; i < count; i++ {
		fmt.Fprintf(&request, "$%d\r\nk%d\r\n", len(fmt.Sprint(i))+1, i)
	}

	cmd, err := NewRESP().Decode(decoderFor(request.String()))
	require.NoError(t, err)
	assert.Equal(t, "DEL", cmd.Name)
	assert.Len(t, cmd.Args, count-1)
}

// TestLimitsAreEnforcedAtTheirBoundary pins each limit to the byte, against a
// codec small enough to test exhaustively.
func TestLimitsAreEnforcedAtTheirBoundary(t *testing.T) {
	codec := NewRESPWithLimits(Limits{MaxInlineLength: 32, MaxMultiBulkLength: 4, MaxBulkLength: 8})

	t.Run("a bulk string at the limit is accepted", func(t *testing.T) {
		cmd, err := codec.Decode(decoderFor("*2\r\n$3\r\nGET\r\n$8\r\n12345678\r\n"))
		require.NoError(t, err)
		assert.Equal(t, "12345678", string(cmd.Args[0]))
	})

	t.Run("one byte over is refused", func(t *testing.T) {
		_, err := codec.Decode(decoderFor("*2\r\n$3\r\nGET\r\n$9\r\n123456789\r\n"))
		assert.ErrorContains(t, err, "invalid bulk length")
	})

	t.Run("an element count at the limit is accepted", func(t *testing.T) {
		cmd, err := codec.Decode(decoderFor("*4\r\n$3\r\nDEL\r\n$1\r\na\r\n$1\r\nb\r\n$1\r\nc\r\n"))
		require.NoError(t, err)
		assert.Len(t, cmd.Args, 3)
	})

	t.Run("one element over is refused", func(t *testing.T) {
		_, err := codec.Decode(decoderFor("*5\r\n$3\r\nDEL\r\n"))
		assert.ErrorContains(t, err, "invalid multibulk length")
	})

	t.Run("an inline request at the limit is accepted", func(t *testing.T) {
		line := "PING " + strings.Repeat("a", 32-len("PING ")-2)
		cmd, err := codec.Decode(decoderFor(line + "\r\n"))
		require.NoError(t, err)
		assert.Equal(t, "PING", cmd.Name)
	})

	t.Run("one byte over is refused", func(t *testing.T) {
		line := "PING " + strings.Repeat("a", 32-len("PING ")-1)
		_, err := codec.Decode(decoderFor(line + "\r\n"))
		assert.ErrorContains(t, err, "too big inline request")
	})
}

// TestUnsetLimitsFallBackToTheDefaults. A partly filled Limits must not read as
// "no limit" on the fields it left alone — that is how a limit silently stops
// being enforced.
func TestUnsetLimitsFallBackToTheDefaults(t *testing.T) {
	codec := NewRESPWithLimits(Limits{MaxBulkLength: 16})

	assert.Equal(t, 16, codec.Limits().MaxBulkLength)
	assert.Equal(t, DefaultLimits().MaxInlineLength, codec.Limits().MaxInlineLength)
	assert.Equal(t, DefaultLimits().MaxMultiBulkLength, codec.Limits().MaxMultiBulkLength)

	assert.Equal(t, DefaultLimits(), NewRESPWithLimits(Limits{}).Limits())
	assert.Equal(t, DefaultLimits(), NewRESP().Limits())
}

// measureAlloc reports how many bytes the heap was asked for while fn ran. It
// is the instrument these tests need: a 512MB allocation is one allocation, so
// counting allocations would not see it, and it is freed by the time the call
// returns, so the live heap would not either.
func measureAlloc(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	fn()

	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// floodReader supplies the same byte forever, counting what was taken. cap is a
// safety net: without the fix the parser reads until something stops it, and a
// test that hangs or exhausts the machine reports nothing useful.
type floodReader struct {
	fill      byte
	cap       int
	read      int
	exhausted bool
}

func (f *floodReader) Read(p []byte) (int, error) {
	if f.read >= f.cap {
		f.exhausted = true
		return 0, io.ErrUnexpectedEOF
	}
	n := min(len(p), f.cap-f.read)
	for i := range n {
		p[i] = f.fill
	}
	f.read += n
	return n, nil
}
