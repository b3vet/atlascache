package harness

import (
	"context"
	"fmt"
	"net"
	"sync"
)

// portAttempts bounds the search for free ports before giving up, so a machine
// with no ephemeral ports left fails with a message instead of spinning.
const portAttempts = 64

// handedOut records every port this process has given to a server and not yet
// released. The kernel will not normally recycle an ephemeral port fast enough
// for two concurrent specs to collide, but "not normally" is exactly the kind
// of assumption that produces a test that fails once a week and gets ignored.
var handedOut = struct {
	sync.Mutex
	ports map[int]bool
}{ports: map[int]bool{}}

func reserve(port int) bool {
	handedOut.Lock()
	defer handedOut.Unlock()
	if handedOut.ports[port] {
		return false
	}
	handedOut.ports[port] = true
	return true
}

func release(ports ...int) {
	handedOut.Lock()
	defer handedOut.Unlock()
	for _, port := range ports {
		delete(handedOut.ports, port)
	}
}

// freePorts returns n distinct ports that were free on the loopback interface a
// moment ago.
//
// Binding port 0, reading the port the kernel assigned, then closing the
// listener leaves a window in which something else can take the port before the
// server binds it. The window is small and the alternative — passing an open
// listener to a child process — would tie the harness to the server's internals.
// So the race is accepted and handled where it surfaces: Start retries the whole
// launch with fresh ports when the server reports the address as taken. A flaky
// gate gets ignored, and an ignored gate is no gate.
//
// The n listeners are held open at once, which is what makes the ports distinct.
func freePorts(n int) ([]int, error) {
	var (
		listeners []net.Listener
		ports     []int
	)
	defer func() {
		for _, ln := range listeners {
			_ = ln.Close()
		}
	}()

	for attempt := 0; len(ports) < n; attempt++ {
		if attempt >= portAttempts {
			release(ports...)
			return nil, fmt.Errorf("no free port found after %d attempts", portAttempts)
		}

		var config net.ListenConfig
		ln, err := config.Listen(context.Background(), "tcp", "127.0.0.1:0")
		if err != nil {
			release(ports...)
			return nil, fmt.Errorf("allocating a free port: %w", err)
		}
		listeners = append(listeners, ln)

		addr, ok := ln.Addr().(*net.TCPAddr)
		if !ok {
			release(ports...)
			return nil, fmt.Errorf("listener address is %T, want *net.TCPAddr", ln.Addr())
		}
		if !reserve(addr.Port) {
			continue // another harness in this process owns it
		}
		ports = append(ports, addr.Port)
	}
	return ports, nil
}
