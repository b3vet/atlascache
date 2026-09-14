package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServer is a RESP server for the CLI's tests: enough of AtlasCache to
// drive every atlasctl command, and none of it shared with the real one.
//
// Sharing the server would make these tests agree with themselves. What is
// under test here is the CLI — its exit codes, its output, its flags — so the
// server only has to be a plausible one, and a wrong answer from it should
// still produce the right behavior from atlasctl.
type fakeServer struct {
	listener net.Listener

	mu      sync.Mutex
	data    map[string][]byte
	token   string
	fail    string // an error reply returned instead of running the next command
	hangFor time.Duration

	wg sync.WaitGroup
}

type fakeOption func(*fakeServer)

// withToken makes the server require AUTH before anything but PING.
func withToken(token string) fakeOption {
	return func(s *fakeServer) { s.token = token }
}

// withTLS serves TLS with the given certificate.
func withTLS(cert tls.Certificate) fakeOption {
	return func(s *fakeServer) {
		s.listener = tls.NewListener(s.listener, &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		})
	}
}

func newFakeServer(t *testing.T, opts ...fakeOption) *fakeServer {
	t.Helper()

	var config net.ListenConfig
	listener, err := config.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	s := &fakeServer{listener: listener, data: map[string][]byte{}}
	for _, opt := range opts {
		opt(s)
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.serve()
	}()

	t.Cleanup(func() {
		_ = s.listener.Close()
		s.wg.Wait()
	})
	return s
}

func (s *fakeServer) addr() string { return s.listener.Addr().String() }

// failNext makes the next command answer with an error reply.
func (s *fakeServer) failNext(reply string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = reply
}

func (s *fakeServer) set(key string, value []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
}

func (s *fakeServer) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() { _ = conn.Close() }()
			s.session(conn)
		}()
	}
}

func (s *fakeServer) session(conn net.Conn) {
	reader := bufio.NewReader(conn)
	authenticated := false

	for {
		args, err := readCommand(reader)
		if err != nil {
			return
		}
		reply, closes := s.dispatch(args, &authenticated)
		if _, err := io.WriteString(conn, reply); err != nil {
			return
		}
		if closes {
			return
		}
	}
}

// preAuth is the allowlist the real server has: a client negotiates and pings
// before it authenticates.
var preAuth = map[string]bool{"PING": true, "HELLO": true, "AUTH": true, "QUIT": true}

func (s *fakeServer) dispatch(args []string, authenticated *bool) (reply string, closes bool) {
	command := strings.ToUpper(args[0])

	s.mu.Lock()
	// The handshake is not what failNext is aimed at: the SDK sends HELLO
	// before anything a caller asked for, and an injected failure landing
	// there would be testing the fallback path instead.
	if s.fail != "" && command != "HELLO" && command != "AUTH" {
		failure := s.fail
		s.fail = ""
		s.mu.Unlock()
		return "-" + failure + crlf, false
	}
	token, hang := s.token, s.hangFor
	s.mu.Unlock()

	if hang > 0 {
		time.Sleep(hang)
	}

	if command == "AUTH" {
		if token == "" {
			return "-ERR Client sent AUTH, but no password is set" + crlf, false
		}
		if args[len(args)-1] != token {
			return "-WRONGPASS invalid username-password pair" + crlf, false
		}
		*authenticated = true
		return "+OK" + crlf, false
	}
	if token != "" && !*authenticated && !preAuth[command] {
		return "-NOAUTH Authentication required" + crlf, false
	}

	switch command {
	case "HELLO":
		// What a v0.1.0 server answers HELLO 3 with (ADR-0028); the SDK is
		// expected to fall back silently.
		return "-NOPROTO unsupported protocol version" + crlf, false
	case "PING":
		return "+PONG" + crlf, false
	case "QUIT":
		return "+OK" + crlf, true
	case "ECHO":
		return bulk([]byte(args[1])), false
	}
	return s.dataCommand(command, args), false
}

// dataCommand answers the commands that read and write single keys.
func (s *fakeServer) dataCommand(command string, args []string) string {
	switch command {
	case "GET":
		s.mu.Lock()
		value, ok := s.data[args[1]]
		s.mu.Unlock()
		if !ok {
			return "$-1" + crlf
		}
		return bulk(value)

	case "SET":
		s.mu.Lock()
		s.data[args[1]] = []byte(args[2])
		s.mu.Unlock()
		return "+OK" + crlf

	case "DEL":
		var removed int64
		s.mu.Lock()
		for _, key := range args[1:] {
			if _, ok := s.data[key]; ok {
				delete(s.data, key)
				removed++
			}
		}
		s.mu.Unlock()
		return integer(removed)

	case "EXISTS":
		var count int64
		s.mu.Lock()
		for _, key := range args[1:] {
			if _, ok := s.data[key]; ok {
				count++
			}
		}
		s.mu.Unlock()
		return integer(count)

	default:
		return s.keyspaceCommand(command, args)
	}
}

