package netbench

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"testing"
	"time"
)

// The null server exists to answer one question: is the load generator fast
// enough to be measuring the server at all?
//
// A generator that saturates before the server does reports its own limit and
// calls it the server's. The control is a server with the same shape as the one
// under test -- a Go process, goroutine per connection, stdlib net, same
// loopback, same pipelining rule -- that does no work at all: it consumes a
// request and writes a canned reply, with no keyspace, no hashing, no copy.
// Whatever the generator drives against that is its ceiling on this machine,
// and every AtlasCache number below the ceiling is a measurement of AtlasCache.
//
// It runs in its own process for two reasons. It has to compete for the same
// ten cores the real server competes for, or the comparison is rigged; and the
// generator's CPU has to be attributable to the generator, which it is not when
// the server it is driving shares its address space.

const (
	nullAddrEnv = "ATLAS_NETBENCH_NULL_ADDR"
	nullSizeEnv = "ATLAS_NETBENCH_NULL_SIZE"
)

// TestNullServerHelperProcess is not a test. It is the entry point the sweep
// re-executes this binary at to get a do-nothing RESP server in a separate
// process; without the environment it skips, which is what it does during a
// normal `go test` run.
func TestNullServerHelperProcess(t *testing.T) {
	addr := os.Getenv(nullAddrEnv)
	if addr == "" {
		t.Skip("helper process for the generator-ceiling control, not a test")
	}
	size, err := strconv.Atoi(os.Getenv(nullSizeEnv))
	if err != nil {
		t.Fatalf("%s: %v", nullSizeEnv, err)
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("listening on %s: %v", addr, err)
	}
	srv := &nullServer{ln: ln, addr: addr, reply: bulkReply(size)}
	srv.accept() // runs until the parent kills this process
}

type nullServer struct {
	ln    net.Listener
	addr  string
	reply []byte
	wg    sync.WaitGroup
}

// cpuSeconds satisfies cpuSampler for the in-process server used by the
// self-tests; there is no separate process to sample.
func (n *nullServer) cpuSeconds() (float64, bool) { return 0, false }

func startNullServer(reply []byte) (*nullServer, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	n := &nullServer{ln: ln, addr: ln.Addr().String(), reply: reply}
	n.wg.Add(1)
	go func() { defer n.wg.Done(); n.accept() }()
	return n, nil
}

func (n *nullServer) accept() {
	for {
		conn, err := n.ln.Accept()
		if err != nil {
			return
		}
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetNoDelay(true)
		}
		n.wg.Add(1)
		go func() {
			defer n.wg.Done()
			n.serve(conn)
		}()
	}
}

func (n *nullServer) Stop() { _ = n.ln.Close() }

// serve mirrors the real server's flush rule: answer what is already buffered,
// then flush once. Anything else would make the control's pipelining behavior
// differ from that of the thing it is bounding.
func (n *nullServer) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReaderSize(conn, readBufSize)
	w := bufio.NewWriterSize(conn, readBufSize)
	for {
		if err := discardRequest(r); err != nil {
			return
		}
		if _, err := w.Write(n.reply); err != nil {
			return
		}
		if r.Buffered() == 0 {
			if err := w.Flush(); err != nil {
				return
			}
		}
	}
}

// nullProc is the control running in its own process.
type nullProc struct {
	cmd  *exec.Cmd
	addr string
}

func (n *nullProc) cpuSeconds() (float64, bool) {
	if n == nil || n.cmd == nil || n.cmd.Process == nil {
		return 0, false
	}
	return cpuOf(n.cmd.Process.Pid)
}

func (n *nullProc) Stop() {
	if n != nil && n.cmd != nil && n.cmd.Process != nil {
		_ = n.cmd.Process.Kill()
		_, _ = n.cmd.Process.Wait()
		n.cmd = nil
	}
}

// startNullServerProc re-executes this test binary as the helper above.
func startNullServerProc(replySize int) (*nullProc, error) {
	ports, err := freePorts(1)
	if err != nil {
		return nil, err
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(ports[0]))

	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(self, "-test.run=^TestNullServerHelperProcess$", "-test.timeout=60m") //nolint:noctx // this very binary, with a lifetime Stop owns
	cmd.Env = append(os.Environ(),
		nullAddrEnv+"="+addr,
		nullSizeEnv+"="+strconv.Itoa(replySize))
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	proc := &nullProc{cmd: cmd, addr: addr}
	if err := waitDialable(addr, 15*time.Second); err != nil {
		proc.Stop()
		return nil, err
	}
	return proc, nil
}

func waitDialable(addr string, timeout time.Duration) error {
	d := &net.Dialer{Timeout: time.Second}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := d.DialContext(context.Background(), "tcp", addr)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("the null server did not accept on %s within %s", addr, timeout)
}

// discardRequest consumes exactly one RESP array-of-bulk-strings request.
func discardRequest(r *bufio.Reader) error {
	line, err := readLine(r)
	if err != nil {
		return err
	}
	if len(line) == 0 || line[0] != '*' {
		return errors.New("the null server expects an array request")
	}
	n, err := strconv.Atoi(string(line[1:]))
	if err != nil || n < 0 {
		return errProtocol
	}
	for i := 0; i < n; i++ {
		header, err := readLine(r)
		if err != nil {
			return err
		}
		if len(header) == 0 || header[0] != '$' {
			return errProtocol
		}
		size, err := strconv.Atoi(string(header[1:]))
		if err != nil || size < 0 {
			return errProtocol
		}
		if _, err := r.Discard(size + len(crlf)); err != nil {
			return err
		}
	}
	return nil
}

// bulkReply is the canned `$n\r\n<n bytes>\r\n` a null GET answers with, so the
// control moves the same bytes as the run it bounds.
func bulkReply(size int) []byte {
	body := payload(size)
	out := make([]byte, 0, len(body)+16)
	out = append(out, '$')
	out = strconv.AppendInt(out, int64(size), 10)
	out = append(out, crlf...)
	out = append(out, body...)
	out = append(out, crlf...)
	return out
}
