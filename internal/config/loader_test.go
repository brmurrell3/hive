//go:build unit

package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseCluster_Valid(t *testing.T) {
	input := []byte(`
apiVersion: hive/v1
kind: Cluster
metadata:
  name: my-cluster
spec:
  nats:
    port: 4222
    jetstream:
      enabled: true
      maxMemory: "2GB"
      maxStorage: "20GB"
  defaults:
    resources:
      memory: "512Mi"
      vcpus: 2
    health:
      interval: "30s"
      timeout: "5s"
      maxFailures: 3
    restart:
      policy: on-failure
      maxRestarts: 5
      backoff: "10s"
`)

	cfg, err := ParseCluster(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Metadata.Name != "my-cluster" {
		t.Errorf("name = %q, want %q", cfg.Metadata.Name, "my-cluster")
	}
	if cfg.Spec.NATS.Port != 4222 {
		t.Errorf("nats.port = %d, want %d", cfg.Spec.NATS.Port, 4222)
	}
	if !cfg.Spec.NATS.JetStream.IsEnabled() {
		t.Error("jetstream should be enabled")
	}
	if cfg.Spec.NATS.JetStream.MaxMemory != "2GB" {
		t.Errorf("jetstream.maxMemory = %q, want %q", cfg.Spec.NATS.JetStream.MaxMemory, "2GB")
	}
	if cfg.Spec.Defaults.Health.Interval != 30*time.Second {
		t.Errorf("health.interval = %v, want %v", cfg.Spec.Defaults.Health.Interval, 30*time.Second)
	}
	if cfg.Spec.Defaults.Health.MaxFailures != 3 {
		t.Errorf("health.maxFailures = %d, want %d", cfg.Spec.Defaults.Health.MaxFailures, 3)
	}
	if cfg.Spec.Defaults.Restart.Policy != "on-failure" {
		t.Errorf("restart.policy = %q, want %q", cfg.Spec.Defaults.Restart.Policy, "on-failure")
	}
	if cfg.Spec.Defaults.Restart.MaxRestarts != 5 {
		t.Errorf("restart.maxRestarts = %d, want %d", cfg.Spec.Defaults.Restart.MaxRestarts, 5)
	}
	if cfg.Spec.Defaults.Restart.Backoff != 10*time.Second {
		t.Errorf("restart.backoff = %v, want %v", cfg.Spec.Defaults.Restart.Backoff, 10*time.Second)
	}
}

func TestParseCluster_MissingName(t *testing.T) {
	input := []byte(`
apiVersion: hive/v1
kind: Cluster
metadata: {}
spec:
  nats:
    port: 4222
`)

	_, err := ParseCluster(input)
	if err == nil {
		t.Fatal("expected error for missing metadata.name")
	}

	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("expected ValidationError, got %T: %v", err, err)
	}

	found := false
	for _, e := range ve.Errors {
		if e == "metadata.name is required" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected error about metadata.name, got: %v", ve.Errors)
	}
}

func TestParseCluster_InvalidPort(t *testing.T) {
	tests := []struct {
		name  string
		port  string
		valid bool
	}{
		{"negative port", "-2", false},
		{"too large port", "70000", false},
		{"valid port", "4222", true},
		{"max valid port", "65535", true},
		{"min valid port", "1", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := []byte(`
apiVersion: hive/v1
kind: Cluster
metadata:
  name: test
spec:
  nats:
    port: ` + tt.port + `
`)
			_, err := ParseCluster(input)
			if tt.valid && err != nil {
				t.Errorf("expected valid, got error: %v", err)
			}
			if !tt.valid && err == nil {
				t.Error("expected error for invalid port")
			}
		})
	}
}

