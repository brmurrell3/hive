package mqtt

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/hivehq/hive/internal/state"
	"github.com/hivehq/hive/internal/types"
	"github.com/nats-io/nats.go"
)

// TopicMapping defines the bidirectional mapping between MQTT topics and NATS subjects.
// MQTT uses / as separator, NATS uses .
// MQTT topic pattern -> NATS subject pattern:
//
//	hive/health/{AGENT_ID}                          -> hive.health.{AGENT_ID}
//	hive/agent/{AGENT_ID}/inbox                     -> agent.{AGENT_ID}.inbox
//	hive/team/{TEAM_ID}/broadcast                   -> team.{TEAM_ID}.broadcast
//	hive/capabilities/{AGENT_ID}/{CAP}/request      -> hive.capabilities.{AGENT_ID}.{CAP}.request
//	hive/capabilities/{AGENT_ID}/{CAP}/response     -> hive.capabilities.{AGENT_ID}.{CAP}.response
//	hive/join/request                               -> hive.join.request
//	hive/join/status/{AGENT_ID}                     -> hive.join.status.{AGENT_ID}

// Bridge translates between MQTT and NATS protocols.
type Bridge struct {
	listener  net.Listener
	port      int
	natsConn  *nats.Conn
	store     *state.Store
	logger    *slog.Logger
	clients   map[string]*Client
	mu        sync.RWMutex
	stopCh    chan struct{}
	natsSubs  []*nats.Subscription
}

// Client represents a connected MQTT client (firmware device).
type Client struct {
	conn     net.Conn
	agentID  string
	verified bool
	topics   []string
	subs     []*nats.Subscription // T1-10: track NATS subscriptions for cleanup
	mu       sync.Mutex
	bridge   *Bridge
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
		port:     port,
		natsConn: cfg.NATSConn,
		store:    cfg.Store,
		logger:   cfg.Logger.With("component", "mqtt-bridge"),
		clients:  make(map[string]*Client),
		stopCh:   make(chan struct{}),
	}
}

// Start begins listening for MQTT connections.
func (b *Bridge) Start() error {
	addr := fmt.Sprintf(":%d", b.port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}
	b.listener = ln

	// Subscribe to NATS subjects that need bridging to MQTT.
	if err := b.subscribeNATS(); err != nil {
		ln.Close()
		return fmt.Errorf("subscribing to NATS: %w", err)
	}

	b.logger.Info("MQTT bridge started", "port", b.port)

	go b.acceptLoop()
	return nil
}

// Stop shuts down the MQTT bridge.
func (b *Bridge) Stop() error {
	close(b.stopCh)

	for _, sub := range b.natsSubs {
		sub.Unsubscribe()
	}

	if b.listener != nil {
		b.listener.Close()
	}

	b.mu.Lock()
	for _, c := range b.clients {
		c.conn.Close()
	}
	b.mu.Unlock()

	b.logger.Info("MQTT bridge stopped")
	return nil
}

// Port returns the actual port the bridge is listening on.
func (b *Bridge) Port() int {
	if b.listener != nil {
		return b.listener.Addr().(*net.TCPAddr).Port
	}
	return b.port
}

func (b *Bridge) acceptLoop() {
	for {
		conn, err := b.listener.Accept()
		if err != nil {
			select {
			case <-b.stopCh:
				return
			default:
				b.logger.Error("accept error", "error", err)
				continue
			}
		}
		go b.handleConnection(conn)
	}
}

