// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

// Package state provides SQLite-backed persistence for agents, nodes, tokens, and users with schema versioning.
package state

import (
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/brmurrell3/hive/internal/auth"
	"github.com/brmurrell3/hive/internal/types"
	_ "modernc.org/sqlite"
)

// schemaVersion is the current database schema version.
// Increment this when making schema changes, and add a migration
// in runMigrations().
const schemaVersion = 2

// DefaultTokenRetention is the default duration to keep expired tokens
// before pruning them.
const DefaultTokenRetention = 7 * 24 * time.Hour

// maxTokensInMemory is the safety limit for token count loaded into memory.
// If the database contains more tokens than this, loadTokens logs a warning
// and the SQL query is capped via LIMIT as a safety net. AllTokens returns
// at most this many entries; use AllTokensPaginated for explicit pagination.
const maxTokensInMemory = 100000

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
// This package-level map is effectively read-only after init; no code mutates it.
var validTransitions = map[AgentStatus][]AgentStatus{
	AgentStatusPending:  {AgentStatusCreating, AgentStatusFailed},
	AgentStatusCreating: {AgentStatusStarting, AgentStatusFailed},
	AgentStatusStarting: {AgentStatusRunning, AgentStatusFailed, AgentStatusStopping},
	AgentStatusRunning:  {AgentStatusStopping, AgentStatusFailed, AgentStatusCreating},
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
	SpecHash       string      `json:"spec_hash,omitempty"`
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
	mu     sync.RWMutex
	path   string
	db     *sql.DB
	state  *State
	logger *slog.Logger

	// pruneStop is used to signal the token pruning goroutine to stop.
	pruneStop    chan struct{}
	pruneWg      sync.WaitGroup
	pruneStarted bool // guards against double-start of StartTokenPruning
	closeOnce    sync.Once
}

