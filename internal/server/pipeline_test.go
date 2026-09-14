package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/b3vet/atlascache/internal/protocol"
)

// Pipelining, measured on what it costs rather than on what it returns
// (FEAT-0024, ADR-0021).
//
// A server that answers a thousand pipelined commands correctly, one syscall at
// a time, passes every assertion about its replies and none of the ones that
// matter: the whole point of pipelining is that N commands cost one read and
// one write instead of N of each. So these tests count the syscalls, which
// means owning the net.Conn under the connection — hence the handler being
// driven directly rather than through a listener.

// countingConn records how many reads and writes a connection really made.
//
// Reads are counted only when they returned data. The loop always has one more
// read outstanding than it has consumed — the blocking one it is sitting in
// waiting for the next batch — and counting that would make every measurement
// off by one for a reason that has nothing to do with batching.
type countingConn struct {
	net.Conn
	reads  atomic.Int64
	writes atomic.Int64
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.reads.Add(1)
	}
	return n, err
}

func (c *countingConn) Write(p []byte) (int, error) {
	c.writes.Add(1)
	return c.Conn.Write(p)
}

// newHandlerServer builds a server with no listener under it, for the tests
// that drive Handle against a connection they own.
func newHandlerServer(store Store, limits ConnLimits) *Server {
	limits = limits.normalize()
	return &Server{
		codec:  protocol.NewRESPWithLimits(limits.codecLimits(store.MaxValueSize())),
		store:  store,
		addr:   "127.0.0.1:0",
		log:    zerolog.Nop(),
		auth:   NewAuthenticator(false, ""),
		limits: limits,
	}
}

// served is one connection being handled, plus the counters under it.
type served struct {
	client  net.Conn
	reader  *bufio.Reader
	counted *countingConn
	done    chan struct{}
}

// serve runs Handle over a synchronous in-memory pipe.
//
// net.Pipe rather than a socket, because the exact syscall counts are the
// point: a kernel is free to split a write into any number of segments, and a
// test that asserted "one read" over TCP would be asserting something about the
// loopback MTU. The TCP case is measured separately, with a bound rather than
// an equality.
func serve(t *testing.T, srv *Server, limits ConnLimits) *served {
	t.Helper()

	client, server := net.Pipe()
	return serveOver(t, srv, limits, client, server)
}

func serveOver(t *testing.T, srv *Server, limits ConnLimits, client, server net.Conn) *served {
	t.Helper()

	counted := &countingConn{Conn: server}
	conn := newNetConn(counted, limits.normalize())
	conn.armRead()

	s := &served{client: client, reader: bufio.NewReader(client), counted: counted, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		defer func() { _ = conn.Close() }()
		srv.Handle(context.Background(), conn)
	}()

	t.Cleanup(func() {
		_ = client.Close()
		select {
		case <-s.done:
		case <-time.After(5 * time.Second):
			t.Error("the connection handler did not return")
		}
	})
	return s
}

// tcpPair returns the two ends of a real loopback connection.
func tcpPair(t *testing.T) (client, server net.Conn) {
	t.Helper()

	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
		close(accepted)
	}()

	var dialer net.Dialer
	client, err = dialer.DialContext(context.Background(), "tcp", ln.Addr().String())
	require.NoError(t, err)
	server = <-accepted
	require.NotNil(t, server)

	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	return client, server
}

// writeAll sends a request stream without waiting for replies, which is what a
// pipelining client does.
func (s *served) writeAll(t *testing.T, payload string) {
	t.Helper()

	written := make(chan error, 1)
	go func() {
		_, err := s.client.Write([]byte(payload))
		written <- err
	}()
	t.Cleanup(func() {
		select {
		case err := <-written:
			if err != nil && !strings.Contains(err.Error(), "closed") && !strings.Contains(err.Error(), "broken pipe") {
				t.Logf("the client's write ended with %v", err)
			}
		default:
		}
	})
}

