package mqtt

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/brmurrell3/hive/internal/state"
	"github.com/brmurrell3/hive/internal/types"
	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/listeners"
	"github.com/mochi-mqtt/server/v2/packets"
	"github.com/nats-io/nats.go"
)

// Bridge translates between MQTT and NATS protocols using mochi-mqtt as the
// embedded MQTT broker.
type Bridge struct {
	server   *mqtt.Server
	port     int
	natsConn *nats.Conn
	store    *state.Store
	logger   *slog.Logger
	mu       sync.RWMutex
	natsSubs []*nats.Subscription

	// Track connected agent IDs (populated by the hook).
	clients   map[string]string // clientID -> agentID
	clientMu  sync.RWMutex
	// Track per-client NATS subscriptions for cleanup.
	clientSubs   map[string][]*nats.Subscription // clientID -> NATS subs
}

// Config holds MQTT bridge configuration.
type Config struct {
	Port     int
	NATSConn *nats.Conn
	Store    *state.Store
	Logger   *slog.Logger
}

// NewBridge creates a new MQTT-NATS bridge.
func NewBridge(cfg Config) *Bridge {
	port := cfg.Port
	if port == 0 {
		port = 1883
	}
	return &Bridge{
		port:       port,
		natsConn:   cfg.NATSConn,
		store:      cfg.Store,
		logger:     cfg.Logger.With("component", "mqtt-bridge"),
		clients:    make(map[string]string),
		clientSubs: make(map[string][]*nats.Subscription),
	}
}

// Start begins listening for MQTT connections.
func (b *Bridge) Start() error {
	b.server = mqtt.New(&mqtt.Options{
		InlineClient: true,
	})

	// Add our bridge hook for NATS bridging and authentication.
	if err := b.server.AddHook(&bridgeHook{bridge: b}, nil); err != nil {
		return fmt.Errorf("adding bridge hook: %w", err)
	}

	// Create TCP listener.
	addr := fmt.Sprintf(":%d", b.port)
	tcp := listeners.NewTCP(listeners.Config{
		ID:      "mqtt-tcp",
		Address: addr,
	})
	if err := b.server.AddListener(tcp); err != nil {
		return fmt.Errorf("adding TCP listener: %w", err)
	}

	// Subscribe to NATS subjects that need bridging to MQTT.
	if err := b.subscribeNATS(); err != nil {
		return fmt.Errorf("subscribing to NATS: %w", err)
	}

	go func() {
		if err := b.server.Serve(); err != nil {
			b.logger.Error("MQTT server error", "error", err)
		}
	}()

	b.logger.Info("MQTT bridge started", "port", b.port)
	return nil
}

// Stop shuts down the MQTT bridge.
func (b *Bridge) Stop() error {
	for _, sub := range b.natsSubs {
		sub.Unsubscribe()
	}

	// Clean up client NATS subs.
	b.clientMu.Lock()
	for _, subs := range b.clientSubs {
		for _, sub := range subs {
			sub.Unsubscribe()
		}
	}
	b.clientSubs = make(map[string][]*nats.Subscription)
	b.clientMu.Unlock()

	if b.server != nil {
		// Brief pause to let in-flight connections finish before closing.
		time.Sleep(50 * time.Millisecond)
		b.server.Close()
	}

	b.logger.Info("MQTT bridge stopped")
	return nil
}

// Port returns the actual port the bridge is listening on.
func (b *Bridge) Port() int {
	if b.server != nil {
		if l, ok := b.server.Listeners.Get("mqtt-tcp"); ok {
			addr := l.Address()
			if addr != "" {
				parts := strings.Split(addr, ":")
				if len(parts) > 0 {
					last := parts[len(parts)-1]
					var p int
					if _, err := fmt.Sscanf(last, "%d", &p); err == nil && p > 0 {
						return p
					}
				}
			}
		}
	}
	return b.port
}

