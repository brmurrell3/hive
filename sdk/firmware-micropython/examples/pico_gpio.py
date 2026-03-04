"""
Hive Framework - Pi Pico W GPIO Example

Demonstrates a Tier 3 firmware agent running on a Raspberry Pi Pico W
that exposes a "gpio-toggle" capability. Other agents in the Hive cluster
can invoke this capability to control GPIO pins on the Pico.

Wiring (optional):
    - Connect an LED + resistor to GP15 (or change LED_PIN below).

Usage:
    1. Copy hive_agent.py and this file to the Pico W filesystem.
    2. Update WIFI_SSID, WIFI_PASS, and MQTT_HOST below.
    3. Run or set as main.py for auto-start on boot.
"""

import machine
import network
from utime import sleep_ms
from hive_agent import HiveAgent

# ---------------------------------------------------------------------------
# Configuration  -- edit these for your environment
# ---------------------------------------------------------------------------

WIFI_SSID = "YourNetworkSSID"
WIFI_PASS = "YourNetworkPassword"
MQTT_HOST = "192.168.1.100"  # IP of the machine running hived
MQTT_PORT = 1883
AGENT_ID = "pico-gpio-01"
JOIN_TOKEN = ""  # Set if your cluster requires join tokens.
TEAM_ID = ""     # Set if this agent belongs to a team.
LED_PIN = 15     # Default GPIO pin for the on-board or external LED.

# ---------------------------------------------------------------------------
# Wi-Fi
# ---------------------------------------------------------------------------

def connect_wifi(ssid, password, timeout_ms=15000):
    """Connect to a Wi-Fi network and return the WLAN interface."""
    wlan = network.WLAN(network.STA_IF)
    wlan.active(True)
    if wlan.isconnected():
        print("wifi: already connected, ip =", wlan.ifconfig()[0])
        return wlan

    print("wifi: connecting to", ssid)
    wlan.connect(ssid, password)

    elapsed = 0
    while not wlan.isconnected() and elapsed < timeout_ms:
        sleep_ms(500)
        elapsed += 500

    if wlan.isconnected():
        print("wifi: connected, ip =", wlan.ifconfig()[0])
    else:
        raise RuntimeError("wifi: failed to connect within {}ms".format(timeout_ms))

    return wlan

# ---------------------------------------------------------------------------
# GPIO setup
# ---------------------------------------------------------------------------

# Track pin objects so we can reuse them across invocations.
_pins = {}

def _get_pin(pin_num):
    """Return a machine.Pin configured as output, creating it if needed."""
    if pin_num not in _pins:
        _pins[pin_num] = machine.Pin(pin_num, machine.Pin.OUT)
    return _pins[pin_num]

# ---------------------------------------------------------------------------
# Capability handler
# ---------------------------------------------------------------------------

def handle_gpio_toggle(inputs):
    """Toggle a GPIO pin or set it to a specific state.

    Accepted inputs:
        pin   (int)  - GPIO pin number. Defaults to LED_PIN.
        state (str)  - "on", "off", or "toggle" (default "toggle").

    Returns:
        pin   (int)  - The pin that was changed.
        state (str)  - The new state ("on" or "off").
    """
    pin_num = inputs.get("pin", LED_PIN)
    desired = inputs.get("state", "toggle")

    pin = _get_pin(pin_num)

    if desired == "on":
        pin.value(1)
    elif desired == "off":
        pin.value(0)
    else:
        # Toggle current state.
        pin.value(1 - pin.value())

    new_state = "on" if pin.value() else "off"
    print("gpio: pin {} -> {}".format(pin_num, new_state))

    return {
        "pin": pin_num,
        "state": new_state,
    }

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

def main():
    # 1. Connect to Wi-Fi.
    connect_wifi(WIFI_SSID, WIFI_PASS)

    # 2. Create and configure the Hive agent.
    agent = HiveAgent({
        "agent_id": AGENT_ID,
        "mqtt_host": MQTT_HOST,
        "mqtt_port": MQTT_PORT,
        "join_token": JOIN_TOKEN,
        "team_id": TEAM_ID,
        "heartbeat_interval": 30,
    })

    # 3. Register capabilities.
    agent.register_capability("gpio-toggle", handle_gpio_toggle)

    # 4. Connect to the MQTT broker.
    agent.connect()
    print("hive: agent '{}' connected".format(AGENT_ID))

    # 5. Enter the main loop (processes messages + heartbeats).
    try:
        agent.run()
    except KeyboardInterrupt:
        print("hive: interrupted, shutting down")
    finally:
        agent.stop()


if __name__ == "__main__":
    main()
