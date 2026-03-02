// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

// Package secrets handles loading and serving secrets for agent injection.
package secrets

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/brmurrell3/hive/internal/protocol"
	"github.com/nats-io/nats.go"
	"gopkg.in/yaml.v3"
)

// Store loads secrets from a YAML file and serves them over NATS.
type Store struct {
	mu      sync.RWMutex
	secrets map[string]string // name -> value
	nc      *nats.Conn
	logger  *slog.Logger
	sub     *nats.Subscription

	// allowedSecrets maps agentID -> set of secret names the agent is
	// permitted to access. If nil, all agents can access all secrets
	// (legacy behavior); if non-nil, only listed secrets are returned.
	allowedSecrets map[string]map[string]bool
}

// secretsFile is the YAML structure of the secrets file.
type secretsFile struct {
	Secrets map[string]string `yaml:"secrets"`
}

// NewStore creates a new secret store. If secretsPath is empty, an empty
// store is created (no secrets available).
func NewStore(secretsPath string, nc *nats.Conn, logger *slog.Logger) (*Store, error) {
	s := &Store{
		secrets: make(map[string]string),
		nc:      nc,
		logger:  logger,
	}

	if secretsPath != "" {
		if err := s.loadFile(secretsPath); err != nil {
			return nil, fmt.Errorf("loading secrets file: %w", err)
		}
	}

	return s, nil
}

// loadFile reads and parses the secrets YAML file.
func (s *Store) loadFile(path string) error {
	// Warn if secrets file is readable by group or others.
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		s.logger.Warn("secrets file has overly permissive permissions, recommend 0600",
			"path", path, "mode", fmt.Sprintf("%04o", perm))
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}

	var sf secretsFile
	if err := yaml.Unmarshal(data, &sf); err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.secrets = sf.Secrets

	s.logger.Info("secrets loaded", "count", len(s.secrets))
	return nil
}

// Get returns a secret value by name.
func (s *Store) Get(name string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	val, ok := s.secrets[name]
	return val, ok
}

// secretRequest is the NATS request payload from a sidecar.
type secretRequest struct {
	AgentID string   `json:"agent_id"`
	Names   []string `json:"names"`
}

// secretResponse is the NATS reply payload.
type secretResponse struct {
	Secrets map[string]string `json:"secrets"`
	Error   string            `json:"error,omitempty"`
}

// Start subscribes to secret requests on NATS.
func (s *Store) Start() error {
	sub, err := s.nc.Subscribe(protocol.SubjSecretsRequest, func(msg *nats.Msg) {
		var req secretRequest
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			resp, _ := json.Marshal(secretResponse{Error: "invalid request"})
			msg.Respond(resp)
			return
		}

		s.logger.Debug("secret request", "agent_id", req.AgentID, "names", req.Names)

		if req.AgentID == "" {
			resp, _ := json.Marshal(secretResponse{Error: "agent_id is required"})
			msg.Respond(resp)
			return
		}

		result := make(map[string]string)
		s.mu.RLock()
		agentAllowed := s.allowedSecrets // nil means legacy mode (all allowed)
		var permitted map[string]bool
		if agentAllowed != nil {
			permitted = agentAllowed[req.AgentID]
			if permitted == nil {
				// Agent has no secrets configured — return empty set.
				s.mu.RUnlock()
				s.logger.Warn("secret request denied: agent not in allowlist",
					"agent_id", req.AgentID)
				resp, _ := json.Marshal(secretResponse{Secrets: result})
				msg.Respond(resp)
				return
			}
		}
		for _, name := range req.Names {
			if permitted != nil && !permitted[name] {
				continue // agent not authorized for this secret
			}
			if val, ok := s.secrets[name]; ok {
				result[name] = val
			}
		}
		s.mu.RUnlock()

		resp, _ := json.Marshal(secretResponse{Secrets: result})
		msg.Respond(resp)
	})
	if err != nil {
		return fmt.Errorf("subscribing to secrets requests: %w", err)
	}

	s.sub = sub

	// Flush to ensure the subscription is registered on the server before
	// returning, preventing race conditions where requests arrive before
	// the subscription is active.
	if err := s.nc.Flush(); err != nil {
		return fmt.Errorf("flushing NATS after subscribe: %w", err)
	}

	s.logger.Info("secret store started")
	return nil
}

// SetAllowedSecrets configures per-agent secret access. The map keys are
// agent IDs and the values are the set of secret names each agent may access.
// Agents not in the map receive no secrets. Pass nil to allow all agents
// to access all secrets (legacy/unconfigured behavior).
func (s *Store) SetAllowedSecrets(allowed map[string]map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.allowedSecrets = allowed
}

// Stop unsubscribes from NATS.
func (s *Store) Stop() {
	if s.sub != nil {
		s.sub.Unsubscribe()
	}
}