// readLines reads n reply lines under a deadline.
func (s *served) readLines(t *testing.T, n int) []string {
	t.Helper()

	require.NoError(t, s.client.SetReadDeadline(time.Now().Add(5*time.Second)))
	lines := make([]string, 0, n)
	for range n {
		line, err := s.reader.ReadString('\n')
		require.NoErrorf(t, err, "after %d of %d replies", len(lines), n)
		lines = append(lines, line)
	}
	return lines
}

// TestPipelinedCommandsAnswerInOrderInOneBatch is the acceptance criterion, with
// the syscall count attached: a thousand commands, one read, one write, and the
// replies in the order they were asked for.
func TestPipelinedCommandsAnswerInOrderInOneBatch(t *testing.T) {
	const commands = 1000

	srv := newHandlerServer(newFakeStore(), ConnLimits{})
	s := serve(t, srv, ConnLimits{})

	// ECHO with the index in it, so an out-of-order reply is caught rather than
	// hidden behind a thousand identical PONGs. The inline form keeps the whole
	// batch inside one read buffer, and exercises the bare LF terminator
	// ISSUE-0019 added while it is there.
	var requests strings.Builder
	for i := range commands {
		fmt.Fprintf(&requests, "ECHO %d\n", i)
	}
	require.Less(t, requests.Len(), connBufferSize, "the batch must fit one read buffer for the counts to mean anything")

	s.writeAll(t, requests.String())

	// Two lines per bulk reply: the length header and the payload.
	lines := s.readLines(t, 2*commands)
	for i := range commands {
		assert.Equal(t, fmt.Sprintf("%d\r\n", i), lines[2*i+1], "reply %d is out of order", i)
	}

	assert.Equal(t, int64(1), s.counted.reads.Load(), "a pipeline batch must cost one read")
	assert.Equal(t, int64(1), s.counted.writes.Load(), "a pipeline batch must cost one write")
}

// TestPipeliningCostsFarFewerSyscallsOverTCP is the same property over a real
// socket, where the kernel decides how the request stream is segmented. The
// claim is weaker and the instrument is honest: whatever the segmentation, a
// thousand commands must not cost a thousand writes.
func TestPipeliningCostsFarFewerSyscallsOverTCP(t *testing.T) {
	const commands = 1000

	srv := newHandlerServer(newFakeStore(), ConnLimits{})
	client, server := tcpPair(t)
	s := serveOver(t, srv, ConnLimits{}, client, server)

	s.writeAll(t, strings.Repeat("*1\r\n$4\r\nPING\r\n", commands))

	lines := s.readLines(t, commands)
	for _, line := range lines {
		require.Equal(t, "+PONG\r\n", line)
	}

	reads, writes := s.counted.reads.Load(), s.counted.writes.Load()
	t.Logf("%d pipelined commands cost %d reads and %d writes", commands, reads, writes)
	assert.Lessf(t, writes, int64(commands/16), "%d commands cost %d writes", commands, writes)
	assert.Lessf(t, reads, int64(commands/16), "%d commands cost %d reads", commands, reads)
}

// TestAPipeliningClientThatWaitsIsNotStalled is the reason the read buffer is
// scanned for a complete request instead of merely being checked for bytes.
//
// The client sends two complete commands and the first two bytes of a third,
// then waits for its two replies — which is what a partial TCP segment looks
// like from the server's side. A loop that kept decoding while anything was
// buffered would block inside the third command with the first two replies
// still in its buffer, and the two sides would wait for each other.
func TestAPipeliningClientThatWaitsIsNotStalled(t *testing.T) {
	srv := newHandlerServer(newFakeStore(), ConnLimits{})
	s := serve(t, srv, ConnLimits{})

	s.writeAll(t, "PING\r\nPING\r\nPI")

	lines := s.readLines(t, 2)
	assert.Equal(t, []string{"+PONG\r\n", "+PONG\r\n"}, lines)
}

