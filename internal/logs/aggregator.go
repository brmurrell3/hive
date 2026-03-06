// Package logs provides log aggregation for Hive agents. It subscribes to
// NATS log subjects (hive.logs.>), persists log entries to a SQLite database,
// and supports querying and following.
package logs

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/brmurrell3/hive/internal/types"
	"github.com/nats-io/nats.go"
	_ "modernc.org/sqlite"
)

const (
	defaultRetentionDays = 30
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
	LogDir        string // directory for the SQLite database file
	RetentionDays int    // default 30
	MaxFileSize   int64  // unused (kept for API compatibility)
	Logger        *slog.Logger
}

// Aggregator collects agent logs via NATS and stores them in SQLite.
type Aggregator struct {
	conn          *nats.Conn
	dbPath        string
	db            *sql.DB
	retentionDays int
	logger        *slog.Logger

	sub       *nats.Subscription
	mu        sync.Mutex
	followers map[string][]chan LogEntry
	stopOnce  sync.Once
	done      chan struct{}
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

	dbPath := cfg.LogDir + "/logs.db"

	return &Aggregator{
		conn:          cfg.NATSConn,
		dbPath:        dbPath,
		retentionDays: retentionDays,
		logger:        logger,
		followers:     make(map[string][]chan LogEntry),
		done:          make(chan struct{}),
	}
}

// Start opens the SQLite database, creates tables, subscribes to NATS, and
// starts retention cleanup.
func (a *Aggregator) Start() error {
	db, err := sql.Open("sqlite", a.dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
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

	// Start periodic retention cleanup in the background.
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-a.done:
				return
			case <-ticker.C:
				if err := a.cleanRetention(); err != nil {
					a.logger.Warn("periodic retention cleanup error", "error", err)
				}
			}
		}
	}()

	sub, err := a.conn.Subscribe(logSubject, a.handleMessage)
	if err != nil {
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

// Stop unsubscribes from NATS, closes the database, and closes all follower channels.
func (a *Aggregator) Stop() {
	a.stopOnce.Do(func() {
		close(a.done)

		if a.sub != nil {
			if err := a.sub.Unsubscribe(); err != nil {
				a.logger.Warn("error unsubscribing from logs", "error", err)
			}
		}

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
	`)
	return err
}

// handleMessage processes an incoming NATS message containing a log envelope.
func (a *Aggregator) handleMessage(msg *nats.Msg) {
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

	if err := a.writeEntry(entry); err != nil {
		a.logger.Error("failed to write log entry",
			"error", err,
			"agent_id", entry.AgentID,
		)
	}

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

	if entry.AgentID == "" {
		entry.AgentID = env.From
	}
	if entry.Timestamp.IsZero() {
		entry.Timestamp = env.Timestamp
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

// cleanRetention removes log entries older than the retention period.
func (a *Aggregator) cleanRetention() error {
	cutoff := time.Now().AddDate(0, 0, -a.retentionDays).Format(time.RFC3339Nano)
	result, err := a.db.Exec(`DELETE FROM logs WHERE timestamp < ?`, cutoff)
	if err != nil {
		return fmt.Errorf("cleaning old logs: %w", err)
	}
	if rows, _ := result.RowsAffected(); rows > 0 {
		a.logger.Info("cleaned old log entries", "deleted", rows)
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
		query += fmt.Sprintf(` LIMIT %d`, opts.Limit)
	}
	if opts.Offset > 0 {
		query += fmt.Sprintf(` OFFSET %d`, opts.Offset)
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
					close(ch)
					break
				}
			}
		})
	}

	return ch, cancel
}
