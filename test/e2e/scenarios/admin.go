package scenarios

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/b3vet/atlascache/test/e2e/runner"
)

func init() {
	runner.RegisterScenario("admin_health_needs_no_credential", adminHealthNeedsNoCredential)
	runner.RegisterScenario("admin_health_reveals_nothing", adminHealthRevealsNothing)
	runner.RegisterScenario("admin_protected_endpoints_need_the_token", adminProtectedEndpointsNeedTheToken)
	runner.RegisterScenario("admin_protected_endpoints_are_still_closed", adminProtectedEndpointsAreStillClosed)
	runner.RegisterScenario("admin_stats_matches_the_stats_command", adminStatsMatchesTheStatsCommand)
	runner.RegisterScenario("admin_bind_guard_refuses_exposure", adminBindGuardRefusesExposure)
	runner.RegisterScenario("admin_config_redacts_every_token", adminConfigRedactsEveryToken)
}

// The admin API's two halves (ADR-0023). Both lists are walked rather than
// sampled, so an endpoint that ships without its guard fails a spec rather than
// a production audit.
var (
	adminPublicPaths    = []string{"/health", "/health/live", "/health/ready"}
	adminProtectedPaths = []string{"/stats", "/stats/memory", "/config"}
)

// The header the admin token travels in, and the configuration field it comes
// from. Both appear often enough below that a typo in one of them would look
// like a server bug.
const (
	authorizationHeader = "Authorization"
	adminTokenField     = "admin.token"
)

// bearer renders an Authorization header value.
func bearer(token string) map[string]string {
	return map[string]string{authorizationHeader: "Bearer " + token}
}

// adminBodyLimit bounds what a scenario will read back. /config is a few
// kilobytes; anything past this is a server bug and reading it would turn that
// bug into a hung test.
const adminBodyLimit = 1 << 20

// adminResponse is one answer from the admin API, body included as text. The
// body is kept whole because the check that matters most — that no token
// appears anywhere in it — is about the bytes and not about the fields.
type adminResponse struct {
	status int
	body   string
	header http.Header
}

// fields decodes the body as a JSON object.
func (r adminResponse) fields() (map[string]any, error) {
	var decoded map[string]any
	if err := json.Unmarshal([]byte(r.body), &decoded); err != nil {
		return nil, fmt.Errorf("the response is not a JSON object: %w (body: %s)", err, r.body)
	}
	return decoded, nil
}

