package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/b3vet/atlascache/internal/config"
)

// healthCheckTimeout bounds the probe. Docker's own HEALTHCHECK timeout is
// 3s by default, so a probe that outlives it is reported as a failure with no
// message at all; failing first, with a message, is strictly more useful.
const healthCheckTimeout = 2 * time.Second

// healthCheck is `atlascache --health-check`: the probe the container image's
// HEALTHCHECK runs.
//
// It exists because the distroless base image (ADR-0024) has no shell and no
// curl, so the only executable in the image is the server binary itself. Rather
// than push users onto a base image with a package manager, the binary answers
// the question it is in the best position to answer.
//
// It reads the same configuration the server does, so a probe and the server it
// probes cannot disagree about the port: `--health-check --config <same file>`
// targets whatever that file configures.
//
// Readiness, not liveness, is what it reports. A probe that only checked
// liveness would report a starting node as healthy, and a container that
// reports healthy before it can serve is worse than one with no health check:
// it makes a rolling update send traffic into a node that has nothing to answer
// with.
func healthCheck(configPath string) int {
	cfg, _, err := loadConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "atlascache: health check: %v\n", err)
		return 1
	}

	target := probeAddr(cfg)
	url := "http://" + target + "/health/ready"

	ctx, cancel := context.WithTimeout(context.Background(), healthCheckTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "atlascache: health check: %v\n", err)
		return 1
	}

	// No credential is sent, and none is needed: ADR-0023 keeps the health
	// endpoints unauthenticated precisely so a probe that cannot hold a secret
	// can still run. Sending the admin token here would put it in the process
	// table of every health check.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "atlascache: health check: %s is not answering: %v\n", url, err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// The body names the phase -- starting, stopping -- which is the
		// difference between "wait" and "something is wrong". Bounded, because
		// this is read from a health endpoint and printed into a log.
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 512))
		if readErr != nil {
			body = []byte("(the response body could not be read: " + readErr.Error() + ")")
		}
		fmt.Fprintf(os.Stderr, "atlascache: health check: %s answered %d: %s\n",
			url, resp.StatusCode, body)
		return 1
	}
	return 0
}

// probeAddr is the address the probe dials for a server configured to bind
// adminAddr.
//
// A wildcard bind is the case worth handling. `0.0.0.0` is an address to listen
// on, not one to connect to: on Linux connecting to it happens to reach
// loopback, on other systems it does not, and `::` fails outright where IPv6 is
// disabled. Since the probe runs inside the same network namespace as the
// server, loopback always reaches it — so a wildcard bind is dialed as
// loopback rather than literally.
func probeAddr(cfg *config.Config) string {
	host := cfg.Admin.BindAddr
	port := strconv.Itoa(cfg.Admin.Port)

	if host == "" {
		return net.JoinHostPort("127.0.0.1", port)
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		if ip.To4() != nil {
			return net.JoinHostPort("127.0.0.1", port)
		}
		return net.JoinHostPort("::1", port)
	}
	return net.JoinHostPort(host, port)
}
