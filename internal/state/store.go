package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/hivehq/hive/internal/auth"
	"github.com/hivehq/hive/internal/types"
)

// AgentStatus represents the lifecycle status of an agent VM.
type AgentStatus string

const (
	AgentStatusPending  AgentStatus = "PENDING"
	AgentStatusCreating AgentStatus = "CREATING"
	AgentStatusStarting AgentStatus = "STARTING"
	AgentStatusRunning  AgentStatus = "RUNNING"
	AgentStatusStopping AgentStatus = "STOPPING"
	AgentStatusStopped  AgentStatus = "STOPPED"
	AgentStatusFailed   AgentStatus = "FAILED"
)

// validTransitions defines the allowed state machine transitions.
var validTransitions = map[AgentStatus][]AgentStatus{
	AgentStatusPending:  {AgentStatusCreating},
	AgentStatusCreating: {AgentStatusStarting, AgentStatusFailed},
	AgentStatusStarting: {AgentStatusRunning, AgentStatusFailed, AgentStatusStopping},
	AgentStatusRunning:  {AgentStatusStopping, AgentStatusFailed},
	AgentStatusStopping: {AgentStatusStopped, AgentStatusFailed},
	AgentStatusStopped:  {AgentStatusCreating},
	AgentStatusFailed:   {AgentStatusCreating},
}

// AgentState holds the runtime state for a single agent.
type AgentState struct {
	ID             string      `json:"id"`
	Team           string      `json:"team"`
	Status         AgentStatus `json:"status"`
	NodeID         string      `json:"node_id,omitempty"`
	MemoryBytes    int64       `json:"memory_bytes,omitempty"`
	VCPUs          int         `json:"vcpus,omitempty"`
	ManifestHash   string      `json:"manifest_hash,omitempty"`
	VMPID          int         `json:"vm_pid,omitempty"`
	VMCID          uint32      `json:"vm_cid,omitempty"`
	VMSocketPath   string      `json:"vm_socket_path,omitempty"`
	RootfsCopyPath string      `json:"rootfs_copy_path,omitempty"`
	RestartCount   int         `json:"restart_count"`
	LastTransition time.Time   `json:"last_transition"`
	StartedAt      time.Time   `json:"started_at,omitempty"`
	Error          string      `json:"error,omitempty"`
}

// State is the top-level runtime state persisted to state.json.
type State struct {
	Agents       map[string]*AgentState        `json:"agents"`
	Nodes        map[string]*types.NodeState   `json:"nodes,omitempty"`
	Tokens       []*types.Token                `json:"tokens,omitempty"`
	Capabilities *types.CapabilityRegistry     `json:"capabilities,omitempty"`
	Users        []*auth.User                  `json:"users,omitempty"`
}

func newState() *State {
	return &State{
		Agents:       make(map[string]*AgentState),
		Nodes:        make(map[string]*types.NodeState),
		Tokens:       []*types.Token{},       // T3-11: initialize
		Capabilities: types.NewCapabilityRegistry(),
		Users:        []*auth.User{},          // T3-11: initialize
	}
}

// Store manages the runtime state persistence via state.json.
type Store struct {
	mu     sync.Mutex
	path   string
	state  *State
	logger *slog.Logger
}