// ConnectedAgents returns the list of currently connected MQTT agent IDs.
func (b *Bridge) ConnectedAgents() []string {
	b.clientMu.RLock()
	defer b.clientMu.RUnlock()

	seen := make(map[string]bool)
	agents := make([]string, 0, len(b.clients))
	for _, agentID := range b.clients {
		if !seen[agentID] {
			seen[agentID] = true
			agents = append(agents, agentID)
		}
	}
	return agents
}

// PublishToAgent publishes a message to a specific MQTT client by agent ID.
func (b *Bridge) PublishToAgent(agentID string, topic string, payload []byte) error {
	if b.server == nil {
		return fmt.Errorf("MQTT server not started")
	}

	b.clientMu.RLock()
	found := false
	for _, id := range b.clients {
		if id == agentID {
			found = true
			break
		}
	}
	b.clientMu.RUnlock()

	if !found {
		return fmt.Errorf("MQTT client %s not connected", agentID)
	}

	return b.server.Publish(topic, payload, false, 0)
}

// PublishJoinResponse publishes a join response to a firmware agent via NATS.
func (b *Bridge) PublishJoinResponse(agentID string, resp types.JoinResponse) error {
	data, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshaling join response: %w", err)
	}

	subject := fmt.Sprintf("hive.join.status.%s", agentID)
	return b.natsConn.Publish(subject, data)
}

// subscribeNATS subscribes to NATS subjects that should be bridged to MQTT clients.
func (b *Bridge) subscribeNATS() error {
	sub, err := b.natsConn.Subscribe("hive.join.status.*", func(msg *nats.Msg) {
		parts := strings.Split(msg.Subject, ".")
		if len(parts) != 4 {
			return
		}
		agentID := parts[3]
		mqttTopic := fmt.Sprintf("hive/join/status/%s", agentID)

		if b.server != nil {
			b.server.Publish(mqttTopic, msg.Data, false, 0)
		}
	})
	if err != nil {
		return err
	}
	b.natsSubs = append(b.natsSubs, sub)

	return nil
}

// mqttTopicToNATSSubject converts an MQTT topic to a NATS subject for
// publish paths. Only the separator is converted (/ -> .); MQTT wildcard
// characters are left as-is because published messages should never contain
// wildcards, and converting them would create literal NATS subjects with
// wildcard characters.
func mqttTopicToNATSSubject(topic string) string {
	return strings.ReplaceAll(topic, "/", ".")
}

// mqttFilterToNATSSubject converts an MQTT subscription filter to a NATS
// subject pattern, translating both separators and wildcards:
//   - / -> .   (separator)
//   - + -> *   (single-level wildcard)
//   - # -> >   (multi-level wildcard)
func mqttFilterToNATSSubject(filter string) string {
	subject := strings.ReplaceAll(filter, "/", ".")
	subject = strings.ReplaceAll(subject, "+", "*")
	subject = strings.ReplaceAll(subject, "#", ">")
	return subject
}

// natsSubjectToMQTTTopic converts a NATS subject to an MQTT topic.
func natsSubjectToMQTTTopic(subject string) string {
	topic := strings.ReplaceAll(subject, ".", "/")
	topic = strings.ReplaceAll(topic, "*", "+")
	topic = strings.ReplaceAll(topic, ">", "#")
	return topic
}

// --- Bridge Hook ---

// bridgeHook implements the mochi-mqtt Hook interface to handle authentication,
// publish bridging, and subscribe bridging.
type bridgeHook struct {
	mqtt.HookBase
	bridge *Bridge
}

func (h *bridgeHook) ID() string {
	return "hive-nats-bridge"
}

func (h *bridgeHook) Provides(b byte) bool {
	return b == mqtt.OnConnectAuthenticate ||
		b == mqtt.OnACLCheck ||
		b == mqtt.OnPublished ||
		b == mqtt.OnSubscribed ||
		b == mqtt.OnDisconnect
}

