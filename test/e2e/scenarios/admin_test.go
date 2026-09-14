package scenarios_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/b3vet/atlascache/test/e2e/runner"
	"github.com/b3vet/atlascache/test/e2e/runner/fakeharness"
)

// The admin scenarios are driven here against model servers that misbehave in
// the specific ways each one exists to catch.
//
// In the suite these scenarios only ever meet a correct server, so a scenario
// that could not fail would pass forever while asserting nothing — and these
// are the assertions a credential disclosure and a Kubernetes restart loop
// depend on. Every defect below is one the real implementation could plausibly
// regress into: a health endpoint that grew a version field, a readiness probe
// wired to liveness, an admin API that accepts the client token, a /config that
// returns the secret it was supposed to redact, a startup guard that warns
// instead of refusing.

const (
	modelAuthToken  = "MODEL-CLIENT-TOKEN-8c14f0"
	modelAdminToken = "MODEL-ADMIN-TOKEN-3a97be"
)

// adminDefects are the ways the model server is allowed to be wrong. All false
// is a server that behaves the way FEAT-0030 specifies.
type adminDefects struct {
	// healthNeedsToken makes the health endpoints demand the admin token, which
	// is the change that breaks every Kubernetes probe.
	healthNeedsToken bool
	// healthLeaks adds an operational field to a health response.
	healthLeaks bool
	// readyIsLiveness answers readiness from liveness, the conflation that has
	// a load balancer send traffic to a node that cannot serve it.
	readyIsLiveness bool
	// clientTokenOpensAdmin accepts the data token on the admin API.
	clientTokenOpensAdmin bool
	// statsDrift makes /stats disagree with the STATS command.
	statsDrift bool
	// configLeaks returns the tokens in /config.
	configLeaks bool
	// configEmpty returns an empty document, which passes a redaction scan
	// while telling an operator nothing.
	configEmpty bool
	// configDropsSecrets omits the token fields instead of marking them.
	configDropsSecrets bool
}

// adminModel is a stand-in admin API with the defects switched on or off, plus
// a stand-in for the server binary the bind-guard scenario runs out of band.
type adminModel struct {
	defects adminDefects
	server  *httptest.Server
	root    string

	// guardEnforced is whether the modeled binary refuses an exposed admin API
	// with no token. guardMessage is what it says when it does.
	guardEnforced bool
	guardMessage  string
}

// modelStats is the accounting both surfaces report, so that agreement is
// something the model can get right or wrong rather than something it cannot
// express.
var modelStats = map[string]int64{
	"keys": 7, "keys_with_ttl": 2, "memory_used": 4096, "memory_max": 0,
	"gets": 11, "sets": 13, "deletes": 3, "hits": 9, "misses": 2,
	"evictions": 0, "expirations": 1, "oom_rejected": 0,
	"scan_cursors": 0, "scan_snapshot_bytes": 0, "max_connections": 10000,
}

const defaultGuardMessage = "atlascache: invalid config: admin.bind_addr - is 0.0.0.0, which is " +
	"reachable from other hosts, while admin.token is not set: the admin API would serve statistics " +
	"and the effective configuration to anyone who can reach that address, with no credential " +
	"(ADR-0023). Set admin.token to require one, or set admin.bind_addr to 127.0.0.1 so the API is " +
	"reachable only from this host.\n"

