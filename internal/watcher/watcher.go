// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

// Package watcher hot-reloads agent MEMORY.md files using fsnotify and triggers callbacks on changes.
package watcher

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/brmurrell3/hive/internal/types"
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
	wg          sync.WaitGroup

	// debounce tracks pending debounce timers per agent ID so that rapid
	// successive writes coalesce into a single onChange invocation.
	mu       sync.Mutex
	debounce map[string]*time.Timer
	timerWg  sync.WaitGroup // tracks in-flight AfterFunc goroutines
	stopped  bool
	started  bool
}

const debounceDelay = 500 * time.Millisecond

// onChangeTimeout is the maximum time allowed for an onChange callback to complete
// before a warning is logged.
const onChangeTimeout = 30 * time.Second

// maxFileSize is the maximum MEMORY.md file size (1 MiB) the watcher will
// read. Files exceeding this limit are logged and skipped.
const maxFileSize = 1 * 1024 * 1024

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
		onChange:    onChange,
		stopCh:      make(chan struct{}),
		debounce:    make(map[string]*time.Timer),
	}, nil
}

// Start walks the agents/ directory to discover existing MEMORY.md files,
// adds them to the fsnotify watcher, and begins processing filesystem events
// in a background goroutine. It also watches the agents/ directory itself
// so that new agent subdirectories are automatically discovered.
func (w *Watcher) Start() error {
	w.mu.Lock()
	if w.started {
		w.mu.Unlock()
		return fmt.Errorf("watcher already started")
	}
	w.started = true
	w.mu.Unlock()

	agentsDir := filepath.Join(w.clusterRoot, "agents")

	// Watch the agents/ directory itself for new subdirectory creation.
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		return fmt.Errorf("creating agents directory %s: %w", agentsDir, err)
	}
	if err := w.fsWatcher.Add(agentsDir); err != nil {
		w.logger.Warn("failed to watch agents directory for new agents",
			"path", agentsDir,
			"error", err,
		)
	}

	// Walk agents/ to find all existing MEMORY.md files.
	entries, err := os.ReadDir(agentsDir)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading agents directory %s: %w", agentsDir, err)
	}
	if os.IsNotExist(err) {
		w.logger.Warn("agents directory does not exist, nothing to watch", "path", agentsDir)
		entries = nil
	}

	watched := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		// Watch each agent subdirectory so we can detect MEMORY.md creation.
		agentDir := filepath.Join(agentsDir, entry.Name())
		if err := w.fsWatcher.Add(agentDir); err != nil {
			w.logger.Warn("failed to watch agent directory",
				"agent_id", entry.Name(),
				"path", agentDir,
				"error", err,
			)
		}

		memoryPath := filepath.Join(agentDir, "MEMORY.md")
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

	w.wg.Add(1)
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

		// Cancel any pending debounce timers. If Stop() returns true the
		// callback was prevented from running, so we must decrement timerWg
		// to avoid blocking on Wait().
		w.mu.Lock()
		w.stopped = true
		for _, t := range w.debounce {
			if t.Stop() {
				w.timerWg.Done()
			}
		}
		w.debounce = nil
		w.mu.Unlock()

		if err := w.fsWatcher.Close(); err != nil {
			stopErr = fmt.Errorf("closing fsnotify watcher: %w", err)
			// Still wait for the eventLoop goroutine and timers to exit.
			w.wg.Wait()
			w.timerWg.Wait()
			return
		}

		// Wait for the eventLoop goroutine to finish.
		w.wg.Wait()

		// Wait for any in-flight AfterFunc goroutines to complete.
		w.timerWg.Wait()

		w.logger.Info("memory watcher stopped")
	})
	return stopErr
}

