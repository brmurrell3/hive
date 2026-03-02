// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build unit

package cluster

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brmurrell3/hive/internal/state"
	"github.com/brmurrell3/hive/internal/testutil"
	"github.com/brmurrell3/hive/internal/types"
	"github.com/nats-io/nats.go"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func testStore(t *testing.T) *state.Store {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	store, err := state.NewStore(path, testLogger())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// embeddedRootConfig returns a minimal valid embedded-mode Config for a root node.
func embeddedRootConfig() Config {
	return Config{
		Role:     RoleRoot,
		NATSMode: "embedded",
	}
}

// generateSelfSignedCert creates a minimal self-signed ECDSA certificate and
// private key, returning them as PEM-encoded byte slices. Intended only for
// unit tests; the cert is valid for 1 hour.
func generateSelfSignedCert(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating private key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		IsCA:         true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}

	var certBuf bytes.Buffer
	if err := pem.Encode(&certBuf, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}); err != nil {
		t.Fatalf("encoding cert PEM: %v", err)
	}

	privDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshaling private key: %v", err)
	}
	var keyBuf bytes.Buffer
	if err := pem.Encode(&keyBuf, &pem.Block{Type: "EC PRIVATE KEY", Bytes: privDER}); err != nil {
		t.Fatalf("encoding key PEM: %v", err)
	}

	return certBuf.Bytes(), keyBuf.Bytes()
}

// writeCertFiles writes certPEM and keyPEM to temp files and returns their paths.
func writeCertFiles(t *testing.T, certPEM, keyPEM []byte) (certFile, keyFile string) {
	t.Helper()
	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, certPEM, 0600); err != nil {
		t.Fatalf("writing cert file: %v", err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0600); err != nil {
		t.Fatalf("writing key file: %v", err)
	}
	return certFile, keyFile
}

// ---------------------------------------------------------------------------
// NewCluster
// ---------------------------------------------------------------------------

func TestNewCluster_EmbeddedRoot(t *testing.T) {
	c := NewCluster(embeddedRootConfig(), testStore(t), testLogger())
	if c == nil {
		t.Fatal("expected non-nil Cluster")
	}
	if c.cfg.Role != RoleRoot {
		t.Errorf("role = %q, want %q", c.cfg.Role, RoleRoot)
	}
	if c.cfg.NATSMode != "embedded" {
		t.Errorf("nats_mode = %q, want %q", c.cfg.NATSMode, "embedded")
	}
	if c.running {
		t.Error("expected running to be false before Start")
	}
	if c.stopCh == nil {
		t.Error("expected non-nil stopCh channel")
	}
	if c.stopped == nil {
		t.Error("expected non-nil stopped channel")
	}
}

func TestNewCluster_WorkerConfig(t *testing.T) {
	cfg := Config{
		Role:      RoleWorker,
		NATSMode:  "external",
		NATSUrls:  []string{"nats://127.0.0.1:4222"},
		AuthToken: "secret",
	}
	c := NewCluster(cfg, testStore(t), testLogger())
	if c == nil {
		t.Fatal("expected non-nil Cluster")
	}
	if c.cfg.Role != RoleWorker {
		t.Errorf("role = %q, want %q", c.cfg.Role, RoleWorker)
	}
	if c.cfg.AuthToken != "secret" {
		t.Errorf("auth_token = %q, want %q", c.cfg.AuthToken, "secret")
	}
}

// TestNewCluster_NATSUrlsDeepCopy verifies the constructor deep-copies the
// NATSUrls slice so that mutating the original does not affect the Cluster.
func TestNewCluster_NATSUrlsDeepCopy(t *testing.T) {
	original := []string{"nats://host1:4222", "nats://host2:4222"}
	cfg := Config{
		Role:     RoleWorker,
		NATSMode: "external",
		NATSUrls: original,
	}
	c := NewCluster(cfg, testStore(t), testLogger())

	// Mutate the original after construction.
	original[0] = "nats://mutated:9999"

	if c.cfg.NATSUrls[0] == "nats://mutated:9999" {
		t.Error("NATSUrls not deep-copied: caller mutation affected Cluster config")
	}
}

