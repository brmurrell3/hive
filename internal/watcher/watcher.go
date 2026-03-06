package watcher

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watcher monitors agents/AGENT_ID/MEMORY.md files in the cluster root for
// changes and invokes a callback with the agent ID and updated file content.
type Watcher struct {
	clusterRoot string
	fsWatcher   *fsnotify.Watcher
	logger      *slog.Logger
	onChange    func(agentID string, content []byte)
	stopCh      chan struct{}
	stopOnce    sync.Once

	// debounce tracks pending debounce timers per agent ID so that rapid
	// successive writes coalesce into a single onChange invocation.
	mu       sync.Mutex
	debounce map[string]*time.Timer
}

const debounceDelay = 500 * time.Millisecond

// NewWatcher creates a Watcher that watches MEMORY.md files under
// clusterRoot/agents/*/MEMORY.md. The onChange callback is invoked after a
// debounce period whenever a watched file is modified.
func NewWatcher(clusterRoot string, onChange func(agentID string, content []byte), logger *slog.Logger) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("creating fsnotify watcher: %w", err)
	}

	return &Watcher{
		clusterRoot: clusterRoot,
		fsWatcher:   fsw,
		logger:      logger.With("component", "memory-watcher"),
		onChange:     onChange,
		stopCh:      make(chan struct{}),
		debounce:    make(map[string]*time.Timer),
	}, nil
}

// Start walks the agents/ directory to discover existing MEMORY.md files,
// adds them to the fsnotify watcher, and begins processing filesystem events
// in a background goroutine.
func (w *Watcher) Start() error {
	agentsDir := filepath.Join(w.clusterRoot, "agents")

	// Walk agents/ to find all existing MEMORY.md files.
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		if os.IsNotExist(err) {
			w.logger.Warn("agents directory does not exist, nothing to watch", "path", agentsDir)
			go w.eventLoop()
			return nil
		}
		return fmt.Errorf("reading agents directory %s: %w", agentsDir, err)
	}

	watched := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		memoryPath := filepath.Join(agentsDir, entry.Name(), "MEMORY.md")
		if _, err := os.Stat(memoryPath); err != nil {
			// MEMORY.md does not exist for this agent yet; skip.
			continue
		}

		if err := w.fsWatcher.Add(memoryPath); err != nil {
			w.logger.Warn("failed to watch MEMORY.md",
				"agent_id", entry.Name(),
				"path", memoryPath,
				"error", err,
			)
			continue
		}

		watched++
		w.logger.Debug("watching MEMORY.md", "agent_id", entry.Name(), "path", memoryPath)
	}

	go w.eventLoop()

	w.logger.Info("memory watcher started", "cluster_root", w.clusterRoot, "files_watched", watched)
	return nil
}

// Stop shuts down the watcher and releases all resources.
// It is safe to call Stop multiple times; only the first call takes effect.
func (w *Watcher) Stop() error {
	var stopErr error
	w.stopOnce.Do(func() {
		close(w.stopCh)

		// Cancel any pending debounce timers.
		w.mu.Lock()
		for _, t := range w.debounce {
			t.Stop()
		}
		w.debounce = nil
		w.mu.Unlock()

		if err := w.fsWatcher.Close(); err != nil {
			stopErr = fmt.Errorf("closing fsnotify watcher: %w", err)
			return
		}

		w.logger.Info("memory watcher stopped")
	})
	return stopErr
}

// eventLoop processes fsnotify events until the stop channel is closed.
func (w *Watcher) eventLoop() {
	for {
		select {
		case <-w.stopCh:
			return

		case event, ok := <-w.fsWatcher.Events:
			if !ok {
				return
			}
			w.handleEvent(event)

		case err, ok := <-w.fsWatcher.Errors:
			if !ok {
				return
			}
			w.logger.Error("fsnotify error", "error", err)
		}
	}
}

// handleEvent processes a single fsnotify event. Write and Create events are
// acted upon (Create handles editor atomic saves); all other event types are
// ignored.
func (w *Watcher) handleEvent(event fsnotify.Event) {
	if !event.Has(fsnotify.Write) && !event.Has(fsnotify.Create) {
		return
	}

	agentID, err := extractAgentID(w.clusterRoot, event.Name)
	if err != nil {
		w.logger.Warn("could not extract agent ID from path",
			"path", event.Name,
			"error", err,
		)
		return
	}

	w.logger.Debug("MEMORY.md write detected", "agent_id", agentID, "path", event.Name)

	w.mu.Lock()
	defer w.mu.Unlock()

	// Reset or create the debounce timer for this agent.
	if existing, ok := w.debounce[agentID]; ok {
		existing.Stop()
	}

	filePath := event.Name
	w.debounce[agentID] = time.AfterFunc(debounceDelay, func() {
		w.fireOnChange(agentID, filePath)
	})
}

// fireOnChange reads the MEMORY.md file and invokes the onChange callback.
func (w *Watcher) fireOnChange(agentID, filePath string) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		w.logger.Error("failed to read MEMORY.md after change",
			"agent_id", agentID,
			"path", filePath,
			"error", err,
		)
		return
	}

	w.logger.Info("MEMORY.md changed, invoking callback",
		"agent_id", agentID,
		"size_bytes", len(content),
	)

	w.onChange(agentID, content)
}

// extractAgentID derives the agent ID from a MEMORY.md file path by locating
// the agents/ directory component and returning the next path segment.
// Expected format: <clusterRoot>/agents/<AGENT_ID>/MEMORY.md
func extractAgentID(clusterRoot, filePath string) (string, error) {
	// Normalize paths for reliable comparison.
	clusterRoot = filepath.Clean(clusterRoot)
	filePath = filepath.Clean(filePath)

	agentsPrefix := filepath.Join(clusterRoot, "agents") + string(filepath.Separator)
	if !strings.HasPrefix(filePath, agentsPrefix) {
		return "", fmt.Errorf("path %q is not under agents directory %q", filePath, agentsPrefix)
	}

	// The remainder after stripping the prefix should be AGENT_ID/MEMORY.md.
	remainder := strings.TrimPrefix(filePath, agentsPrefix)
	parts := strings.SplitN(remainder, string(filepath.Separator), 2)
	if len(parts) < 1 || parts[0] == "" {
		return "", fmt.Errorf("cannot extract agent ID from path %q", filePath)
	}

	return parts[0], nil
}
