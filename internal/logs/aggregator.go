// Package logs provides log aggregation for Hive agents. It subscribes to
// NATS log subjects (hive.logs.>), persists log entries to per-agent JSONL
// files with rotation and retention, and supports querying and following.
package logs

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hivehq/hive/internal/types"
	"github.com/nats-io/nats.go"
)

const (
	defaultRetentionDays = 30
	defaultMaxFileSize   = 100 * 1024 * 1024 // 100MB
	logSubject           = "hive.logs.>"
)

// LogEntry represents a single log line from an agent.
type LogEntry struct {
	AgentID   string                 `json:"agent_id"`
	Timestamp time.Time              `json:"timestamp"`
	Level     string                 `json:"level"`
	Message   string                 `json:"message"`
	Fields    map[string]interface{} `json:"fields,omitempty"`
}

// QueryOpts controls log querying behavior.
type QueryOpts struct {
	Since  time.Time // only entries at or after this time
	Until  time.Time // only entries before this time
	Level  string    // filter by log level (empty = all)
	Limit  int       // maximum number of entries to return (0 = unlimited)
	Offset int       // number of entries to skip
}

// AggregatorConfig configures the log Aggregator.
type AggregatorConfig struct {
	NATSConn      *nats.Conn
	LogDir        string        // directory for log files
	RetentionDays int           // default 30
	MaxFileSize   int64         // max log file size before rotation, default 100MB
	Logger        *slog.Logger
}

// Aggregator collects agent logs via NATS and stores them locally.
type Aggregator struct {
	conn          *nats.Conn
	logDir        string
	retentionDays int
	maxFileSize   int64
	logger        *slog.Logger

	sub       *nats.Subscription
	mu        sync.Mutex
	followers map[string][]chan LogEntry
	stopOnce  sync.Once
	done      chan struct{}

	// fileMu protects the openFiles map.
	fileMu    sync.Mutex
	openFiles map[string]*os.File // keyed by file path
}

// NewAggregator creates a new log aggregator. It does not start subscribing
// until Start() is called.
func NewAggregator(cfg AggregatorConfig) *Aggregator {
	retentionDays := cfg.RetentionDays
	if retentionDays <= 0 {
		retentionDays = defaultRetentionDays
	}

	maxFileSize := cfg.MaxFileSize
	if maxFileSize <= 0 {
		maxFileSize = defaultMaxFileSize
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &Aggregator{
		conn:          cfg.NATSConn,
		logDir:        cfg.LogDir,
		retentionDays: retentionDays,
		maxFileSize:   maxFileSize,
		logger:        logger,
		followers:     make(map[string][]chan LogEntry),
		done:          make(chan struct{}),
		openFiles:     make(map[string]*os.File),
	}
}

// Start subscribes to the NATS log subject and begins collecting logs.
// It also runs retention cleanup for old log files.
func (a *Aggregator) Start() error {
	if err := os.MkdirAll(a.logDir, 0755); err != nil {
		return fmt.Errorf("creating log directory %s: %w", a.logDir, err)
	}

	// Clean up old logs on startup.
	if err := a.cleanRetention(); err != nil {
		a.logger.Warn("error during retention cleanup", "error", err)
	}

	sub, err := a.conn.Subscribe(logSubject, a.handleMessage)
	if err != nil {
		return fmt.Errorf("subscribing to %s: %w", logSubject, err)
	}
	a.sub = sub

	a.logger.Info("log aggregator started",
		"subject", logSubject,
		"log_dir", a.logDir,
		"retention_days", a.retentionDays,
	)

	return nil
}

// Stop unsubscribes from NATS, closes all open log files, and closes all follower channels.
func (a *Aggregator) Stop() {
	a.stopOnce.Do(func() {
		close(a.done)

		if a.sub != nil {
			if err := a.sub.Unsubscribe(); err != nil {
				a.logger.Warn("error unsubscribing from logs", "error", err)
			}
		}

		// Close all open file handles.
		a.fileMu.Lock()
		for path, f := range a.openFiles {
			if err := f.Close(); err != nil {
				a.logger.Warn("error closing log file", "path", path, "error", err)
			}
			delete(a.openFiles, path)
		}
		a.fileMu.Unlock()

		// Close all follower channels.
		a.mu.Lock()
		defer a.mu.Unlock()
		for agentID, channels := range a.followers {
			for _, ch := range channels {
				close(ch)
			}
			delete(a.followers, agentID)
		}

		a.logger.Info("log aggregator stopped")
	})
}

// handleMessage processes an incoming NATS message containing a log envelope.
func (a *Aggregator) handleMessage(msg *nats.Msg) {
	var env types.Envelope
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		a.logger.Warn("failed to parse log envelope", "error", err)
		return
	}

	// Extract the LogEntry from the envelope payload.
	entry, err := a.extractLogEntry(env)
	if err != nil {
		a.logger.Warn("failed to extract log entry from envelope",
			"error", err,
			"from", env.From,
		)
		return
	}

	// Write to file.
	if err := a.writeEntry(entry); err != nil {
		a.logger.Error("failed to write log entry",
			"error", err,
			"agent_id", entry.AgentID,
		)
	}

	// Notify followers.
	a.notifyFollowers(entry)
}