// TestNewCluster_NilNATSUrls verifies nil NATSUrls is handled without panicking.
func TestNewCluster_NilNATSUrls(t *testing.T) {
	cfg := Config{
		Role:     RoleRoot,
		NATSMode: "embedded",
		NATSUrls: nil,
	}
	c := NewCluster(cfg, testStore(t), testLogger())
	if c == nil {
		t.Fatal("expected non-nil Cluster")
	}
	if c.cfg.NATSUrls != nil {
		t.Error("expected NATSUrls to remain nil for nil input")
	}
}

// ---------------------------------------------------------------------------
// IsRoot
// ---------------------------------------------------------------------------

func TestIsRoot(t *testing.T) {
	store := testStore(t)
	logger := testLogger()

	root := NewCluster(Config{Role: RoleRoot, NATSMode: "embedded"}, store, logger)
	if !root.IsRoot() {
		t.Error("expected IsRoot() == true for RoleRoot")
	}

	worker := NewCluster(Config{Role: RoleWorker, NATSMode: "embedded"}, store, logger)
	if worker.IsRoot() {
		t.Error("expected IsRoot() == false for RoleWorker")
	}
}

// ---------------------------------------------------------------------------
// NATSMode / NATSUrls accessors
// ---------------------------------------------------------------------------

func TestNATSMode(t *testing.T) {
	c := NewCluster(embeddedRootConfig(), testStore(t), testLogger())
	if c.NATSMode() != "embedded" {
		t.Errorf("NATSMode() = %q, want %q", c.NATSMode(), "embedded")
	}
}

func TestNATSUrls_EmbeddedReturnsNil(t *testing.T) {
	c := NewCluster(embeddedRootConfig(), testStore(t), testLogger())
	if c.NATSUrls() != nil {
		t.Errorf("NATSUrls() = %v, want nil for embedded mode", c.NATSUrls())
	}
}

func TestNATSUrls_ExternalReturnsCopy(t *testing.T) {
	urls := []string{"nats://host1:4222", "nats://host2:4222"}
	cfg := Config{
		Role:     RoleWorker,
		NATSMode: "external",
		NATSUrls: urls,
	}
	c := NewCluster(cfg, testStore(t), testLogger())

	got := c.NATSUrls()
	if len(got) != len(urls) {
		t.Fatalf("NATSUrls() len = %d, want %d", len(got), len(urls))
	}
	for i, u := range urls {
		if got[i] != u {
			t.Errorf("NATSUrls()[%d] = %q, want %q", i, got[i], u)
		}
	}

	// Mutating the returned slice must not affect the internal copy.
	got[0] = "nats://mutated:9999"
	got2 := c.NATSUrls()
	if got2[0] == "nats://mutated:9999" {
		t.Error("NATSUrls() returned a reference; mutation affected internal state")
	}
}

// ---------------------------------------------------------------------------
// Start – embedded mode lifecycle
// ---------------------------------------------------------------------------

func TestStart_EmbeddedMode(t *testing.T) {
	c := NewCluster(embeddedRootConfig(), testStore(t), testLogger())

	if err := c.Start(); err != nil {
		t.Fatalf("Start() error = %v, want nil", err)
	}
	defer c.Stop()

	c.mu.Lock()
	running := c.running
	c.mu.Unlock()
	if !running {
		t.Error("expected running == true after Start()")
	}
}

func TestStart_AlreadyRunning(t *testing.T) {
	c := NewCluster(embeddedRootConfig(), testStore(t), testLogger())

	if err := c.Start(); err != nil {
		t.Fatalf("first Start() error = %v", err)
	}
	defer c.Stop()

	err := c.Start()
	if err == nil {
		t.Error("expected error on second Start(), got nil")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("error = %q, want message containing \"already running\"", err.Error())
	}
}

func TestStart_InvalidRole(t *testing.T) {
	cfg := Config{
		Role:     Role("bogus-role"),
		NATSMode: "embedded",
	}
	c := NewCluster(cfg, testStore(t), testLogger())

	err := c.Start()
	if err == nil {
		t.Fatal("expected error for invalid role, got nil")
	}
	if !strings.Contains(err.Error(), "invalid Role") {
		t.Errorf("error = %q, want message containing \"invalid Role\"", err.Error())
	}
}

