package ota

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/nats-io/nats.go"
)

const (
	// DefaultChunkSize is the default size of each OTA chunk in bytes (4KB).
	DefaultChunkSize = 4096

	// StatusTransferring indicates chunks are being sent to the agent.
	StatusTransferring = "transferring"
	// StatusVerifying indicates the agent is verifying the firmware.
	StatusVerifying = "verifying"
	// StatusApplying indicates the agent is applying the firmware update.
	StatusApplying = "applying"
	// StatusComplete indicates the OTA update completed successfully.
	StatusComplete = "complete"
	// StatusFailed indicates the OTA update failed.
	StatusFailed = "failed"
)

// Update describes a firmware update to be pushed to an agent.
type Update struct {
	AgentID         string `json:"agent_id"`
	BinaryPath      string `json:"binary_path"`
	FirmwareVersion string `json:"firmware_version"`
	ChunkSize       int    `json:"chunk_size,omitempty"`
}

// UpdateStatus reports the progress of an OTA update.
type UpdateStatus struct {
	AgentID  string  `json:"agent_id"`
	Progress float64 `json:"progress"` // 0.0 to 1.0
	Status   string  `json:"status"`
	Error    string  `json:"error,omitempty"`
}

// Manifest is published to the agent before sending chunks.
// It describes the firmware binary so the agent knows what to expect.
type Manifest struct {
	FirmwareVersion string `json:"firmware_version"`
	TotalSize       int    `json:"total_size"`
	TotalChunks     int    `json:"total_chunks"`
	ChunkSize       int    `json:"chunk_size"`
	SHA256          string `json:"sha256"`
}

// ChunkRequest is sent by the agent to request a specific chunk.
type ChunkRequest struct {
	Index int `json:"index"`
}

// Chunk is a single chunk of the firmware binary.
type Chunk struct {
	Index int    `json:"index"`
	Data  []byte `json:"data"`
}

// CompletionMessage is sent by the agent when it finishes receiving and applying.
type CompletionMessage struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

// Updater manages OTA firmware update delivery over NATS (bridged to MQTT).
type Updater struct {
	nc     *nats.Conn
	logger *slog.Logger
}

// NewUpdater creates a new OTA Updater.
func NewUpdater(nc *nats.Conn, logger *slog.Logger) *Updater {
	return &Updater{
		nc:     nc,
		logger: logger,
	}
}

