package state

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/brmurrell3/hive/internal/auth"
	"github.com/brmurrell3/hive/internal/types"
	_ "modernc.org/sqlite"
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
	AgentStatusPending:  {AgentStatusCreating, AgentStatusFailed},
	AgentStatusCreating: {AgentStatusStarting, AgentStatusFailed},
	AgentStatusStarting: {AgentStatusRunning, AgentStatusFailed, AgentStatusStopping},
	AgentStatusRunning:  {AgentStatusStopping, AgentStatusFailed},
	AgentStatusStopping: {AgentStatusStopped, AgentStatusFailed},
	AgentStatusStopped:  {AgentStatusCreating, AgentStatusPending},
	AgentStatusFailed:   {AgentStatusCreating, AgentStatusStopped, AgentStatusPending},
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

// State is the top-level runtime state (kept in-memory, persisted to SQLite).
type State struct {
	Agents       map[string]*AgentState      `json:"agents"`
	Nodes        map[string]*types.NodeState `json:"nodes,omitempty"`
	Tokens       []*types.Token              `json:"tokens,omitempty"`
	Capabilities *types.CapabilityRegistry   `json:"capabilities,omitempty"`
	Users        []*auth.User                `json:"users,omitempty"`
}

func newState() *State {
	return &State{
		Agents:       make(map[string]*AgentState),
		Nodes:        make(map[string]*types.NodeState),
		Tokens:       []*types.Token{},
		Capabilities: types.NewCapabilityRegistry(),
		Users:        []*auth.User{},
	}
}

// Store manages the runtime state persistence via SQLite.
type Store struct {
	mu     sync.Mutex
	path   string
	db     *sql.DB
	state  *State
	logger *slog.Logger
}

// NewStore creates a new Store backed by a SQLite database at the given path.
// If an existing JSON state file is found, it is migrated to SQLite automatically.
func NewStore(path string, logger *slog.Logger) (*Store, error) {
	s := &Store{
		path:   path,
		logger: logger,
		state:  newState(),
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("creating state directory: %w", err)
	}

	db, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("opening state database %s: %w", path, err)
	}
	s.db = db

	if err := s.createTables(); err != nil {
		db.Close()
		return nil, fmt.Errorf("creating state tables: %w", err)
	}

	if err := s.loadFromDB(); err != nil {
		logger.Warn("failed to load state from database, starting fresh",
			"path", path, "error", err)
		s.state = newState()
	} else {
		logger.Info("loaded state from database", "path", path,
			"agents", len(s.state.Agents))
	}

	return s, nil
}