// OnACLCheck allows all publish/subscribe for authenticated clients.
func (h *bridgeHook) OnACLCheck(cl *mqtt.Client, topic string, write bool) bool {
	return true
}

// OnConnectAuthenticate validates MQTT credentials against join tokens.
func (h *bridgeHook) OnConnectAuthenticate(cl *mqtt.Client, pk packets.Packet) bool {
	username := string(pk.Connect.Username)
	password := string(pk.Connect.Password)

	agentID := username
	if agentID == "" {
		agentID = cl.ID
	}

	// Require a password (join token).
	if password == "" {
		h.bridge.logger.Warn("MQTT auth rejected: no password", "agent_id", agentID)
		return false
	}

	t := h.bridge.store.ValidateToken(password)
	if t == nil {
		h.bridge.logger.Warn("MQTT auth failed", "agent_id", agentID)
		return false
	}

	// Track the connected client.
	h.bridge.clientMu.Lock()
	h.bridge.clients[cl.ID] = agentID
	h.bridge.clientMu.Unlock()

	h.bridge.logger.Info("MQTT client connected", "agent_id", agentID)
	return true
}

// OnPublished bridges MQTT publishes to NATS.
func (h *bridgeHook) OnPublished(cl *mqtt.Client, pk packets.Packet) {
	topic := pk.TopicName
	natsSubject := mqttTopicToNATSSubject(topic)
	if natsSubject == "" {
		return
	}

	if err := h.bridge.natsConn.Publish(natsSubject, pk.Payload); err != nil {
		h.bridge.logger.Warn("failed to publish to NATS",
			"mqtt_topic", topic,
			"nats_subject", natsSubject,
			"error", err,
		)
		return
	}

	h.bridge.logger.Debug("MQTT->NATS bridge",
		"mqtt_topic", topic,
		"nats_subject", natsSubject,
		"size", len(pk.Payload),
	)
}

// OnSubscribed creates NATS subscriptions that forward messages back to MQTT.
func (h *bridgeHook) OnSubscribed(cl *mqtt.Client, pk packets.Packet, reasonCodes []byte) {
	for _, sub := range pk.Filters {
		topic := sub.Filter
		natsSubject := mqttFilterToNATSSubject(topic)
		if natsSubject == "" {
			continue
		}

		natsSub, err := h.bridge.natsConn.Subscribe(natsSubject, func(msg *nats.Msg) {
			mqttTopic := natsSubjectToMQTTTopic(msg.Subject)
			if h.bridge.server != nil {
				h.bridge.server.Publish(mqttTopic, msg.Data, false, 0)
			}
		})
		if err != nil {
			h.bridge.logger.Warn("failed to subscribe for client",
				"client_id", cl.ID,
				"subject", natsSubject,
				"error", err,
			)
			continue
		}

		// Track the subscription for cleanup on disconnect.
		h.bridge.clientMu.Lock()
		h.bridge.clientSubs[cl.ID] = append(h.bridge.clientSubs[cl.ID], natsSub)
		h.bridge.clientMu.Unlock()

		h.bridge.logger.Debug("MQTT SUBSCRIBE",
			"client_id", cl.ID,
			"topic", topic,
		)
	}
}

// OnDisconnect cleans up NATS subscriptions for the disconnected client.
func (h *bridgeHook) OnDisconnect(cl *mqtt.Client, err error, expire bool) {
	h.bridge.clientMu.Lock()
	agentID := h.bridge.clients[cl.ID]
	delete(h.bridge.clients, cl.ID)

	// Unsubscribe all NATS subscriptions for this client.
	if subs, ok := h.bridge.clientSubs[cl.ID]; ok {
		for _, sub := range subs {
			sub.Unsubscribe()
		}
		delete(h.bridge.clientSubs, cl.ID)
	}
	h.bridge.clientMu.Unlock()

	if agentID != "" {
		h.bridge.logger.Info("MQTT client disconnected", "agent_id", agentID)
	}
}
