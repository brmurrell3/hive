// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package types

// DesiredState represents the complete desired state assembled from all manifests
// in a cluster root directory (cluster.yaml, agents/*/manifest.yaml, teams/*.yaml).
type DesiredState struct {
	Cluster *ClusterConfig            `json:"cluster"`
	Agents  map[string]*AgentManifest `json:"agents"` // keyed by agent ID
	Teams   map[string]*TeamManifest  `json:"teams"`  // keyed by team ID
}