func (s *Store) createTables() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS agents (
			id TEXT PRIMARY KEY,
			team TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'PENDING',
			node_id TEXT NOT NULL DEFAULT '',
			memory_bytes INTEGER NOT NULL DEFAULT 0,
			vcpus INTEGER NOT NULL DEFAULT 0,
			manifest_hash TEXT NOT NULL DEFAULT '',
			vm_pid INTEGER NOT NULL DEFAULT 0,
			vm_cid INTEGER NOT NULL DEFAULT 0,
			vm_socket_path TEXT NOT NULL DEFAULT '',
			rootfs_copy_path TEXT NOT NULL DEFAULT '',
			restart_count INTEGER NOT NULL DEFAULT 0,
			last_transition TEXT NOT NULL DEFAULT '',
			started_at TEXT NOT NULL DEFAULT '',
			error TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE IF NOT EXISTS nodes (
			id TEXT PRIMARY KEY,
			data_json TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS tokens (
			hash TEXT PRIMARY KEY,
			data_json TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS capabilities (
			agent_id TEXT PRIMARY KEY,
			data_json TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS users (
			id TEXT PRIMARY KEY,
			data_json TEXT NOT NULL
		);
	`)
	return err
}

// loadFromDB loads all state from the SQLite database into memory.
func (s *Store) loadFromDB() error {
	if err := s.loadAgents(); err != nil {
		return fmt.Errorf("loading agents: %w", err)
	}
	if err := s.loadNodes(); err != nil {
		return fmt.Errorf("loading nodes: %w", err)
	}
	if err := s.loadTokens(); err != nil {
		return fmt.Errorf("loading tokens: %w", err)
	}
	if err := s.loadCapabilities(); err != nil {
		return fmt.Errorf("loading capabilities: %w", err)
	}
	if err := s.loadUsers(); err != nil {
		return fmt.Errorf("loading users: %w", err)
	}
	return nil
}

func (s *Store) loadAgents() error {
	rows, err := s.db.Query(`SELECT id, team, status, node_id, memory_bytes, vcpus,
		manifest_hash, vm_pid, vm_cid, vm_socket_path, rootfs_copy_path,
		restart_count, last_transition, started_at, error FROM agents`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var a AgentState
		var lastTransStr, startedAtStr string
		if err := rows.Scan(&a.ID, &a.Team, &a.Status, &a.NodeID, &a.MemoryBytes,
			&a.VCPUs, &a.ManifestHash, &a.VMPID, &a.VMCID, &a.VMSocketPath,
			&a.RootfsCopyPath, &a.RestartCount, &lastTransStr, &startedAtStr, &a.Error); err != nil {
			return err
		}
		if lastTransStr != "" {
			a.LastTransition, _ = time.Parse(time.RFC3339Nano, lastTransStr)
		}
		if startedAtStr != "" {
			a.StartedAt, _ = time.Parse(time.RFC3339Nano, startedAtStr)
		}
		s.state.Agents[a.ID] = &a
	}
	return rows.Err()
}

func (s *Store) loadNodes() error {
	rows, err := s.db.Query(`SELECT data_json FROM nodes`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var dataJSON string
		if err := rows.Scan(&dataJSON); err != nil {
			return err
		}
		var n types.NodeState
		if err := json.Unmarshal([]byte(dataJSON), &n); err != nil {
			return err
		}
		s.state.Nodes[n.ID] = &n
	}
	return rows.Err()
}

func (s *Store) loadTokens() error {
	rows, err := s.db.Query(`SELECT data_json FROM tokens`)
	if err != nil {
		return err
	}
	defer rows.Close()

	s.state.Tokens = []*types.Token{}
	for rows.Next() {
		var dataJSON string
		if err := rows.Scan(&dataJSON); err != nil {
			return err
		}
		var t types.Token
		if err := json.Unmarshal([]byte(dataJSON), &t); err != nil {
			return err
		}
		s.state.Tokens = append(s.state.Tokens, &t)
	}
	return rows.Err()
}

func (s *Store) loadCapabilities() error {
	rows, err := s.db.Query(`SELECT agent_id, data_json FROM capabilities`)
	if err != nil {
		return err
	}
	defer rows.Close()

	s.state.Capabilities = types.NewCapabilityRegistry()
	for rows.Next() {
		var agentID, dataJSON string
		if err := rows.Scan(&agentID, &dataJSON); err != nil {
			return err
		}
		var entry types.CapabilityRegistryEntry
		if err := json.Unmarshal([]byte(dataJSON), &entry); err != nil {
			return err
		}
		s.state.Capabilities.Agents[agentID] = &entry
	}
	return rows.Err()
}

func (s *Store) loadUsers() error {
	rows, err := s.db.Query(`SELECT data_json FROM users`)
	if err != nil {
		return err
	}
	defer rows.Close()

	s.state.Users = []*auth.User{}
	for rows.Next() {
		var dataJSON string
		if err := rows.Scan(&dataJSON); err != nil {
			return err
		}
		var u auth.User
		if err := json.Unmarshal([]byte(dataJSON), &u); err != nil {
			return err
		}
		s.state.Users = append(s.state.Users, &u)
	}
	return rows.Err()
}

// --- Agent persistence helpers ---

func (s *Store) upsertAgent(a *AgentState) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO agents
		(id, team, status, node_id, memory_bytes, vcpus, manifest_hash,
		 vm_pid, vm_cid, vm_socket_path, rootfs_copy_path,
		 restart_count, last_transition, started_at, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.Team, a.Status, a.NodeID, a.MemoryBytes, a.VCPUs, a.ManifestHash,
		a.VMPID, a.VMCID, a.VMSocketPath, a.RootfsCopyPath,
		a.RestartCount, formatTime(a.LastTransition), formatTime(a.StartedAt), a.Error,
	)
	return err
}

func (s *Store) deleteAgent(id string) error {
	_, err := s.db.Exec(`DELETE FROM agents WHERE id = ?`, id)
	return err
}

// --- Node persistence helpers ---

func (s *Store) upsertNode(n *types.NodeState) error {
	data, err := json.Marshal(n)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT OR REPLACE INTO nodes (id, data_json) VALUES (?, ?)`,
		n.ID, string(data))
	return err
}

func (s *Store) deleteNode(id string) error {
	_, err := s.db.Exec(`DELETE FROM nodes WHERE id = ?`, id)
	return err
}

// --- Token persistence helpers ---

func (s *Store) saveAllTokens() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM tokens`); err != nil {
		return err
	}
	for _, t := range s.state.Tokens {
		data, err := json.Marshal(t)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO tokens (hash, data_json) VALUES (?, ?)`,
			t.Hash, string(data)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// --- Capability persistence helpers ---

func (s *Store) upsertCapability(agentID string, entry *types.CapabilityRegistryEntry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT OR REPLACE INTO capabilities (agent_id, data_json) VALUES (?, ?)`,
		agentID, string(data))
	return err
}

func (s *Store) deleteCapability(agentID string) error {
	_, err := s.db.Exec(`DELETE FROM capabilities WHERE agent_id = ?`, agentID)
	return err
}

// --- User persistence helpers ---

func (s *Store) saveAllUsers() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM users`); err != nil {
		return err
	}
	for _, u := range s.state.Users {
		data, err := json.Marshal(u)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO users (id, data_json) VALUES (?, ?)`,
			u.ID, string(data)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// --- Time helpers ---

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339Nano)
}

// --- Public API (same interface as before) ---

// Load is a no-op for SQLite (data is loaded on construction).
// Kept for API compatibility.
func (s *Store) Load() error {
	return nil
}

// Save is a no-op for SQLite (each mutation persists immediately).
// Kept for API compatibility.
func (s *Store) Save() error {
	return nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	if s.db != nil {
		return s.db.Close()
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

// SetAgent updates or inserts the agent state and persists to SQLite.
// If the agent already exists and the status is changing, the transition
// is validated against the agent lifecycle state machine. New agents
// (not yet in state) are accepted with any initial status.
func (s *Store) SetAgent(agent *AgentState) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Validate state transition when the agent already exists and status changes.
	if existing, ok := s.state.Agents[agent.ID]; ok {
		if existing.Status != agent.Status {
			if err := ValidateTransition(existing.Status, agent.Status); err != nil {
				return fmt.Errorf("setting agent %s: %w", agent.ID, err)
			}
		}
	}

	// Store a copy so external mutations don't affect persisted state.
	cp := *agent
	s.state.Agents[cp.ID] = &cp

	if err := s.upsertAgent(&cp); err != nil {
		return fmt.Errorf("persisting agent %s: %w", cp.ID, err)
	}

	s.logger.Debug("agent state updated", "agent_id", cp.ID, "status", cp.Status)
	return nil
}

// ModifyAgent performs an atomic read-modify-write on agent state.
func (s *Store) ModifyAgent(id string, fn func(*AgentState) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	agent, ok := s.state.Agents[id]
	if !ok {
		return fmt.Errorf("agent %q not found in state", id)
	}

	// Work on a deep copy so the callback cannot mutate live state on error.
	cp := *agent
	oldStatus := cp.Status
	if err := fn(&cp); err != nil {
		return err
	}

	// Validate state transition if the callback changed the status.
	if cp.Status != oldStatus {
		if err := ValidateTransition(oldStatus, cp.Status); err != nil {
			return fmt.Errorf("modifying agent %s: %w", id, err)
		}
	}

	// Callback succeeded — commit the copy to live state.
	s.state.Agents[id] = &cp

	if err := s.upsertAgent(&cp); err != nil {
		return fmt.Errorf("persisting agent %s after modify: %w", id, err)
	}

	s.logger.Debug("agent state modified atomically", "agent_id", id, "status", cp.Status)
	return nil
}

// RemoveAgent removes the agent from state and persists to SQLite.
func (s *Store) RemoveAgent(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.state.Agents[id]; !ok {
		return fmt.Errorf("agent %q not found in state", id)
	}

	delete(s.state.Agents, id)

	if err := s.deleteAgent(id); err != nil {
		return fmt.Errorf("deleting agent %s from database: %w", id, err)
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

// SetNode updates or inserts a node state and persists to SQLite.
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

	if err := s.upsertNode(&cp); err != nil {
		return fmt.Errorf("persisting node %s: %w", cp.ID, err)
	}
	s.logger.Debug("node state updated", "node_id", cp.ID, "status", cp.Status)
	return nil
}

// RemoveNode removes a node from state and persists to SQLite.
func (s *Store) RemoveNode(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.state.Nodes[id]; !ok {
		return fmt.Errorf("node %q not found in state", id)
	}
	delete(s.state.Nodes, id)

	if err := s.deleteNode(id); err != nil {
		return fmt.Errorf("deleting node %s from database: %w", id, err)
	}
	s.logger.Debug("node removed from state", "node_id", id)
	return nil
}

// AllNodes returns all node states sorted alphabetically by ID.
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

// AddToken stores a hashed token and persists to SQLite.
func (s *Store) AddToken(token *types.Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cp := *token
	s.state.Tokens = append(s.state.Tokens, &cp)

	if err := s.saveAllTokens(); err != nil {
		return fmt.Errorf("persisting tokens: %w", err)
	}
	s.logger.Debug("token added", "prefix", token.Prefix)
	return nil
}

// ValidateToken checks a raw token against stored hashes.
// Returns a copy of the matching token if valid, nil otherwise.
func (s *Store) ValidateToken(rawToken string) *types.Token {
	s.mu.Lock()
	defer s.mu.Unlock()

	hash := hashToken(rawToken)
	for _, t := range s.state.Tokens {
		if t.Hash == hash && t.IsValid() {
			t.LastUsed = time.Now()
			if err := s.saveAllTokens(); err != nil {
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

	if err := s.saveAllTokens(); err != nil {
		return fmt.Errorf("persisting tokens after revoke: %w", err)
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

	entry := s.state.Capabilities.Agents[agentID]
	if err := s.upsertCapability(agentID, entry); err != nil {
		return fmt.Errorf("persisting capabilities for %s: %w", agentID, err)
	}
	s.logger.Debug("capabilities registered", "agent_id", agentID, "count", len(caps))
	return nil
}

// DeregisterCapabilities removes an agent's capabilities and persists.
func (s *Store) DeregisterCapabilities(agentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.state.Capabilities.Deregister(agentID)

	if err := s.deleteCapability(agentID); err != nil {
		return fmt.Errorf("deleting capabilities for %s from database: %w", agentID, err)
	}
	s.logger.Debug("capabilities deregistered", "agent_id", agentID)
	return nil
}

// --- User management ---

// AddUser adds a user to state and persists to SQLite.
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

	if err := s.saveAllUsers(); err != nil {
		return fmt.Errorf("persisting users: %w", err)
	}
	s.logger.Debug("user added", "user_id", user.ID, "role", user.Role)
	return nil
}

// RemoveUser removes a user from state and persists to SQLite.
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

	if err := s.saveAllUsers(); err != nil {
		return fmt.Errorf("persisting users after remove: %w", err)
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

// UpdateUser updates an existing user in state and persists to SQLite.
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

	if err := s.saveAllUsers(); err != nil {
		return fmt.Errorf("persisting users after update: %w", err)
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
