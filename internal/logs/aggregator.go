// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

// Package logs provides log aggregation for Hive agents. It subscribes to
// NATS log subjects (hive.logs.>), persists log entries to a SQLite database,
// and supports querying and following.
package logs

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/brmurrell3/hive/internal/protocol"
	"github.com/brmurrell3/hive/internal/types"
	"github.com/nats-io/nats.go"
	_ "modernc.org/sqlite"
)

const (
	defaultRetentionDays = 30
	logSubject           = protocol.SubjLogsAll
	maxFollowersPerAgent = 100
	maxTotalFollowers    = 1000
	maxLogEntrySize      = 64 * 1024 // 64KB per log entry

	// retentionCleanupInterval is how often periodic retention cleanup runs.
	retentionCleanupInterval = 1 * time.Hour

	// dbSizeCheckInterval is how often the database file size is checked.
	dbSizeCheckInterval = 5 * time.Minute

	// writeFlushInterval is how often the write loop flushes buffered entries.
	writeFlushInterval = 500 * time.Millisecond

	// followerDropStaleCutoff is how long after the last drop warning a follower
	// drop entry is considered stale and eligible for cleanup.
	followerDropStaleCutoff = 1 * time.Hour
)

// validLogLevels is the whitelist of accepted log levels.
var validLogLevels = map[string]bool{
	"debug": true,
	"info":  true,
	"warn":  true,
	"error": true,
}

// sanitizeLevel validates the log level against a whitelist and defaults to "info".
func sanitizeLevel(level string) string {
	lower := strings.ToLower(level)
	if validLogLevels[lower] {
		return lower
	}
	return "info"
}

// stripControlChars removes control characters (except tab and newline) from a string.
func stripControlChars(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '\t' || r == '\n' || r >= 32 {
			b.WriteRune(r)
		}
	}
	return b.String()
}

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
	NATSConn            *nats.Conn
	LogDir              string // directory for the SQLite database file
	RetentionDays       int    // default 30
	MaxFileSize         int64  // unused (kept for API compatibility)
	WriteBufferSize     int    // write channel buffer size; default 1000
	DBSizeWarnThreshold int64  // database size in bytes above which warnings are emitted; default 500MB
	FollowerBufferSize  int    // follower channel buffer size; default 64
	Logger              *slog.Logger
}

// defaultDBSizeWarnThreshold is the default database file size (in bytes)
// above which we start logging warnings and run retention cleanup more
// aggressively. Used when AggregatorConfig.DBSizeWarnThreshold is not set.
const defaultDBSizeWarnThreshold int64 = 500 * 1024 * 1024 // 500 MB

// Aggregator collects agent logs via NATS and stores them in SQLite.
type Aggregator struct {
	conn                *nats.Conn
	dbPath              string
	db                  *sql.DB
	retentionDays       int
	writeBufferSize     int
	dbSizeWarnThreshold int64
	followerBufferSize  int
	logger              *slog.Logger

	sub            *nats.Subscription
	mu             sync.Mutex
	followers      map[string][]chan LogEntry
	totalFollowers int
	stopOnce       sync.Once
	done           chan struct{}
	writeCh        chan *LogEntry // buffered write queue for decoupling NATS callbacks from SQLite
	writeWg        sync.WaitGroup

	// dropMu protects followerDrops and followerDropLastLog for tracking
	// dropped follower entries with rate-limited warnings.
	dropMu              sync.Mutex
	followerDrops       map[string]int64     // agent_id -> count of dropped entries
	followerDropLastLog map[string]time.Time // agent_id -> last time a drop warning was logged
}