func TestParseCluster_Defaults(t *testing.T) {
	input := []byte(`
apiVersion: hive/v1
kind: Cluster
metadata:
  name: test
`)

	cfg, err := ParseCluster(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Spec.NATS.Port != 4222 {
		t.Errorf("default nats.port = %d, want 4222", cfg.Spec.NATS.Port)
	}
	if cfg.Spec.NATS.ClusterPort != 6222 {
		t.Errorf("default nats.clusterPort = %d, want 6222", cfg.Spec.NATS.ClusterPort)
	}
	if !cfg.Spec.NATS.JetStream.IsEnabled() {
		t.Error("default jetstream should be enabled")
	}
	if cfg.Spec.NATS.JetStream.MaxMemory != "1GB" {
		t.Errorf("default jetstream.maxMemory = %q, want %q", cfg.Spec.NATS.JetStream.MaxMemory, "1GB")
	}
	if cfg.Spec.Defaults.Health.Interval != 30*time.Second {
		t.Errorf("default health.interval = %v, want %v", cfg.Spec.Defaults.Health.Interval, 30*time.Second)
	}
	if cfg.Spec.Defaults.Health.MaxFailures != 3 {
		t.Errorf("default health.maxFailures = %d, want 3", cfg.Spec.Defaults.Health.MaxFailures)
	}
	if cfg.Spec.Defaults.Restart.Policy != "on-failure" {
		t.Errorf("default restart.policy = %q, want %q", cfg.Spec.Defaults.Restart.Policy, "on-failure")
	}
	if cfg.Spec.Defaults.Restart.MaxRestarts != 5 {
		t.Errorf("default restart.maxRestarts = %d, want 5", cfg.Spec.Defaults.Restart.MaxRestarts)
	}
	if cfg.Spec.Defaults.Restart.Backoff != 10*time.Second {
		t.Errorf("default restart.backoff = %v, want %v", cfg.Spec.Defaults.Restart.Backoff, 10*time.Second)
	}
}

func TestParseCluster_JetStreamDisabled(t *testing.T) {
	input := []byte(`
apiVersion: hive/v1
kind: Cluster
metadata:
  name: test
spec:
  nats:
    jetstream:
      enabled: false
`)

	cfg, err := ParseCluster(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Spec.NATS.JetStream.IsEnabled() {
		t.Error("jetstream should be disabled")
	}
}

func TestParseCluster_InvalidYAML(t *testing.T) {
	input := []byte(`{invalid yaml: [`)

	_, err := ParseCluster(input)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestParseCluster_WrongAPIVersion(t *testing.T) {
	input := []byte(`
apiVersion: hive/v2
kind: Cluster
metadata:
  name: test
`)

	_, err := ParseCluster(input)
	if err == nil {
		t.Fatal("expected error for wrong apiVersion")
	}
}

func TestParseCluster_WrongKind(t *testing.T) {
	input := []byte(`
apiVersion: hive/v1
kind: Agent
metadata:
  name: test
`)

	_, err := ParseCluster(input)
	if err == nil {
		t.Fatal("expected error for wrong kind")
	}
}

func TestParseCluster_InvalidRestartPolicy(t *testing.T) {
	input := []byte(`
apiVersion: hive/v1
kind: Cluster
metadata:
  name: test
spec:
  defaults:
    restart:
      policy: invalid
`)

	_, err := ParseCluster(input)
	if err == nil {
		t.Fatal("expected error for invalid restart policy")
	}
}

func TestLoadCluster(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "cluster.yaml", `
apiVersion: hive/v1
kind: Cluster
metadata:
  name: file-test
`)

	cfg, err := LoadCluster(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Metadata.Name != "file-test" {
		t.Errorf("name = %q, want %q", cfg.Metadata.Name, "file-test")
	}
}

func TestLoadCluster_MissingFile(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadCluster(dir)
	if err == nil {
		t.Fatal("expected error for missing cluster.yaml")
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := writeFileHelper(dir, name, content); err != nil {
		t.Fatal(err)
	}
}

func writeFileHelper(dir, name, content string) error {
	return os.WriteFile(filepath.Join(dir, name), []byte(content), 0644)
}
