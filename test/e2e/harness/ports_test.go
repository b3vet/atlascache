package harness

import "testing"

// TestFreePortsAreDistinctAndStayReservedUntilReleased. Two harnesses handed the
// same port collide on a machine where the kernel recycles ephemeral ports
// quickly — the once-a-week failure that gets a gate ignored. The in-process
// ledger is what makes that impossible, so a port that has been handed out must
// not be reservable again until it is released.
func TestFreePortsAreDistinctAndStayReservedUntilReleased(t *testing.T) {
	t.Parallel()

	ports, err := freePorts(4)
	if err != nil {
		t.Fatalf("freePorts(4): %v", err)
	}
	t.Cleanup(func() { release(ports...) })

	if len(ports) != 4 {
		t.Fatalf("freePorts(4) returned %d ports", len(ports))
	}

	seen := map[int]bool{}
	for _, port := range ports {
		if seen[port] {
			t.Errorf("port %d was handed out twice in one call", port)
		}
		seen[port] = true
		if reserve(port) {
			t.Errorf("port %d was handed out but left unreserved; another harness could take it", port)
		}
	}

	release(ports...)
	for _, port := range ports {
		if !reserve(port) {
			t.Errorf("port %d is still reserved after release; the ledger leaks ports across specs", port)
		}
	}
}