// NewStore creates a new Store backed by a SQLite database at the given path.
// If the database exists but is corrupted (fails to load), NewStore returns
// an error instead of silently starting with fresh state.
func NewStore(path string, logger *slog.Logger) (*Store, error) {
	s := &Store{
		path:      path,
		logger:    logger,
		state:     newState(),
		pruneStop: make(chan struct{}),
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("creating state directory: %w", err)
	}

	db, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_busy_timeout=5000&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("opening state database %s: %w", path, err)
	}
	s.db = db

	// Limit SQLite to a single connection to avoid locking issues.
	db.SetMaxOpenConns(1)

	if err := s.createTables(); err != nil {
		db.Close()
		return nil, fmt.Errorf("creating state tables: %w", err)
	}

	if err := s.migrateSchema(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating schema: %w", err)
	}

	if err := s.loadFromDB(); err != nil {
		db.Close()
		return nil, fmt.Errorf("loading state from database %s: %w", path, err)
	}

	logger.Info("loaded state from database", "path", path,
		"agents", len(s.state.Agents))

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
			spec_hash TEXT NOT NULL DEFAULT '',
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
		CREATE TABLE IF NOT EXISTS schema_version (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			version INTEGER NOT NULL DEFAULT 1
		);
		CREATE INDEX IF NOT EXISTS idx_agents_status ON agents(status);
		CREATE INDEX IF NOT EXISTS idx_agents_team ON agents(team);
	`)
	return err
}

// migrateSchema checks the current schema version and runs any needed
// migrations to bring it up to the current schemaVersion.
//
// IMPORTANT: Only one process instance should run migrations at startup.
// In a multi-process deployment, ensure that only a single instance performs
// schema migration (e.g., via an init container or leader election). The
// version check and baseline insert are wrapped in a BEGIN EXCLUSIVE
// transaction to prevent TOCTOU races from concurrent SQLite connections.
//
// Migration framework:
//   - Each migration is a function that takes a *sql.Tx and returns an error.
//   - Migrations are numbered starting at 1. The schema_version table stores
//     the current version.
//   - On a fresh database (no version row), we insert version 1 (the baseline).
//   - To add a new migration: increment schemaVersion, add a case to the
//     switch in runMigrations(), and handle the schema change in a transaction.
func (s *Store) migrateSchema() error {
	// Single-connection mode (SetMaxOpenConns(1)) provides serialization of
	// all database access, preventing TOCTOU races from concurrent SQLite
	// connections. No explicit PRAGMA locking_mode is needed since the single
	// connection is the sole writer.
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("beginning migration transaction: %w", err)
	}

	var version int
	err = tx.QueryRow(`SELECT version FROM schema_version WHERE id = 1`).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		// Fresh database: insert baseline version.
		if _, err := tx.Exec(`INSERT INTO schema_version (id, version) VALUES (1, ?)`, schemaVersion); err != nil {
			tx.Rollback()
			return fmt.Errorf("inserting baseline schema version: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("committing baseline schema version: %w", err)
		}
		return nil
	}
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("reading schema version: %w", err)
	}

	// Release the exclusive transaction; runMigrations uses its own transactions.
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing version check: %w", err)
	}

	if version > schemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported version %d", version, schemaVersion)
	}

	if version < schemaVersion {
		if err := s.runMigrations(version); err != nil {
			return err
		}
	}

	return nil
}

// runMigrations applies sequential migrations from fromVersion+1 to schemaVersion.
//
// To add a new migration:
//  1. Increment the schemaVersion constant.
//  2. Add a case for the new version number in the switch below.
//  3. Each case should execute its DDL/DML on the provided *sql.Tx.
//  4. The version update and commit are handled after each successful case.
func (s *Store) runMigrations(fromVersion int) error {
	for v := fromVersion + 1; v <= schemaVersion; v++ {
		s.logger.Info("running schema migration", "from", v-1, "to", v)

		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("beginning migration %d: %w", v, err)
		}

		migrationErr := s.applyMigration(tx, v)
		if migrationErr != nil {
			tx.Rollback()
			return migrationErr
		}

		if _, err := tx.Exec(`UPDATE schema_version SET version = ? WHERE id = 1`, v); err != nil {
			tx.Rollback()
			return fmt.Errorf("updating schema version to %d: %w", v, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("committing migration %d: %w", v, err)
		}
	}
	return nil
}

// applyMigration runs schema changes for a single version step.
// Add new cases here as the schema evolves.
func (s *Store) applyMigration(tx *sql.Tx, version int) error {
	switch version {
	case 2:
		// Add spec_hash column to agents table for lightweight change detection.
		// Check if column already exists first (idempotent on retry after crash).
		exists, err := s.columnExists(tx, "agents", "spec_hash")
		if err != nil {
			return fmt.Errorf("migration %d: checking spec_hash column: %w", version, err)
		}
		if !exists {
			if _, err := tx.Exec(`ALTER TABLE agents ADD COLUMN spec_hash TEXT NOT NULL DEFAULT ''`); err != nil {
				return fmt.Errorf("migration %d: adding spec_hash column: %w", version, err)
			}
		}
		return nil
	default:
		return fmt.Errorf("unknown migration version %d", version)
	}
}

// allowedTables is the whitelist of table names that may be passed to
// columnExists. This prevents SQL injection via the table parameter,
// which is interpolated directly into the PRAGMA query.
var allowedTables = map[string]bool{
	"agents":       true,
	"nodes":        true,
	"tokens":       true,
	"users":        true,
	"capabilities": true,
}

// tableNameRegex validates that a table name contains only safe characters.
var tableNameRegex = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

// columnExists checks whether a column exists in a table using PRAGMA table_info.
// The table name must be in the allowedTables whitelist.
func (s *Store) columnExists(tx *sql.Tx, table, column string) (bool, error) {
	if !allowedTables[table] {
		return false, fmt.Errorf("columnExists: table %q is not in the allowed whitelist", table)
	}
	if !tableNameRegex.MatchString(table) {
		return false, fmt.Errorf("columnExists: table %q contains invalid characters", table)
	}

	rows, err := tx.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name, colType string
		var notNull int
		var dfltValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
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
		manifest_hash, spec_hash, vm_pid, vm_cid, vm_socket_path, rootfs_copy_path,
		restart_count, last_transition, started_at, error FROM agents`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var a AgentState
		var lastTransStr, startedAtStr string
		if err := rows.Scan(&a.ID, &a.Team, &a.Status, &a.NodeID, &a.MemoryBytes,
			&a.VCPUs, &a.ManifestHash, &a.SpecHash, &a.VMPID, &a.VMCID, &a.VMSocketPath,
			&a.RootfsCopyPath, &a.RestartCount, &lastTransStr, &startedAtStr, &a.Error); err != nil {
			return err
		}
		if lastTransStr != "" {
			if t, err := time.Parse(time.RFC3339Nano, lastTransStr); err != nil {
				s.logger.Warn("invalid LastTransition timestamp", "agent", a.ID, "value", lastTransStr, "error", err)
			} else {
				a.LastTransition = t
			}
		}
		if startedAtStr != "" {
			if t, err := time.Parse(time.RFC3339Nano, startedAtStr); err != nil {
				s.logger.Warn("invalid StartedAt timestamp", "agent", a.ID, "value", startedAtStr, "error", err)
			} else {
				a.StartedAt = t
			}
		}
		// Validate the status loaded from DB against known statuses.
		if _, known := validTransitions[a.Status]; !known {
			s.logger.Warn("unknown agent status loaded from database, setting to FAILED",
				"agent", a.ID, "status", a.Status)
			a.Status = AgentStatusFailed
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
			return fmt.Errorf("loading nodes: scan row: %w", err)
		}
		var n types.NodeState
		if err := json.Unmarshal([]byte(dataJSON), &n); err != nil {
			return fmt.Errorf("loading nodes: unmarshal row: %w", err)
		}
		s.state.Nodes[n.ID] = deepCopyNode(&n)
	}
	return rows.Err()
}