// NewStore creates a new Store backed by the given file path.
// If the file exists it is loaded; if it is corrupt, the backup
// (state.json.bak) is tried; if both fail, an empty state is used so
// that hived can always start.
func NewStore(path string, logger *slog.Logger) (*Store, error) {
	s := &Store{
		path:   path,
		logger: logger,
		state:  newState(),
	}

	if _, err := os.Stat(path); err == nil {
		if err := s.Load(); err != nil {
			logger.Warn("state file corrupt, attempting backup recovery",
				"path", path, "error", err)
			if bakErr := s.loadFromBackup(); bakErr != nil {
				logger.Warn("backup state also unusable, starting with empty state",
					"backup_path", s.backupPath(), "error", bakErr)
				s.state = newState()
			} else {
				logger.Info("recovered state from backup", "path", s.backupPath(),
					"agents", len(s.state.Agents))
			}
		} else {
			logger.Info("loaded existing state", "path", path, "agents", len(s.state.Agents))
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("checking state file %s: %w", path, err)
	} else {
		logger.Info("no existing state file, starting fresh", "path", path)
	}

	return s, nil
}

// Load reads state.json from disk into memory.
func (s *Store) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("reading state file: %w", err)
	}

	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return fmt.Errorf("parsing state file: %w", err)
	}

	if st.Agents == nil {
		st.Agents = make(map[string]*AgentState)
	}
	if st.Nodes == nil {
		st.Nodes = make(map[string]*types.NodeState)
	}
	if st.Capabilities == nil {
		st.Capabilities = types.NewCapabilityRegistry()
	}
	if st.Tokens == nil {
		st.Tokens = []*types.Token{}
	}
	if st.Users == nil {
		st.Users = []*auth.User{}
	}

	s.state = &st
	return nil
}

// loadFromBackup attempts to load state from the backup file (state.json.bak).
// It is called when the primary state file is corrupt.
func (s *Store) loadFromBackup() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	bakPath := s.backupPath()
	data, err := os.ReadFile(bakPath)
	if err != nil {
		return fmt.Errorf("reading backup state file: %w", err)
	}

	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return fmt.Errorf("parsing backup state file: %w", err)
	}

	if st.Agents == nil {
		st.Agents = make(map[string]*AgentState)
	}
	if st.Nodes == nil {
		st.Nodes = make(map[string]*types.NodeState)
	}
	if st.Capabilities == nil {
		st.Capabilities = types.NewCapabilityRegistry()
	}
	if st.Tokens == nil {
		st.Tokens = []*types.Token{}
	}
	if st.Users == nil {
		st.Users = []*auth.User{}
	}

	s.state = &st
	return nil
}

// Save writes the current state to disk atomically by writing to a temporary
// file first then renaming it over the target path.
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.saveLocked()
}

// backupPath returns the path of the backup state file.
func (s *Store) backupPath() string {
	return s.path + ".bak"
}

// saveLocked performs the atomic save while the caller already holds the mutex.
// Before overwriting state.json, it copies the current file to state.json.bak
// so that a prior good state is always recoverable.
func (s *Store) saveLocked() error {
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating state directory: %w", err)
	}

	// Back up the current state file before overwriting. Errors here are
	// logged but not fatal — the primary save must still proceed.
	if existing, readErr := os.ReadFile(s.path); readErr == nil {
		if writeErr := os.WriteFile(s.backupPath(), existing, 0644); writeErr != nil {
			s.logger.Warn("failed to write state backup", "error", writeErr)
		}
	}

	tmp, err := os.CreateTemp(dir, "state-*.json.tmp")
	if err != nil {
		return fmt.Errorf("creating temp state file: %w", err)
	}
	tmpPath := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("writing temp state file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("syncing temp state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("closing temp state file: %w", err)
	}

	if err := os.Rename(tmpPath, s.path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("renaming temp state file: %w", err)
	}

	return nil
}

// GetAgent returns the agent state for the given ID, or nil if not found.
func (s *Store) GetAgent(id string) *AgentState {
	s.mu.Lock()
	defer s.mu.Unlock()

	agent, ok := s.state.Agents[id]
	if !ok {
		return nil
	}

	// Return a copy to prevent mutations without going through SetAgent.
	cp := *agent
	return &cp
}

// SetAgent updates or inserts the agent state and persists to disk.
func (s *Store) SetAgent(agent *AgentState) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Store a copy so external mutations don't affect persisted state.
	cp := *agent
	s.state.Agents[cp.ID] = &cp

	if err := s.saveLocked(); err != nil {
		return fmt.Errorf("saving state after setting agent %s: %w", cp.ID, err)
	}

	s.logger.Debug("agent state updated", "agent_id", cp.ID, "status", cp.Status)
	return nil
}

