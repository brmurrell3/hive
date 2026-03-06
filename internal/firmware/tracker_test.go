//go:build integration

package firmware

import (
	"encoding/json"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/brmurrell3/hive/internal/state"
	"github.com/brmurrell3/hive/internal/testutil"
	"github.com/brmurrell3/hive/internal/types"
)

func TestTracker_ReceivesFirmwareHeartbeat(t *testing.T) {
	ns := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, ns)

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store, err := state.NewStore(t.TempDir()+"/state.db", logger)
	if err != nil {
		t.Fatal(err)
	}

	tracker := NewTracker(TrackerConfig{
		Store:          store,
		NATSConn:       nc,
		Logger:         logger,
		CheckInterval:  100 * time.Millisecond,
		OfflineTimeout: 500 * time.Millisecond,
	})

	if err := tracker.Start(); err != nil {
		t.Fatal(err)
	}
	defer tracker.Stop()

	// Publish a firmware heartbeat
	payload := types.HealthPayload{
		Healthy:       true,
		UptimeSeconds: 120,
		Tier:          "firmware",
		FreeHeapBytes: 32000,
	}
	env := types.Envelope{
		ID:        "test-id",
		From:      "sensor-01",
		To:        "hived",
		Type:      types.MessageTypeHealth,
		Timestamp: time.Now().UTC(),
		Payload:   payload,
	}
	data, _ := json.Marshal(env)
	nc.Publish("hive.health.sensor-01", data)
	nc.Flush()

	time.Sleep(100 * time.Millisecond)

	device := tracker.GetDevice("sensor-01")
	if device == nil {
		t.Fatal("expected device to be tracked")
	}
	if device.Status != DeviceOnline {
		t.Errorf("expected ONLINE, got %s", device.Status)
	}
	if device.UptimeSeconds != 120 {
		t.Errorf("expected uptime 120, got %d", device.UptimeSeconds)
	}
}

func TestTracker_IgnoresNonFirmwareHeartbeats(t *testing.T) {
	ns := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, ns)

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store, err := state.NewStore(t.TempDir()+"/state.db", logger)
	if err != nil {
		t.Fatal(err)
	}

	tracker := NewTracker(TrackerConfig{
		Store:    store,
		NATSConn: nc,
		Logger:   logger,
	})

	if err := tracker.Start(); err != nil {
		t.Fatal(err)
	}
	defer tracker.Stop()

	// Publish a VM tier heartbeat
	payload := types.HealthPayload{
		Healthy:       true,
		UptimeSeconds: 60,
		Tier:          "vm",
	}
	env := types.Envelope{
		ID:        "test-id",
		From:      "vm-agent",
		To:        "hived",
		Type:      types.MessageTypeHealth,
		Timestamp: time.Now().UTC(),
		Payload:   payload,
	}
	data, _ := json.Marshal(env)
	nc.Publish("hive.health.vm-agent", data)
	nc.Flush()

	time.Sleep(100 * time.Millisecond)

	device := tracker.GetDevice("vm-agent")
	if device != nil {
		t.Error("expected VM heartbeat to be ignored by firmware tracker")
	}
}

func TestTracker_MarksDeviceOffline(t *testing.T) {
	ns := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, ns)

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store, err := state.NewStore(t.TempDir()+"/state.db", logger)
	if err != nil {
		t.Fatal(err)
	}

	tracker := NewTracker(TrackerConfig{
		Store:          store,
		NATSConn:       nc,
		Logger:         logger,
		CheckInterval:  50 * time.Millisecond,
		OfflineTimeout: 200 * time.Millisecond,
	})

	if err := tracker.Start(); err != nil {
		t.Fatal(err)
	}
	defer tracker.Stop()

	// Send initial heartbeat
	payload := types.HealthPayload{
		Healthy:       true,
		UptimeSeconds: 10,
		Tier:          "firmware",
	}
	env := types.Envelope{
		ID:        "test-id",
		From:      "sensor-02",
		To:        "hived",
		Type:      types.MessageTypeHealth,
		Timestamp: time.Now().UTC(),
		Payload:   payload,
	}
	data, _ := json.Marshal(env)
	nc.Publish("hive.health.sensor-02", data)
	nc.Flush()

	time.Sleep(50 * time.Millisecond)

	device := tracker.GetDevice("sensor-02")
	if device == nil || device.Status != DeviceOnline {
		t.Fatal("expected device ONLINE")
	}

	// Wait for offline timeout
	time.Sleep(400 * time.Millisecond)

	device = tracker.GetDevice("sensor-02")
	if device == nil {
		t.Fatal("expected device to still be tracked")
	}
	if device.Status != DeviceOffline {
		t.Errorf("expected OFFLINE, got %s", device.Status)
	}
}

func TestTracker_AllDevices(t *testing.T) {
	ns := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, ns)

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store, err := state.NewStore(t.TempDir()+"/state.db", logger)
	if err != nil {
		t.Fatal(err)
	}

	tracker := NewTracker(TrackerConfig{
		Store:    store,
		NATSConn: nc,
		Logger:   logger,
	})

	if err := tracker.Start(); err != nil {
		t.Fatal(err)
	}
	defer tracker.Stop()

	// Send heartbeats from 3 devices
	for _, id := range []string{"sensor-a", "sensor-b", "sensor-c"} {
		env := types.Envelope{
			ID:        "test-" + id,
			From:      id,
			To:        "hived",
			Type:      types.MessageTypeHealth,
			Timestamp: time.Now().UTC(),
			Payload: types.HealthPayload{
				Healthy:       true,
				UptimeSeconds: 10,
				Tier:          "firmware",
			},
		}
		data, _ := json.Marshal(env)
		nc.Publish("hive.health."+id, data)
	}
	nc.Flush()

	time.Sleep(100 * time.Millisecond)

	devices := tracker.AllDevices()
	if len(devices) != 3 {
		t.Errorf("expected 3 devices, got %d", len(devices))
	}
}
