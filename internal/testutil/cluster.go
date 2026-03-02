// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

// Package testutil provides shared test helpers for creating temporary cluster roots and embedded NATS servers.
package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

// ClusterRoot creates a temporary cluster root directory with a valid cluster.yaml.
// Returns the path to the cluster root. Automatically cleaned up after the test.
func ClusterRoot(t *testing.T) string {
	t.Helper()
	return ClusterRootWithConfig(t, validClusterYAML)
}

// ClusterRootWithConfig creates a temporary cluster root with the given cluster.yaml content.
func ClusterRootWithConfig(t *testing.T, clusterYAML string) string {
	t.Helper()

	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "cluster.yaml"), []byte(clusterYAML), 0644); err != nil {
		t.Fatalf("writing cluster.yaml: %v", err)
	}

	return dir
}

const validClusterYAML = `apiVersion: hive/v1
kind: Cluster
metadata:
  name: test-cluster
spec:
  nats:
    port: 0
    jetstream:
      enabled: true
`