// ModifyAgent performs an atomic read-modify-write on agent state.
// T2-02: The callback receives a deep copy of the current state, mutates it,
// and only if the callback succeeds does the store update the real state and
// persist atomically while holding the lock.
func (s *Store) ModifyAgent(id string, fn func(*AgentState) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	agent, ok := s.state.Agents[id]
	if !ok {
		return fmt.Errorf("agent %q not found in state", id)
	}

	// Work on a deep copy so the callback cannot mutate live state on error.
	cp := *agent
	if err := fn(&cp); err != nil {
		return err
	}

	// Callback succeeded — commit the copy to live state.
	s.state.Agents[id] = &cp

	if err := s.saveLocked(); err != nil {
		return fmt.Errorf("saving state after modifying agent %s: %w", id, err)
	}

	s.logger.Debug("agent state modified atomically", "agent_id", id, "status", cp.Status)
	return nil
}

// RemoveAgent removes the agent from state and persists to disk.
func (s *Store) RemoveAgent(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.state.Agents[id]; !ok {
		return fmt.Errorf("agent %q not found in state", id)
	}

	delete(s.state.Agents, id)

	if err := s.saveLocked(); err != nil {
		return fmt.Errorf("saving state after removing agent %s: %w", id, err)
	}

	s.logger.Debug("agent removed from state", "agent_id", id)
	return nil
}

// AllAgents returns all agent states sorted alphabetically by ID.
func (s *Store) AllAgents() []*AgentState {
	s.mu.Lock()
	defer s.mu.Unlock()

	agents := make([]*AgentState, 0, len(s.state.Agents))
	for _, a := range s.state.Agents {
		cp := *a
		agents = append(agents, &cp)
	}

	sort.Slice(agents, func(i, j int) bool {
		return agents[i].ID < agents[j].ID
	})

	return agents
}

// --- Node management ---

// GetNode returns the node state for the given ID, or nil if not found.
// T2-05: Deep-copies Labels map and Agents slice.
func (s *Store) GetNode(id string) *types.NodeState {
	s.mu.Lock()
	defer s.mu.Unlock()

	node, ok := s.state.Nodes[id]
	if !ok {
		return nil
	}
	cp := *node
	if node.Labels != nil {
		cp.Labels = make(map[string]string, len(node.Labels))
		for k, v := range node.Labels {
			cp.Labels[k] = v
		}
	}
	if node.Agents != nil {
		cp.Agents = make([]string, len(node.Agents))
		copy(cp.Agents, node.Agents)
	}
	return &cp
}

// SetNode updates or inserts a node state and persists to disk.
// T2-05: Deep-copies Labels map and Agents slice.
func (s *Store) SetNode(node *types.NodeState) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cp := *node
	if node.Labels != nil {
		cp.Labels = make(map[string]string, len(node.Labels))
		for k, v := range node.Labels {
			cp.Labels[k] = v
		}
	}
	if node.Agents != nil {
		cp.Agents = make([]string, len(node.Agents))
		copy(cp.Agents, node.Agents)
	}
	s.state.Nodes[cp.ID] = &cp

	if err := s.saveLocked(); err != nil {
		return fmt.Errorf("saving state after setting node %s: %w", cp.ID, err)
	}
	s.logger.Debug("node state updated", "node_id", cp.ID, "status", cp.Status)
	return nil
}

// RemoveNode removes a node from state and persists to disk.
func (s *Store) RemoveNode(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.state.Nodes[id]; !ok {
		return fmt.Errorf("node %q not found in state", id)
	}
	delete(s.state.Nodes, id)

	if err := s.saveLocked(); err != nil {
		return fmt.Errorf("saving state after removing node %s: %w", id, err)
	}
	s.logger.Debug("node removed from state", "node_id", id)
	return nil
}

// AllNodes returns all node states sorted alphabetically by ID.
// Deep-copies Labels map and Agents slice for each node.
func (s *Store) AllNodes() []*types.NodeState {
	s.mu.Lock()
	defer s.mu.Unlock()

	nodes := make([]*types.NodeState, 0, len(s.state.Nodes))
	for _, n := range s.state.Nodes {
		cp := *n
		if n.Labels != nil {
			cp.Labels = make(map[string]string, len(n.Labels))
			for k, v := range n.Labels {
				cp.Labels[k] = v
			}
		}
		if n.Agents != nil {
			cp.Agents = make([]string, len(n.Agents))
			copy(cp.Agents, n.Agents)
		}
		nodes = append(nodes, &cp)
	}
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].ID < nodes[j].ID
	})
	return nodes
}