func TestStart_EmptyRole(t *testing.T) {
	cfg := Config{
		Role:     Role(""),
		NATSMode: "embedded",
	}
	c := NewCluster(cfg, testStore(t), testLogger())

	if err := c.Start(); err == nil {
		t.Fatal("expected error for empty role, got nil")
	}
}

func TestStart_InvalidNATSMode(t *testing.T) {
	cfg := Config{
		Role:     RoleRoot,
		NATSMode: "in-memory",
	}
	c := NewCluster(cfg, testStore(t), testLogger())

	err := c.Start()
	if err == nil {
		t.Fatal("expected error for invalid NATSMode, got nil")
	}
	if !strings.Contains(err.Error(), "invalid NATSMode") {
		t.Errorf("error = %q, want message containing \"invalid NATSMode\"", err.Error())
	}
}

func TestStart_EmptyNATSMode(t *testing.T) {
	cfg := Config{
		Role:     RoleRoot,
		NATSMode: "",
	}
	c := NewCluster(cfg, testStore(t), testLogger())

	if err := c.Start(); err == nil {
		t.Fatal("expected error for empty NATSMode, got nil")
	}
}

func TestStart_ExternalModeNoURLs(t *testing.T) {
	cfg := Config{
		Role:     RoleWorker,
		NATSMode: "external",
		NATSUrls: nil,
	}
	c := NewCluster(cfg, testStore(t), testLogger())

	err := c.Start()
	if err == nil {
		t.Fatal("expected error for external mode with no URLs, got nil")
	}
	if !strings.Contains(err.Error(), "at least one URL") {
		t.Errorf("error = %q, want message containing \"at least one URL\"", err.Error())
	}
}

func TestStart_ExternalModeEmptyURLSlice(t *testing.T) {
	cfg := Config{
		Role:     RoleWorker,
		NATSMode: "external",
		NATSUrls: []string{},
	}
	c := NewCluster(cfg, testStore(t), testLogger())

	err := c.Start()
	if err == nil {
		t.Fatal("expected error for external mode with empty URL slice, got nil")
	}
}

func TestStart_ExternalModeUnreachable(t *testing.T) {
	cfg := Config{
		Role:     RoleWorker,
		NATSMode: "external",
		// Use a port that should have nothing listening.
		NATSUrls: []string{"nats://127.0.0.1:19999"},
	}
	c := NewCluster(cfg, testStore(t), testLogger())

	err := c.Start()
	if err == nil {
		c.Stop()
		t.Fatal("expected connection error for unreachable NATS, got nil")
	}
	if !strings.Contains(err.Error(), "connecting to external NATS") {
		t.Errorf("error = %q, want message containing \"connecting to external NATS\"", err.Error())
	}
}

// TestStart_ExternalMode_TLSMissingCert verifies that a TLS configuration
// pointing to non-existent files returns an error before attempting the
// NATS connection.
func TestStart_ExternalMode_TLSMissingCert(t *testing.T) {
	cfg := Config{
		Role:     RoleWorker,
		NATSMode: "external",
		NATSUrls: []string{"nats://127.0.0.1:4222"},
		TLS: &types.TLSConfig{
			Enabled:  true,
			CertFile: "/nonexistent/cert.pem",
			KeyFile:  "/nonexistent/key.pem",
		},
	}
	c := NewCluster(cfg, testStore(t), testLogger())

	err := c.Start()
	if err == nil {
		c.Stop()
		t.Fatal("expected error for TLS with missing cert, got nil")
	}
	if !strings.Contains(err.Error(), "loading cluster TLS config") {
		t.Errorf("error = %q, want message containing \"loading cluster TLS config\"", err.Error())
	}
}

// ---------------------------------------------------------------------------
// Stop lifecycle
// ---------------------------------------------------------------------------

func TestStop_BeforeStart(t *testing.T) {
	c := NewCluster(embeddedRootConfig(), testStore(t), testLogger())

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Stop()
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop() blocked when called before Start()")
	}
}

func TestStop_AfterStart_EmbeddedMode(t *testing.T) {
	c := NewCluster(embeddedRootConfig(), testStore(t), testLogger())
	if err := c.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Stop()
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop() blocked after embedded Start()")
	}

	c.mu.Lock()
	running := c.running
	c.mu.Unlock()
	if running {
		t.Error("expected running == false after Stop()")
	}
}