// handleConnection manages a single MQTT client connection.
// This implements a minimal subset of MQTT v3.1.1 needed for firmware agents.
// T1-08: Uses proper MQTT packet framing instead of one-packet-per-read assumption.
func (b *Bridge) handleConnection(conn net.Conn) {
	client := &Client{
		conn:   conn,
		bridge: b,
	}

	defer func() {
		// T1-10: Unsubscribe all NATS subscriptions for this client.
		client.mu.Lock()
		for _, sub := range client.subs {
			sub.Unsubscribe()
		}
		client.subs = nil
		client.mu.Unlock()

		b.mu.Lock()
		delete(b.clients, client.agentID)
		b.mu.Unlock()
		conn.Close()
		if client.agentID != "" {
			b.logger.Info("MQTT client disconnected", "agent_id", client.agentID)
		}
	}()

	buf := make([]byte, 0, 8192)
	readBuf := make([]byte, 4096)
	for {
		select {
		case <-b.stopCh:
			return
		default:
		}

		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		n, err := conn.Read(readBuf)
		if err != nil {
			return
		}
		if n == 0 {
			continue
		}

		buf = append(buf, readBuf[:n]...)

		// T1-08: Process all complete packets in the buffer.
		for len(buf) >= 2 {
			// Read fixed header to determine remaining length.
			remainLen, bytesUsed := decodeRemainingLength(buf[1:])
			if bytesUsed == 0 {
				break // need more data for remaining length
			}
			// Check if the last byte of remaining length still has continuation bit.
			if buf[1+bytesUsed-1]&0x80 != 0 && bytesUsed < 4 {
				break // incomplete remaining length encoding
			}

			totalLen := 1 + bytesUsed + remainLen
			if len(buf) < totalLen {
				break // incomplete packet, wait for more data
			}

			// Extract one complete packet.
			packet := make([]byte, totalLen)
			copy(packet, buf[:totalLen])
			buf = buf[totalLen:]

			if err := client.handlePacket(packet); err != nil {
				b.logger.Warn("packet handling error", "error", err, "agent_id", client.agentID)
				return
			}
		}
	}
}

// handlePacket processes a raw MQTT packet.
func (c *Client) handlePacket(data []byte) error {
	if len(data) < 2 {
		return fmt.Errorf("packet too short")
	}

	packetType := data[0] >> 4

	switch packetType {
	case 1: // CONNECT
		return c.handleConnect(data)
	case 3: // PUBLISH
		return c.handlePublish(data)
	case 8: // SUBSCRIBE
		return c.handleSubscribe(data)
	case 12: // PINGREQ
		return c.handlePing()
	case 14: // DISCONNECT
		return fmt.Errorf("client disconnected")
	default:
		c.bridge.logger.Debug("unhandled MQTT packet type", "type", packetType)
		return nil
	}
}

// handleConnect processes an MQTT CONNECT packet.
func (c *Client) handleConnect(data []byte) error {
	// Minimal MQTT CONNECT parsing:
	// Fixed header (1 byte type + remaining length)
	// Variable header: protocol name, protocol level, connect flags, keep alive
	// Payload: client ID, username (agent_id), password (token)

	offset := 1
	remainLen, bytesUsed := decodeRemainingLength(data[offset:])
	offset += bytesUsed

	if len(data) < offset+remainLen {
		return fmt.Errorf("CONNECT packet too short")
	}

	// Skip protocol name and level (variable header)
	// Protocol Name Length (2 bytes) + "MQTT" (4 bytes) + Protocol Level (1 byte) = 7 bytes
	if offset+7 > len(data) {
		return fmt.Errorf("CONNECT variable header too short")
	}
	offset += 7

	// Connect flags
	if offset >= len(data) {
		return fmt.Errorf("CONNECT flags missing")
	}
	connectFlags := data[offset]
	offset++
	hasUsername := connectFlags&0x80 != 0
	hasPassword := connectFlags&0x40 != 0

	// Keep alive (2 bytes)
	offset += 2

	// Payload: Client ID
	clientID, n, err := readMQTTString(data[offset:])
	if err != nil {
		return fmt.Errorf("reading client ID: %w", err)
	}
	offset += n

	var username, password string
	if hasUsername {
		username, n, err = readMQTTString(data[offset:])
		if err != nil {
			return fmt.Errorf("reading username: %w", err)
		}
		offset += n
	}
	if hasPassword {
		password, n, err = readMQTTString(data[offset:])
		if err != nil {
			return fmt.Errorf("reading password: %w", err)
		}
		_ = n
	}

	// Authenticate: username=agent_id, password=join_token
	agentID := username
	if agentID == "" {
		agentID = clientID
	}

	// T3-03: Require authentication — empty password connections are rejected.
	returnCode := byte(0) // success
	if password == "" {
		returnCode = 5 // not authorized
		c.bridge.logger.Warn("MQTT auth rejected: no password", "agent_id", agentID)
	} else {
		t := c.bridge.store.ValidateToken(password)
		if t == nil {
			returnCode = 5 // not authorized
			c.bridge.logger.Warn("MQTT auth failed", "agent_id", agentID)
		}
	}

	c.agentID = agentID
	c.verified = returnCode == 0

	// Send CONNACK
	connack := []byte{
		0x20, // CONNACK
		0x02, // remaining length
		0x00, // session present = 0
		returnCode,
	}
	c.conn.Write(connack)

	if returnCode != 0 {
		return fmt.Errorf("authentication failed for %s", agentID)
	}

	c.bridge.mu.Lock()
	c.bridge.clients[agentID] = c
	c.bridge.mu.Unlock()

	c.bridge.logger.Info("MQTT client connected", "agent_id", agentID)
	return nil
}

