# Hive Firmware C SDK

Minimal C SDK for Tier 3 firmware agents in the Hive framework. Connects to the Hive control plane via MQTT and provides capability registration, heartbeat publishing, and message envelope construction.

**Target footprint:** ~15-30KB flash, ~5-10KB RAM (excluding user code and MQTT/TLS libraries).

## Supported Platforms

- ESP-IDF (ESP32, ESP32-S3, ESP32-C3)
- Arduino framework
- Raspberry Pi Pico SDK
- Zephyr RTOS
- Bare metal ARM

## Quick Start

### 1. Add to your project

**ESP-IDF:** Copy or symlink this directory into your project's `components/` folder:

```
components/hive/ -> sdk/firmware-c/
```

**CMake (Pico SDK, standalone):**

```cmake
add_subdirectory(path/to/sdk/firmware-c)
target_link_libraries(your_target PRIVATE hive_firmware)
```

**Zephyr:** Add as a module in your `west.yml`.

### 2. Implement platform hooks

The SDK requires platform-specific implementations for MQTT I/O and timing. Create a file (e.g., `platform_esp32.c`) that implements:

```c
int  hive_platform_mqtt_connect(const char *host, uint16_t port,
                                 const char *client_id,
                                 const char *user, const char *pass);
int  hive_platform_mqtt_publish(const char *topic, const char *payload, size_t len);
int  hive_platform_mqtt_subscribe(const char *topic);
int  hive_platform_mqtt_loop(void);
void hive_platform_mqtt_disconnect(void);
void hive_platform_mqtt_set_callback(hive_mqtt_msg_callback_t cb);

uint32_t hive_platform_millis(void);
uint32_t hive_platform_free_heap(void);
uint32_t hive_platform_random(void);
```

For ESP-IDF, these would wrap the `esp_mqtt_client` API. For Arduino, they would wrap `PubSubClient`. See the examples directory for guidance.

### 3. Write your agent

```c
#include "hive.h"

static int handle_my_capability(const char *inputs_json,
                                 char *outputs_json,
                                 size_t max_output_len)
{
    snprintf(outputs_json, max_output_len, "{\"value\": 42}");
    return HIVE_OK;
}

void app_main(void)
{
    hive_config_t config = {
        .agent_id              = "my-agent",
        .mqtt_host             = "192.168.1.10",
        .mqtt_port             = 1883,
        .heartbeat_interval_ms = 30000,
        .mode                  = HIVE_MODE_TOOL,
    };

    hive_init(&config);
    hive_register_capability("my-capability", handle_my_capability);

    while (1) {
        hive_loop();
        /* platform-specific delay, e.g. vTaskDelay(pdMS_TO_TICKS(100)) */
    }
}
```

### 4. Build and flash

```bash
hivectl firmware build my-agent --target esp-idf
hivectl firmware flash my-agent --port /dev/ttyUSB0
```

## Operating Modes

**HIVE_MODE_TOOL** -- The agent only responds to incoming capability requests. It publishes heartbeats and waits for invocations. This is the default mode for sensor devices.

**HIVE_MODE_PEER** -- The agent can initiate messages, broadcast to its team, and send messages to other agents. Use this mode for devices that need to push data proactively.

## Compile-Time Configuration

Override these with `-D` flags:

| Define | Default | Description |
|--------|---------|-------------|
| `HIVE_MAX_CAPABILITIES` | 8 | Maximum registered capabilities |
| `HIVE_MAX_TOPIC_LEN` | 128 | Maximum MQTT topic length |
| `HIVE_MAX_PAYLOAD_LEN` | 1024 | Maximum payload buffer size |
| `HIVE_MAX_AGENT_ID_LEN` | 64 | Maximum agent ID length |
| `HIVE_MAX_CAP_NAME_LEN` | 48 | Maximum capability name length |
| `HIVE_ENVELOPE_BUF_LEN` | 2048 | Envelope construction buffer |

## Message Format

All messages use the Hive envelope format:

```json
{
  "id": "uuid-v4",
  "from": "agent-id",
  "to": "target",
  "type": "health|capability-request|capability-response|broadcast",
  "timestamp": "RFC3339",
  "payload": {}
}
```

The SDK constructs envelopes automatically. Capability handlers receive the extracted `inputs` object and write the `outputs` object; the SDK wraps them in the proper envelope.

## Examples

- `examples/esp32-sensor/` -- ESP32 temperature sensor with tool and peer mode demonstrations.
