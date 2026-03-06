package firmware

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/brmurrell3/hive/internal/state"
	"github.com/brmurrell3/hive/internal/types"
	"github.com/nats-io/nats.go"
)

// DeviceStatus represents a firmware device's connectivity status.
type DeviceStatus string

const (
	DeviceOnline  DeviceStatus = "ONLINE"
	DeviceOffline DeviceStatus = "OFFLINE"
)

// DeviceState tracks the state of a Tier 3 firmware device.
type DeviceState struct {
	AgentID         string       `json:"agent_id"`
	Status          DeviceStatus `json:"status"`
	LastHeartbeat   time.Time    `json:"last_heartbeat"`
	FirmwareVersion string       `json:"firmware_version,omitempty"`
	FreeHeap        int          `json:"free_heap_bytes,omitempty"`
	UptimeSeconds   int          `json:"uptime_seconds,omitempty"`
}

// Tracker monitors Tier 3 firmware devices via MQTT heartbeats bridged to NATS.
type Tracker struct {
	store          *state.Store
	natsConn       *nats.Conn
	logger         *slog.Logger
	devices        map[string]*DeviceState
	mu             sync.RWMutex
	sub            *nats.Subscription
	stopCh         chan struct{}
	stopOnce       sync.Once
	checkInterval  time.Duration
	offlineTimeout time.Duration
}

// TrackerConfig configures the firmware device tracker.
type TrackerConfig struct {
	Store          *state.Store
	NATSConn       *nats.Conn
	Logger         *slog.Logger
	CheckInterval  time.Duration // How often to check for stale devices (default 30s)
	OfflineTimeout time.Duration // Duration after which a device is marked offline (default 90s)
}

// NewTracker creates a new firmware device tracker.
func NewTracker(cfg TrackerConfig) *Tracker {
	checkInterval := cfg.CheckInterval
	if checkInterval == 0 {
		checkInterval = 30 * time.Second
	}
	offlineTimeout := cfg.OfflineTimeout
	if offlineTimeout == 0 {
		offlineTimeout = 90 * time.Second
	}

	return &Tracker{
		store:          cfg.Store,
		natsConn:       cfg.NATSConn,
		logger:         cfg.Logger.With("component", "firmware-tracker"),
		devices:        make(map[string]*DeviceState),
		stopCh:         make(chan struct{}),
		checkInterval:  checkInterval,
		offlineTimeout: offlineTimeout,
	}
}

// Start begins tracking firmware devices.
func (t *Tracker) Start() error {
	// Subscribe to firmware health heartbeats (bridged from MQTT)
	sub, err := t.natsConn.Subscribe("hive.health.*", func(msg *nats.Msg) {
		t.handleHeartbeat(msg)
	})
	if err != nil {
		return fmt.Errorf("subscribing to health subjects: %w", err)
	}
	t.sub = sub

	go t.checkLoop()

	t.logger.Info("firmware tracker started")
	return nil
}

// Stop stops the tracker. It is safe to call Stop multiple times; only the
// first call takes effect.
func (t *Tracker) Stop() {
	t.stopOnce.Do(func() {
		close(t.stopCh)
		if t.sub != nil {
			t.sub.Unsubscribe()
		}
		t.logger.Info("firmware tracker stopped")
	})
}

// handleHeartbeat processes a health heartbeat message.
func (t *Tracker) handleHeartbeat(msg *nats.Msg) {
	var env types.Envelope
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		return
	}

	// Only handle firmware tier heartbeats
	payloadBytes, err := json.Marshal(env.Payload)
	if err != nil {
		return
	}

	var health types.HealthPayload
	if err := json.Unmarshal(payloadBytes, &health); err != nil {
		return
	}

	if health.Tier != "firmware" {
		return
	}

	agentID := env.From

	t.mu.Lock()
	device, exists := t.devices[agentID]
	if !exists {
		device = &DeviceState{
			AgentID: agentID,
		}
		t.devices[agentID] = device
	}

	device.Status = DeviceOnline
	device.LastHeartbeat = time.Now()
	device.UptimeSeconds = health.UptimeSeconds
	device.FreeHeap = health.FreeHeapBytes
	t.mu.Unlock()

	t.logger.Debug("firmware heartbeat received",
		"agent_id", agentID,
		"uptime", health.UptimeSeconds,
		"free_heap", health.FreeHeapBytes,
	)
}

// checkLoop periodically checks for offline devices.
func (t *Tracker) checkLoop() {
	ticker := time.NewTicker(t.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			t.checkDevices()
		case <-t.stopCh:
			return
		}
	}
}

// checkDevices marks devices as offline if heartbeats have stopped.
func (t *Tracker) checkDevices() {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	for agentID, device := range t.devices {
		if device.Status == DeviceOnline && now.Sub(device.LastHeartbeat) > t.offlineTimeout {
			device.Status = DeviceOffline
			t.logger.Warn("firmware device offline",
				"agent_id", agentID,
				"last_heartbeat", device.LastHeartbeat,
			)
		}
	}
}

// AllDevices returns a copy of all tracked firmware devices.
func (t *Tracker) AllDevices() []DeviceState {
	t.mu.RLock()
	defer t.mu.RUnlock()

	devices := make([]DeviceState, 0, len(t.devices))
	for _, d := range t.devices {
		devices = append(devices, *d)
	}
	return devices
}

// GetDevice returns the state of a specific firmware device.
func (t *Tracker) GetDevice(agentID string) *DeviceState {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if d, ok := t.devices[agentID]; ok {
		cp := *d
		return &cp
	}
	return nil
}