// eventLoop processes fsnotify events until the stop channel is closed.
func (w *Watcher) eventLoop() {
	defer w.wg.Done()
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
// ignored. Directory creation events in agents/ trigger watching of new
// agent subdirectories.
func (w *Watcher) handleEvent(event fsnotify.Event) {
	// Handle removal events: clean up watches for deleted directories.
	if event.Has(fsnotify.Remove) {
		w.handleRemove(event.Name)
		return
	}

	if !event.Has(fsnotify.Write) && !event.Has(fsnotify.Create) {
		return
	}

	// Check if this is a new subdirectory created under agents/.
	agentsDir := filepath.Join(w.clusterRoot, "agents")
	if event.Has(fsnotify.Create) {
		info, err := os.Stat(event.Name)
		if err == nil && info.IsDir() {
			// A new agent directory was created. Watch it for MEMORY.md.
			dir := filepath.Clean(event.Name)
			parent := filepath.Dir(dir)
			if filepath.Clean(parent) == filepath.Clean(agentsDir) {
				w.logger.Info("new agent directory detected, adding watch",
					"path", dir,
				)
				if err := w.fsWatcher.Add(dir); err != nil {
					w.logger.Warn("failed to watch new agent directory",
						"path", dir,
						"error", err,
					)
				}
				// Check if MEMORY.md already exists in the new directory.
				memPath := filepath.Join(dir, "MEMORY.md")
				if _, statErr := os.Stat(memPath); statErr == nil {
					if err := w.fsWatcher.Add(memPath); err != nil {
						w.logger.Warn("failed to watch MEMORY.md in new agent directory",
							"path", memPath,
							"error", err,
						)
					}
				}
				return
			}
		}
	}

	// Check if this is a MEMORY.md file being created in an agent directory.
	if event.Has(fsnotify.Create) && filepath.Base(event.Name) == "MEMORY.md" {
		if err := w.fsWatcher.Add(event.Name); err != nil {
			w.logger.Warn("failed to watch newly created MEMORY.md",
				"path", event.Name,
				"error", err,
			)
		}
	}

	agentID, err := extractAgentID(w.clusterRoot, event.Name)
	if err != nil {
		// Not a file under agents/*/; might be the agents dir itself.
		return
	}

	// Only process MEMORY.md files.
	if filepath.Base(event.Name) != "MEMORY.md" {
		return
	}

	w.logger.Debug("MEMORY.md write detected", "agent_id", agentID, "path", event.Name)

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.stopped {
		return
	}

	// Reset or create the debounce timer for this agent.
	// If Stop() returns true, the old callback was prevented from running
	// so we must decrement timerWg for the superseded timer.
	if existing, ok := w.debounce[agentID]; ok {
		if existing.Stop() {
			w.timerWg.Done()
		}
	}

	filePath := event.Name
	w.timerWg.Add(1)
	w.debounce[agentID] = time.AfterFunc(debounceDelay, func() {
		defer w.timerWg.Done()

		// Recover from panics in the onChange callback to prevent the
		// watcher from crashing. The lock is NOT held here so a panic
		// will not cause a deadlock.
		defer func() {
			if r := recover(); r != nil {
				w.logger.Error("panic in onChange callback, recovered",
					"agent_id", agentID,
					"panic", fmt.Sprintf("%v", r),
				)
			}
		}()

		// Hold the lock only long enough to check the stopped flag and
		// clean up the debounce entry. Release it before calling
		// fireOnChange which does file I/O and invokes the user's
		// callback, so that change detection is not blocked.
		w.mu.Lock()
		if w.stopped {
			w.mu.Unlock()
			return
		}
		delete(w.debounce, agentID)
		w.mu.Unlock()

		// Re-resolve the agentID from filePath at fire time rather than
		// using the closure-captured value. The file may have been moved
		// or replaced between the event and the timer firing.
		currentAgentID, err := extractAgentID(w.clusterRoot, filePath)
		if err != nil {
			w.logger.Warn("stale debounce: cannot re-resolve agentID from path, skipping",
				"original_agent_id", agentID,
				"path", filePath,
				"error", err,
			)
			return
		}
		if currentAgentID != agentID {
			w.logger.Warn("stale debounce: agentID changed between event and fire, skipping",
				"original_agent_id", agentID,
				"current_agent_id", currentAgentID,
				"path", filePath,
			)
			return
		}

		// Staleness check: verify the file still exists before proceeding.
		if _, statErr := os.Stat(filePath); statErr != nil {
			w.logger.Warn("stale debounce: file no longer exists at fire time, skipping",
				"agent_id", agentID,
				"path", filePath,
				"error", statErr,
			)
			return
		}

		w.fireOnChange(agentID, filePath)
	})
}

// handleRemove cleans up watches when a directory or file is removed.
func (w *Watcher) handleRemove(path string) {
	// Explicitly remove the fsnotify watch for the path. Ignore errors
	// since the path may already be gone or the watch may have been
	// removed automatically.
	_ = w.fsWatcher.Remove(path)

	agentsDir := filepath.Join(w.clusterRoot, "agents")
	dir := filepath.Clean(path)
	parent := filepath.Dir(dir)

	// If a direct child of agents/ was removed, it was an agent directory.
	if filepath.Clean(parent) == filepath.Clean(agentsDir) {
		agentID := filepath.Base(dir)
		w.logger.Info("agent directory removed, cleaning up",
			"agent_id", agentID,
			"path", dir,
		)
		// Clean up debounce timers for the removed agent. If Stop()
		// returns true, the callback was prevented, so decrement timerWg.
		w.mu.Lock()
		if timer, ok := w.debounce[agentID]; ok {
			if timer.Stop() {
				w.timerWg.Done()
			}
			delete(w.debounce, agentID)
		}
		w.mu.Unlock()
	}
}

// fireOnChange reads the MEMORY.md file and invokes the onChange callback.
// Files larger than maxFileSize (1 MiB) are rejected to prevent memory issues.
func (w *Watcher) fireOnChange(agentID, filePath string) {
	// Verify the resolved path has not escaped the agents directory via
	// symlinks before reading the file content.
	resolvedPath, err := filepath.EvalSymlinks(filePath)
	if err != nil {
		w.logger.Error("failed to resolve symlinks for MEMORY.md",
			"agent_id", agentID,
			"path", filePath,
			"error", err,
		)
		return
	}
	agentsDir := filepath.Clean(filepath.Join(w.clusterRoot, "agents"))
	// Also resolve symlinks on the agents directory itself so that the
	// comparison works when the cluster root contains symlinks (e.g.
	// /var -> /private/var on macOS).
	resolvedAgentsDir, err := filepath.EvalSymlinks(agentsDir)
	if err != nil {
		w.logger.Error("failed to resolve symlinks for agents directory",
			"path", agentsDir,
			"error", err,
		)
		return
	}
	if !strings.HasPrefix(filepath.Clean(resolvedPath), filepath.Clean(resolvedAgentsDir)+string(filepath.Separator)) {
		w.logger.Warn("MEMORY.md resolved path escapes agents directory, skipping",
			"agent_id", agentID,
			"path", filePath,
			"resolved_path", resolvedPath,
			"agents_dir", resolvedAgentsDir,
		)
		return
	}

	// Use os.Open + io.LimitReader to hard-limit bytes read, eliminating
	// the TOCTOU race between os.Stat() and os.ReadFile() where the file
	// could grow between the two calls.
	f, err := os.Open(resolvedPath)
	if err != nil {
		w.logger.Error("failed to open MEMORY.md after change",
			"agent_id", agentID,
			"path", filePath,
			"error", err,
		)
		return
	}
	defer f.Close()

	// Read at most maxFileSize+1 bytes. If we get more than maxFileSize
	// bytes, the file is too large.
	content, err := io.ReadAll(io.LimitReader(f, int64(maxFileSize)+1))
	if err != nil {
		w.logger.Error("failed to read MEMORY.md after change",
			"agent_id", agentID,
			"path", filePath,
			"error", err,
		)
		return
	}
	if int64(len(content)) > maxFileSize {
		w.logger.Warn("MEMORY.md exceeds maximum file size, skipping",
			"agent_id", agentID,
			"path", filePath,
			"max_bytes", maxFileSize,
		)
		return
	}

	w.logger.Info("MEMORY.md changed, invoking callback",
		"agent_id", agentID,
		"size_bytes", len(content),
	)

	onChangeDone := make(chan struct{})
	go func() {
		defer close(onChangeDone)
		w.onChange(agentID, content)
	}()
	timer := time.NewTimer(onChangeTimeout)
	select {
	case <-onChangeDone:
		timer.Stop()
	case <-timer.C:
		w.logger.Warn("onChange callback timed out",
			"agent_id", agentID,
			"timeout", onChangeTimeout,
		)
		<-onChangeDone // still wait for completion to avoid data races
	}
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

	id := parts[0]

	// Validate the extracted agent ID to ensure it contains only safe characters.
	if err := types.ValidateSubjectComponent("agent_id", id); err != nil {
		return "", fmt.Errorf("invalid agent ID %q extracted from path %q: %w", id, filePath, err)
	}

	return id, nil
}
