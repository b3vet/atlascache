package scenarios

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/b3vet/atlascache/pkg/client"
	"github.com/b3vet/atlascache/test/e2e/runner"
)

// The SDK specs are the one part of this suite that runs through the code under
// test rather than around it (ADR-0003, P3 §6).
//
// Everywhere else the suite dials the server with its own client, so that a bug
// in the SDK cannot hide a bug in the server. Here that is inverted on purpose:
// these specs drive pkg/client, so an SDK regression shows up as an sdk-* spec
// failing while every protocol spec stays green — which is what tells a reader
// which of the two broke.
//
// The SDK has thorough unit tests of its own, including against a real server
// process. These do not repeat them. What they add is the gate: a behavior
// asserted only in the SDK module's tests is checked when someone runs those
// tests, and a behavior asserted here is checked by `make e2e-full`.

func init() {
	runner.RegisterScenario("sdk_typed_methods_round_trip", sdkTypedMethodsRoundTrip)
	runner.RegisterScenario("sdk_do_sends_arbitrary_commands", sdkDoSendsArbitraryCommands)
	runner.RegisterScenario("sdk_tls_and_auth", sdkTLSAndAuth)
}

// sdkBudget bounds any one SDK call a scenario makes. It is a hang detector,
// not a performance assertion: every call below is a loopback round trip.
const sdkBudget = 10 * time.Second

// binaryValue is the value every round-trip assertion uses: a null byte, a
// two-byte sequence that is not valid UTF-8, a newline and a CR.
//
// A cache stores serialized blobs, and every layer between a caller and the
// bytes — the SDK's arguments, the codec, the CLI's stdout — has a plausible
// bug that survives ASCII and loses this.
var binaryValue = []byte{'a', 0x00, 'b', 0xff, 0xfe, '\n', '\r', 'z'}

// sdkClient builds an SDK client pointed at the server under test, with the
// spec's TLS and auth already applied.
//
// A scenario that needed to assemble those itself would be asserting on its own
// setup as much as on the SDK; this way `sdkClient(c)` is the client a user
// would have, and the options a scenario passes are the ones it is testing.
func sdkClient(c *runner.Ctx, opts ...client.Option) (client.Client, error) {
	base := []client.Option{client.WithAddr(c.Info().ClientAddr)}

	if harness, ok := c.Harness.(tlsHarness); ok && harness.TLSEnabled() {
		config, err := harness.ClientTLSConfig()
		if err != nil {
			return nil, err
		}
		base = append(base, client.WithTLS(config))
	}

	token, err := specToken(c)
	if err != nil {
		return nil, err
	}
	if token != "" {
		base = append(base, client.WithAuth(token))
	}

	sdk, err := client.New(append(base, opts...)...)
	if err != nil {
		return nil, fmt.Errorf("building the SDK client: %w", err)
	}
	return sdk, nil
}