// extractLogEntry converts an envelope payload into a LogEntry.
func (a *Aggregator) extractLogEntry(env types.Envelope) (LogEntry, error) {
	payloadBytes, err := json.Marshal(env.Payload)
	if err != nil {
		return LogEntry{}, fmt.Errorf("marshaling payload: %w", err)
	}

	var entry LogEntry
	if err := json.Unmarshal(payloadBytes, &entry); err != nil {
		return LogEntry{}, fmt.Errorf("unmarshaling log entry: %w", err)
	}

	// Fill in defaults from envelope if not set in the payload.
	if entry.AgentID == "" {
		entry.AgentID = env.From
	}
	if entry.Timestamp.IsZero() {
		entry.Timestamp = env.Timestamp
	}

	return entry, nil
}

// writeEntry appends a log entry to the appropriate agent log file.
// File handles are kept open and reused across calls for performance.
func (a *Aggregator) writeEntry(entry LogEntry) error {
	agentDir := filepath.Join(a.logDir, entry.AgentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		return fmt.Errorf("creating agent log directory: %w", err)
	}

	date := entry.Timestamp.Format("2006-01-02")
	logFile := filepath.Join(agentDir, date+".jsonl")

	// Check if rotation is needed. If rotation happens, close the cached handle.
	rotated, err := a.rotateIfNeeded(logFile)
	if err != nil {
		a.logger.Warn("error during log rotation", "file", logFile, "error", err)
	}
	if rotated {
		a.closeFile(logFile)
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshaling log entry: %w", err)
	}

	f, err := a.getOrOpenFile(logFile)
	if err != nil {
		return fmt.Errorf("opening log file %s: %w", logFile, err)
	}

	if _, err := f.Write(append(data, '\n')); err != nil {
		// On write error, close and remove the cached handle.
		a.closeFile(logFile)
		return fmt.Errorf("writing to log file %s: %w", logFile, err)
	}

	return nil
}

// getOrOpenFile returns a cached file handle or opens a new one.
func (a *Aggregator) getOrOpenFile(path string) (*os.File, error) {
	a.fileMu.Lock()
	defer a.fileMu.Unlock()

	if f, ok := a.openFiles[path]; ok {
		return f, nil
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	a.openFiles[path] = f
	return f, nil
}

// closeFile closes and removes a cached file handle.
func (a *Aggregator) closeFile(path string) {
	a.fileMu.Lock()
	defer a.fileMu.Unlock()

	if f, ok := a.openFiles[path]; ok {
		f.Close()
		delete(a.openFiles, path)
	}
}

// rotateIfNeeded renames the log file if it exceeds the maximum file size.
// Returns true if rotation occurred.
func (a *Aggregator) rotateIfNeeded(logFile string) (bool, error) {
	info, err := os.Stat(logFile)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat %s: %w", logFile, err)
	}

	if info.Size() < a.maxFileSize {
		return false, nil
	}

	// Find the next rotation number.
	ext := filepath.Ext(logFile)
	base := strings.TrimSuffix(logFile, ext)

	for i := 1; ; i++ {
		rotated := fmt.Sprintf("%s.%d%s", base, i, ext)
		if _, err := os.Stat(rotated); os.IsNotExist(err) {
			if err := os.Rename(logFile, rotated); err != nil {
				return false, fmt.Errorf("rotating %s to %s: %w", logFile, rotated, err)
			}
			a.logger.Info("rotated log file", "from", logFile, "to", rotated)
			return true, nil
		}
	}
}