// TestAPartialFrameIsRetainedNotMisparsed. The rest of the frame arrives later
// and the command it completes must be the one the client sent, not a
// resynchronization guess.
func TestAPartialFrameIsRetainedNotMisparsed(t *testing.T) {
	srv := newHandlerServer(newFakeStore(), ConnLimits{})
	s := serve(t, srv, ConnLimits{})

	// A complete PING, then a SET cut in the middle of its key.
	s.writeAll(t, "*1\r\n$4\r\nPING\r\n*3\r\n$3\r\nSET\r\n$5\r\nsplit")
	assert.Equal(t, []string{"+PONG\r\n"}, s.readLines(t, 1))

	// The tail completes the SET rather than being read as a fresh request.
	s.writeAll(t, "\r\n$5\r\nvalue\r\n*2\r\n$3\r\nGET\r\n$5\r\nsplit\r\n")
	assert.Equal(t, []string{"+OK\r\n", "$5\r\n", "value\r\n"}, s.readLines(t, 3))
}

// TestBatchIsCappedByCommandCount. Ten commands under a cap of four are three
// batches, so the reply buffer a client can build by pipelining is bounded by
// something the server chose rather than by how much the client sent.
func TestBatchIsCappedByCommandCount(t *testing.T) {
	limits := ConnLimits{MaxPipelineCommands: 4}

	srv := newHandlerServer(newFakeStore(), limits)
	s := serve(t, srv, limits)

	s.writeAll(t, strings.Repeat("*1\r\n$4\r\nPING\r\n", 10))
	require.Len(t, s.readLines(t, 10), 10)

	assert.Equal(t, int64(3), s.counted.writes.Load(),
		"ten commands under a cap of four are batches of 4, 4 and 2")
}

// TestBatchIsCappedByByteSize. The command count is not enough on its own: four
// commands whose replies are ten kilobytes each would otherwise buffer forty
// kilobytes before the first byte went out.
func TestBatchIsCappedByByteSize(t *testing.T) {
	store := newFakeStore()
	// Sized so one reply is comfortably under the flush threshold and two are
	// comfortably over it, which makes the batch boundary exact.
	const keys = 200
	for i := range keys {
		require.NoError(t, store.Set([]byte(fmt.Sprintf("bulky:%s:%03d", strings.Repeat("k", 32), i)), []byte("v"), 0))
	}

	srv := newHandlerServer(store, ConnLimits{})
	s := serve(t, srv, ConnLimits{})

	reply := replyBytes(t, srv, store)
	require.Greater(t, reply, pipelineFlushBytes/2, "one reply must be over half the flush threshold")
	require.Less(t, reply, pipelineFlushBytes, "one reply must be under the flush threshold")

	s.writeAll(t, strings.Repeat("*2\r\n$4\r\nKEYS\r\n$1\r\n*\r\n", 4))

	// Drain concurrently: the pipe is synchronous, so the server cannot finish
	// a batch the client is not reading.
	read := make(chan int, 1)
	go func() {
		n, copyErr := io.CopyN(io.Discard, s.reader, int64(4*reply))
		if copyErr != nil {
			read <- -1
			return
		}
		read <- int(n)
	}()
	require.NoError(t, s.client.SetReadDeadline(time.Now().Add(5*time.Second)))
	select {
	case n := <-read:
		require.Equal(t, 4*reply, n)
	case <-time.After(5 * time.Second):
		t.Fatal("the replies never arrived")
	}

	assert.Equal(t, int64(2), s.counted.writes.Load(),
		"four replies of %d bytes under a %d byte flush threshold are two batches", reply, pipelineFlushBytes)
}

// replyBytes measures one encoded KEYS reply, so the test above asserts against
// the real encoding rather than an estimate of it.
func replyBytes(t *testing.T, srv *Server, store Store) int {
	t.Helper()

	var out outputBuffer
	require.NoError(t, out.append(srv.codec, protocol.Array(bulkKeys(store.Keys("*")))))
	return out.Len()
}

func bulkKeys(keys [][]byte) []protocol.Reply {
	replies := make([]protocol.Reply, 0, len(keys))
	for _, key := range keys {
		replies = append(replies, protocol.BulkString(key))
	}
	return replies
}