func TestStop_Idempotent(t *testing.T) {
	c := NewCluster(embeddedRootConfig(), testStore(t), testLogger())
	if err := c.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	c.Stop() // first call

	// Second Stop must not panic or deadlock.
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Stop()
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("second Stop() call blocked")
	}
}

// TestStop_StoppedChannelClosed verifies the internal stopped channel is
// closed after Stop so that waiters on <-c.stopped unblock.
func TestStop_StoppedChannelClosed(t *testing.T) {
	c := NewCluster(embeddedRootConfig(), testStore(t), testLogger())
	if err := c.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	c.Stop()

	select {
	case <-c.stopped:
		// expected
	case <-time.After(2 * time.Second):
		t.Fatal("c.stopped was not closed after Stop()")
	}
}

// TestStop_StoppedChannelClosed_WithoutStart verifies <-c.stopped is unblocked
// even when Stop is called without a prior Start (monitorLaunched == false).
func TestStop_StoppedChannelClosed_WithoutStart(t *testing.T) {
	c := NewCluster(embeddedRootConfig(), testStore(t), testLogger())
	c.Stop()

	select {
	case <-c.stopped:
		// expected
	case <-time.After(2 * time.Second):
		t.Fatal("c.stopped was not closed after Stop() without prior Start()")
	}
}

// ---------------------------------------------------------------------------
// Start/Stop with a real embedded NATS server (external mode)
// ---------------------------------------------------------------------------

func TestStartStop_ExternalMode(t *testing.T) {
	srv := testutil.NATSServer(t)

	cfg := Config{
		Role:      RoleWorker,
		NATSMode:  "external",
		NATSUrls:  []string{srv.ClientURL()},
		AuthToken: srv.AuthToken(),
	}
	c := NewCluster(cfg, testStore(t), testLogger())

	if err := c.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Stop()
	}()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Stop() with external NATS timed out")
	}
}

func TestStart_ExternalMode_ConnectedAndRunning(t *testing.T) {
	srv := testutil.NATSServer(t)

	cfg := Config{
		Role:      RoleRoot,
		NATSMode:  "external",
		NATSUrls:  []string{srv.ClientURL()},
		AuthToken: srv.AuthToken(),
	}
	c := NewCluster(cfg, testStore(t), testLogger())
	defer c.Stop()

	if err := c.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	c.mu.Lock()
	running := c.running
	nc := c.nc
	c.mu.Unlock()

	if !running {
		t.Error("expected running == true after Start()")
	}
	if nc == nil {
		t.Fatal("expected non-nil NATS connection after external Start()")
	}
	if !nc.IsConnected() {
		t.Error("expected NATS connection to be connected")
	}
}

// ---------------------------------------------------------------------------
// ReplicateState
// ---------------------------------------------------------------------------

func TestReplicateState_EmbeddedModeIsNoOp(t *testing.T) {
	c := NewCluster(embeddedRootConfig(), testStore(t), testLogger())
	if err := c.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer c.Stop()

	if err := c.ReplicateState("hive.cluster.state.agents", []byte(`{"test":true}`)); err != nil {
		t.Errorf("ReplicateState() in embedded mode error = %v, want nil", err)
	}
}

func TestReplicateState_ClusterNotRunning(t *testing.T) {
	c := NewCluster(embeddedRootConfig(), testStore(t), testLogger())
	// Do not call Start.

	err := c.ReplicateState("hive.cluster.state.agents", []byte("data"))
	if err == nil {
		t.Fatal("expected error when cluster not running, got nil")
	}
	if !strings.Contains(err.Error(), "cluster not running") {
		t.Errorf("error = %q, want message containing \"cluster not running\"", err.Error())
	}
}

func TestReplicateState_InvalidSubject(t *testing.T) {
	c := NewCluster(embeddedRootConfig(), testStore(t), testLogger())
	if err := c.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer c.Stop()

	invalidSubjects := []string{
		"",
		"subject with spaces",
		"subject.with.*.wildcard",
		"subject.with.>.wildcard",
		"subject..double.dot",
		".leading.dot",
		"trailing.dot.",
	}

	for _, subject := range invalidSubjects {
		if err := c.ReplicateState(subject, []byte("data")); err == nil {
			t.Errorf("ReplicateState(%q): expected error, got nil", subject)
		}
	}
}