// adminGet issues one request against the admin API of the server this spec is
// running.
func adminGet(c *runner.Ctx, path string, headers map[string]string) (adminResponse, error) {
	url := "http://" + c.Info().AdminAddr + path

	req, err := http.NewRequestWithContext(c.Context(), http.MethodGet, url, nil)
	if err != nil {
		return adminResponse{}, fmt.Errorf("building the request for %s: %w", path, err)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return adminResponse{}, fmt.Errorf("GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, adminBodyLimit))
	if err != nil {
		return adminResponse{}, fmt.Errorf("reading the response to GET %s: %w", url, err)
	}

	return adminResponse{status: resp.StatusCode, body: string(body), header: resp.Header}, nil
}

// adminHealthNeedsNoCredential covers the half of ADR-0023 the deployment model
// depends on: a Kubernetes probe cannot present a token, so none of the three
// health endpoints may ever ask for one — including on a node that has one
// configured.
func adminHealthNeedsNoCredential(c *runner.Ctx) error {
	for _, path := range adminPublicPaths {
		resp, err := adminGet(c, path, nil)
		if err != nil {
			return err
		}
		c.Logf("GET %s -> %d %s", path, resp.status, strings.TrimSpace(resp.body))

		if resp.status != http.StatusOK {
			return fmt.Errorf("GET %s with no credential returned %d, want 200: %s",
				path, resp.status, strings.TrimSpace(resp.body))
		}
		if contentType := resp.header.Get("Content-Type"); !strings.Contains(contentType, "application/json") {
			return fmt.Errorf("GET %s returned Content-Type %q, want JSON", path, contentType)
		}
	}
	return nil
}

// adminHealthRevealsNothing is the leak check on the unauthenticated half.
//
// It asserts the field set rather than the absence of particular strings,
// because the risk is not a key count appearing today — it is the next
// well-meant addition. A version, an uptime, a node id: each looks harmless on
// its own, and each is a fact about the host handed to anyone who can reach the
// port without a credential.
func adminHealthRevealsNothing(c *runner.Ctx) error {
	// The words a probe may answer with. Each is about liveness or readiness
	// and nothing else.
	allowed := map[string]bool{"ok": true, "alive": true, "ready": true, "starting": true, "stopping": true}

	for _, path := range adminPublicPaths {
		resp, err := adminGet(c, path, nil)
		if err != nil {
			return err
		}

		fields, err := resp.fields()
		if err != nil {
			return fmt.Errorf("GET %s: %w", path, err)
		}
		if len(fields) != 1 {
			return fmt.Errorf("GET %s returned %d fields, want only a status: %v", path, len(fields), fields)
		}

		status, ok := fields["status"].(string)
		if !ok {
			return fmt.Errorf("GET %s returned no string status: %v", path, fields)
		}
		if !allowed[status] {
			return fmt.Errorf("GET %s answered %q, which is not a liveness or readiness word", path, status)
		}
		c.Logf("GET %s -> {status: %s}, and nothing else", path, status)
	}

	// The node is serving, so readiness must say so. A readiness probe that was
	// wired to the same constant as liveness would pass every check above.
	ready, err := adminGet(c, "/health/ready", nil)
	if err != nil {
		return err
	}
	if !strings.Contains(ready.body, `"ready"`) {
		return fmt.Errorf("a serving node answered /health/ready with %s", strings.TrimSpace(ready.body))
	}
	return nil
}

// adminProtectedEndpointsNeedTheToken walks the ways a request can fail to
// carry the admin credential, including the one the second credential exists
// for: a caller holding the client token.
func adminProtectedEndpointsNeedTheToken(c *runner.Ctx) error {
	tokens, err := configuredAdminTokens(c)
	if err != nil {
		return err
	}
	if tokens.admin == "" {
		return errors.New("this scenario needs a spec that sets " + adminTokenField)
	}
	if tokens.auth == "" {
		return errors.New("this scenario needs a spec that sets auth.token, to prove it does not work here")
	}

	refused := []struct {
		name    string
		headers map[string]string
	}{
		{name: "no credential", headers: nil},
		{name: "a wrong token", headers: bearer("not-the-token")},
		// The whole point of a separate admin.token: a client holding the data
		// token must not thereby hold an administrative one.
		{name: "the client token", headers: bearer(tokens.auth)},
		{name: "the client token as X-Admin-Token", headers: map[string]string{"X-Admin-Token": tokens.auth}},
	}

	for _, attempt := range refused {
		for _, path := range adminProtectedPaths {
			resp, err := adminGet(c, path, attempt.headers)
			if err != nil {
				return err
			}
			if resp.status != http.StatusUnauthorized {
				return fmt.Errorf("GET %s with %s returned %d, want 401: %s",
					path, attempt.name, resp.status, strings.TrimSpace(resp.body))
			}
			if strings.Contains(resp.body, tokens.auth) || strings.Contains(resp.body, tokens.admin) {
				return fmt.Errorf("the 401 from GET %s carries a token in its body: %s",
					path, strings.TrimSpace(resp.body))
			}
		}
		c.Logf("%s: 401 on every protected endpoint", attempt.name)
	}

	for _, headers := range []map[string]string{
		bearer(tokens.admin),
		{"X-Admin-Token": tokens.admin},
	} {
		for _, path := range adminProtectedPaths {
			resp, err := adminGet(c, path, headers)
			if err != nil {
				return err
			}
			if resp.status != http.StatusOK {
				return fmt.Errorf("GET %s with the admin token returned %d, want 200: %s",
					path, resp.status, strings.TrimSpace(resp.body))
			}
		}
	}
	c.Logf("the admin token opens all %d protected endpoints, in both header forms", len(adminProtectedPaths))

	// And the unauthenticated half is still unauthenticated on the same node.
	return adminHealthNeedsNoCredential(c)
}

// adminProtectedEndpointsAreStillClosed is the counterweight to the health
// checks: on the same node, in the same run, the endpoints that do require a
// credential still require one.
//
// Without it a spec that asserted "the health endpoints answer without a token"
// would pass just as happily against a server where nothing required one.
func adminProtectedEndpointsAreStillClosed(c *runner.Ctx) error {
	tokens, err := configuredAdminTokens(c)
	if err != nil {
		return err
	}
	if tokens.admin == "" {
		return errors.New("this scenario needs a spec that sets " + adminTokenField)
	}

	for _, path := range adminProtectedPaths {
		anonymous, err := adminGet(c, path, nil)
		if err != nil {
			return err
		}
		if anonymous.status != http.StatusUnauthorized {
			return fmt.Errorf("GET %s with no credential returned %d, want 401: %s",
				path, anonymous.status, strings.TrimSpace(anonymous.body))
		}

		authorized, err := adminGet(c, path, bearer(tokens.admin))
		if err != nil {
			return err
		}
		if authorized.status != http.StatusOK {
			return fmt.Errorf("GET %s with the admin token returned %d, want 200: %s",
				path, authorized.status, strings.TrimSpace(authorized.body))
		}
	}

	c.Logf("all %d protected endpoints are 401 without the token and 200 with it", len(adminProtectedPaths))
	return nil
}

// adminStatsMatchesTheStatsCommand is the anti-drift check across the two
// surfaces.
//
// Both read one set of counters through one seam, so every field they share
// must agree exactly. Two numbers with one name is worse than one number: an
// operator who cannot reconcile a dashboard with a CLI stops trusting both.
//
// Counters move while the check runs, so the HTTP read is bracketed around the
// command and only fields that held still across the bracket are compared. A
// field that moved is skipped and logged rather than failed, because a racing
// counter is not a disagreement.
func adminStatsMatchesTheStatsCommand(c *runner.Ctx) error {
	tokens, err := configuredAdminTokens(c)
	if err != nil {
		return err
	}
	headers := bearer(tokens.admin)

	before, err := adminStatsFields(c, headers)
	if err != nil {
		return err
	}

	reply, err := c.Send("STATS")
	if err != nil {
		return fmt.Errorf("sending STATS: %w", err)
	}
	command := reply.Fields()
	if len(command) == 0 {
		return fmt.Errorf("STATS returned no fields: %s", reply.String())
	}

	after, err := adminStatsFields(c, headers)
	if err != nil {
		return err
	}

	compared, skipped := 0, 0
	for name, httpValue := range before {
		commandValue, shared := command[name]
		if !shared {
			return fmt.Errorf("/stats reports %q, which the STATS command does not: the two must use one vocabulary", name)
		}
		if after[name] != httpValue {
			skipped++
			continue
		}
		if commandValue != httpValue {
			return fmt.Errorf("%s is %s over HTTP and %s from the STATS command; the two surfaces have drifted apart",
				name, httpValue, commandValue)
		}
		compared++
	}

	if compared == 0 {
		return errors.New("no field held still long enough to be compared, so nothing was actually checked")
	}
	c.Logf("%d of %d fields compared against the STATS command and identical (%d moved mid-check)",
		compared, len(before), skipped)
	return nil
}

// adminStatsFields reads /stats as string values, so that the comparison
// against the command's text output is a comparison of the same thing. JSON
// numbers decode to float64 and would print 1e+06 for a counter the command
// renders as 1000000.
func adminStatsFields(c *runner.Ctx, headers map[string]string) (map[string]string, error) {
	resp, err := adminGet(c, "/stats", headers)
	if err != nil {
		return nil, err
	}
	if resp.status != http.StatusOK {
		return nil, fmt.Errorf("GET /stats returned %d: %s", resp.status, strings.TrimSpace(resp.body))
	}

	var raw map[string]json.Number
	decoder := json.NewDecoder(strings.NewReader(resp.body))
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("decoding /stats: %w (body: %s)", err, resp.body)
	}

	fields := make(map[string]string, len(raw))
	for name, value := range raw {
		fields[name] = value.String()
	}
	return fields, nil
}

// adminConfigRedactsEveryToken is the check FEAT-0030 names as the one that
// matters, and it is a scan of the whole body rather than a reading of two
// fields.
//
// Checking auth.token and admin.token by name passes against a response that
// leaked the same secret through a section added later, a nested duplicate, or
// a debug field somebody added while chasing something else. Searching the
// bytes cannot be satisfied by looking in the wrong place.
//
// The client token is the one with the most to lose. The caller already holds
// the admin token — that is how they reached the endpoint — but a reply that
// carried auth.token would hand an operator with read-only administrative
// access a credential for the data port.
func adminConfigRedactsEveryToken(c *runner.Ctx) error {
	tokens, err := configuredAdminTokens(c)
	if err != nil {
		return err
	}
	if tokens.auth == "" || tokens.admin == "" {
		return errors.New("this scenario needs a spec that sets both auth.token and " + adminTokenField)
	}

	resp, err := adminGet(c, "/config", bearer(tokens.admin))
	if err != nil {
		return err
	}
	if resp.status != http.StatusOK {
		return fmt.Errorf("GET /config returned %d: %s", resp.status, strings.TrimSpace(resp.body))
	}

	// The scan.
	if strings.Contains(resp.body, tokens.auth) {
		return fmt.Errorf("GET /config returned the client token somewhere in its body: %s", resp.body)
	}
	if strings.Contains(resp.body, tokens.admin) {
		return fmt.Errorf("GET /config returned the admin token somewhere in its body: %s", resp.body)
	}
	c.Logf("neither token appears anywhere in %d bytes of /config", len(resp.body))

	// A response that returned nothing would pass the scan, so the document has
	// to be shown to be a real one: the non-secret settings are present and
	// correct, and the secret fields are present and marked.
	fields, err := resp.fields()
	if err != nil {
		return err
	}
	if err := adminConfigIsComplete(fields); err != nil {
		return err
	}

	// Last: the tokens are not merely absent from the body, they are absent
	// from the log too. An endpoint that redacted its response and logged the
	// request would have moved the disclosure rather than removed it.
	logs := c.Harness.Logs()
	if strings.Contains(logs, tokens.auth) || strings.Contains(logs, tokens.admin) {
		return errors.New("a token appears in the server log")
	}
	return nil
}

// adminConfigIsComplete checks that /config answered with the configuration and
// not with an empty document that would pass any redaction check.
func adminConfigIsComplete(fields map[string]any) error {
	for _, section := range []string{"node", "server", "admin", "storage", "ttl", "eviction", "logging", "tls", "auth"} {
		if _, present := fields[section]; !present {
			return fmt.Errorf("/config is missing the %s section: %v", section, fields)
		}
	}

	for _, secret := range []struct{ section, field string }{{"auth", "token"}, {"admin", "token"}} {
		section, ok := fields[secret.section].(map[string]any)
		if !ok {
			return fmt.Errorf("/config has no %s section to redact", secret.section)
		}
		value, present := section[secret.field]
		if !present {
			return fmt.Errorf("/config dropped %s.%s rather than redacting it; an operator cannot tell "+
				"a configured token from an absent one", secret.section, secret.field)
		}
		if value == "" {
			return fmt.Errorf("/config reports %s.%s as unset, but the spec configured it",
				secret.section, secret.field)
		}
	}

	server, ok := fields["server"].(map[string]any)
	if !ok {
		return errors.New("/config has no server section")
	}
	if server["bind_addr"] != loopbackAddr {
		return fmt.Errorf("/config reports server.bind_addr as %v, but the harness binds %s",
			server["bind_addr"], loopbackAddr)
	}
	return nil
}

// loopbackAddr is what every harnessed server binds, and therefore what /config
// must report back.
const loopbackAddr = "127.0.0.1"

// adminBindGuardRefusesExposure is the P0 debt, checked end to end.
//
// FEAT-0010 could only warn here, because there was no admin.token to require.
// ADR-0023 always meant the combination — reachable from other hosts, with no
// credential — to be a startup failure, and this asserts that the process now
// refuses to start and says why.
//
// Every case is run out of band, and every one of them is refused before
// anything binds: a server that actually started on a routable interface would
// be reachable from off the machine for the length of the test run.
func adminBindGuardRefusesExposure(c *runner.Ctx) error {
	binary, err := binaryOf(c)
	if err != nil {
		return err
	}

	refused := []struct {
		name    string
		content string
		// mentions is what the message must contain to be actionable. A bare
		// refusal of a value the operator typed on purpose reads as a bug in
		// the server, and their next move is to work around it.
		mentions []string
	}{
		{
			name:    "admin-exposed-no-token.yaml",
			content: guardConfig(c, "  bind_addr: \"0.0.0.0\"\n", ""),
			mentions: []string{
				"admin.bind_addr", "reachable from other hosts", "no credential",
				adminTokenField, "127.0.0.1", "ADR-0023",
			},
		},
		{
			// The client token does not stand in for the admin one; that
			// separation is the reason there are two.
			name: "admin-exposed-client-token-only.yaml",
			content: guardConfig(c, "  bind_addr: \"10.4.2.7\"\n",
				"auth:\n  enabled: true\n  token: \"a-client-token\"\n"),
			mentions: []string{"admin.bind_addr", adminTokenField},
		},
		{
			name: "admin-token-same-as-auth.yaml",
			content: guardConfig(c, "  token: \"shared-secret\"\n",
				"auth:\n  enabled: true\n  token: \"shared-secret\"\n"),
			mentions: []string{adminTokenField, "auth.token", "administrative access"},
		},
	}

	for _, tc := range refused {
		output, err := runRefusedConfig(c, binary, tc.name, tc.content)
		if err != nil {
			return err
		}
		for _, want := range tc.mentions {
			if !strings.Contains(output, want) {
				return fmt.Errorf("%s: the message does not mention %q, so it does not say what to fix: %s",
					tc.name, want, strings.TrimSpace(output))
			}
		}
	}

	return adminBindGuardAllowsAToken(c, binary)
}

// adminBindGuardAllowsAToken is the positive control: with admin.token set, the
// guard does not fire on a non-loopback bind.
//
// It is proved by a configuration that fails for an entirely different reason,
// rather than by starting a server. A server that started here would bind a
// routable interface for as long as the spec ran, which is exactly the exposure
// the guard exists to prevent — and on a developer's machine it would raise a
// firewall prompt on every run.
func adminBindGuardAllowsAToken(c *runner.Ctx, binary serverBinary) error {
	const name = "admin-exposed-with-token.yaml"
	content := guardConfig(c,
		"  bind_addr: \"0.0.0.0\"\n  token: \"an-admin-token\"\n",
		"eviction:\n  policy: \"telepathy\"\n")

	output, err := runRefusedConfig(c, binary, name, content)
	if err != nil {
		return err
	}

	if !strings.Contains(output, "eviction.policy") {
		return fmt.Errorf("%s: expected the unrelated eviction failure, got: %s", name, strings.TrimSpace(output))
	}
	if strings.Contains(output, "reachable from other hosts") {
		return fmt.Errorf("%s: the exposure guard fired even though admin.token was set: %s",
			name, strings.TrimSpace(output))
	}
	c.Logf("with admin.token set, the only complaint is the unrelated one")
	return nil
}

// guardConfig renders a configuration for an out-of-band run: the admin section
// the case is about, any other sections it needs, and the port interlock.
//
// The interlock pins both listeners to the ports this spec's own server is
// already holding, which is a safety property rather than a detail: if the
// guard ever stopped firing, the server under test would otherwise start — on
// 0.0.0.0, the address the guard exists to refuse — and serve an
// unauthenticated admin API for as long as the run allowed.
//
// It is a belt and not a guarantee. Linux refuses a wildcard bind over a
// specific one and the collision stops the launch outright; the BSDs allow it
// under SO_REUSEADDR, which Go sets, so on macOS the server would start
// anyway. The braces are in runRefusedConfig: the run is bounded by the startup
// budget rather than by the harness's 20-second ceiling, and a server that is
// still alive when that expires is reported as the guard regression it is.
func guardConfig(c *runner.Ctx, adminSection, extraSections string) string {
	admin := "admin:\n" + adminSection
	if port := portOf(c.Info().AdminAddr); port != "" {
		admin += "  port: " + port + "\n"
	}

	server := ""
	if port := portOf(c.Info().ClientAddr); port != "" {
		server = "server:\n  client_port: " + port + "\n"
	}

	return server + admin + extraSections
}

func portOf(addr string) string {
	index := strings.LastIndex(addr, ":")
	if index < 0 {
		return ""
	}
	return addr[index+1:]
}

// runRefusedConfig writes a config, runs the binary against it, and requires a
// fast non-zero exit.
func runRefusedConfig(c *runner.Ctx, binary serverBinary, name, content string) (string, error) {
	path := filepath.Join(binary.Root(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return "", fmt.Errorf("writing %s: %w", name, err)
	}

	// Bounded by the startup budget rather than by the harness's own ceiling,
	// so that a configuration which is wrongly accepted is killed in seconds
	// instead of running — possibly on a routable interface — for twenty.
	ctx, cancel := context.WithTimeout(c.Context(), startupBudget)
	defer cancel()

	started := time.Now()
	code, output, err := binary.RunBinary(ctx, "--config", path)
	elapsed := time.Since(started)
	if errors.Is(err, context.DeadlineExceeded) {
		return "", fmt.Errorf("%s: the server was still running after %s, so it accepted a configuration "+
			"that must be refused at startup: %s", name, startupBudget, strings.TrimSpace(output))
	}
	if err != nil {
		return "", fmt.Errorf("running the server with %s: %w", name, err)
	}
	c.Logf("%s: exit %d in %s: %s", name, code, elapsed.Round(time.Millisecond), strings.TrimSpace(output))

	if code == 0 {
		return "", fmt.Errorf("%s: the server started with a configuration that must be refused", name)
	}
	if strings.Contains(output, "address already in use") {
		return "", fmt.Errorf("%s: the configuration passed validation and was stopped only by the port "+
			"interlock, so the startup guard did not fire: %s", name, strings.TrimSpace(output))
	}

	if elapsed > startupBudget {
		return "", fmt.Errorf("%s: the server took %s to refuse the configuration, over the %s budget",
			name, elapsed.Round(time.Millisecond), startupBudget)
	}
	return output, nil
}

// adminTokens is what the spec configured, read back from the file the harness
// wrote so that the scenario and the server cannot disagree about the secrets.
type adminTokens struct {
	auth  string
	admin string
}

func configuredAdminTokens(c *runner.Ctx) (adminTokens, error) {
	binary, err := binaryOf(c)
	if err != nil {
		return adminTokens{}, err
	}

	path := filepath.Join(binary.Root(), "config.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return adminTokens{}, fmt.Errorf("reading the server's config at %s: %w", path, err)
	}

	var document struct {
		Auth struct {
			Token string `yaml:"token"`
		} `yaml:"auth"`
		Admin struct {
			Token string `yaml:"token"`
		} `yaml:"admin"`
	}
	if err := yaml.Unmarshal(raw, &document); err != nil {
		return adminTokens{}, fmt.Errorf("parsing the server's config: %w", err)
	}

	return adminTokens{auth: document.Auth.Token, admin: document.Admin.Token}, nil
}
