//go:build e2e

package e2e

import (
	"encoding/binary"
	"fmt"
	"net"
	"testing"
	"time"
)

// Raw TCP MQTT v3.1.1 packet builders for e2e testing.
// These are standalone copies of the patterns in internal/mqtt/bridge_test.go
// to avoid importing internal packages.

func buildMQTTConnect(clientID, username, password string) []byte {
	// Variable header
	var varHeader []byte
	varHeader = append(varHeader, 0x00, 0x04) // Protocol Name length
	varHeader = append(varHeader, []byte("MQTT")...)
	varHeader = append(varHeader, 0x04) // Protocol Level 4 (MQTT 3.1.1)
	flags := byte(0x02)                 // clean session
	if username != "" {
		flags |= 0x80
	}
	if password != "" {
		flags |= 0x40
	}
	varHeader = append(varHeader, flags)
	varHeader = append(varHeader, 0x00, 0x3C) // Keep Alive: 60s

	// Payload
	var payload []byte
	payload = append(payload, mqttString(clientID)...)
	if username != "" {
		payload = append(payload, mqttString(username)...)
	}
	if password != "" {
		payload = append(payload, mqttString(password)...)
	}

	remainLen := len(varHeader) + len(payload)
	var packet []byte
	packet = append(packet, 0x10) // CONNECT
	packet = append(packet, encodeRemainingLength(remainLen)...)
	packet = append(packet, varHeader...)
	packet = append(packet, payload...)
	return packet
}

func buildMQTTPublish(topic string, payload []byte) []byte {
	topicBytes := []byte(topic)
	remainLen := 2 + len(topicBytes) + len(payload)
	var packet []byte
	packet = append(packet, 0x30) // PUBLISH, QoS 0
	packet = append(packet, encodeRemainingLength(remainLen)...)
	packet = append(packet, byte(len(topicBytes)>>8), byte(len(topicBytes)))
	packet = append(packet, topicBytes...)
	packet = append(packet, payload...)
	return packet
}

func mqttString(s string) []byte {
	b := make([]byte, 2+len(s))
	binary.BigEndian.PutUint16(b, uint16(len(s)))
	copy(b[2:], s)
	return b
}

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

// mqttConnect dials the MQTT bridge over TCP, sends a CONNECT packet, and
// reads the CONNACK. Returns the connection and the CONNACK return code.
func mqttConnect(t *testing.T, port int, clientID, username, password string) (net.Conn, byte) {
	t.Helper()

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("TCP connect to MQTT bridge at %s: %v", addr, err)
	}

	pkt := buildMQTTConnect(clientID, username, password)
	if _, err := conn.Write(pkt); err != nil {
		conn.Close()
		t.Fatalf("sending MQTT CONNECT: %v", err)
	}

	// Read CONNACK (4 bytes: 0x20, 0x02, session_present, return_code)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 4)
	n, err := conn.Read(buf)
	if err != nil {
		conn.Close()
		t.Fatalf("reading MQTT CONNACK: %v", err)
	}
	if n < 4 {
		conn.Close()
		t.Fatalf("CONNACK too short: got %d bytes", n)
	}
	if buf[0] != 0x20 {
		conn.Close()
		t.Fatalf("expected CONNACK (0x20), got 0x%02X", buf[0])
	}

	return conn, buf[3]
}