// handlePublish processes an MQTT PUBLISH packet and bridges to NATS.
func (c *Client) handlePublish(data []byte) error {
	if !c.verified {
		return fmt.Errorf("not authenticated")
	}

	offset := 1
	_, bytesUsed := decodeRemainingLength(data[offset:])
	offset += bytesUsed

	// Topic name
	topic, n, err := readMQTTString(data[offset:])
	if err != nil {
		return fmt.Errorf("reading topic: %w", err)
	}
	offset += n

	// QoS from fixed header
	qos := (data[0] >> 1) & 0x03

	// T1-09: Capture packet ID immediately before advancing offset.
	var packetIDBytes [2]byte
	if qos > 0 {
		if offset+2 > len(data) {
			return fmt.Errorf("PUBLISH packet too short for packet ID")
		}
		packetIDBytes[0] = data[offset]
		packetIDBytes[1] = data[offset+1]
		offset += 2
	}

	// Payload is the rest
	payload := data[offset:]

	// Convert MQTT topic to NATS subject
	natsSubject := mqttTopicToNATSSubject(topic)
	if natsSubject == "" {
		c.bridge.logger.Debug("unmapped MQTT topic", "topic", topic)
		return nil
	}

	// Publish to NATS
	if err := c.bridge.natsConn.Publish(natsSubject, payload); err != nil {
		return fmt.Errorf("publishing to NATS: %w", err)
	}

	c.bridge.logger.Debug("MQTT→NATS bridge",
		"mqtt_topic", topic,
		"nats_subject", natsSubject,
		"size", len(payload),
	)

	// T1-09: Send PUBACK for QoS 1 using the captured packet ID.
	if qos == 1 {
		puback := []byte{0x40, 0x02, packetIDBytes[0], packetIDBytes[1]}
		c.conn.Write(puback)
	}

	return nil
}

// handleSubscribe processes an MQTT SUBSCRIBE packet.
func (c *Client) handleSubscribe(data []byte) error {
	if !c.verified {
		return fmt.Errorf("not authenticated")
	}

	offset := 1
	_, bytesUsed := decodeRemainingLength(data[offset:])
	offset += bytesUsed

	// Packet identifier
	if offset+2 > len(data) {
		return fmt.Errorf("SUBSCRIBE packet too short")
	}
	packetID := data[offset : offset+2]
	offset += 2

	// Parse topic filters
	var qosResults []byte
	for offset < len(data) {
		topic, n, err := readMQTTString(data[offset:])
		if err != nil {
			break
		}
		offset += n

		if offset >= len(data) {
			break
		}
		requestedQoS := data[offset]
		offset++

		c.mu.Lock()
		c.topics = append(c.topics, topic)
		c.mu.Unlock()

		// Subscribe on NATS for this topic
		natsSubject := mqttTopicToNATSSubject(topic)
		if natsSubject != "" {
			c.bridge.subscribeForClient(c, natsSubject, topic)
		}

		grantedQoS := requestedQoS
		if grantedQoS > 1 {
			grantedQoS = 1
		}
		qosResults = append(qosResults, grantedQoS)

		c.bridge.logger.Debug("MQTT SUBSCRIBE", "agent_id", c.agentID, "topic", topic)
	}

	// Send SUBACK
	suback := []byte{0x90}
	remainLen := 2 + len(qosResults)
	suback = append(suback, byte(remainLen))
	suback = append(suback, packetID...)
	suback = append(suback, qosResults...)
	c.conn.Write(suback)

	return nil
}

// handlePing responds to MQTT PINGREQ with PINGRESP.
func (c *Client) handlePing() error {
	pingresp := []byte{0xD0, 0x00}
	_, err := c.conn.Write(pingresp)
	return err
}

// subscribeForClient creates a NATS subscription that forwards messages to an MQTT client.
// T1-10: The subscription is stored on the client for cleanup on disconnect.
func (b *Bridge) subscribeForClient(c *Client, natsSubject, mqttTopic string) {
	sub, err := b.natsConn.Subscribe(natsSubject, func(msg *nats.Msg) {
		c.publishToMQTT(mqttTopic, msg.Data)
	})
	if err != nil {
		b.logger.Warn("failed to subscribe for client", "agent_id", c.agentID, "subject", natsSubject, "error", err)
		return
	}
	c.mu.Lock()
	c.subs = append(c.subs, sub)
	c.mu.Unlock()
}