func (s *Store) loadTokens() error {
	// SQL LIMIT as a safety net to prevent unbounded memory growth.
	rows, err := s.db.Query(`SELECT data_json FROM tokens LIMIT ?`, maxTokensInMemory+1)
	if err != nil {
		return err
	}
	defer rows.Close()

	s.state.Tokens = []*types.Token{}
	for rows.Next() {
		var dataJSON string
		if err := rows.Scan(&dataJSON); err != nil {
			return fmt.Errorf("loading tokens: scan row: %w", err)
		}
		var t types.Token
		if err := json.Unmarshal([]byte(dataJSON), &t); err != nil {
			return fmt.Errorf("loading tokens: unmarshal row: %w", err)
		}
		s.state.Tokens = append(s.state.Tokens, &t)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if len(s.state.Tokens) > maxTokensInMemory {
		s.logger.Warn("token count exceeds maxTokensInMemory safety limit; consider pruning expired tokens",
			"loaded", len(s.state.Tokens), "limit", maxTokensInMemory)
		// Trim to the safety limit (extra row was fetched only for detection).
		s.state.Tokens = s.state.Tokens[:maxTokensInMemory]
	}

	return nil
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
			return fmt.Errorf("loading capabilities: scan row: %w", err)
		}
		var entry types.CapabilityRegistryEntry
		if err := json.Unmarshal([]byte(dataJSON), &entry); err != nil {
			return fmt.Errorf("loading capabilities: unmarshal row for agent %q: %w", agentID, err)
		}
		// Validate each capability name loaded from DB; skip invalid entries
		// to prevent corrupt data from propagating into the registry.
		validCaps := make([]types.AgentCapability, 0, len(entry.Capabilities))
		for _, cap := range entry.Capabilities {
			if err := types.ValidateSubjectComponent("capability_name", cap.Name); err != nil {
				s.logger.Warn("skipping invalid capability name loaded from database",
					"agent_id", agentID, "capability_name", cap.Name, "error", err)
				continue
			}
			validCaps = append(validCaps, cap)
		}
		entry.Capabilities = validCaps
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
			return fmt.Errorf("loading users: scan row: %w", err)
		}
		var u auth.User
		if err := json.Unmarshal([]byte(dataJSON), &u); err != nil {
			return fmt.Errorf("loading users: unmarshal row: %w", err)
		}
		s.state.Users = append(s.state.Users, &u)
	}
	return rows.Err()
}

// --- Agent persistence helpers ---