// --- Token management ---

// AddToken stores a hashed token and persists to disk.
// T2-04: Makes a defensive copy before storing.
func (s *Store) AddToken(token *types.Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cp := *token
	s.state.Tokens = append(s.state.Tokens, &cp)

	if err := s.saveLocked(); err != nil {
		return fmt.Errorf("saving state after adding token: %w", err)
	}
	s.logger.Debug("token added", "prefix", token.Prefix)
	return nil
}

// ValidateToken checks a raw token against stored hashes.
// Returns a copy of the matching token if valid, nil otherwise.
// T2-03: Persists LastUsed and returns a copy (not a pointer into internal state).
func (s *Store) ValidateToken(rawToken string) *types.Token {
	s.mu.Lock()
	defer s.mu.Unlock()

	hash := hashToken(rawToken)
	for _, t := range s.state.Tokens {
		if t.Hash == hash && t.IsValid() {
			t.LastUsed = time.Now()
			if err := s.saveLocked(); err != nil {
				s.logger.Error("failed to persist token LastUsed", "error", err)
			}
			cp := *t
			return &cp
		}
	}
	return nil
}

// AllTokens returns all tokens (for listing).
func (s *Store) AllTokens() []*types.Token {
	s.mu.Lock()
	defer s.mu.Unlock()

	tokens := make([]*types.Token, 0, len(s.state.Tokens))
	for _, t := range s.state.Tokens {
		cp := *t
		tokens = append(tokens, &cp)
	}
	return tokens
}

// RevokeToken revokes a token matching the given prefix.
func (s *Store) RevokeToken(prefix string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	found := false
	for _, t := range s.state.Tokens {
		if t.Prefix == prefix {
			t.Revoked = true
			found = true
		}
	}
	if !found {
		return fmt.Errorf("no token found with prefix %q", prefix)
	}

	if err := s.saveLocked(); err != nil {
		return fmt.Errorf("saving state after revoking token: %w", err)
	}
	s.logger.Debug("token revoked", "prefix", prefix)
	return nil
}

// hashToken returns the SHA-256 hex digest of a raw token string.
func hashToken(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

// HashToken is the exported version for use by token generation code.
func HashToken(raw string) string {
	return hashToken(raw)
}

// --- Capability Registry ---

// GetCapabilityRegistry returns a deep copy of the capability registry.
// T2-05: Deep-copies Capabilities slice and nested Inputs/Outputs.
func (s *Store) GetCapabilityRegistry() *types.CapabilityRegistry {
	s.mu.Lock()
	defer s.mu.Unlock()

	reg := types.NewCapabilityRegistry()
	for k, v := range s.state.Capabilities.Agents {
		cp := *v
		if v.Capabilities != nil {
			cp.Capabilities = make([]types.AgentCapability, len(v.Capabilities))
			for i, c := range v.Capabilities {
				cc := c
				if c.Inputs != nil {
					cc.Inputs = make([]types.CapabilityParam, len(c.Inputs))
					copy(cc.Inputs, c.Inputs)
				}
				if c.Outputs != nil {
					cc.Outputs = make([]types.CapabilityParam, len(c.Outputs))
					copy(cc.Outputs, c.Outputs)
				}
				cp.Capabilities[i] = cc
			}
		}
		reg.Agents[k] = &cp
	}
	return reg
}

// RegisterCapabilities registers an agent's capabilities and persists.
func (s *Store) RegisterCapabilities(agentID, teamID, tier, nodeID string, caps []types.AgentCapability) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.state.Capabilities.Register(agentID, teamID, tier, nodeID, caps)

	if err := s.saveLocked(); err != nil {
		return fmt.Errorf("saving state after registering capabilities for %s: %w", agentID, err)
	}
	s.logger.Debug("capabilities registered", "agent_id", agentID, "count", len(caps))
	return nil
}