func TestReplicateState_ValidSubjects(t *testing.T) {
	c := NewCluster(embeddedRootConfig(), testStore(t), testLogger())
	if err := c.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer c.Stop()

	// All of these are valid in embedded mode (no-op publish).
	validSubjects := []string{
		"hive.cluster.state.agents",
		"hive.state",
		"a",
		"a-b_c.d-e",
	}

	for _, subject := range validSubjects {
		if err := c.ReplicateState(subject, []byte("data")); err != nil {
			t.Errorf("ReplicateState(%q) unexpected error = %v", subject, err)
		}
	}
}

func TestReplicateState_OversizedPayload(t *testing.T) {
	srv := testutil.NATSServer(t)

	cfg := Config{
		Role:      RoleRoot,
		NATSMode:  "external",
		NATSUrls:  []string{srv.ClientURL()},
		AuthToken: srv.AuthToken(),
	}
	c := NewCluster(cfg, testStore(t), testLogger())
	if err := c.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer c.Stop()

	oversized := bytes.Repeat([]byte("x"), 2*1024*1024+1) // 2 MB + 1 byte
	err := c.ReplicateState("hive.cluster.state.agents", oversized)
	if err == nil {
		t.Fatal("expected error for oversized payload, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Errorf("error = %q, want message containing \"exceeds maximum size\"", err.Error())
	}
}