// keyspaceCommand answers the commands that walk or describe the keyspace.
func (s *fakeServer) keyspaceCommand(command string, args []string) string {
	switch command {
	case "DBSIZE":
		s.mu.Lock()
		size := int64(len(s.data))
		s.mu.Unlock()
		return integer(size)

	case "KEYS":
		keys := s.sortedKeys(args[1])
		items := make([]string, 0, len(keys))
		for _, key := range keys {
			items = append(items, bulk([]byte(key)))
		}
		return array(items)

	case "SCAN":
		return s.scan(args)

	case "STATS":
		s.mu.Lock()
		size := int64(len(s.data))
		s.mu.Unlock()
		return array([]string{
			bulk([]byte("keys")), integer(size),
			bulk([]byte("commands_processed")), integer(42),
		})

	case "INFO":
		return bulk([]byte(infoText(args[1:])))

	default:
		return "-ERR unknown command '" + command + "'" + crlf
	}
}

// scan walks the keyspace one page at a time, so that the CLI's iteration is
// exercised rather than answered in a single page.
func (s *fakeServer) scan(args []string) string {
	pattern := ""
	pageSize := 2
	for i := 2; i+1 < len(args); i += 2 {
		switch strings.ToUpper(args[i]) {
		case "MATCH":
			pattern = args[i+1]
		case "COUNT":
			if n, err := strconv.Atoi(args[i+1]); err == nil && n > 0 {
				pageSize = n
			}
		}
	}

	cursor, err := strconv.Atoi(args[1])
	if err != nil || cursor < 0 {
		return "-ERR invalid cursor" + crlf
	}

	keys := s.sortedKeys(pattern)
	end := min(cursor+pageSize, len(keys))
	if cursor > len(keys) {
		return "-ERR invalid cursor" + crlf
	}

	next := end
	if end >= len(keys) {
		next = 0
	}

	page := make([]string, 0, end-cursor)
	for _, key := range keys[cursor:end] {
		page = append(page, bulk([]byte(key)))
	}
	return array([]string{bulk([]byte(strconv.Itoa(next))), array(page)})
}

// infoText is INFO's format: CRLF-terminated key:value lines under # Section
// headers, which is what every tool that has ever parsed a Redis expects.
func infoText(sections []string) string {
	server := "# Server" + crlf + "atlascache_version:0.1.0-test" + crlf + "go_version:go1.24" + crlf
	keyspace := "# Keyspace" + crlf + "db0:keys=3" + crlf

	if len(sections) == 1 && strings.EqualFold(sections[0], "server") {
		return server
	}
	return server + crlf + keyspace
}

// crlf is RESP's line ending.
const crlf = "\r\n"

func bulk(value []byte) string {
	return "$" + strconv.Itoa(len(value)) + crlf + string(value) + crlf
}

func integer(n int64) string { return ":" + strconv.FormatInt(n, 10) + crlf }

func array(items []string) string {
	var b strings.Builder
	b.WriteString("*" + strconv.Itoa(len(items)) + crlf)
	for _, item := range items {
		b.WriteString(item)
	}
	return b.String()
}

// readCommand reads one RESP array of bulk strings, the only request form the
// SDK sends.
func readCommand(r *bufio.Reader) ([]string, error) {
	header, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	count, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(header, "*")))
	if err != nil || count <= 0 {
		return nil, errors.New("not a command array: " + header)
	}

	args := make([]string, 0, count)
	for range count {
		sizeLine, readErr := r.ReadString('\n')
		if readErr != nil {
			return nil, readErr
		}
		size, convErr := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(sizeLine, "$")))
		if convErr != nil || size < 0 {
			return nil, errors.New("not a bulk string: " + sizeLine)
		}
		payload := make([]byte, size+2)
		if _, readErr = io.ReadFull(r, payload); readErr != nil {
			return nil, readErr
		}
		args = append(args, string(payload[:size]))
	}
	return args, nil
}

// certificate generates a self-signed certificate for 127.0.0.1 and writes the
// PEM the CLI's --tls-ca flag reads.
func certificate(t *testing.T) (tls.Certificate, string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "atlasctl-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating a certificate: %v", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshaling the key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("building the key pair: %v", err)
	}

	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, certPEM, 0o600); err != nil {
		t.Fatalf("writing the CA file: %v", err)
	}
	return pair, path
}

// deadAddr returns an address nothing is listening on.
func deadAddr(t *testing.T) string {
	t.Helper()

	var config net.ListenConfig
	listener, err := config.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("closing the listener: %v", err)
	}
	return addr
}

// sortedKeys is what KEYS and SCAN answer with, in a fixed order so that an
// assertion can be written against them.
func (s *fakeServer) sortedKeys(pattern string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var keys []string
	for key := range s.data {
		if matches(pattern, key) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

// matches is a glob with just enough to it for these tests: a trailing star,
// or an exact match.
func matches(pattern, key string) bool {
	switch {
	case pattern == "" || pattern == "*":
		return true
	case strings.HasSuffix(pattern, "*"):
		return strings.HasPrefix(key, strings.TrimSuffix(pattern, "*"))
	default:
		return pattern == key
	}
}