// DeregisterCapabilities removes an agent's capabilities and persists.
func (s *Store) DeregisterCapabilities(agentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.state.Capabilities.Deregister(agentID)

	if err := s.saveLocked(); err != nil {
		return fmt.Errorf("saving state after deregistering capabilities for %s: %w", agentID, err)
	}
	s.logger.Debug("capabilities deregistered", "agent_id", agentID)
	return nil
}

// --- User management ---

// AddUser adds a user to state and persists to disk.
func (s *Store) AddUser(user *auth.User) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Check for duplicate IDs.
	for _, u := range s.state.Users {
		if u.ID == user.ID {
			return fmt.Errorf("user %q already exists", user.ID)
		}
	}

	cp := *user
	s.state.Users = append(s.state.Users, &cp)

	if err := s.saveLocked(); err != nil {
		return fmt.Errorf("saving state after adding user %s: %w", user.ID, err)
	}
	s.logger.Debug("user added", "user_id", user.ID, "role", user.Role)
	return nil
}

// RemoveUser removes a user from state and persists to disk.
func (s *Store) RemoveUser(userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	found := false
	users := make([]*auth.User, 0, len(s.state.Users))
	for _, u := range s.state.Users {
		if u.ID == userID {
			found = true
			continue
		}
		users = append(users, u)
	}

	if !found {
		return fmt.Errorf("user %q not found", userID)
	}

	s.state.Users = users

	if err := s.saveLocked(); err != nil {
		return fmt.Errorf("saving state after removing user %s: %w", userID, err)
	}
	s.logger.Debug("user removed", "user_id", userID)
	return nil
}

// deepCopyUser returns a deep copy of a User, including slices.
func deepCopyUser(u *auth.User) *auth.User {
	cp := *u
	if u.Teams != nil {
		cp.Teams = make([]string, len(u.Teams))
		copy(cp.Teams, u.Teams)
	}
	if u.Agents != nil {
		cp.Agents = make([]string, len(u.Agents))
		copy(cp.Agents, u.Agents)
	}
	return &cp
}

// GetUser returns a copy of the user with the given ID, or nil if not found.
// T2-05: Deep-copies Teams and Agents slices.
func (s *Store) GetUser(userID string) *auth.User {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, u := range s.state.Users {
		if u.ID == userID {
			return deepCopyUser(u)
		}
	}
	return nil
}

// AllUsers returns all users sorted alphabetically by ID.
// T2-05: Deep-copies Teams and Agents slices.
func (s *Store) AllUsers() []*auth.User {
	s.mu.Lock()
	defer s.mu.Unlock()

	users := make([]*auth.User, 0, len(s.state.Users))
	for _, u := range s.state.Users {
		users = append(users, deepCopyUser(u))
	}

	sort.Slice(users, func(i, j int) bool {
		return users[i].ID < users[j].ID
	})

	return users
}

// UpdateUser updates an existing user in state and persists to disk.
func (s *Store) UpdateUser(user *auth.User) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	found := false
	for i, u := range s.state.Users {
		if u.ID == user.ID {
			cp := *user
			s.state.Users[i] = &cp
			found = true
			break
		}
	}

	if !found {
		return fmt.Errorf("user %q not found", user.ID)
	}

	if err := s.saveLocked(); err != nil {
		return fmt.Errorf("saving state after updating user %s: %w", user.ID, err)
	}
	s.logger.Debug("user updated", "user_id", user.ID, "role", user.Role)
	return nil
}

// ValidateTransition checks whether a transition from one status to another
// is allowed by the agent lifecycle state machine.
func ValidateTransition(from, to AgentStatus) error {
	allowed, ok := validTransitions[from]
	if !ok {
		return fmt.Errorf("unknown agent status %q", from)
	}
	for _, a := range allowed {
		if a == to {
			return nil
		}
	}
	return fmt.Errorf("invalid state transition from %s to %s", from, to)
}
