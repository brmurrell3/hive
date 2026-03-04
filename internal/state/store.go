package state

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
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
	AgentStatusStarting: {AgentStatusRunning, AgentStatusFailed},
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
	Agents map[string]*AgentState `json:"agents"`
}

// Store manages the runtime state persistence via state.json.
type Store struct {
	mu     sync.Mutex
	path   string
	state  *State
	logger *slog.Logger
}

// NewStore creates a new Store backed by the given file path.
// If the file exists it is loaded; otherwise an empty state is created.
func NewStore(path string, logger *slog.Logger) (*Store, error) {
	s := &Store{
		path:   path,
		logger: logger,
		state: &State{
			Agents: make(map[string]*AgentState),
		},
	}

	if _, err := os.Stat(path); err == nil {
		if err := s.Load(); err != nil {
			return nil, fmt.Errorf("loading state from %s: %w", path, err)
		}
		logger.Info("loaded existing state", "path", path, "agents", len(s.state.Agents))
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

// saveLocked performs the atomic save while the caller already holds the mutex.
func (s *Store) saveLocked() error {
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating state directory: %w", err)
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
