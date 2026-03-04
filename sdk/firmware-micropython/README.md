# Hive MicroPython Firmware SDK

MicroPython SDK for Tier 3 firmware agents in a Hive cluster. Connects to the control plane over MQTT and supports capability registration, heartbeats, and the standard Hive message envelope.

## Supported Platforms

- Raspberry Pi Pico W
- ESP32 (all variants)
- Any MicroPython-capable board with Wi-Fi or network connectivity

## Requirements

- MicroPython firmware with `umqtt.simple` (included in most standard builds)
- Network connectivity to the machine running `hived`

## Installation

Copy `hive_agent.py` to the device filesystem (e.g. via `mpremote`, Thonny, or `ampy`):

```
mpremote cp hive_agent.py :hive_agent.py
```

## Quick Start

```python
from hive_agent import HiveAgent

agent = HiveAgent({
    "agent_id": "my-sensor",
    "mqtt_host": "192.168.1.100",
    "mqtt_port": 1883,
})

def read_temperature(inputs):
    # Your sensor logic here.
    return {"celsius": 22.5}

agent.register_capability("read-temp", read_temperature)
agent.connect()
agent.run()
```

## Configuration

The `HiveAgent` constructor takes a config dict with the following keys:

| Key | Required | Default | Description |
|-----|----------|---------|-------------|
| `agent_id` | yes | -- | Unique agent identifier |
| `mqtt_host` | yes | -- | MQTT broker host (hived address) |
| `mqtt_port` | no | 1883 | MQTT broker port |
| `join_token` | no | `""` | Authentication token |
| `heartbeat_interval` | no | 30 | Seconds between health reports |
| `mode` | no | `"tool"` | `"tool"` or `"peer"` |
| `team_id` | no | `""` | Team this agent belongs to |
| `keepalive` | no | 60 | MQTT keepalive in seconds |

## Capabilities

Register capabilities before calling `connect()` or at any time during operation:

```python
def handle_toggle(inputs):
    pin = inputs.get("pin", 15)
    # ... do hardware work ...
    return {"pin": pin, "state": "on"}

agent.register_capability("gpio-toggle", handle_toggle)
```

Capability handlers receive an `inputs` dict and must return a dict of outputs. Raise an exception to signal an error to the caller.

## Peer Mode

For agents that need to receive direct messages or team broadcasts:

```python
agent = HiveAgent({
    "agent_id": "my-peer",
    "mqtt_host": "192.168.1.100",
    "mode": "peer",
    "team_id": "sensor-team",
})

agent.on_inbox(lambda env: print("Got message:", env))
agent.on_broadcast(lambda env: print("Broadcast:", env))
agent.connect()
agent.run()
```

## Examples

See `examples/pico_gpio.py` for a complete Pi Pico W example that exposes GPIO control as a Hive capability.

## Message Format

All messages use the Hive JSON envelope:

```json
{
  "id": "uuid-v4",
  "from": "agent-id",
  "to": "target",
  "type": "capability-request",
  "timestamp": "2025-01-15T10:30:00Z",
  "payload": {},
  "reply_to": "optional",
  "correlation_id": "optional"
}
```

## Memory Footprint

Approximately 20KB additional to base MicroPython, excluding the MQTT library.