// NewAggregator creates a new log aggregator. It does not start subscribing
// until Start() is called.
func NewAggregator(cfg AggregatorConfig) *Aggregator {
	retentionDays := cfg.RetentionDays
	if retentionDays <= 0 {
		retentionDays = defaultRetentionDays
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	writeBufferSize := cfg.WriteBufferSize
	if writeBufferSize <= 0 {
		writeBufferSize = 1000
	}

	dbSizeWarnThreshold := cfg.DBSizeWarnThreshold
	if dbSizeWarnThreshold <= 0 {
		dbSizeWarnThreshold = defaultDBSizeWarnThreshold
	}

	followerBufferSize := cfg.FollowerBufferSize
	if followerBufferSize <= 0 {
		followerBufferSize = 64
	}

	dbPath := filepath.Join(cfg.LogDir, "logs.db")

	return &Aggregator{
		conn:                cfg.NATSConn,
		dbPath:              dbPath,
		retentionDays:       retentionDays,
		writeBufferSize:     writeBufferSize,
		dbSizeWarnThreshold: dbSizeWarnThreshold,
		followerBufferSize:  followerBufferSize,
		logger:              logger,
		followers:           make(map[string][]chan LogEntry),
		done:                make(chan struct{}),
		followerDrops:       make(map[string]int64),
		followerDropLastLog: make(map[string]time.Time),
	}
}

// Start opens the SQLite database, creates tables, subscribes to NATS, and
// starts retention cleanup and the buffered write loop.
func (a *Aggregator) Start() error {
	db, err := sql.Open("sqlite", a.dbPath+"?_journal_mode=WAL&_busy_timeout=5000&_txlock=immediate")
	if err != nil {
		return fmt.Errorf("opening SQLite database %s: %w", a.dbPath, err)
	}
	a.db = db

	if err := a.createTables(); err != nil {
		db.Close()
		return fmt.Errorf("creating tables: %w", err)
	}

	// Clean up old logs on startup.
	if err := a.cleanRetention(); err != nil {
		a.logger.Warn("error during retention cleanup", "error", err)
	}

	// Initialize the buffered write channel and start the write loop.
	a.writeCh = make(chan *LogEntry, a.writeBufferSize)
	a.writeWg.Add(1)
	go a.writeLoop()

	// Start periodic retention cleanup and DB size monitoring in the background.
	a.writeWg.Add(1)
	go func() {
		defer a.writeWg.Done()
		ticker := time.NewTicker(retentionCleanupInterval)
		dbCheckTicker := time.NewTicker(dbSizeCheckInterval)
		defer ticker.Stop()
		defer dbCheckTicker.Stop()
		for {
			select {
			case <-a.done:
				return
			case <-ticker.C:
				if err := a.cleanRetention(); err != nil {
					a.logger.Warn("periodic retention cleanup error", "error", err)
				}
				// Clean up stale followerDrops entries.
				a.cleanFollowerDrops()
			case <-dbCheckTicker.C:
				a.checkDBSize()
			}
		}
	}()

	sub, err := a.conn.Subscribe(logSubject, a.handleMessage)
	if err != nil {
		close(a.done)
		a.writeWg.Wait()
		db.Close()
		return fmt.Errorf("subscribing to %s: %w", logSubject, err)
	}
	a.sub = sub

	a.logger.Info("log aggregator started",
		"subject", logSubject,
		"db_path", a.dbPath,
		"retention_days", a.retentionDays,
	)

	return nil
}

// Stop unsubscribes from NATS, flushes the write buffer, closes the database,
// and closes all follower channels.
func (a *Aggregator) Stop() {
	a.stopOnce.Do(func() {
		// Signal all background goroutines (retention cleanup, DB size monitor, write loop).
		close(a.done)

		if a.sub != nil {
			if err := a.sub.Unsubscribe(); err != nil {
				a.logger.Warn("error unsubscribing from logs", "error", err)
			}
		}

		// Wait for the write loop to flush remaining entries before closing the DB.
		a.writeWg.Wait()

		if a.db != nil {
			if err := a.db.Close(); err != nil {
				a.logger.Warn("error closing database", "error", err)
			}
		}

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

func (a *Aggregator) createTables() error {
	_, err := a.db.Exec(`
		CREATE TABLE IF NOT EXISTS logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			agent_id TEXT NOT NULL,
			timestamp TEXT NOT NULL,
			level TEXT NOT NULL DEFAULT '',
			message TEXT NOT NULL DEFAULT '',
			fields TEXT
		);
		CREATE INDEX IF NOT EXISTS idx_logs_agent_ts ON logs(agent_id, timestamp);
		CREATE INDEX IF NOT EXISTS idx_logs_level ON logs(level);
		CREATE INDEX IF NOT EXISTS idx_logs_timestamp ON logs(timestamp);
	`)
	return err
}

// handleMessage processes an incoming NATS message containing a log envelope.
// Instead of writing to SQLite directly in the NATS callback, entries are
// buffered through a channel and written in batches by the write loop.
func (a *Aggregator) handleMessage(msg *nats.Msg) {
	// Drop oversized entries before processing.
	if len(msg.Data) > maxLogEntrySize {
		a.logger.Warn("log entry exceeds size limit, dropping",
			"size", len(msg.Data),
			"limit", maxLogEntrySize,
		)
		return
	}

	var env types.Envelope
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		a.logger.Warn("failed to parse log envelope", "error", err)
		return
	}

	entry, err := a.extractLogEntry(env)
	if err != nil {
		a.logger.Warn("failed to extract log entry from envelope",
			"error", err,
			"from", env.From,
		)
		return
	}

	// Buffer the entry for batched writing.
	select {
	case a.writeCh <- &entry:
		// Only notify followers if the entry was successfully buffered.
		// If dropped, followers should not see phantom entries.
		a.notifyFollowers(entry)
	default:
		a.logger.Warn("log write buffer full, dropping entry",
			"agent_id", entry.AgentID,
		)
	}
}

// writeLoop drains the write channel in batches for efficient SQLite writes.
// It flushes either when the batch reaches 100 entries or every 500ms.
func (a *Aggregator) writeLoop() {
	defer a.writeWg.Done()

	batch := make([]*LogEntry, 0, 100)
	ticker := time.NewTicker(writeFlushInterval)
	defer ticker.Stop()

	for {
		select {
		case entry := <-a.writeCh:
			batch = append(batch, entry)
			if len(batch) >= 100 {
				a.writeBatch(batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			if len(batch) > 0 {
				a.writeBatch(batch)
				batch = batch[:0]
			}
		case <-a.done:
			// Flush remaining entries from both the batch and channel.
			for {
				select {
				case entry := <-a.writeCh:
					batch = append(batch, entry)
				default:
					if len(batch) > 0 {
						a.writeBatch(batch)
					}
					return
				}
			}
		}
	}
}

// writeBatch inserts a batch of log entries into SQLite within a single
// transaction for better throughput.
func (a *Aggregator) writeBatch(entries []*LogEntry) {
	if len(entries) == 0 {
		return
	}

	tx, err := a.db.Begin()
	if err != nil {
		a.logger.Error("failed to begin transaction for log batch", "error", err)
		// Fall back to individual writes.
		for _, entry := range entries {
			if err := a.writeEntry(*entry); err != nil {
				a.logger.Error("failed to write log entry",
					"error", err,
					"agent_id", entry.AgentID,
				)
			}
		}
		return
	}

	stmt, err := tx.Prepare(`INSERT INTO logs (agent_id, timestamp, level, message, fields) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		a.logger.Error("failed to prepare batch insert statement", "error", err)
		tx.Rollback()
		// Fall back to individual writes.
		for _, entry := range entries {
			if err := a.writeEntry(*entry); err != nil {
				a.logger.Error("failed to write log entry",
					"error", err,
					"agent_id", entry.AgentID,
				)
			}
		}
		return
	}
	defer stmt.Close()

	for _, entry := range entries {
		var fieldsJSON *string
		if entry.Fields != nil {
			data, err := json.Marshal(entry.Fields)
			if err != nil {
				a.logger.Warn("failed to marshal fields in batch", "error", err)
				continue
			}
			s := string(data)
			fieldsJSON = &s
		}

		if _, err := stmt.Exec(
			entry.AgentID,
			entry.Timestamp.Format(time.RFC3339Nano),
			entry.Level,
			entry.Message,
			fieldsJSON,
		); err != nil {
			a.logger.Error("failed to insert log entry in batch",
				"error", err,
				"agent_id", entry.AgentID,
			)
		}
	}

	if err := tx.Commit(); err != nil {
		a.logger.Error("failed to commit log batch", "error", err)
	}
}

// extractLogEntry converts an envelope payload into a LogEntry.
func (a *Aggregator) extractLogEntry(env types.Envelope) (LogEntry, error) {
	var entry LogEntry
	if err := json.Unmarshal(env.Payload, &entry); err != nil {
		return LogEntry{}, fmt.Errorf("unmarshaling log entry: %w", err)
	}

	if entry.AgentID == "" {
		// Validate env.From before using as agentID fallback.
		if err := types.ValidateSubjectComponent("agent_id", env.From); err != nil {
			return LogEntry{}, fmt.Errorf("envelope From is invalid as agent_id fallback: %w", err)
		}
		entry.AgentID = env.From
	} else if err := types.ValidateSubjectComponent("agent_id", entry.AgentID); err != nil {
		// Payload contained an invalid AgentID; fall back to the envelope's
		// From field which has already been validated by Envelope.Validate().
		if err2 := types.ValidateSubjectComponent("agent_id", env.From); err2 != nil {
			return LogEntry{}, fmt.Errorf("both payload agent_id and envelope From are invalid: payload=%w, from=%w", err, err2)
		}
		a.logger.Warn("log entry contains invalid agent_id, falling back to envelope From",
			"payload_agent_id", entry.AgentID,
			"envelope_from", env.From,
			"error", err,
		)
		entry.AgentID = env.From
	}
	if entry.Timestamp.IsZero() {
		entry.Timestamp = env.Timestamp
	}

	// Sanitize level and message to prevent log injection.
	entry.Level = sanitizeLevel(entry.Level)
	entry.Message = stripControlChars(entry.Message)

	// Sanitize Fields string values to strip control characters.
	for k, v := range entry.Fields {
		if s, ok := v.(string); ok {
			entry.Fields[k] = stripControlChars(s)
		}
	}

	return entry, nil
}

// writeEntry inserts a log entry into the SQLite database.
func (a *Aggregator) writeEntry(entry LogEntry) error {
	var fieldsJSON *string
	if entry.Fields != nil {
		data, err := json.Marshal(entry.Fields)
		if err != nil {
			return fmt.Errorf("marshaling fields: %w", err)
		}
		s := string(data)
		fieldsJSON = &s
	}

	_, err := a.db.Exec(
		`INSERT INTO logs (agent_id, timestamp, level, message, fields) VALUES (?, ?, ?, ?, ?)`,
		entry.AgentID,
		entry.Timestamp.Format(time.RFC3339Nano),
		entry.Level,
		entry.Message,
		fieldsJSON,
	)
	return err
}

// cleanRetention removes log entries older than the retention period and
// reclaims freed database pages via incremental vacuum.
func (a *Aggregator) cleanRetention() error {
	cutoff := time.Now().AddDate(0, 0, -a.retentionDays).Format(time.RFC3339Nano)
	result, err := a.db.Exec(`DELETE FROM logs WHERE timestamp < ?`, cutoff)
	if err != nil {
		return fmt.Errorf("cleaning old logs: %w", err)
	}
	if rows, _ := result.RowsAffected(); rows > 0 {
		a.logger.Info("cleaned old log entries", "deleted", rows)
		// Reclaim freed pages after deletion to prevent unbounded DB growth.
		if _, err := a.db.Exec(`PRAGMA incremental_vacuum`); err != nil {
			a.logger.Warn("incremental vacuum failed", "error", err)
		}
	}
	return nil
}

// checkDBSize checks the SQLite database file size and runs an extra retention
// cleanup if the database exceeds the warning threshold.
func (a *Aggregator) checkDBSize() {
	info, err := os.Stat(a.dbPath)
	if err != nil {
		// File may not exist yet or be temporarily unavailable.
		return
	}

	sizeBytes := info.Size()
	sizeMB := sizeBytes / (1024 * 1024)

	if sizeBytes > a.dbSizeWarnThreshold {
		a.logger.Warn("log database size exceeds threshold",
			"db_path", a.dbPath,
			"size_mb", sizeMB,
			"threshold_mb", a.dbSizeWarnThreshold/(1024*1024),
		)
		// Run an extra retention cleanup to try to reclaim space.
		if err := a.cleanRetention(); err != nil {
			a.logger.Warn("retention cleanup during size check failed", "error", err)
		}
	}
}

// followerDropLogInterval is the minimum time between drop warning logs
// for the same agent, to avoid log spam under sustained pressure.
const followerDropLogInterval = 1 * time.Minute

// notifyFollowers sends a log entry to all followers for the entry's agent.
// Releases mu before calling recordFollowerDrop to avoid nested lock
// acquisition (mu -> dropMu).
//
// Lock ordering: mu must never be held when calling recordFollowerDrop, which
// acquires dropMu. The required ordering is: acquire mu, release mu, then
// acquire dropMu. Violating this ordering will cause a deadlock if another
// goroutine holds dropMu and attempts to acquire mu.
func (a *Aggregator) notifyFollowers(entry LogEntry) {
	a.mu.Lock()

	channels, ok := a.followers[entry.AgentID]
	if !ok {
		a.mu.Unlock()
		return
	}

	var drops []string
	for _, ch := range channels {
		select {
		case ch <- entry:
		default:
			// Channel is full, drop the entry to avoid blocking.
			drops = append(drops, entry.AgentID)
		}
	}
	a.mu.Unlock()

	// Record drops outside mu to avoid nested lock acquisition (mu -> dropMu).
	for _, agentID := range drops {
		a.recordFollowerDrop(agentID)
	}
}

// recordFollowerDrop increments the drop counter for an agent and logs a
// warning at most once per minute per agent to avoid log spam.
func (a *Aggregator) recordFollowerDrop(agentID string) {
	a.dropMu.Lock()
	defer a.dropMu.Unlock()

	a.followerDrops[agentID]++
	total := a.followerDrops[agentID]

	lastLog, ok := a.followerDropLastLog[agentID]
	if !ok || time.Since(lastLog) >= followerDropLogInterval {
		a.logger.Warn("follower channel full, dropping log entries",
			"agent_id", agentID,
			"total_dropped", total,
		)
		a.followerDropLastLog[agentID] = time.Now()
	}
}

// cleanFollowerDrops removes entries from followerDrops and followerDropLastLog
// for agents that haven't had drops logged in the last hour, preventing unbounded
// map growth from agents that are no longer active.
func (a *Aggregator) cleanFollowerDrops() {
	a.dropMu.Lock()
	defer a.dropMu.Unlock()

	cutoff := time.Now().Add(-followerDropStaleCutoff)
	for agentID, lastLog := range a.followerDropLastLog {
		if lastLog.Before(cutoff) {
			delete(a.followerDrops, agentID)
			delete(a.followerDropLastLog, agentID)
		}
	}
}

// Query reads log entries for a specific agent, applying the given filters.
func (a *Aggregator) Query(agentID string, opts QueryOpts) ([]LogEntry, error) {
	if err := types.ValidateSubjectComponent("agent_id", agentID); err != nil {
		return nil, fmt.Errorf("invalid agent ID: %w", err)
	}

	const maxQueryLimit = 10000
	const maxQueryOffset = 100000
	if opts.Limit <= 0 || opts.Limit > maxQueryLimit {
		opts.Limit = maxQueryLimit
	}
	if opts.Offset > maxQueryOffset {
		opts.Offset = maxQueryOffset
	}
	if opts.Level != "" {
		opts.Level = sanitizeLevel(opts.Level)
	}

	query := `SELECT agent_id, timestamp, level, message, fields FROM logs WHERE agent_id = ?`
	args := []interface{}{agentID}

	if !opts.Since.IsZero() {
		query += ` AND timestamp >= ?`
		args = append(args, opts.Since.Format(time.RFC3339Nano))
	}
	if !opts.Until.IsZero() {
		query += ` AND timestamp < ?`
		args = append(args, opts.Until.Format(time.RFC3339Nano))
	}
	if opts.Level != "" {
		query += ` AND level = ?`
		args = append(args, opts.Level)
	}

	query += ` ORDER BY timestamp ASC`

	if opts.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, opts.Limit)
	}
	if opts.Offset > 0 {
		query += ` OFFSET ?`
		args = append(args, opts.Offset)
	}

	rows, err := a.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying logs: %w", err)
	}
	defer rows.Close()

	var results []LogEntry
	for rows.Next() {
		var entry LogEntry
		var tsStr string
		var fieldsJSON sql.NullString

		if err := rows.Scan(&entry.AgentID, &tsStr, &entry.Level, &entry.Message, &fieldsJSON); err != nil {
			return nil, fmt.Errorf("scanning log entry: %w", err)
		}

		entry.Timestamp, _ = time.Parse(time.RFC3339Nano, tsStr)

		if fieldsJSON.Valid && fieldsJSON.String != "" {
			if err := json.Unmarshal([]byte(fieldsJSON.String), &entry.Fields); err != nil {
				a.logger.Warn("failed to unmarshal fields", "error", err)
			}
		}

		results = append(results, entry)
	}

	return results, rows.Err()
}

// Follow returns a channel that receives new log entries for the given agent
// in real time, and a cancel function that stops the follow.
// Returns an error if the maximum number of followers per agent is exceeded.
// Callers should drain the returned channel before calling cancel.
func (a *Aggregator) Follow(agentID string) (<-chan LogEntry, func(), error) {
	if err := types.ValidateSubjectComponent("agent_id", agentID); err != nil {
		return nil, func() {}, fmt.Errorf("invalid agent ID: %w", err)
	}

	ch := make(chan LogEntry, a.followerBufferSize)

	a.mu.Lock()
	if a.totalFollowers >= maxTotalFollowers {
		a.mu.Unlock()
		return nil, func() {}, fmt.Errorf("maximum total followers (%d) exceeded", maxTotalFollowers)
	}
	if len(a.followers[agentID]) >= maxFollowersPerAgent {
		a.mu.Unlock()
		return nil, func() {}, fmt.Errorf("maximum followers (%d) exceeded for agent %s", maxFollowersPerAgent, agentID)
	}
	a.followers[agentID] = append(a.followers[agentID], ch)
	a.totalFollowers++
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
					a.totalFollowers--
					close(ch)
					break
				}
			}
		})
	}

	return ch, cancel, nil
}