func TestReplicateState_ExternalMode_Publish(t *testing.T) {
	srv := testutil.NATSServer(t)

	cfg := Config{
		Role:      RoleRoot,
		NATSMode:  "external",
		NATSUrls:  []string{srv.ClientURL()},
		AuthToken: srv.AuthToken(),
	}
	c := NewCluster(cfg, testStore(t), testLogger())
	if err := c.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer c.Stop()

	// Subscribe on an independent client connection to verify publication.
	nc := testutil.NATSConnect(t, srv)
	received := make(chan []byte, 1)
	sub, err := nc.Subscribe("hive.cluster.state.agents", func(msg *nats.Msg) {
		received <- append([]byte(nil), msg.Data...)
	})
	if err != nil {
		t.Fatalf("nc.Subscribe() error = %v", err)
	}
	defer sub.Unsubscribe()
	if err := nc.Flush(); err != nil {
		t.Fatalf("nc.Flush() error = %v", err)
	}

	payload := []byte(`{"agent_id":"test-agent","status":"RUNNING"}`)
	if err := c.ReplicateState("hive.cluster.state.agents", payload); err != nil {
		t.Fatalf("ReplicateState() error = %v", err)
	}

	select {
	case got := <-received:
		if !bytes.Equal(got, payload) {
			t.Errorf("received %q, want %q", got, payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for replicated state message")
	}
}

// ---------------------------------------------------------------------------
// Subscribe
// ---------------------------------------------------------------------------

func TestSubscribe_EmbeddedModeReturnsError(t *testing.T) {
	c := NewCluster(embeddedRootConfig(), testStore(t), testLogger())
	if err := c.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer c.Stop()

	_, err := c.Subscribe("hive.cluster.state.agents", func(_ []byte) {})
	if err == nil {
		t.Fatal("expected error for Subscribe in embedded mode, got nil")
	}
	if !strings.Contains(err.Error(), "embedded mode") {
		t.Errorf("error = %q, want message containing \"embedded mode\"", err.Error())
	}
}

func TestSubscribe_ClusterNotRunning(t *testing.T) {
	c := NewCluster(embeddedRootConfig(), testStore(t), testLogger())
	// No Start call.

	_, err := c.Subscribe("hive.cluster.state.agents", func(_ []byte) {})
	if err == nil {
		t.Fatal("expected error when cluster not running, got nil")
	}
	if !strings.Contains(err.Error(), "cluster not running") {
		t.Errorf("error = %q, want message containing \"cluster not running\"", err.Error())
	}
}

func TestSubscribe_InvalidSubject(t *testing.T) {
	srv := testutil.NATSServer(t)

	cfg := Config{
		Role:      RoleWorker,
		NATSMode:  "external",
		NATSUrls:  []string{srv.ClientURL()},
		AuthToken: srv.AuthToken(),
	}
	c := NewCluster(cfg, testStore(t), testLogger())
	if err := c.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer c.Stop()

	invalidSubjects := []string{
		"",
		"has spaces",
		"has.*.wildcard",
		"has.>.wildcard",
		"..empty.segments",
		".leading.dot",
		"trailing.dot.",
	}
	for _, subject := range invalidSubjects {
		if _, err := c.Subscribe(subject, func(_ []byte) {}); err == nil {
			t.Errorf("Subscribe(%q): expected error, got nil", subject)
		}
	}
}

func TestSubscribe_ExternalMode_ReceivesMessages(t *testing.T) {
	srv := testutil.NATSServer(t)

	cfg := Config{
		Role:      RoleWorker,
		NATSMode:  "external",
		NATSUrls:  []string{srv.ClientURL()},
		AuthToken: srv.AuthToken(),
	}
	c := NewCluster(cfg, testStore(t), testLogger())
	if err := c.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer c.Stop()

	received := make(chan []byte, 1)
	sub, err := c.Subscribe("hive.cluster.state.agents", func(data []byte) {
		received <- append([]byte(nil), data...)
	})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	if sub == nil {
		t.Fatal("expected non-nil subscription")
	}

	// Publish from an independent connection.
	pub := testutil.NATSConnect(t, srv)
	payload := []byte(`{"hello":"world"}`)
	if err := pub.Publish("hive.cluster.state.agents", payload); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if err := pub.Flush(); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}

	select {
	case got := <-received:
		if !bytes.Equal(got, payload) {
			t.Errorf("received %q, want %q", got, payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for subscribed message")
	}
}

// TestSubscribe_TrackedAndCleanedOnStop verifies that subscriptions registered
// via Subscribe are tracked and cleared when Stop is called.
func TestSubscribe_TrackedAndCleanedOnStop(t *testing.T) {
	srv := testutil.NATSServer(t)

	cfg := Config{
		Role:      RoleWorker,
		NATSMode:  "external",
		NATSUrls:  []string{srv.ClientURL()},
		AuthToken: srv.AuthToken(),
	}
	c := NewCluster(cfg, testStore(t), testLogger())
	if err := c.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	if _, err := c.Subscribe("hive.cluster.state.agents", func(_ []byte) {}); err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	c.mu.Lock()
	numSubs := len(c.subs)
	c.mu.Unlock()
	if numSubs != 1 {
		t.Errorf("expected 1 tracked subscription, got %d", numSubs)
	}

	c.Stop()

	c.mu.Lock()
	numSubsAfterStop := len(c.subs)
	c.mu.Unlock()
	if numSubsAfterStop != 0 {
		t.Errorf("expected 0 tracked subscriptions after Stop, got %d", numSubsAfterStop)
	}
}

// ---------------------------------------------------------------------------
// SubscribeStateUpdates
// ---------------------------------------------------------------------------

func TestSubscribeStateUpdates_EmbeddedModeIsNoOp(t *testing.T) {
	c := NewCluster(embeddedRootConfig(), testStore(t), testLogger())
	if err := c.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer c.Stop()

	if err := c.SubscribeStateUpdates(func(_ []byte) {}); err != nil {
		t.Errorf("SubscribeStateUpdates() in embedded mode error = %v, want nil", err)
	}
}

func TestSubscribeStateUpdates_ExternalMode_ReceivesMessages(t *testing.T) {
	srv := testutil.NATSServer(t)

	cfg := Config{
		Role:      RoleWorker,
		NATSMode:  "external",
		NATSUrls:  []string{srv.ClientURL()},
		AuthToken: srv.AuthToken(),
	}
	c := NewCluster(cfg, testStore(t), testLogger())
	if err := c.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer c.Stop()

	received := make(chan []byte, 1)
	if err := c.SubscribeStateUpdates(func(data []byte) {
		received <- append([]byte(nil), data...)
	}); err != nil {
		t.Fatalf("SubscribeStateUpdates() error = %v", err)
	}

	pub := testutil.NATSConnect(t, srv)
	payload := []byte(`{"agent_id":"a1"}`)
	if err := pub.Publish("hive.cluster.state.agents", payload); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if err := pub.Flush(); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}

	select {
	case got := <-received:
		if !bytes.Equal(got, payload) {
			t.Errorf("received %q, want %q", got, payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for state update")
	}
}

// ---------------------------------------------------------------------------
// ReplicateAgentState
// ---------------------------------------------------------------------------

func TestReplicateAgentState_EmbeddedModeIsNoOp(t *testing.T) {
	c := NewCluster(embeddedRootConfig(), testStore(t), testLogger())
	if err := c.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer c.Stop()

	if err := c.ReplicateAgentState("agent-1", []byte(`{"id":"agent-1","status":"RUNNING"}`)); err != nil {
		t.Errorf("ReplicateAgentState() in embedded mode error = %v, want nil", err)
	}
}

func TestReplicateAgentState_ClusterNotRunning(t *testing.T) {
	c := NewCluster(embeddedRootConfig(), testStore(t), testLogger())
	// No Start call.

	err := c.ReplicateAgentState("agent-1", []byte("data"))
	if err == nil {
		t.Fatal("expected error when cluster not running, got nil")
	}
	if !strings.Contains(err.Error(), "replicating agent state") {
		t.Errorf("error = %q, want message containing \"replicating agent state\"", err.Error())
	}
}

func TestReplicateAgentState_ExternalMode(t *testing.T) {
	srv := testutil.NATSServer(t)

	cfg := Config{
		Role:      RoleRoot,
		NATSMode:  "external",
		NATSUrls:  []string{srv.ClientURL()},
		AuthToken: srv.AuthToken(),
	}
	c := NewCluster(cfg, testStore(t), testLogger())
	if err := c.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer c.Stop()

	// Use an independent client to receive the published message.
	nc := testutil.NATSConnect(t, srv)
	received := make(chan []byte, 1)
	sub, err := nc.Subscribe("hive.cluster.state.agents", func(msg *nats.Msg) {
		received <- append([]byte(nil), msg.Data...)
	})
	if err != nil {
		t.Fatalf("nc.Subscribe() error = %v", err)
	}
	defer sub.Unsubscribe()
	if err := nc.Flush(); err != nil {
		t.Fatalf("nc.Flush() error = %v", err)
	}

	payload := []byte(`{"id":"agent-2","status":"STOPPED"}`)
	if err := c.ReplicateAgentState("agent-2", payload); err != nil {
		t.Fatalf("ReplicateAgentState() error = %v", err)
	}

	select {
	case got := <-received:
		if !bytes.Equal(got, payload) {
			t.Errorf("received %q, want %q", got, payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for agent state replication")
	}
}

// ---------------------------------------------------------------------------
// extractHostFromURL (package-internal helper)
// ---------------------------------------------------------------------------

func TestExtractHostFromURL(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"nats://myhost.example.com:4222", "myhost.example.com"},
		{"tls://secure-host:4222", "secure-host"},
		{"nats+tls://tls-host:4222", "tls-host"},
		{"nats://127.0.0.1:4222", "127.0.0.1"},
		// Empty host: returned as-is.
		{"nats://", "nats://"},
		// Unparseable URL (url.Parse actually tolerates most strings;
		// a raw colon-slash makes it return an empty host, so we get the raw input back).
		{"://bad", "://bad"},
	}

	for _, tt := range tests {
		got := extractHostFromURL(tt.input)
		if got != tt.want {
			t.Errorf("extractHostFromURL(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// loadClusterTLSConfig (package-internal helper)
// ---------------------------------------------------------------------------

func TestLoadClusterTLSConfig_MissingCertFile(t *testing.T) {
	_, err := loadClusterTLSConfig("/nonexistent/cert.pem", "/nonexistent/key.pem", "", "localhost")
	if err == nil {
		t.Fatal("expected error for missing cert file, got nil")
	}
	if !strings.Contains(err.Error(), "loading cert/key pair") {
		t.Errorf("error = %q, want message containing \"loading cert/key pair\"", err.Error())
	}
}

func TestLoadClusterTLSConfig_InvalidCAFile(t *testing.T) {
	certPEM, keyPEM := generateSelfSignedCert(t)
	certFile, keyFile := writeCertFiles(t, certPEM, keyPEM)

	dir := t.TempDir()
	caFile := filepath.Join(dir, "invalid_ca.pem")
	if err := os.WriteFile(caFile, []byte("not-valid-pem"), 0600); err != nil {
		t.Fatalf("writing invalid CA file: %v", err)
	}

	_, err := loadClusterTLSConfig(certFile, keyFile, caFile, "localhost")
	if err == nil {
		t.Fatal("expected error for invalid CA PEM, got nil")
	}
	if !strings.Contains(err.Error(), "CA certificate") {
		t.Errorf("error = %q, want message containing \"CA certificate\"", err.Error())
	}
}

func TestLoadClusterTLSConfig_ValidCertNoCA(t *testing.T) {
	certPEM, keyPEM := generateSelfSignedCert(t)
	certFile, keyFile := writeCertFiles(t, certPEM, keyPEM)

	tlsCfg, err := loadClusterTLSConfig(certFile, keyFile, "", "myhost")
	if err != nil {
		t.Fatalf("loadClusterTLSConfig() error = %v, want nil", err)
	}
	if tlsCfg == nil {
		t.Fatal("expected non-nil tls.Config")
	}
	if tlsCfg.ServerName != "myhost" {
		t.Errorf("ServerName = %q, want %q", tlsCfg.ServerName, "myhost")
	}
	if len(tlsCfg.Certificates) == 0 {
		t.Error("expected at least one certificate in tls.Config")
	}
	if tlsCfg.RootCAs != nil {
		t.Error("expected nil RootCAs when no CA file provided")
	}
}

func TestLoadClusterTLSConfig_ValidCertWithCA(t *testing.T) {
	certPEM, keyPEM := generateSelfSignedCert(t)
	certFile, keyFile := writeCertFiles(t, certPEM, keyPEM)

	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.pem")
	// Re-use the self-signed cert PEM as the trust anchor.
	if err := os.WriteFile(caFile, certPEM, 0600); err != nil {
		t.Fatalf("writing CA file: %v", err)
	}

	tlsCfg, err := loadClusterTLSConfig(certFile, keyFile, caFile, "secure-host")
	if err != nil {
		t.Fatalf("loadClusterTLSConfig() error = %v, want nil", err)
	}
	if tlsCfg == nil {
		t.Fatal("expected non-nil tls.Config")
	}
	if tlsCfg.RootCAs == nil {
		t.Error("expected non-nil RootCAs when CA file provided")
	}
	if tlsCfg.ServerName != "secure-host" {
		t.Errorf("ServerName = %q, want %q", tlsCfg.ServerName, "secure-host")
	}
}

func TestLoadClusterTLSConfig_MinVersionAndCiphers(t *testing.T) {
	certPEM, keyPEM := generateSelfSignedCert(t)
	certFile, keyFile := writeCertFiles(t, certPEM, keyPEM)

	tlsCfg, err := loadClusterTLSConfig(certFile, keyFile, "", "host")
	if err != nil {
		t.Fatalf("loadClusterTLSConfig() error = %v", err)
	}

	if tlsCfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = 0x%04X, want 0x%04X (TLS 1.2)", tlsCfg.MinVersion, tls.VersionTLS12)
	}
	if len(tlsCfg.CipherSuites) == 0 {
		t.Error("expected at least one cipher suite to be configured")
	}
}

// ---------------------------------------------------------------------------
// Concurrency: concurrent Stop calls must not race or deadlock
// ---------------------------------------------------------------------------

func TestStop_ConcurrentCalls(t *testing.T) {
	c := NewCluster(embeddedRootConfig(), testStore(t), testLogger())
	if err := c.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			c.Stop()
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent Stop() calls blocked or deadlocked")
	}
}

// TestStartStop_ConcurrentReplicateState verifies that ReplicateState called
// concurrently while Start/Stop are happening does not panic or deadlock.
func TestStartStop_ConcurrentReplicateState(t *testing.T) {
	c := NewCluster(embeddedRootConfig(), testStore(t), testLogger())
	if err := c.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	var wg sync.WaitGroup
	const goroutines = 5
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			// Errors are acceptable here; we only care about no panic/deadlock.
			_ = c.ReplicateState("hive.cluster.state.agents", []byte("payload"))
		}()
	}

	// Stop concurrently with the goroutines.
	go func() { c.Stop() }()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent ReplicateState/Stop calls blocked or deadlocked")
	}
}