// specToken reads auth.token out of the config the harness wrote, so that a
// scenario and the server it is talking to cannot disagree about the secret.
// An empty string means the spec did not enable authentication.
func specToken(c *runner.Ctx) (string, error) {
	binary, ok := c.Harness.(serverBinary)
	if !ok {
		return "", nil
	}

	raw, err := os.ReadFile(filepath.Join(binary.Root(), "config.yaml"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("reading the server's config: %w", err)
	}

	var config struct {
		Auth struct {
			Token string `yaml:"token"`
		} `yaml:"auth"`
	}
	if err := yaml.Unmarshal(raw, &config); err != nil {
		return "", fmt.Errorf("parsing the server's config: %w", err)
	}
	return config.Auth.Token, nil
}

// sdkTypedMethodsRoundTrip drives every typed method against a live server.
//
// The assertions that matter are the ones where a plausible implementation
// gives a different answer: a miss is not an error, an empty value is not a
// miss, a value is bytes rather than text, and a scan is over when its cursor
// says so rather than when a page comes back short.
func sdkTypedMethodsRoundTrip(c *runner.Ctx) error {
	sdk, err := sdkClient(c)
	if err != nil {
		return err
	}
	defer func() { _ = sdk.Close() }()

	ctx := c.Context()

	if err := sdk.Ping(ctx); err != nil {
		return fmt.Errorf("PING through the SDK: %w", err)
	}

	if err := sdkValuesAndMisses(c, sdk); err != nil {
		return err
	}
	if err := sdkStringsAndCounts(c, sdk); err != nil {
		return err
	}
	if err := sdkExpiry(c, sdk); err != nil {
		return err
	}
	return sdkKeyspaceAndIntrospection(c, sdk)
}

// sdkValuesAndMisses covers the three answers a GET can give — a value, an
// empty value, and no key at all — and the bytes that survive the round trip.
func sdkValuesAndMisses(c *runner.Ctx, sdk client.Client) error {
	ctx := c.Context()

	// A value of arbitrary bytes, back byte for byte. []byte is the primitive
	// for exactly this reason (FEAT-0027).
	if err := sdk.Set(ctx, "sdk:binary", binaryValue, 0); err != nil {
		return fmt.Errorf("SET of a binary value: %w", err)
	}
	value, found, err := sdk.Get(ctx, "sdk:binary")
	if err != nil {
		return fmt.Errorf("GET of a binary value: %w", err)
	}
	if !found {
		return errors.New("a key that was just written reads as missing")
	}
	if !bytes.Equal(value, binaryValue) {
		return fmt.Errorf("a binary value came back as %q, want %q", value, binaryValue)
	}
	c.Logf("a %d-byte value with a null, invalid UTF-8 and a CRLF round-tripped", len(binaryValue))

	// The distinction ADR-0022 was amended for: found tells a missing key from
	// a key holding nothing, and a two-value signature could not.
	if setErr := sdk.Set(ctx, "sdk:empty", []byte{}, 0); setErr != nil {
		return fmt.Errorf("SET of an empty value: %w", setErr)
	}
	empty, found, err := sdk.Get(ctx, "sdk:empty")
	if err != nil {
		return fmt.Errorf("GET of an empty value: %w", err)
	}
	if !found {
		return errors.New("a key holding an empty value reads as missing; the SDK has lost the distinction")
	}
	if len(empty) != 0 {
		return fmt.Errorf("a key written empty came back holding %q", empty)
	}

	missing, found, err := sdk.Get(ctx, "sdk:never-written")
	if err != nil {
		return fmt.Errorf("a cache miss came back as an error, and a miss is not one: %w", err)
	}
	if found || missing != nil {
		return fmt.Errorf("a key that was never written reads as present, holding %q", missing)
	}
	c.Logf("a miss and an empty value are distinguishable")

	return nil
}

// sdkStringsAndCounts covers the string conveniences and the counting commands.
func sdkStringsAndCounts(c *runner.Ctx, sdk client.Client) error {
	ctx := c.Context()

	if err := sdk.SetString(ctx, "sdk:text", "hello", 0); err != nil {
		return fmt.Errorf("a SetString: %w", err)
	}
	text, found, err := sdk.GetString(ctx, "sdk:text")
	if err != nil || !found || text != "hello" {
		return fmt.Errorf("a GetString returned (%q, %v, %v), want (\"hello\", true, nil)", text, found, err)
	}

	stored, err := sdk.SetNX(ctx, "sdk:nx", []byte("first"))
	if err != nil {
		return fmt.Errorf("SETNX on a free key: %w", err)
	}
	if !stored {
		return errors.New("SETNX reported that it did not store a value under a free key")
	}
	stored, err = sdk.SetNX(ctx, "sdk:nx", []byte("second"))
	if err != nil {
		return fmt.Errorf("SETNX on a taken key: %w", err)
	}
	if stored {
		return errors.New("SETNX overwrote a key that was already there")
	}

	// A repeated key counts once per mention, which is what the server does and
	// what a caller written against Redis expects.
	count, err := sdk.Exists(ctx, "sdk:text", "sdk:text", "sdk:never-written")
	if err != nil {
		return fmt.Errorf("EXISTS: %w", err)
	}
	if count != 2 {
		return fmt.Errorf("EXISTS of one present key named twice and one absent answered %d, want 2", count)
	}

	removed, err := sdk.Del(ctx, "sdk:nx", "sdk:never-written")
	if err != nil {
		return fmt.Errorf("DEL: %w", err)
	}
	if removed != 1 {
		return fmt.Errorf("DEL of one present and one absent key removed %d, want 1", removed)
	}

	echoed, err := sdk.Echo(ctx, binaryValue)
	if err != nil {
		return fmt.Errorf("ECHO: %w", err)
	}
	if !bytes.Equal(echoed, binaryValue) {
		return fmt.Errorf("ECHO answered %q, want the bytes it was sent", echoed)
	}
	return nil
}

// sdkExpiry covers TTL, EXPIRE and the two sentinels a caller has to be able to
// tell apart.
func sdkExpiry(c *runner.Ctx, sdk client.Client) error {
	ctx := c.Context()

	ttl, err := sdk.TTL(ctx, "sdk:text")
	if err != nil {
		return fmt.Errorf("TTL of a key with no expiry: %w", err)
	}
	if ttl != client.TTLNoExpiry {
		return fmt.Errorf("TTL of a key with no expiry answered %s, want TTLNoExpiry", ttl)
	}

	ttl, err = sdk.TTL(ctx, "sdk:never-written")
	if err != nil {
		return fmt.Errorf("TTL of a missing key: %w", err)
	}
	if ttl != client.TTLNoKey {
		return fmt.Errorf("TTL of a missing key answered %s, want TTLNoKey", ttl)
	}

	return sdkExpireAndSetTTL(c, sdk)
}

// sdkExpireAndSetTTL covers attaching an expiry, after the fact and at write
// time.
func sdkExpireAndSetTTL(c *runner.Ctx, sdk client.Client) error {
	ctx := c.Context()

	applied, err := sdk.Expire(ctx, "sdk:text", time.Minute)
	if err != nil {
		return fmt.Errorf("EXPIRE: %w", err)
	}
	if !applied {
		return errors.New("EXPIRE reported that it found no key to expire")
	}
	ttl, err := sdk.TTL(ctx, "sdk:text")
	if err != nil {
		return fmt.Errorf("TTL after EXPIRE: %w", err)
	}
	if ttl <= 0 || ttl > time.Minute {
		return fmt.Errorf("TTL after a one minute EXPIRE answered %s", ttl)
	}

	applied, err = sdk.Expire(ctx, "sdk:never-written", time.Minute)
	if err != nil {
		return fmt.Errorf("EXPIRE of a missing key: %w", err)
	}
	if applied {
		return errors.New("EXPIRE reported that it expired a key that does not exist")
	}

	// A TTL passed to Set, rather than attached afterwards.
	if setErr := sdk.Set(ctx, "sdk:ttl", []byte("v"), 30*time.Second); setErr != nil {
		return fmt.Errorf("SET with a TTL: %w", setErr)
	}
	ttl, err = sdk.TTL(ctx, "sdk:ttl")
	if err != nil {
		return fmt.Errorf("TTL of a key written with one: %w", err)
	}
	if ttl <= 0 || ttl > 30*time.Second {
		return fmt.Errorf("SET EX 30 left a TTL of %s", ttl)
	}
	c.Logf("expiry round-trips, and both TTL sentinels are distinguishable")
	return nil
}

// sdkKeyspaceAndIntrospection covers KEYS, SCAN, DBSIZE, INFO and STATS.
func sdkKeyspaceAndIntrospection(c *runner.Ctx, sdk client.Client) error {
	ctx := c.Context()

	for i := range 20 {
		key := fmt.Sprintf("sdk:walk:%02d", i)
		if err := sdk.Set(ctx, key, []byte("v"), 0); err != nil {
			return fmt.Errorf("seeding %s: %w", key, err)
		}
	}

	keys, err := sdk.Keys(ctx, "sdk:walk:*")
	if err != nil {
		return fmt.Errorf("KEYS: %w", err)
	}
	if len(keys) != 20 {
		return fmt.Errorf("KEYS sdk:walk:* returned %d keys, want 20", len(keys))
	}

	// A scan is over when the cursor says so, and a short page says nothing:
	// COUNT bounds the work a page does, not the keys it finds.
	seen := map[string]bool{}
	cursor := client.ScanStart
	pages := 0
	for {
		page, scanErr := sdk.Scan(ctx, cursor, "sdk:walk:*", 3)
		if scanErr != nil {
			return fmt.Errorf("SCAN at cursor %s: %w", cursor, scanErr)
		}
		pages++
		for _, key := range page.Keys {
			seen[key] = true
		}
		cursor = page.Cursor
		if page.Done() {
			break
		}
		if pages > 1000 {
			return errors.New("SCAN did not finish within 1000 pages")
		}
	}
	if len(seen) != 20 {
		return fmt.Errorf("a full scan saw %d of 20 keys over %d pages", len(seen), pages)
	}
	c.Logf("a full scan saw every key over %d pages", pages)

	return sdkIntrospection(c, sdk)
}

// sdkIntrospection covers what the server reports about itself, and the client
// lifecycle at the end of it.
func sdkIntrospection(c *runner.Ctx, sdk client.Client) error {
	ctx := c.Context()

	size, err := sdk.DBSize(ctx)
	if err != nil {
		return fmt.Errorf("DBSIZE: %w", err)
	}
	if size < 20 {
		return fmt.Errorf("DBSIZE answered %d with at least 20 keys written", size)
	}

	info, err := sdk.Info(ctx, "server")
	if err != nil {
		return fmt.Errorf("INFO: %w", err)
	}
	if !strings.Contains(info, "atlascache_version") {
		return fmt.Errorf("INFO server does not report atlascache_version: %q", info)
	}

	stats, err := sdk.Stats(ctx)
	if err != nil {
		return fmt.Errorf("STATS: %w", err)
	}
	if _, ok := stats.Value("keys"); !ok {
		return fmt.Errorf("STATS reported no keys counter; it returned %d counters", len(stats))
	}
	if _, ok := stats.Value("commands_processed"); !ok {
		return errors.New("STATS reported no commands_processed counter")
	}

	// Close is idempotent, which is what a caller with a defer and an explicit
	// close both need.
	if err := sdk.Close(); err != nil {
		return fmt.Errorf("closing the client: %w", err)
	}
	if err := sdk.Close(); err != nil {
		return fmt.Errorf("closing the client a second time: %w", err)
	}
	if err := sdk.Ping(ctx); !errors.Is(err, client.ErrClosed) {
		return fmt.Errorf("a call on a closed client answered %v, want ErrClosed", err)
	}
	return nil
}

// sdkDoSendsArbitraryCommands covers the escape hatch that keeps SDK releases
// decoupled from server releases (ADR-0022).
func sdkDoSendsArbitraryCommands(c *runner.Ctx) error {
	sdk, err := sdkClient(c)
	if err != nil {
		return err
	}
	defer func() { _ = sdk.Close() }()

	ctx := c.Context()

	reply, err := sdk.Do(ctx, "SET", "do:key", binaryValue)
	if err != nil {
		return fmt.Errorf("a Do of SET: %w", err)
	}
	if text, textErr := reply.Text(); textErr != nil || text != "OK" {
		return fmt.Errorf("a Do of SET answered (%q, %v), want OK", text, textErr)
	}

	reply, err = sdk.Do(ctx, "GET", "do:key")
	if err != nil {
		return fmt.Errorf("a Do of GET: %w", err)
	}
	value, err := reply.Bytes()
	if err != nil {
		return fmt.Errorf("reading the bytes of a Do GET: %w", err)
	}
	if !bytes.Equal(value, binaryValue) {
		return fmt.Errorf("a Do of GET answered %q, want the bytes that were stored", value)
	}

	// A miss through Do is a nil reply rather than an error, the same
	// distinction the typed Get makes with found.
	reply, err = sdk.Do(ctx, "GET", "do:missing")
	if err != nil {
		return fmt.Errorf("a Do of GET on a missing key: %w", err)
	}
	if !reply.IsNil() {
		return fmt.Errorf("a Do of GET on a missing key answered a %s reply, want nil", reply.Type)
	}

	// Numbers and durations are rendered by the SDK rather than by the caller,
	// which is the difference between Do and hand-building a wire frame.
	if _, err = sdk.Do(ctx, "SET", "do:ttl", "v", "EX", 60); err != nil {
		return fmt.Errorf("a Do of SET with an integer argument: %w", err)
	}
	reply, err = sdk.Do(ctx, "TTL", "do:ttl")
	if err != nil {
		return fmt.Errorf("a Do of TTL: %w", err)
	}
	seconds, err := reply.Int64()
	if err != nil {
		return fmt.Errorf("reading the integer of a Do TTL: %w", err)
	}
	if seconds <= 0 || seconds > 60 {
		return fmt.Errorf("a Do of SET ... EX 60 left a TTL of %d seconds", seconds)
	}

	return sdkDoAggregatesAndRefusals(c, sdk)
}

// sdkDoAggregatesAndRefusals covers the reply shapes with elements in them, and
// the two refusals that keep a mistake from being stored.
func sdkDoAggregatesAndRefusals(c *runner.Ctx, sdk client.Client) error {
	ctx := c.Context()

	// An array reply, and a map one. STATS is a map to the server and an array
	// on a RESP2 wire, and the SDK presents it the same way either way.
	reply, err := sdk.Do(ctx, "KEYS", "do:*")
	if err != nil {
		return fmt.Errorf("a Do of KEYS: %w", err)
	}
	keys, err := reply.Strings()
	if err != nil {
		return fmt.Errorf("reading the strings of a Do KEYS: %w", err)
	}
	if len(keys) != 2 {
		return fmt.Errorf("a Do of KEYS do:* answered %d keys, want 2: %v", len(keys), keys)
	}

	reply, err = sdk.Do(ctx, "STATS")
	if err != nil {
		return fmt.Errorf("a Do of STATS: %w", err)
	}
	pairs, err := reply.Map()
	if err != nil {
		return fmt.Errorf("reading the map of a Do STATS: %w", err)
	}
	if _, ok := pairs["keys"]; !ok {
		return fmt.Errorf("a Do of STATS answered %d pairs, none of them keys", len(pairs))
	}

	// A command the server refuses is an error with a category, not a panic and
	// not a nil reply — and the connection is still usable afterwards, which is
	// what makes a typo survivable.
	_, err = sdk.Do(ctx, "NOSUCHCOMMAND", "arg")
	if !errors.Is(err, client.ErrServer) {
		return fmt.Errorf("a Do of an unknown command answered %v, want an ErrServer", err)
	}
	if client.Retryable(err) {
		return errors.New("a refused command reports as retryable; retrying it would only repeat the refusal")
	}
	if pingErr := sdk.Ping(c.Context()); pingErr != nil {
		return fmt.Errorf("the connection did not survive a refused command: %w", pingErr)
	}

	// Do rejects what it cannot render rather than guessing, which is what
	// keeps a mistyped argument from being stored as Go formatting.
	if _, err = sdk.Do(ctx, "SET", "do:bad", struct{ A int }{1}); err == nil {
		return errors.New("a Do accepted a struct as an argument; it must refuse what it cannot render")
	}
	c.Logf("Do reached every reply shape and refused an argument it could not render")
	return nil
}

// sdkTLSAndAuth is the connection-setup half of the SDK: the handshake, the
// credential, and the failures of each.
//
// The negative cases are the point. A client that "works with TLS" but accepts
// any certificate, or one that reports a refused token as a network failure,
// passes a positive test and fails its user.
func sdkTLSAndAuth(c *runner.Ctx) error {
	harness, err := tlsOf(c)
	if err != nil {
		return err
	}
	token, err := specToken(c)
	if err != nil {
		return err
	}
	if token == "" {
		return errors.New("this scenario needs a spec with auth.token set")
	}

	// The positive control: verified TLS, correct token.
	sdk, err := sdkClient(c)
	if err != nil {
		return err
	}
	defer func() { _ = sdk.Close() }()

	if setErr := sdk.Set(c.Context(), "sdk:tls", binaryValue, 0); setErr != nil {
		return fmt.Errorf("SET over an authenticated TLS connection: %w", setErr)
	}
	value, found, err := sdk.Get(c.Context(), "sdk:tls")
	if err != nil || !found || !bytes.Equal(value, binaryValue) {
		return fmt.Errorf("GET over an authenticated TLS connection returned (%q, %v, %v)", value, found, err)
	}
	c.Logf("TLS and auth together serve a binary value")

	// The two-argument AUTH form, which every client written against Redis 6
	// ACLs sends unconditionally (ADR-0020).
	config, err := harness.ClientTLSConfig()
	if err != nil {
		return err
	}
	userClient, err := client.New(
		client.WithAddr(c.Info().ClientAddr),
		client.WithTLS(config),
		client.WithUserAuth("default", token),
	)
	if err != nil {
		return fmt.Errorf("building a client with the two-argument AUTH form: %w", err)
	}
	defer func() { _ = userClient.Close() }()
	if err := userClient.Ping(c.Context()); err != nil {
		return fmt.Errorf("the two-argument AUTH form was refused: %w", err)
	}

	if err := sdkRejectsBadCredentials(c, harness); err != nil {
		return err
	}
	return sdkRejectsBadTransport(c, harness)
}

// sdkRejectsBadCredentials checks that a refused token is an ErrAuth, and that
// no token at all is refused at the first gated command rather than silently.
func sdkRejectsBadCredentials(c *runner.Ctx, harness tlsHarness) error {
	config, err := harness.ClientTLSConfig()
	if err != nil {
		return err
	}

	wrong, err := client.New(
		client.WithAddr(c.Info().ClientAddr),
		client.WithTLS(config),
		client.WithAuth("not-the-token"),
	)
	if err != nil {
		return fmt.Errorf("building a client with a wrong token: %w", err)
	}
	defer func() { _ = wrong.Close() }()

	err = wrong.Ping(c.Context())
	if !errors.Is(err, client.ErrAuth) {
		return fmt.Errorf("a wrong token answered %v, want an ErrAuth", err)
	}
	if client.Retryable(err) {
		return errors.New("a refused token reports as retryable; every retry is another failure in the server's log")
	}
	var detail *client.Error
	if !errors.As(err, &detail) {
		return fmt.Errorf("a wrong token did not produce a *client.Error: %v", err)
	}
	if detail.Kind != "WRONGPASS" {
		return fmt.Errorf("a wrong token reported kind %q, want WRONGPASS", detail.Kind)
	}

	// No credential at all. PING is on the pre-auth allowlist, so the refusal
	// arrives at the first command that is not — which is the case a client
	// that authenticated lazily would get wrong.
	anonymous, err := client.New(client.WithAddr(c.Info().ClientAddr), client.WithTLS(config))
	if err != nil {
		return fmt.Errorf("building a client with no token: %w", err)
	}
	defer func() { _ = anonymous.Close() }()

	if _, _, err = anonymous.Get(c.Context(), "sdk:tls"); !errors.Is(err, client.ErrAuth) {
		return fmt.Errorf("a GET with no credential answered %v, want an ErrAuth", err)
	}
	c.Logf("a wrong token and a missing one are both ErrAuth")
	return nil
}

// sdkRejectsBadTransport checks that the SDK's TLS actually verifies, and that
// plaintext against a TLS port fails rather than hanging.
func sdkRejectsBadTransport(c *runner.Ctx, _ tlsHarness) error {
	token, err := specToken(c)
	if err != nil {
		return err
	}

	// An empty trust store: the server's certificate is self-signed, so this
	// must fail. A client that "supports TLS" by accepting any certificate
	// passes every positive test and protects nobody.
	untrusting, err := client.New(
		client.WithAddr(c.Info().ClientAddr),
		client.WithTLS(&tls.Config{MinVersion: tls.VersionTLS13}),
		client.WithAuth(token),
		client.WithReconnectWindow(0),
	)
	if err != nil {
		return fmt.Errorf("building a client that trusts nothing: %w", err)
	}
	defer func() { _ = untrusting.Close() }()

	if pingErr := untrusting.Ping(c.Context()); pingErr == nil {
		return errors.New("a client trusting no certificate authority completed a handshake with a self-signed server")
	}

	// Plaintext against a TLS port. The failure must arrive rather than hang:
	// a client left waiting looks like a wedged server, and that is the
	// diagnosis its operator will pursue.
	plaintext, err := client.New(
		client.WithAddr(c.Info().ClientAddr),
		client.WithAuth(token),
		client.WithReconnectWindow(0),
		client.WithDialTimeout(sdkBudget),
		client.WithReadTimeout(sdkBudget),
	)
	if err != nil {
		return fmt.Errorf("building a plaintext client: %w", err)
	}
	defer func() { _ = plaintext.Close() }()

	started := time.Now()
	err = plaintext.Ping(c.Context())
	elapsed := time.Since(started)
	if err == nil {
		return errors.New("a plaintext client was served by a TLS port")
	}
	if elapsed >= sdkBudget {
		return fmt.Errorf("a plaintext client took %s to fail against a TLS port", elapsed)
	}
	if errors.Is(err, client.ErrAuth) {
		return fmt.Errorf("a plaintext client against a TLS port reported an authentication failure: %v", err)
	}
	c.Logf("an untrusted certificate and a plaintext dial both fail, the latter in %s", elapsed.Round(time.Millisecond))
	return nil
}
