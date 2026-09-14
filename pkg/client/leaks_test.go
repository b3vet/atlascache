package client

import (
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Goroutine leak detection, written here rather than taken from go.uber.org/goleak.
//
// The SDK's go.mod has no require block and that is a property worth keeping:
// a test-only dependency still appears in the published module's dependency
// list, which is exactly the auditable surface ADR-0015's split exists to keep
// clean. What goleak does for this package is forty lines of runtime.Stack, and
// they are below.
//
// The filter is the package's own name. Every goroutine this SDK starts — the
// context watcher in conn.watch is the only one — has it in the stack, as do
// the test server's, so a connection or a watcher left running is caught.
const packageMarker = "atlascache/pkg/client."

// TestMain fails the package if anything is still running when the last test
// has finished.
func TestMain(m *testing.M) {
	code := m.Run()
	if code == 0 {
		if leaked := waitForNoGoroutines(3 * time.Second); len(leaked) > 0 {
			for _, stack := range leaked {
				println("leaked goroutine:\n" + stack)
			}
			println("the SDK left goroutines running after its tests finished")
			code = 1
		}
	}
	os.Exit(code)
}

// noLeaks registers a per-test check that the test started no goroutine it did
// not also stop.
//
// It is registered first in a test so that its cleanup runs last, after the
// client has been closed and the server stopped — cleanups run in reverse
// order, and a check that ran before them would be measuring the wrong moment.
func noLeaks(t *testing.T) {
	t.Helper()
	before := len(packageGoroutines())

	t.Cleanup(func() {
		deadline := time.Now().Add(3 * time.Second)
		for {
			stacks := packageGoroutines()
			if len(stacks) <= before {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("%d goroutines leaked (started with %d, ended with %d):\n%s",
					len(stacks)-before, before, len(stacks), strings.Join(stacks, "\n"))
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
}

// waitForNoGoroutines gives goroutines that are on their way out a moment to
// finish, so an ordinary shutdown race is not reported as a leak.
func waitForNoGoroutines(within time.Duration) []string {
	deadline := time.Now().Add(within)
	for {
		stacks := packageGoroutines()
		if len(stacks) == 0 || time.Now().After(deadline) {
			return stacks
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// packageGoroutines returns the stack of every running goroutine that has this
// package in it.
func packageGoroutines() []string {
	buf := make([]byte, 64<<10)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, len(buf)*2)
	}

	var stacks []string
	for _, stack := range strings.Split(string(buf), "\n\n") {
		if !strings.Contains(stack, packageMarker) {
			continue
		}
		// The goroutine doing the counting is in this package too. Excluding it
		// by the frame it is standing in is exact, and does not depend on it
		// being the first stack the runtime printed.
		if strings.Contains(stack, "packageGoroutines") {
			continue
		}
		stacks = append(stacks, strings.TrimSpace(stack))
	}
	return stacks
}

// waitFor polls until cond is true or the budget runs out. Tests use it instead
// of a fixed sleep so they are neither slow nor flaky.
func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", within, what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