// cleanRetention removes log files older than the retention period.
func (a *Aggregator) cleanRetention() error {
	cutoff := time.Now().AddDate(0, 0, -a.retentionDays)

	entries, err := os.ReadDir(a.logDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading log directory %s: %w", a.logDir, err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		agentDir := filepath.Join(a.logDir, entry.Name())
		files, err := os.ReadDir(agentDir)
		if err != nil {
			a.logger.Warn("error reading agent log directory", "dir", agentDir, "error", err)
			continue
		}

		for _, f := range files {
			if f.IsDir() {
				continue
			}

			info, err := f.Info()
			if err != nil {
				continue
			}

			if info.ModTime().Before(cutoff) {
				filePath := filepath.Join(agentDir, f.Name())
				if err := os.Remove(filePath); err != nil {
					a.logger.Warn("error removing old log file", "path", filePath, "error", err)
				} else {
					a.logger.Info("removed old log file", "path", filePath)
				}
			}
		}
	}

	return nil
}

// notifyFollowers sends a log entry to all followers for the entry's agent.
func (a *Aggregator) notifyFollowers(entry LogEntry) {
	a.mu.Lock()
	defer a.mu.Unlock()

	channels, ok := a.followers[entry.AgentID]
	if !ok {
		return
	}

	for _, ch := range channels {
		select {
		case ch <- entry:
		default:
			// Channel is full, drop the entry to avoid blocking.
		}
	}
}

// Query reads log entries for a specific agent, applying the given filters.
func (a *Aggregator) Query(agentID string, opts QueryOpts) ([]LogEntry, error) {
	agentDir := filepath.Join(a.logDir, agentID)
	if _, err := os.Stat(agentDir); os.IsNotExist(err) {
		return nil, nil
	}

	files, err := os.ReadDir(agentDir)
	if err != nil {
		return nil, fmt.Errorf("reading agent log directory %s: %w", agentDir, err)
	}

	// Sort files by name to get chronological order.
	sort.Slice(files, func(i, j int) bool {
		return files[i].Name() < files[j].Name()
	})

	var results []LogEntry
	skipped := 0

	for _, f := range files {
		if f.IsDir() {
			continue
		}

		filePath := filepath.Join(agentDir, f.Name())
		entries, err := a.readLogFile(filePath)
		if err != nil {
			a.logger.Warn("error reading log file", "path", filePath, "error", err)
			continue
		}

		for _, entry := range entries {
			if !a.matchesQuery(entry, opts) {
				continue
			}

			if opts.Offset > 0 && skipped < opts.Offset {
				skipped++
				continue
			}

			results = append(results, entry)

			if opts.Limit > 0 && len(results) >= opts.Limit {
				return results, nil
			}
		}
	}

	return results, nil
}

// readLogFile reads all log entries from a JSONL file.
func (a *Aggregator) readLogFile(path string) ([]LogEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening log file %s: %w", path, err)
	}
	defer f.Close()

	var entries []LogEntry
	scanner := bufio.NewScanner(f)

	// Increase scanner buffer size for long lines.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var entry LogEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			a.logger.Warn("skipping malformed log line", "path", path, "error", err)
			continue
		}
		entries = append(entries, entry)
	}

	if err := scanner.Err(); err != nil {
		return entries, fmt.Errorf("scanning log file %s: %w", path, err)
	}

	return entries, nil
}

// matchesQuery checks whether a log entry matches the given query options.
func (a *Aggregator) matchesQuery(entry LogEntry, opts QueryOpts) bool {
	if !opts.Since.IsZero() && entry.Timestamp.Before(opts.Since) {
		return false
	}
	if !opts.Until.IsZero() && !entry.Timestamp.Before(opts.Until) {
		return false
	}
	if opts.Level != "" && !strings.EqualFold(entry.Level, opts.Level) {
		return false
	}
	return true
}

// Follow returns a channel that receives new log entries for the given agent
// in real time, and a cancel function that stops the follow and closes the
// channel. The cancel function is safe to call multiple times.
func (a *Aggregator) Follow(agentID string) (<-chan LogEntry, func()) {
	ch := make(chan LogEntry, 64)

	a.mu.Lock()
	a.followers[agentID] = append(a.followers[agentID], ch)
	a.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			a.mu.Lock()
			defer a.mu.Unlock()

			channels := a.followers[agentID]
			for i, c := range channels {
				if c == ch {
					a.followers[agentID] = append(channels[:i], channels[i+1:]...)
					break
				}
			}
			close(ch)
		})
	}

	return ch, cancel
}