// Push initiates an OTA firmware update for the specified agent. The provided
// context can be used to cancel the operation before it completes.
//
// OTA protocol:
//  1. Read binary from disk and compute SHA-256.
//  2. Split into chunks (default 4KB).
//  3. Publish manifest to hive.ota.{AGENT_ID}.manifest.
//  4. Wait for chunk requests on hive.ota.{AGENT_ID}.request.
//  5. Publish chunks to hive.ota.{AGENT_ID}.chunk.{INDEX}.
//  6. Wait for completion or failure on hive.ota.{AGENT_ID}.complete.
func (u *Updater) Push(ctx context.Context, update Update) (*UpdateStatus, error) {
	if update.AgentID == "" {
		return nil, fmt.Errorf("agent ID is required")
	}
	if update.BinaryPath == "" {
		return nil, fmt.Errorf("binary path is required")
	}

	chunkSize := update.ChunkSize
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}

	// Read binary.
	data, err := os.ReadFile(update.BinaryPath)
	if err != nil {
		return nil, fmt.Errorf("reading firmware binary %s: %w", update.BinaryPath, err)
	}

	// Compute SHA-256.
	hash := ComputeSHA256(data)

	// Split into chunks.
	chunks := SplitChunks(data, chunkSize)

	u.logger.Info("starting OTA push",
		"agent_id", update.AgentID,
		"firmware_version", update.FirmwareVersion,
		"binary_size", len(data),
		"chunk_count", len(chunks),
		"chunk_size", chunkSize,
		"sha256", hash,
	)

	// Build and publish manifest.
	manifest := Manifest{
		FirmwareVersion: update.FirmwareVersion,
		TotalSize:       len(data),
		TotalChunks:     len(chunks),
		ChunkSize:       chunkSize,
		SHA256:          hash,
	}

	manifestSubject := fmt.Sprintf("hive.ota.%s.manifest", update.AgentID)
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("marshaling OTA manifest: %w", err)
	}

	if err := u.nc.Publish(manifestSubject, manifestData); err != nil {
		return nil, fmt.Errorf("publishing OTA manifest: %w", err)
	}

	u.logger.Info("OTA manifest published",
		"agent_id", update.AgentID,
		"subject", manifestSubject,
	)

	// Check context before proceeding.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("OTA push cancelled: %w", err)
	}

	// Subscribe to chunk requests with context awareness.
	requestSubject := fmt.Sprintf("hive.ota.%s.request", update.AgentID)
	requestSub, err := u.nc.Subscribe(requestSubject, func(msg *nats.Msg) {
		// Check context before processing each chunk request.
		if ctx.Err() != nil {
			return
		}

		var req ChunkRequest
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			u.logger.Error("failed to unmarshal chunk request",
				"agent_id", update.AgentID,
				"error", err,
			)
			return
		}

		if req.Index < 0 || req.Index >= len(chunks) {
			u.logger.Warn("invalid chunk index requested",
				"agent_id", update.AgentID,
				"index", req.Index,
				"total_chunks", len(chunks),
			)
			return
		}

		chunk := Chunk{
			Index: req.Index,
			Data:  chunks[req.Index],
		}

		chunkSubject := fmt.Sprintf("hive.ota.%s.chunk.%d", update.AgentID, req.Index)
		chunkData, err := json.Marshal(chunk)
		if err != nil {
			u.logger.Error("failed to marshal chunk",
				"agent_id", update.AgentID,
				"index", req.Index,
				"error", err,
			)
			return
		}

		if err := u.nc.Publish(chunkSubject, chunkData); err != nil {
			u.logger.Error("failed to publish chunk",
				"agent_id", update.AgentID,
				"index", req.Index,
				"error", err,
			)
			return
		}

		u.logger.Debug("chunk published",
			"agent_id", update.AgentID,
			"index", req.Index,
			"size", len(chunks[req.Index]),
		)
	})
	if err != nil {
		return nil, fmt.Errorf("subscribing to chunk requests: %w", err)
	}
	defer func() {
		_ = requestSub.Unsubscribe()
	}()

	// Wait for completion message with context cancellation support.
	completeSubject := fmt.Sprintf("hive.ota.%s.complete", update.AgentID)
	completeSub, err := u.nc.SubscribeSync(completeSubject)
	if err != nil {
		return nil, fmt.Errorf("subscribing to completion: %w", err)
	}
	defer func() {
		_ = completeSub.Unsubscribe()
	}()

	// Poll for completion in short intervals, checking context between polls.
	// This allows the caller to cancel the operation via the context instead
	// of blocking for the full 5-minute timeout.
	var msg *nats.Msg
	pollInterval := 500 * time.Millisecond
	deadline := time.Now().Add(5 * time.Minute)
	for {
		select {
		case <-ctx.Done():
			return &UpdateStatus{
				AgentID:  update.AgentID,
				Progress: 0,
				Status:   StatusFailed,
				Error:    fmt.Sprintf("OTA push cancelled: %s", ctx.Err()),
			}, nil
		default:
		}

		if time.Now().After(deadline) {
			return &UpdateStatus{
				AgentID:  update.AgentID,
				Progress: 0,
				Status:   StatusFailed,
				Error:    "OTA update timed out waiting for completion",
			}, nil
		}

		msg, err = completeSub.NextMsg(pollInterval)
		if err != nil {
			if err == nats.ErrTimeout {
				continue // keep polling
			}
			return nil, fmt.Errorf("waiting for OTA completion: %w", err)
		}
		break // got a completion message
	}

	var completion CompletionMessage
	if err := json.Unmarshal(msg.Data, &completion); err != nil {
		return nil, fmt.Errorf("unmarshaling completion message: %w", err)
	}

	if completion.Success {
		u.logger.Info("OTA update completed successfully",
			"agent_id", update.AgentID,
			"firmware_version", update.FirmwareVersion,
		)
		return &UpdateStatus{
			AgentID:  update.AgentID,
			Progress: 1.0,
			Status:   StatusComplete,
		}, nil
	}

	u.logger.Warn("OTA update failed",
		"agent_id", update.AgentID,
		"error", completion.Error,
	)
	return &UpdateStatus{
		AgentID:  update.AgentID,
		Progress: 0,
		Status:   StatusFailed,
		Error:    completion.Error,
	}, nil
}

// ComputeSHA256 returns the hex-encoded SHA-256 hash of the given data.
func ComputeSHA256(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// SplitChunks splits data into chunks of the given size.
// The last chunk may be smaller than chunkSize.
func SplitChunks(data []byte, chunkSize int) [][]byte {
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}

	var chunks [][]byte
	for i := 0; i < len(data); i += chunkSize {
		end := i + chunkSize
		if end > len(data) {
			end = len(data)
		}
		// Make a copy of the slice to avoid holding references to the entire data.
		chunk := make([]byte, end-i)
		copy(chunk, data[i:end])
		chunks = append(chunks, chunk)
	}

	return chunks
}

// GenerateManifest creates an OTA manifest for the given binary data.
func GenerateManifest(data []byte, version string, chunkSize int) Manifest {
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}

	totalChunks := len(data) / chunkSize
	if len(data)%chunkSize != 0 {
		totalChunks++
	}

	return Manifest{
		FirmwareVersion: version,
		TotalSize:       len(data),
		TotalChunks:     totalChunks,
		ChunkSize:       chunkSize,
		SHA256:          ComputeSHA256(data),
	}
}
