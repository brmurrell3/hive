[← Back to Documentation](README.md)

# Hive Firmware SDK

Two SDKs for Tier 3 devices:

## C SDK (firmware-c)
Target platforms: ESP-IDF, Arduino framework, Pico SDK, Zephyr, bare metal ARM

Provides:
- `hive_init(config)`: connect MQTT, register with control plane
- `hive_register_capability(name, handler_fn)`: register capability with C callback
- `hive_publish(topic, payload)`: send message
- `hive_loop()`: main loop, handles MQTT keepalive and incoming messages
- `hive_health_report()`: send heartbeat with device metrics
- `hive_ota_check()`: check for pending OTA update
- `hive_ota_apply(data, len)`: write OTA chunk to flash

Memory footprint: ~15-30KB flash, ~5-10KB RAM (excluding user code and MQTT/TLS libs)

## MicroPython SDK (firmware-micropython)
Target platforms: ESP32, Pi Pico W, MicroPython-capable boards

Provides:
- `HiveAgent` class:
  - `connect()`: connect MQTT
  - `register_capability(name, fn)`: register capability
  - `run()`: main event loop (uses uasyncio)
  - `publish(topic, payload)`: send message
  - `health_report()`: heartbeat
- Async-compatible
- JSON message handling built in

Memory footprint: ~20KB additional to base MicroPython

## Message Format

Default: JSON (same envelope as NATS messages)
Compact: MessagePack for constrained devices (ESP8266, small Arduinos)

Device declares format preference in join request field: `message_format: enum(json|msgpack)`, default `json`
MQTT bridge on control plane handles translation between formats

---

# BUILD TOOLCHAIN

Command: `hivectl firmware build AGENT_ID [--target PLATFORM]`

Compiles firmware from `agents/{AGENT_ID}/firmware/` directory. Build delegates to platform-specific toolchains based on agent manifest `spec.firmware` configuration.

## Build Configuration (in agent manifest)

```
spec.firmware:
  platform: enum(esp-idf|arduino|pico-sdk|zephyr|bare-metal)  # REQUIRED
  board: string  # REQUIRED, platform-specific board identifier
  partitionTable: string  # OPTIONAL, path relative to firmware/
  extraLibs: list[string]  # OPTIONAL
  buildFlags: map[string]string  # OPTIONAL
  flashMethod: enum(serial|ota)  # default serial
  flashBaud: int  # default 460800
```

## Platform-Specific Build Behavior

### esp-idf
- Toolchain: ESP-IDF (installed via Nix or system)
- Build command: `idf.py build` (invoked by hivectl)
- Board mapping: `spec.firmware.board` → IDF_TARGET (e.g. `esp32`, `esp32s3`, `esp32c3`)
- Partition table: `spec.firmware.partitionTable` or default with OTA partitions
- Output: `build/firmware.bin`
- Flash: `idf.py flash --port PORT --baud BAUD` or esptool.py
- SDK integration: hive SDK sources injected as IDF component at `components/hive/`

### arduino
- Toolchain: arduino-cli (installed via Nix or system)
- Build command: `arduino-cli compile --fqbn BOARD`
- Board mapping: `spec.firmware.board` → FQBN (e.g. `esp32:esp32:esp32`, `rp2040:rp2040:rpipicow`)
- Extra libs: installed via `arduino-cli lib install`
- Output: `build/firmware.ino.bin`
- Flash: `arduino-cli upload --port PORT`
- SDK integration: hive SDK as Arduino library

### pico-sdk
- Toolchain: Pico SDK + arm-none-eabi-gcc (via Nix)
- Build command: `cmake + make`
- Board mapping: `spec.firmware.board` → PICO_BOARD (e.g. `pico_w`)
- Output: `build/firmware.uf2`
- Flash: `picotool load firmware.uf2`
- SDK integration: hive SDK as CMake `add_subdirectory`

### zephyr
- Toolchain: Zephyr SDK + west (via Nix)
- Build command: `west build -b BOARD`
- Board mapping: `spec.firmware.board` → Zephyr board name (e.g. `nrf52840dk_nrf52840`, `esp32`)
- Output: `build/zephyr/zephyr.bin`
- Flash: `west flash`
- SDK integration: hive SDK as Zephyr module

### bare-metal
- Toolchain: arm-none-eabi-gcc or xtensa-esp32-elf-gcc (user provides Makefile)
- Build command: `make -C firmware/ BOARD=board HIVE_SDK_PATH=path`
- Output: user-defined in Makefile (convention: `build/firmware.bin`)
- Flash: user-defined (convention: `make flash PORT=port`)
- SDK integration: hive SDK headers + static library linked by user Makefile

## Build Environment