// publishToMQTT sends an MQTT PUBLISH packet to the client.
func (c *Client) publishToMQTT(topic string, payload []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	topicBytes := []byte(topic)
	remainingLen := 2 + len(topicBytes) + len(payload)

	var packet []byte
	packet = append(packet, 0x30) // PUBLISH, QoS 0
	packet = append(packet, encodeRemainingLength(remainingLen)...)
	packet = append(packet, byte(len(topicBytes)>>8), byte(len(topicBytes)))
	packet = append(packet, topicBytes...)
	packet = append(packet, payload...)

	c.conn.Write(packet)
}

// subscribeNATS subscribes to NATS subjects that should be bridged to MQTT clients.
func (b *Bridge) subscribeNATS() error {
	// Subscribe to health subjects to bridge responses back
	sub, err := b.natsConn.Subscribe("hive.join.status.*", func(msg *nats.Msg) {
		parts := strings.Split(msg.Subject, ".")
		if len(parts) != 4 {
			return
		}
		agentID := parts[3]
		mqttTopic := fmt.Sprintf("hive/join/status/%s", agentID)

		b.mu.RLock()
		client, ok := b.clients[agentID]
		b.mu.RUnlock()

		if ok {
			client.publishToMQTT(mqttTopic, msg.Data)
		}
	})
	if err != nil {
		return err
	}
	b.natsSubs = append(b.natsSubs, sub)

	return nil
}

// mqttTopicToNATSSubject converts an MQTT topic to a NATS subject.
func mqttTopicToNATSSubject(topic string) string {
	// Direct mapping: replace / with .
	subject := strings.ReplaceAll(topic, "/", ".")
	return subject
}

// natsSubjectToMQTTTopic converts a NATS subject to an MQTT topic.
func natsSubjectToMQTTTopic(subject string) string {
	return strings.ReplaceAll(subject, ".", "/")
}

// readMQTTString reads a length-prefixed UTF-8 string from MQTT packet data.
func readMQTTString(data []byte) (string, int, error) {
	if len(data) < 2 {
		return "", 0, fmt.Errorf("string too short")
	}
	length := int(data[0])<<8 | int(data[1])
	if len(data) < 2+length {
		return "", 0, fmt.Errorf("string data too short: need %d, have %d", 2+length, len(data))
	}
	return string(data[2 : 2+length]), 2 + length, nil
}

// decodeRemainingLength decodes the MQTT variable-length remaining length field.
func decodeRemainingLength(data []byte) (int, int) {
	multiplier := 1
	value := 0
	index := 0

	for index < len(data) && index < 4 {
		encodedByte := data[index]
		value += int(encodedByte&0x7F) * multiplier
		multiplier *= 128
		index++
		if encodedByte&0x80 == 0 {
			break
		}
	}

	return value, index
}

// encodeRemainingLength encodes a length value using MQTT variable-length encoding.
func encodeRemainingLength(length int) []byte {
	var encoded []byte
	for {
		encodedByte := byte(length % 128)
		length /= 128
		if length > 0 {
			encodedByte |= 0x80
		}
		encoded = append(encoded, encodedByte)
		if length == 0 {
			break
		}
	}
	return encoded
}

// PublishToAgent publishes a message to a specific MQTT client by agent ID.
func (b *Bridge) PublishToAgent(agentID string, topic string, payload []byte) error {
	b.mu.RLock()
	client, ok := b.clients[agentID]
	b.mu.RUnlock()

	if !ok {
		return fmt.Errorf("MQTT client %s not connected", agentID)
	}

	client.publishToMQTT(topic, payload)
	return nil
}

// ConnectedAgents returns the list of currently connected MQTT agent IDs.
func (b *Bridge) ConnectedAgents() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()

	agents := make([]string, 0, len(b.clients))
	for id := range b.clients {
		agents = append(agents, id)
	}
	return agents
}

// Envelope helpers for MQTT messages.

// PublishJoinResponse publishes a join response to a firmware agent via NATS
// (which gets bridged to MQTT if the device is connected).
func (b *Bridge) PublishJoinResponse(agentID string, resp types.JoinResponse) error {
	data, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshaling join response: %w", err)
	}

	subject := fmt.Sprintf("hive.join.status.%s", agentID)
	return b.natsConn.Publish(subject, data)
}