// TestOutputLimitDisconnectsAClientThatWillNotRead is the cap moved in from P6.
//
// KEYS on a large keyspace builds a reply whose size the client chose and the
// server pays for. The connection is closed rather than buffered for, and the
// reason is recorded so the disconnect is attributable.
func TestOutputLimitDisconnectsAClientThatWillNotRead(t *testing.T) {
	store := newFakeStore()
	for i := range 4000 {
		require.NoError(t, store.Set([]byte(fmt.Sprintf("out:%s:%05d", strings.Repeat("k", 32), i)), []byte("v"), 0))
	}

	limits := ConnLimits{MaxOutputBytes: 64 << 10}
	srv := newHandlerServer(store, limits)
	s := serve(t, srv, limits)

	s.writeAll(t, "*2\r\n$4\r\nKEYS\r\n$1\r\n*\r\n")

	// The client never reads. The server must hang up rather than hold the
	// reply, so the read below ends rather than waiting for a reply that is
	// over the limit.
	require.NoError(t, s.client.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, err := s.reader.ReadByte()
	require.Error(t, err, "the server must close rather than deliver a reply over the output limit")

	assert.Equal(t, uint64(1), srv.conns.outputClosed.Load(), "the disconnect must be attributable")
}

// TestOutputBufferGivesBackWhatOneLargeReplyGrewIt. Without this a connection
// that served one big reply would hold a buffer the size of it for as long as
// the client stayed connected, which at max_connections is the difference
// between a bounded server and an unbounded one.
func TestOutputBufferGivesBackWhatOneLargeReplyGrewIt(t *testing.T) {
	out := newOutputBuffer(0)

	_, err := out.Write(make([]byte, 4*pipelineFlushBytes))
	require.NoError(t, err)
	require.Greater(t, cap(out.buf), pipelineFlushBytes)

	out.reset()
	assert.LessOrEqual(t, cap(out.buf), pipelineFlushBytes)
	assert.Zero(t, out.Len())
	assert.Zero(t, out.commands)
}

// TestRequestBufferedOnlyClaimsWhatCannotBlock pins the one-sidedness the whole
// batching loop depends on.
//
// Reporting "no request" when there was one costs a flush. Reporting "a
// request" when there was not deadlocks a client that pipelined and waited, so
// every case that is not certainly complete must come back false — including
// the ones a decoder would skip rather than refuse.
func TestRequestBufferedOnlyClaimsWhatCannotBlock(t *testing.T) {
	complete := map[string]string{
		"an inline command":                "PING\r\n",
		"an inline command with a bare LF": "PING\n",
		"a multibulk command":              "*1\r\n$4\r\nPING\r\n",
		"a command with arguments":         "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n",
		"an empty argument":                "*2\r\n$4\r\nECHO\r\n$0\r\n\r\n",
		"an argument holding a CRLF":       "*2\r\n$4\r\nECHO\r\n$4\r\na\r\nb\r\n",
		"a blank line before a command":    "\r\n*1\r\n$4\r\nPING\r\n",
		"an empty request before one":      "*0\r\n*1\r\n$4\r\nPING\r\n",
		"a malformed multibulk header":     "*abc\r\n",
		"a negative element count":         "*-3\r\n",
		"an element that is not a bulk":    "*1\r\n+PING\r\n",
		"a malformed bulk length":          "*1\r\n$xx\r\n",
		"two commands":                     "PING\r\nPING\r\n",
		"a command then a partial one":     "*1\r\n$4\r\nPING\r\n*1\r\n$4\r\nPI",
	}
	for name, input := range complete {
		t.Run("complete: "+name, func(t *testing.T) {
			assert.True(t, requestBuffered(buffered(t, input)))
		})
	}

	incomplete := map[string]string{
		"nothing at all":                   "",
		"a line with no terminator":        "PING",
		"a header with no elements":        "*1\r\n",
		"a bulk header with no payload":    "*1\r\n$4\r\n",
		"a payload cut short":              "*1\r\n$4\r\nPI",
		"a payload with no CRLF yet":       "*1\r\n$4\r\nPING",
		"the second element missing":       "*2\r\n$3\r\nGET\r\n",
		"a blank line and nothing else":    "\r\n",
		"an empty request and nothing":     "*0\r\n",
		"a line of spaces":                 "   \r\n",
		"a line the decoder would skip":    "\t \t\r\n",
		"an element count past the buffer": "*99999\r\n$1\r\na\r\n",
	}
	for name, input := range incomplete {
		t.Run("incomplete: "+name, func(t *testing.T) {
			assert.False(t, requestBuffered(buffered(t, input)))
		})
	}
}

// buffered returns a reader holding input and nothing behind it, which is the
// state the scan is asked about: bytes in the buffer, and a socket that will
// block if anything reads past them.
func buffered(t *testing.T, input string) *bufio.Reader {
	t.Helper()

	r := bufio.NewReaderSize(&onceReader{data: []byte(input)}, connBufferSize)
	if input != "" {
		_, err := r.Peek(1)
		require.NoError(t, err)
	}
	return r
}

// TestRequestBufferedAgreesWithTheDecoder is the property the table above
// samples: whenever the scan says a request is there, decoding it must not read
// another byte. The reader underneath refuses to be read from a second time, so
// a decoder that tried would fail rather than quietly succeed.
func TestRequestBufferedAgreesWithTheDecoder(t *testing.T) {
	inputs := []string{
		"PING\r\n",
		"PING\n",
		"*1\r\n$4\r\nPING\r\n",
		"*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n",
		"*2\r\n$4\r\nECHO\r\n$0\r\n\r\n",
		"\r\n\r\nPING\r\n",
		"*0\r\n*1\r\n$4\r\nPING\r\n",
		"*abc\r\n",
		"*1\r\n+PING\r\n",
		"*1\r\n$-1\r\n",
		"PING\r\nPING\r\nPI",
		"*1\r\n$4\r\nPING\r\n*2\r\n$3\r\nGET\r\n",
	}

	codec := protocol.NewRESP()
	for _, input := range inputs {
		t.Run(strings.ReplaceAll(input, "\r\n", "|"), func(t *testing.T) {
			source := &onceReader{data: []byte(input)}
			r := bufio.NewReaderSize(source, connBufferSize)
			_, err := r.Peek(1)
			require.NoError(t, err)

			if !requestBuffered(r) {
				t.Skip("the scan reports no complete request, which is always safe")
			}
			_, err = codec.Decode(r)
			require.NotErrorIs(t, err, errSecondRead,
				"the scan claimed a complete request and the decoder read past the buffer for it")
		})
	}
}

// errSecondRead is what onceReader returns when something reads past what was
// buffered, which is exactly the stall the scan exists to prevent.
var errSecondRead = fmt.Errorf("read past the buffered request")

type onceReader struct {
	data []byte
	done bool
}

func (o *onceReader) Read(p []byte) (int, error) {
	if o.done {
		return 0, errSecondRead
	}
	o.done = true
	return copy(p, o.data), nil
}

// TestHandleFlushesBeforeItBlocks. The batching loop must never be sitting in a
// read with replies still buffered; everything else about pipelining follows
// from that.
func TestHandleFlushesBeforeItBlocks(t *testing.T) {
	srv := newHandlerServer(newFakeStore(), ConnLimits{})
	s := serve(t, srv, ConnLimits{})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 5 {
			_, err := s.client.Write([]byte("*1\r\n$4\r\nPING\r\n"))
			if err != nil {
				return
			}
			line, err := s.reader.ReadString('\n')
			if err != nil {
				return
			}
			assert.Equal(t, "+PONG\r\n", line)
		}
	}()

	require.NoError(t, s.client.SetReadDeadline(time.Now().Add(5*time.Second)))
	waited := make(chan struct{})
	go func() { wg.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("a client sending one command at a time was not answered")
	}
}