`hivectl firmware build` sets up build environment:
1. Determine platform from `spec.firmware.platform`
2. Verify toolchain available (check PATH for required tools)
3. Inject Hive SDK source into firmware/ build tree (platform-specific method)
4. Inject build-time config: WiFi SSID/password (from cluster secrets), join token, control plane address, agent_id, MQTT port
5. Invoke platform-specific build command
6. Copy output binary to `.state/agents/{AGENT_ID}/firmware.bin`

## Nix Toolchain Provisioning

For NixOS users: Hive flake provides devShells with all toolchains:
- `nix develop .#firmware-esp-idf`: ESP-IDF + esptool
- `nix develop .#firmware-arduino`: arduino-cli + cores
- `nix develop .#firmware-pico`: Pico SDK + arm toolchain
- `nix develop .#firmware-zephyr`: Zephyr SDK + west

`hivectl firmware build` auto-detects if running in Nix and uses appropriate shell.

---

# FLASHING

## Serial Flash

```
hivectl firmware flash AGENT_ID --port /dev/ttyUSB0 [--baud 460800]
```

Process:
1. Read `spec.firmware.platform` and board
2. Invoke platform-specific flash command with built binary
3. Monitor serial output for boot confirmation
4. On successful boot: device joins cluster via MQTT

## OTA Flash

```
hivectl firmware update AGENT_ID --binary path/to/firmware.bin
```

Or triggered automatically when firmware/ source changes and build produces new binary.

### OTA Protocol

1. Control plane reads binary, computes SHA-256 hash
2. Splits binary into chunks (default 4KB, configurable for constrained devices)
3. Publishes OTA manifest to `hive/ota/{AGENT_ID}/manifest`:
   ```json
   {
     "firmware_version": "string",
     "size": "int",
     "sha256": "string",
     "chunks": "int",
     "chunk_size": "int"
   }
   ```
4. Device receives manifest, verifies sufficient flash space
5. Device requests chunks sequentially: publishes to `hive/ota/{AGENT_ID}/request` with `chunk_index`
6. Control plane publishes chunk data to `hive/ota/{AGENT_ID}/chunk/{INDEX}`
7. Device writes each chunk to OTA partition
8. After all chunks received: device computes SHA-256 of received data
9. If hash matches manifest: device sets OTA partition as boot target, reboots
10. On first boot from new firmware: device verifies functionality, marks OTA as valid
11. If boot fails (watchdog timeout): device rolls back to previous firmware

---

# OTA SECURITY

## Current State (homelab acceptable)

- **Authentication**: OTA manifest and chunks delivered via authenticated MQTT (join token)
- **Integrity**: SHA-256 checksum verification
- **Rollback**: automatic on boot failure via watchdog
- **Firmware signing**: Not in Phase 1-2

## Threat Model & Mitigation Path

Threat: Compromised NATS bus allows attacker to push malicious firmware

Mitigation path (Phase 3+):
1. Firmware signing: build produces signed binary using ed25519 key
2. Signing key stored in cluster secrets
3. Device stores public key in flash (provisioned at first flash)
4. OTA manifest includes signature
5. Device verifies signature before writing to OTA partition
6. Unsigned or bad-signature firmware rejected

Commands:
- `hivectl firmware sign AGENT_ID --key path/to/private.key`: sign built firmware
- `cluster.yaml spec.firmware.signingKey`: secret name for signing key (Phase 3)

**Acknowledged risk for Phase 1-2**: OTA relies on network security (token auth, optional TLS). Acceptable for homelab. Signing available as opt-in hardening.

---

# SUPPORTED PLATFORMS

| Platform | Specs | Notes |
|----------|-------|-------|
| **ESP32** (all variants) | WiFi native, 520KB RAM, dual core | Primary Tier 3 target. Supports: esp-idf, arduino, zephyr |
| **ESP8266** | WiFi native, 80KB RAM | Minimal. 1-2 simple capabilities. Supports: arduino |
| **Raspberry Pi Pico W** | WiFi, 264KB RAM, RP2040 | Good MicroPython support. Supports: pico-sdk, arduino, micropython |
| **Arduino with WiFi/Ethernet shield** | varies | Minimum ~32KB free RAM. Supports: arduino |
| **STM32 with network** | Ethernet or WiFi module | Supports: zephyr, bare-metal |
| **Any MQTT-capable device** | Runs MQTT client, parses JSON/MessagePack, has network | Potential Tier 3 node |

---

# SECURITY CONSIDERATIONS

## MQTT Connections

- Many MCUs lack TLS. Connections may be unencrypted on local network.
- Sensitive deployments: MQTT over TLS where hardware supports it.
- Network isolation: IoT VLAN recommended.
- Join token: primary authentication mechanism.

## Attack Surface

- **Firmware agents**: minimal. Only respond to capability invocations and send heartbeats.
- **Cannot access**: other agents' data, control plane state, filesystem.
- **No shell access**. No code execution beyond compiled firmware.