func (s *Store) upsertAgent(a *AgentState) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO agents
		(id, team, status, node_id, memory_bytes, vcpus, manifest_hash, spec_hash,
		 vm_pid, vm_cid, vm_socket_path, rootfs_copy_path,
		 restart_count, last_transition, started_at, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.Team, a.Status, a.NodeID, a.MemoryBytes, a.VCPUs, a.ManifestHash, a.SpecHash,
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

// saveTokensList persists the given token list to the database using targeted
// upsert and delete operations within a transaction. The DSN includes
// _txlock=immediate so all transactions acquire the write lock upfront,
// preventing the crash-unsafe window of DEFERRED transactions.
func (s *Store) saveTokensList(tokens []*types.Token) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Build a set of token hashes that should exist after this operation.
	keepHashes := make(map[string]struct{}, len(tokens))
	for _, t := range tokens {
		keepHashes[t.Hash] = struct{}{}
	}

	// Upsert each token (INSERT OR REPLACE handles both new and existing rows).
	for _, t := range tokens {
		data, err := json.Marshal(t)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO tokens (hash, data_json) VALUES (?, ?)`,
			t.Hash, string(data)); err != nil {
			return err
		}
	}

	// Delete tokens not in the keep set. Query existing hashes and delete
	// those not present in the desired set.
	rows, err := tx.Query(`SELECT hash FROM tokens`)
	if err != nil {
		return err
	}
	var toDelete []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return err
		}
		if _, keep := keepHashes[h]; !keep {
			toDelete = append(toDelete, h)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, h := range toDelete {
		if _, err := tx.Exec(`DELETE FROM tokens WHERE hash = ?`, h); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// upsertToken persists a single token by hash using INSERT OR REPLACE.
func (s *Store) upsertToken(t *types.Token) error {
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT OR REPLACE INTO tokens (hash, data_json) VALUES (?, ?)`,
		t.Hash, string(data))
	return err
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

// saveUsersList persists the given user list to the database using targeted
// upsert and delete operations within a transaction. The DSN includes
// _txlock=immediate so all transactions acquire the write lock upfront,
// preventing the crash-unsafe window of DEFERRED transactions.
func (s *Store) saveUsersList(users []*auth.User) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Build a set of user IDs that should exist after this operation.
	keepIDs := make(map[string]struct{}, len(users))
	for _, u := range users {
		keepIDs[u.ID] = struct{}{}
	}

	// Upsert each user (INSERT OR REPLACE handles both new and existing rows).
	for _, u := range users {
		data, err := json.Marshal(u)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO users (id, data_json) VALUES (?, ?)`,
			u.ID, string(data)); err != nil {
			return err
		}
	}

	// Delete users not in the keep set.
	rows, err := tx.Query(`SELECT id FROM users`)
	if err != nil {
		return err
	}
	var toDelete []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		if _, keep := keepIDs[id]; !keep {
			toDelete = append(toDelete, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, id := range toDelete {
		if _, err := tx.Exec(`DELETE FROM users WHERE id = ?`, id); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// --- Time helpers ---

// formatTime converts a time.Time to its RFC3339Nano string representation.
// A zero time is stored as an empty string in the database; on load, an empty
// string is treated as time.Time{} (the zero value). The two representations
// are equivalent for all store operations.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339Nano)
}

// --- Public API (same interface as before) ---

// Close closes the underlying database connection and stops background
// goroutines. It is safe to call multiple times.
func (s *Store) Close() error {
	var dbErr error
	s.closeOnce.Do(func() {
		// Signal the pruning goroutine (if running) to stop.
		close(s.pruneStop)
		// Wait for the pruning goroutine to finish before closing the DB.
		s.pruneWg.Wait()

		if s.db != nil {
			dbErr = s.db.Close()
		}
	})
	return dbErr
}

// GetAgent returns the agent state for the given ID, or nil if not found.
func (s *Store) GetAgent(id string) *AgentState {
	s.mu.RLock()
	defer s.mu.RUnlock()

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
// (not yet in state) must have a known initial status (a key in
// validTransitions); unknown statuses are rejected.
//
// The DB write is performed first; in-memory state is updated only on
// successful persistence.
func (s *Store) SetAgent(agent *AgentState) error {
	if agent.ID == "" {
		return fmt.Errorf("agent ID must not be empty")
	}
	if err := types.ValidateSubjectComponent("agent_id", agent.ID); err != nil {
		return fmt.Errorf("invalid agent ID: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Validate state transition when the agent already exists and status changes.
	if existing, ok := s.state.Agents[agent.ID]; ok {
		if existing.Status != agent.Status {
			if err := validateTransition(existing.Status, agent.Status); err != nil {
				return fmt.Errorf("setting agent %s: %w", agent.ID, err)
			}
		}
	} else {
		// New agent: validate that the initial status is a known status
		// (i.e., a key in validTransitions).
		if _, known := validTransitions[agent.Status]; !known {
			return fmt.Errorf("setting agent %s: unknown initial status %q", agent.ID, agent.Status)
		}
	}

	cp := *agent

	if err := s.upsertAgent(&cp); err != nil {
		return fmt.Errorf("persisting agent %s: %w", cp.ID, err)
	}

	s.state.Agents[cp.ID] = &cp

	s.logger.Debug("agent state updated", "agent_id", cp.ID, "status", cp.Status)
	return nil
}

// ModifyAgent performs an atomic read-modify-write on agent state.
//
// The DB write is performed first; in-memory state is updated only on
// successful persistence.
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
	if err := safeCallAgentFn(fn, &cp); err != nil {
		return err
	}

	if cp.Status != oldStatus {
		if err := validateTransition(oldStatus, cp.Status); err != nil {
			return fmt.Errorf("modifying agent %s: %w", id, err)
		}
	}

	if err := s.upsertAgent(&cp); err != nil {
		return fmt.Errorf("persisting agent %s after modify: %w", id, err)
	}

	s.state.Agents[id] = &cp

	s.logger.Debug("agent state modified atomically", "agent_id", id, "status", cp.Status)
	return nil
}

// RemoveAgent removes the agent from state and persists to SQLite.
//
// The DB delete is performed first; in-memory state is updated only on
// successful persistence.
func (s *Store) RemoveAgent(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.state.Agents[id]; !ok {
		return fmt.Errorf("agent %q not found in state", id)
	}

	if err := s.deleteAgent(id); err != nil {
		return fmt.Errorf("deleting agent %s from database: %w", id, err)
	}

	delete(s.state.Agents, id)

	s.logger.Debug("agent removed from state", "agent_id", id)
	return nil
}

// AllAgents returns all agent states sorted alphabetically by ID.
func (s *Store) AllAgents() []*AgentState {
	s.mu.RLock()
	defer s.mu.RUnlock()

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

// deepCopyNode returns a deep copy of a NodeState, including slices and maps.
func deepCopyNode(node *types.NodeState) *types.NodeState {
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
	if node.Hardware.GPUs != nil {
		cp.Hardware.GPUs = make([]string, len(node.Hardware.GPUs))
		copy(cp.Hardware.GPUs, node.Hardware.GPUs)
	}
	if node.Hardware.Peripherals != nil {
		cp.Hardware.Peripherals = make([]string, len(node.Hardware.Peripherals))
		copy(cp.Hardware.Peripherals, node.Hardware.Peripherals)
	}
	return &cp
}

// GetNode returns the node state for the given ID, or nil if not found.
func (s *Store) GetNode(id string) *types.NodeState {
	s.mu.RLock()
	defer s.mu.RUnlock()

	node, ok := s.state.Nodes[id]
	if !ok {
		return nil
	}
	return deepCopyNode(node)
}

// SetNode updates or inserts a node state and persists to SQLite.
//
// The DB write is performed first; in-memory state is updated only on
// successful persistence.
func (s *Store) SetNode(node *types.NodeState) error {
	if node.ID == "" {
		return fmt.Errorf("node ID must not be empty")
	}
	if err := types.ValidateSubjectComponent("node_id", node.ID); err != nil {
		return fmt.Errorf("invalid node ID: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	cp := deepCopyNode(node)

	if err := s.upsertNode(cp); err != nil {
		return fmt.Errorf("persisting node %s: %w", cp.ID, err)
	}

	s.state.Nodes[cp.ID] = cp

	s.logger.Debug("node state updated", "node_id", cp.ID, "status", cp.Status)
	return nil
}

// RemoveNode removes a node from state and persists to SQLite.
//
// The DB delete is performed first; in-memory state is updated only on
// successful persistence.
func (s *Store) RemoveNode(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.state.Nodes[id]; !ok {
		return fmt.Errorf("node %q not found in state", id)
	}

	if err := s.deleteNode(id); err != nil {
		return fmt.Errorf("deleting node %s from database: %w", id, err)
	}

	delete(s.state.Nodes, id)

	s.logger.Debug("node removed from state", "node_id", id)
	return nil
}

// ModifyNode performs an atomic read-modify-write on node state.
// The fn callback receives a deep copy of the node; changes are persisted
// only if fn returns nil and the DB write succeeds.
func (s *Store) ModifyNode(id string, fn func(*types.NodeState) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	node, ok := s.state.Nodes[id]
	if !ok {
		return fmt.Errorf("node %q not found in state", id)
	}

	cp := deepCopyNode(node)
	if err := safeCallNodeFn(fn, cp); err != nil {
		return err
	}

	if err := s.upsertNode(cp); err != nil {
		return fmt.Errorf("persisting node %s after modify: %w", id, err)
	}

	s.state.Nodes[id] = cp

	s.logger.Debug("node state modified atomically", "node_id", id, "status", cp.Status)
	return nil
}

// safeCallAgentFn invokes a user-supplied callback with panic recovery so that
// a panicking callback does not crash the process.
func safeCallAgentFn(fn func(*AgentState) error, agent *AgentState) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("ModifyAgent callback panicked: %v", r)
		}
	}()
	return fn(agent)
}

// safeCallNodeFn invokes a user-supplied callback with panic recovery so that
// a panicking callback does not crash the process.
func safeCallNodeFn(fn func(*types.NodeState) error, node *types.NodeState) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("ModifyNode callback panicked: %v", r)
		}
	}()
	return fn(node)
}

// safeCallUserFn invokes a user-supplied callback with panic recovery so that
// a panicking callback does not crash the process.
func safeCallUserFn(fn func(*auth.User) error, user *auth.User) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("ModifyUser callback panicked: %v", r)
		}
	}()
	return fn(user)
}

// FindNodeByHostnameArch returns the first node matching the given hostname
// and architecture, or nil if none is found. The caller must hold no lock.
func (s *Store) FindNodeByHostnameArch(hostname, arch string) *types.NodeState {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, n := range s.state.Nodes {
		if n.Hostname == hostname && n.Arch == arch {
			return deepCopyNode(n)
		}
	}
	return nil
}

// AllNodes returns all node states sorted alphabetically by ID.
func (s *Store) AllNodes() []*types.NodeState {
	s.mu.RLock()
	defer s.mu.RUnlock()

	nodes := make([]*types.NodeState, 0, len(s.state.Nodes))
	for _, n := range s.state.Nodes {
		nodes = append(nodes, deepCopyNode(n))
	}
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].ID < nodes[j].ID
	})
	return nodes
}

// NodeCount returns the number of registered nodes.
func (s *Store) NodeCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.state.Nodes)
}

// --- Token management ---

// AddToken stores a hashed token and persists to SQLite.
//
// The DB write is performed first; in-memory state is updated only on
// successful persistence.
func (s *Store) AddToken(token *types.Token) error {
	if token.Hash == "" || token.Prefix == "" {
		return fmt.Errorf("token hash and prefix must not be empty")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Check for duplicate token hash to prevent silent overwrites.
	for _, existing := range s.state.Tokens {
		if subtle.ConstantTimeCompare([]byte(existing.Hash), []byte(token.Hash)) == 1 {
			return fmt.Errorf("token with hash prefix %q already exists", token.Prefix)
		}
	}

	cp := *token

	if err := s.upsertToken(&cp); err != nil {
		return fmt.Errorf("persisting token: %w", err)
	}

	s.state.Tokens = append(s.state.Tokens, &cp)

	s.logger.Debug("token added", "prefix", token.Prefix)
	return nil
}

// ValidateToken checks a raw token against stored hashes.
// Returns a copy of the matching token if valid, nil otherwise.
//
// Uses a targeted UPDATE query for LastUsed and UsageCount instead of
// rewriting all tokens. If MaxUses is set and UsageCount reaches it,
// the token is automatically revoked.
func (s *Store) ValidateToken(rawToken string) *types.Token {
	s.mu.Lock()
	defer s.mu.Unlock()

	hash := hashToken(rawToken)
	for _, t := range s.state.Tokens {
		if subtle.ConstantTimeCompare([]byte(t.Hash), []byte(hash)) == 1 && t.IsValid() {
			now := time.Now()

			updatedToken := *t
			updatedToken.LastUsed = now
			updatedToken.UsageCount++

			if updatedToken.MaxUses > 0 && updatedToken.UsageCount >= updatedToken.MaxUses {
				updatedToken.Revoked = true
				s.logger.Info("token max uses reached, auto-revoking",
					"prefix", updatedToken.Prefix,
					"usage_count", updatedToken.UsageCount,
					"max_uses", updatedToken.MaxUses,
				)
			}

			if err := s.upsertToken(&updatedToken); err != nil {
				s.logger.Error("failed to persist token usage update, rejecting token", "error", err)
				// Cannot safely track usage increment; reject to prevent reuse past MaxUses after restart.
				return nil
			}

			// O(N) linear scan over tokens. Acceptable because token counts are
			// bounded by maxTokensInMemory and this path is not hot.
			for i := range s.state.Tokens {
				if s.state.Tokens[i].Hash == updatedToken.Hash {
					cp := updatedToken
					s.state.Tokens[i] = &cp
					break
				}
			}

			cp := updatedToken
			return &cp
		}
	}
	return nil
}

// DecrementTokenUsage decrements a token's usage count by 1 as a best-effort
// compensating action when a token was validated but the subsequent operation
// (e.g. node registration) failed. If the token was auto-revoked due to
// reaching MaxUses, it is un-revoked since the use didn't actually succeed.
// This provides best-effort recovery for the TOCTOU between ValidateToken
// and the operation that consumes the token.
func (s *Store) DecrementTokenUsage(tokenHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, t := range s.state.Tokens {
		if subtle.ConstantTimeCompare([]byte(t.Hash), []byte(tokenHash)) == 1 {
			if t.UsageCount <= 0 {
				return nil // nothing to decrement
			}

			updated := *t
			updated.UsageCount--

			// If the token was auto-revoked because it hit MaxUses, un-revoke
			// it since the use that triggered the revocation didn't succeed.
			if updated.MaxUses > 0 && updated.UsageCount < updated.MaxUses && t.Revoked {
				updated.Revoked = false
			}

			if err := s.upsertToken(&updated); err != nil {
				return fmt.Errorf("persisting token usage decrement: %w", err)
			}

			cp := updated
			s.state.Tokens[i] = &cp
			return nil
		}
	}
	return fmt.Errorf("token not found")
}

// AllTokens returns all tokens (for listing), capped at maxTokensInMemory.
// For explicit pagination, use AllTokensPaginated.
func (s *Store) AllTokens() []*types.Token {
	s.mu.RLock()
	defer s.mu.RUnlock()

	count := len(s.state.Tokens)
	if count > maxTokensInMemory {
		count = maxTokensInMemory
	}

	tokens := make([]*types.Token, 0, count)
	for i, t := range s.state.Tokens {
		if i >= count {
			break
		}
		cp := *t
		tokens = append(tokens, &cp)
	}
	return tokens
}

// AllTokensPaginated returns a page of tokens with the given limit and offset.
// If limit <= 0, it defaults to maxTokensInMemory. If offset is out of range,
// an empty slice is returned.
func (s *Store) AllTokensPaginated(limit, offset int) []*types.Token {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 || limit > maxTokensInMemory {
		limit = maxTokensInMemory
	}
	if offset < 0 {
		offset = 0
	}

	total := len(s.state.Tokens)
	if offset >= total {
		return []*types.Token{}
	}

	end := offset + limit
	if end > total {
		end = total
	}

	tokens := make([]*types.Token, 0, end-offset)
	for i := offset; i < end; i++ {
		cp := *s.state.Tokens[i]
		tokens = append(tokens, &cp)
	}
	return tokens
}

// RevokeToken revokes a token matching the given prefix.
//
// A copy of the token list is built with the revocation applied, persisted
// to DB first, and only then committed to in-memory state.
func (s *Store) RevokeToken(prefix string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Build a candidate token list with revocations applied to copies.
	revokeCount := 0
	candidateTokens := make([]*types.Token, len(s.state.Tokens))
	for i, t := range s.state.Tokens {
		if t.Prefix == prefix {
			cp := *t
			cp.Revoked = true
			candidateTokens[i] = &cp
			revokeCount++
		} else {
			// Deep-copy non-revoked tokens to avoid sharing pointers
			// between the old and candidate slices.
			cp := *t
			candidateTokens[i] = &cp
		}
	}
	if revokeCount == 0 {
		return fmt.Errorf("no token found with prefix %q", prefix)
	}

	if err := s.saveTokensList(candidateTokens); err != nil {
		return fmt.Errorf("persisting tokens after revoke: %w", err)
	}

	s.state.Tokens = candidateTokens

	s.logger.Debug("token revoked", "prefix", prefix, "count", revokeCount)
	return nil
}

// PruneExpiredTokens removes expired tokens that have been expired for
// longer than the given retention duration. This reduces token table bloat.
func (s *Store) PruneExpiredTokens(retention time.Duration) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-retention)

	kept := make([]*types.Token, 0, len(s.state.Tokens))
	pruned := 0
	for _, t := range s.state.Tokens {
		// Prune if: expired AND the expiry time is before the cutoff,
		// OR revoked AND the token was created before the cutoff.
		shouldPrune := false
		if !t.ExpiresAt.IsZero() && t.IsExpired() && t.ExpiresAt.Before(cutoff) {
			shouldPrune = true
		}
		if t.Revoked && t.CreatedAt.Before(cutoff) {
			shouldPrune = true
		}
		if shouldPrune {
			pruned++
		} else {
			cp := *t
			kept = append(kept, &cp)
		}
	}

	if pruned == 0 {
		return 0, nil
	}

	if err := s.saveTokensList(kept); err != nil {
		return 0, fmt.Errorf("persisting tokens after prune: %w", err)
	}

	s.state.Tokens = kept

	s.logger.Info("pruned expired tokens", "count", pruned, "remaining", len(kept))
	return pruned, nil
}

// StartTokenPruning starts a background goroutine that periodically prunes
// expired tokens. The goroutine runs every interval and removes tokens that
// have been expired for longer than retention. Call Close() to stop it.
func (s *Store) StartTokenPruning(interval, retention time.Duration) {
	s.mu.Lock()
	if s.pruneStarted {
		s.mu.Unlock()
		s.logger.Warn("StartTokenPruning called more than once, ignoring")
		return
	}
	s.pruneStarted = true
	s.pruneWg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.pruneWg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-s.pruneStop:
				return
			case <-ticker.C:
				if pruned, err := s.PruneExpiredTokens(retention); err != nil {
					s.logger.Error("token pruning failed", "error", err)
				} else if pruned > 0 {
					s.logger.Info("periodic token prune completed", "pruned", pruned)
				}
			}
		}
	}()
}

// hashToken delegates to auth.HashToken to avoid duplicate implementations.
func hashToken(raw string) string {
	return auth.HashToken(raw)
}

// HashToken is exported for use by token generation code and tests.
// Deprecated: Use auth.HashToken directly. This wrapper exists only
// for backward compatibility.
func HashToken(raw string) string {
	return auth.HashToken(raw)
}

// --- Capability Registry ---
//
// Locking note: The CapabilityRegistry's own mutex is NOT used when accessed
// through Store methods. All access MUST go through the Store to ensure
// proper synchronization via the Store's RWMutex. The registry's own mutex
// exists only for standalone usage outside the Store (e.g., in tests).
// Callers must never bypass Store methods to access the registry directly.

// GetCapabilityRegistry returns a deep copy of the capability registry.
//
// Performance note: This performs a full deep copy including all nested slices
// and the Required *bool fields on every CapabilityParam. The cost is O(agents
// * capabilities * params). For hot paths that only need to read capability
// names or routing info, consider adding a targeted read-only accessor instead
// of copying the entire registry. The bool pointer is copied by value
// (dereferenced) to avoid per-field heap allocation overhead.
func (s *Store) GetCapabilityRegistry() *types.CapabilityRegistry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	reg := types.NewCapabilityRegistry()
	for k, v := range s.state.Capabilities.Agents {
		cp := *v
		if v.Capabilities != nil {
			cp.Capabilities = make([]types.AgentCapability, len(v.Capabilities))
			for i, c := range v.Capabilities {
				cc := c
				if c.Inputs != nil {
					cc.Inputs = make([]types.CapabilityParam, len(c.Inputs))
					for j, param := range c.Inputs {
						copiedParam := param
						if param.Required != nil {
							// Copy the bool by value to avoid a pointer-to-pointer
							// alias. This dereferences once instead of allocating a
							// new *bool on the heap for each parameter.
							req := *param.Required
							copiedParam.Required = &req
						}
						cc.Inputs[j] = copiedParam
					}
				}
				if c.Outputs != nil {
					cc.Outputs = make([]types.CapabilityParam, len(c.Outputs))
					for j, param := range c.Outputs {
						copiedParam := param
						if param.Required != nil {
							req := *param.Required
							copiedParam.Required = &req
						}
						cc.Outputs[j] = copiedParam
					}
				}
				cp.Capabilities[i] = cc
			}
		}
		reg.Agents[k] = &cp
	}
	return reg
}

// RegisterCapabilities registers an agent's capabilities and persists.
//
// The DB write is performed first; in-memory state is updated only on
// successful persistence.
func (s *Store) RegisterCapabilities(agentID, teamID, tier, nodeID string, caps []types.AgentCapability) error {
	if agentID == "" {
		return fmt.Errorf("agent ID must not be empty")
	}
	if err := types.ValidateSubjectComponent("agent_id", agentID); err != nil {
		return fmt.Errorf("invalid agent ID: %w", err)
	}

	// Validate each capability name.
	for _, c := range caps {
		if err := types.ValidateSubjectComponent("capability_name", c.Name); err != nil {
			return fmt.Errorf("invalid capability name %q: %w", c.Name, err)
		}
	}

	// Deep copy caps slice and each capability's Inputs/Outputs slices,
	// including the Required *bool pointer to avoid aliasing.
	copiedCaps := make([]types.AgentCapability, len(caps))
	for i, c := range caps {
		cc := c
		if c.Inputs != nil {
			cc.Inputs = make([]types.CapabilityParam, len(c.Inputs))
			for j, param := range c.Inputs {
				copiedParam := param
				if param.Required != nil {
					req := *param.Required
					copiedParam.Required = &req
				}
				cc.Inputs[j] = copiedParam
			}
		}
		if c.Outputs != nil {
			cc.Outputs = make([]types.CapabilityParam, len(c.Outputs))
			for j, param := range c.Outputs {
				copiedParam := param
				if param.Required != nil {
					req := *param.Required
					copiedParam.Required = &req
				}
				cc.Outputs[j] = copiedParam
			}
		}
		copiedCaps[i] = cc
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Build the entry without mutating live state yet.
	entry := &types.CapabilityRegistryEntry{
		TeamID:       teamID,
		Tier:         tier,
		NodeID:       nodeID,
		Capabilities: copiedCaps,
	}

	// DB write first.
	if err := s.upsertCapability(agentID, entry); err != nil {
		return fmt.Errorf("persisting capabilities for %s: %w", agentID, err)
	}

	// DB succeeded; update in-memory state.
	s.state.Capabilities.Agents[agentID] = entry

	s.logger.Debug("capabilities registered", "agent_id", agentID, "count", len(copiedCaps))
	return nil
}

// DeregisterCapabilities removes an agent's capabilities and persists.
func (s *Store) DeregisterCapabilities(agentID string) error {
	if err := types.ValidateSubjectComponent("agent_id", agentID); err != nil {
		return fmt.Errorf("invalid agent ID: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// DB delete first.
	if err := s.deleteCapability(agentID); err != nil {
		return fmt.Errorf("deleting capabilities for %s from database: %w", agentID, err)
	}

	// DB succeeded; update in-memory state directly (avoid acquiring the
	// registry's own mutex while already holding the Store lock).
	delete(s.state.Capabilities.Agents, agentID)

	s.logger.Debug("capabilities deregistered", "agent_id", agentID)
	return nil
}

// --- User management ---

// AddUser adds a user to state and persists to SQLite.
//
// The DB write is performed first; in-memory state is updated only on
// successful persistence.
func (s *Store) AddUser(user *auth.User) error {
	if user.ID == "" {
		return fmt.Errorf("user ID must not be empty")
	}
	if err := auth.ValidateRole(user.Role); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Check for duplicate IDs.
	for _, u := range s.state.Users {
		if u.ID == user.ID {
			return fmt.Errorf("user %q already exists", user.ID)
		}
	}

	cp := deepCopyUser(user)

	// Build candidate list for DB write.
	candidateUsers := make([]*auth.User, len(s.state.Users), len(s.state.Users)+1)
	copy(candidateUsers, s.state.Users)
	candidateUsers = append(candidateUsers, cp)

	// DB write first with candidate data (no in-memory swap).
	if err := s.saveUsersList(candidateUsers); err != nil {
		return fmt.Errorf("persisting users: %w", err)
	}

	// DB succeeded; now update in-memory state.
	s.state.Users = candidateUsers

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
		users = append(users, deepCopyUser(u))
	}

	if !found {
		return fmt.Errorf("user %q not found", userID)
	}

	// DB write first with candidate data (no in-memory swap).
	if err := s.saveUsersList(users); err != nil {
		return fmt.Errorf("persisting users after remove: %w", err)
	}

	// DB succeeded; now update in-memory state.
	s.state.Users = users

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
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, u := range s.state.Users {
		if u.ID == userID {
			return deepCopyUser(u)
		}
	}
	return nil
}

// AllUsers returns all users sorted alphabetically by ID.
func (s *Store) AllUsers() []*auth.User {
	s.mu.RLock()
	defer s.mu.RUnlock()

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
//
// Uses deepCopyUser for proper deep copying. The DB write is performed first;
// in-memory state is updated only on successful persistence.
func (s *Store) UpdateUser(user *auth.User) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	foundIdx := -1
	for i, u := range s.state.Users {
		if u.ID == user.ID {
			foundIdx = i
			break
		}
	}

	if foundIdx < 0 {
		return fmt.Errorf("user %q not found", user.ID)
	}

	cp := deepCopyUser(user)

	// Build candidate list with deep copies of each user.
	candidateUsers := make([]*auth.User, len(s.state.Users))
	for i, u := range s.state.Users {
		candidateUsers[i] = deepCopyUser(u)
	}
	candidateUsers[foundIdx] = cp

	// DB write first with candidate data (no in-memory swap).
	if err := s.saveUsersList(candidateUsers); err != nil {
		return fmt.Errorf("persisting users after update: %w", err)
	}

	// DB succeeded; now update in-memory state.
	s.state.Users = candidateUsers

	s.logger.Debug("user updated", "user_id", user.ID, "role", user.Role)
	return nil
}

// ModifyUser performs an atomic read-modify-write on user state.
// The fn callback receives a deep copy of the user; changes are persisted
// only if fn returns nil and the DB write succeeds.
func (s *Store) ModifyUser(userID string, fn func(*auth.User) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	foundIdx := -1
	for i, u := range s.state.Users {
		if u.ID == userID {
			foundIdx = i
			break
		}
	}
	if foundIdx < 0 {
		return fmt.Errorf("user %q not found", userID)
	}

	cp := deepCopyUser(s.state.Users[foundIdx])

	if err := safeCallUserFn(fn, cp); err != nil {
		return err
	}

	// Build candidate list with deep copies of each user.
	candidateUsers := make([]*auth.User, len(s.state.Users))
	for i, u := range s.state.Users {
		candidateUsers[i] = deepCopyUser(u)
	}
	candidateUsers[foundIdx] = cp

	// DB write first.
	if err := s.saveUsersList(candidateUsers); err != nil {
		return fmt.Errorf("persisting users after modify: %w", err)
	}

	// DB succeeded; commit to in-memory state.
	s.state.Users = candidateUsers

	s.logger.Debug("user modified atomically", "user_id", userID, "role", cp.Role)
	return nil
}

// CheckTransition is the exported entry point for external packages (e.g., vm)
// that need to validate agent state transitions.
func CheckTransition(from, to AgentStatus) error {
	return validateTransition(from, to)
}

// validateTransition checks whether a transition from one status to another
// is allowed by the agent lifecycle state machine.
func validateTransition(from, to AgentStatus) error {
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