func newAdminModel(t *testing.T, defects adminDefects) *adminModel {
	t.Helper()

	model := &adminModel{
		defects:       defects,
		root:          t.TempDir(),
		guardEnforced: true,
		guardMessage:  defaultGuardMessage,
	}
	model.server = httptest.NewServer(http.HandlerFunc(model.serve))
	t.Cleanup(model.server.Close)

	// The config file the scenarios read the secrets back out of, written the
	// way the harness writes it.
	config := fmt.Sprintf("server:\n  bind_addr: \"127.0.0.1\"\nadmin:\n  token: %q\nauth:\n  enabled: true\n  token: %q\n",
		modelAdminToken, modelAuthToken)
	if err := os.WriteFile(filepath.Join(model.root, "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatalf("writing the model config: %v", err)
	}
	return model
}

func (m *adminModel) addr() string { return strings.TrimPrefix(m.server.URL, "http://") }

func (m *adminModel) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/health", "/health/live", "/health/ready":
		m.serveHealth(w, r)
	case "/stats", "/stats/memory", "/config":
		m.serveProtected(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (m *adminModel) serveHealth(w http.ResponseWriter, r *http.Request) {
	if m.defects.healthNeedsToken && !m.authorized(r) {
		writeModelJSON(w, http.StatusUnauthorized, map[string]any{"error": "admin token required"})
		return
	}

	body := map[string]any{"status": "ok"}
	switch r.URL.Path {
	case "/health/live":
		body["status"] = "alive"
	case "/health/ready":
		body["status"] = "ready"
		if m.defects.readyIsLiveness {
			body["status"] = "alive"
		}
	}
	if m.defects.healthLeaks {
		body["version"] = "0.1.0"
	}
	writeModelJSON(w, http.StatusOK, body)
}

func (m *adminModel) serveProtected(w http.ResponseWriter, r *http.Request) {
	if !m.authorized(r) {
		writeModelJSON(w, http.StatusUnauthorized, map[string]any{"error": "admin token required"})
		return
	}

	switch r.URL.Path {
	case "/stats":
		stats := map[string]int64{}
		for name, value := range modelStats {
			stats[name] = value
		}
		if m.defects.statsDrift {
			stats["hits"]++
		}
		writeModelJSON(w, http.StatusOK, stats)
	case "/config":
		writeModelJSON(w, http.StatusOK, m.configDocument())
	default:
		writeModelJSON(w, http.StatusOK, map[string]any{"memory_used": modelStats["memory_used"]})
	}
}

func (m *adminModel) configDocument() map[string]any {
	if m.defects.configEmpty {
		return map[string]any{}
	}

	authToken, adminToken := any("<redacted>"), any("<redacted>")
	if m.defects.configLeaks {
		authToken, adminToken = modelAuthToken, modelAdminToken
	}

	auth := map[string]any{"enabled": true, "token": authToken}
	admin := map[string]any{"bind_addr": "127.0.0.1", "port": 8080, "token": adminToken}
	if m.defects.configDropsSecrets {
		delete(auth, "token")
		delete(admin, "token")
	}

	return map[string]any{
		"node":     map[string]any{"id": "model"},
		"server":   map[string]any{"bind_addr": "127.0.0.1", "client_port": 6379},
		"admin":    admin,
		"storage":  map[string]any{"shard_count": 0},
		"ttl":      map[string]any{"check_interval": "100ms"},
		"eviction": map[string]any{"policy": "lru"},
		"logging":  map[string]any{"level": "debug"},
		"tls":      map[string]any{"enabled": false},
		"auth":     auth,
	}
}

// authorized mirrors the real token check, including the defect where the
// client token is accepted.
func (m *adminModel) authorized(r *http.Request) bool {
	presented := r.Header.Get("X-Admin-Token")
	if presented == "" {
		presented = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	if presented == modelAdminToken {
		return true
	}
	return m.defects.clientTokenOpensAdmin && presented == modelAuthToken
}

func writeModelJSON(w http.ResponseWriter, status int, body any) {
	encoded, err := json.Marshal(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(encoded); err != nil {
		panic(err)
	}
}

// modelConfig is the part of a configuration file the modeled binary's
// validation looks at.
type modelConfig struct {
	Admin struct {
		BindAddr string `yaml:"bind_addr"`
		Token    string `yaml:"token"`
	} `yaml:"admin"`
	Auth struct {
		Token string `yaml:"token"`
	} `yaml:"auth"`
	Eviction struct {
		Policy string `yaml:"policy"`
	} `yaml:"eviction"`
}

// RunBinary models the server binary's startup validation.
//
// A configuration it accepts blocks until the context expires, because that is
// what a server that started does — and it is the behavior the scenario has to
// notice when the guard stops firing.
func (m *adminModel) RunBinary(ctx context.Context, args ...string) (int, string, error) {
	path := args[len(args)-1]
	raw, err := os.ReadFile(path)
	if err != nil {
		return -1, "", err
	}

	var config modelConfig
	if err := yaml.Unmarshal(raw, &config); err != nil {
		return -1, "", err
	}

	switch {
	case config.Eviction.Policy != "" && config.Eviction.Policy != "lru":
		return 1, "atlascache: invalid config: eviction.policy - must be one of: lru, lfu, fifo, none\n", nil
	case config.Admin.Token != "" && config.Admin.Token == config.Auth.Token:
		return 1, "atlascache: invalid config: admin.token - must not be the same value as auth.token: " +
			"the admin API is more powerful than the data port, and sharing the secret gives every data " +
			"client administrative access (ADR-0023).\n", nil
	case m.guardEnforced && isExposed(config.Admin.BindAddr) && config.Admin.Token == "":
		return 1, m.guardMessage, nil
	}

	// Accepted: the modeled server starts and keeps running.
	<-ctx.Done()
	return -1, "", fmt.Errorf("running atlascache: %w", ctx.Err())
}

func isExposed(bindAddr string) bool {
	return bindAddr != "" && bindAddr != "127.0.0.1" && bindAddr != "localhost" && bindAddr != "::1"
}

func (m *adminModel) Root() string { return m.root }

// adminHarness is a fake harness pointing at a model admin API, and the binary
// stand-in the bind-guard scenario needs.
type adminHarness struct {
	*fakeharness.Harness
	model *adminModel
}

func (h *adminHarness) Info() runner.ServerInfo {
	return runner.ServerInfo{
		ClientAddr: "127.0.0.1:6379",
		AdminAddr:  h.model.addr(),
		DataDir:    h.model.root,
	}
}

func (h *adminHarness) RunBinary(ctx context.Context, args ...string) (int, string, error) {
	return h.model.RunBinary(ctx, args...)
}

func (h *adminHarness) Root() string { return h.model.Root() }

// statsReply is what the STATS command answers with, built from the same
// figures the model serves over HTTP so that agreement is the default and
// disagreement has to be switched on.
func statsReply() runner.Reply {
	pairs := make(map[string]runner.Reply, len(modelStats))
	for name, value := range modelStats {
		pairs[name] = runner.IntegerReply(value)
	}
	return runner.MapReply(pairs)
}

func newAdminHarness(t *testing.T, defects adminDefects) *adminHarness {
	t.Helper()

	fake := fakeharness.New(map[string]runner.Reply{
		"PING":  runner.StatusReply("PONG"),
		"STATS": statsReply(),
	})
	t.Cleanup(func() {
		if err := fake.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	return &adminHarness{Harness: fake, model: newAdminModel(t, defects)}
}

// runAdminScenario drives one scenario through the executor, which is how a
// spec reaches it, and returns whatever failure came back.
func runAdminScenario(t *testing.T, h runner.Harness, scenario string) *runner.Failure {
	t.Helper()

	source := fmt.Sprintf("version: 1\nname: admin-model\ntier: full\nfeature: FEAT-0030\nsteps:\n  - scenario: %s\n", scenario)
	spec, err := runner.ParseSpec("admin-model.yaml", []byte(source))
	if err != nil {
		t.Fatalf("parsing the spec: %v", err)
	}

	executor := &runner.Executor{
		Factory: func(runner.HarnessOptions) (runner.Harness, error) { return h, nil },
	}
	return executor.Run(context.Background(), spec).Failure
}

// requirePasses runs a scenario against a correct model and fails the test if
// it complains, which is the control every defect case is read against.
func requirePasses(t *testing.T, scenario string) {
	t.Helper()

	if failure := runAdminScenario(t, newAdminHarness(t, adminDefects{}), scenario); failure != nil {
		t.Fatalf("%s failed against a correct server: %s\n%s", scenario, failure.Message, strings.Join(failure.Notes, "\n"))
	}
}

// requireCatches runs a scenario against a defective model and fails the test
// if the scenario is happy with it.
func requireCatches(t *testing.T, scenario string, defects adminDefects, wants string) {
	t.Helper()

	failure := runAdminScenario(t, newAdminHarness(t, defects), scenario)
	if failure == nil {
		t.Fatalf("%s passed against a server with %+v", scenario, defects)
	}
	if !strings.Contains(failure.Message, wants) {
		t.Errorf("%s reported %q, which does not mention %q", scenario, failure.Message, wants)
	}
}

func TestAdminScenariosAreRegistered(t *testing.T) {
	for _, name := range []string{
		"admin_health_needs_no_credential",
		"admin_health_reveals_nothing",
		"admin_protected_endpoints_need_the_token",
		"admin_protected_endpoints_are_still_closed",
		"admin_stats_matches_the_stats_command",
		"admin_bind_guard_refuses_exposure",
		"admin_config_redacts_every_token",
	} {
		if _, ok := runner.LookupScenario(name); !ok {
			t.Errorf("%s did not register itself", name)
		}
	}
}

func TestAdminHealthScenarios(t *testing.T) {
	requirePasses(t, "admin_health_needs_no_credential")
	requirePasses(t, "admin_health_reveals_nothing")

	// The change that breaks every Kubernetes probe, and which would otherwise
	// only be noticed by a pod that stopped receiving traffic.
	requireCatches(t, "admin_health_needs_no_credential",
		adminDefects{healthNeedsToken: true}, "with no credential returned 401")

	// The next well-meant field on an unauthenticated endpoint.
	requireCatches(t, "admin_health_reveals_nothing",
		adminDefects{healthLeaks: true}, "want only a status")

	// Readiness answered from liveness: the conflation the feature is about.
	requireCatches(t, "admin_health_reveals_nothing",
		adminDefects{readyIsLiveness: true}, "/health/ready")
}

func TestAdminAuthScenarios(t *testing.T) {
	requirePasses(t, "admin_protected_endpoints_need_the_token")
	requirePasses(t, "admin_protected_endpoints_are_still_closed")

	// The privilege separation ADR-0023 exists for: a data client must not hold
	// a working administrative credential.
	requireCatches(t, "admin_protected_endpoints_need_the_token",
		adminDefects{clientTokenOpensAdmin: true}, "the client token")
}

func TestAdminStatsScenarioCatchesDrift(t *testing.T) {
	requirePasses(t, "admin_stats_matches_the_stats_command")

	requireCatches(t, "admin_stats_matches_the_stats_command",
		adminDefects{statsDrift: true}, "drifted apart")
}

func TestAdminConfigRedactionScenario(t *testing.T) {
	requirePasses(t, "admin_config_redacts_every_token")

	// The disclosure the spec exists to catch.
	requireCatches(t, "admin_config_redacts_every_token",
		adminDefects{configLeaks: true}, "token somewhere in its body")

	// And the two ways a response could pass a naive scan while being useless:
	// returning nothing at all, and dropping the fields rather than marking
	// them, which leaves an operator unable to tell a configured token from an
	// absent one.
	requireCatches(t, "admin_config_redacts_every_token",
		adminDefects{configEmpty: true}, "missing the")
	requireCatches(t, "admin_config_redacts_every_token",
		adminDefects{configDropsSecrets: true}, "rather than redacting it")
}

func TestAdminBindGuardScenario(t *testing.T) {
	requirePasses(t, "admin_bind_guard_refuses_exposure")
}

// TestAdminBindGuardScenarioCatchesAWarningThatStayedAWarning is the P0 debt's
// own regression test: a build that logged the exposure and started anyway is
// exactly what FEAT-0010 shipped, and the scenario has to notice.
func TestAdminBindGuardScenarioCatchesAWarningThatStayedAWarning(t *testing.T) {
	h := newAdminHarness(t, adminDefects{})
	h.model.guardEnforced = false

	failure := runAdminScenario(t, h, "admin_bind_guard_refuses_exposure")
	if failure == nil {
		t.Fatal("the scenario passed against a server that starts with an exposed, unauthenticated admin API")
	}
	if !strings.Contains(failure.Message, "still running") {
		t.Errorf("message = %q, want it to say the server did not exit", failure.Message)
	}
}

// TestAdminBindGuardScenarioRequiresBothFixes holds the message to the standard
// the ADR sets. A refusal that names the problem and one way out leaves an
// operator who cannot take that way out with nothing.
func TestAdminBindGuardScenarioRequiresBothFixes(t *testing.T) {
	cases := map[string]string{
		"a bare refusal": "atlascache: invalid config: admin.bind_addr\n",
		"the cause without either fix": "atlascache: invalid config: admin.bind_addr - " +
			"is reachable from other hosts with no credential\n",
		"only the loopback fix": "atlascache: invalid config: admin.bind_addr - is reachable from " +
			"other hosts with no credential; bind 127.0.0.1 instead (ADR-0023)\n",
	}

	for name, message := range cases {
		t.Run(name, func(t *testing.T) {
			h := newAdminHarness(t, adminDefects{})
			h.model.guardMessage = message

			failure := runAdminScenario(t, h, "admin_bind_guard_refuses_exposure")
			if failure == nil {
				t.Fatalf("the scenario accepted a refusal that reads %q", message)
			}
			if !strings.Contains(failure.Message, "does not mention") {
				t.Errorf("message = %q", failure.Message)
			}
		})
	}
}
