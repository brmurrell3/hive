"""
Hive Framework - MicroPython Firmware SDK

Provides the HiveAgent class for Tier 3 firmware devices to participate
in a Hive cluster over MQTT. Compatible with ESP32, Pi Pico W, and other
MicroPython-capable boards.

Target footprint: ~20KB additional to base MicroPython.
"""

import gc
import ujson
from utime import time, ticks_ms, ticks_diff, sleep_ms
from ubinascii import hexlify
from uos import urandom


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _uuid4():
    """Generate a UUID v4 string from urandom bytes."""
    b = urandom(16)
    # Set version (4) and variant (RFC 4122) bits.
    # MicroPython bytes are immutable, so build via bytearray.
    ba = bytearray(b)
    ba[6] = (ba[6] & 0x0F) | 0x40  # version 4
    ba[8] = (ba[8] & 0x3F) | 0x80  # variant 1
    h = hexlify(ba).decode()
    return "{}-{}-{}-{}-{}".format(h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])


def _rfc3339_utc():
    """Return an RFC 3339 UTC timestamp string from the system clock.

    MicroPython's time.time() returns seconds since 2000-01-01 on most ports.
    gmtime() expects and returns values relative to that same epoch, with the
    year field already being the correct calendar year (e.g. 2026).  We call
    gmtime() directly without any offset and format the result.
    """
    try:
        from utime import gmtime
    except ImportError:
        from time import gmtime
    t = time()
    tm = gmtime(t) if callable(gmtime) else None  # pragma: no cover
    if tm is None:
        # Fallback: just return epoch seconds as string.
        return str(t)
    return "{:04d}-{:02d}-{:02d}T{:02d}:{:02d}:{:02d}Z".format(
        tm[0], tm[1], tm[2], tm[3], tm[4], tm[5]
    )


# ---------------------------------------------------------------------------
# Agent modes
# ---------------------------------------------------------------------------

MODE_TOOL = "tool"
MODE_PEER = "peer"


# ---------------------------------------------------------------------------
# HiveAgent
# ---------------------------------------------------------------------------

class HiveAgent:
    """MicroPython agent that connects to a Hive cluster over MQTT.

    Parameters
    ----------
    config : dict
        Required keys:
            agent_id          - Unique agent identifier (lowercase, hyphens ok).
            mqtt_host         - Hostname or IP of the MQTT broker (hived).
            mqtt_port         - MQTT broker port (default 1883).
        Optional keys:
            join_token        - Token for authenticating with the control plane.
            heartbeat_interval - Seconds between heartbeats (default 30).
            mode              - "tool" (default) or "peer".
            team_id           - Team this agent belongs to.
            keepalive         - MQTT keepalive in seconds (default 60).
    """

    def __init__(self, config):
        self.agent_id = config["agent_id"]
        self.mqtt_host = config["mqtt_host"]
        self.mqtt_port = config.get("mqtt_port", 1883)
        self.join_token = config.get("join_token", "")
        self.heartbeat_interval = config.get("heartbeat_interval", 30)
        self.mode = config.get("mode", MODE_TOOL)
        self.team_id = config.get("team_id", "")
        self.keepalive = config.get("keepalive", 60)

        # Internal state
        self._capabilities = {}       # name -> handler_fn
        self._client = None
        self._running = False
        self._boot_ticks = ticks_ms()
        self._last_heartbeat = 0      # ticks_ms of last heartbeat
        self._inbox_handler = None    # callback for peer-mode inbox messages
        self._broadcast_handler = None  # callback for team broadcast messages

    # ------------------------------------------------------------------
    # Connection
    # ------------------------------------------------------------------

    def connect(self):
        """Connect to the MQTT broker and subscribe to base topics."""
        from umqtt.simple import MQTTClient

        client_id = "hive-{}".format(self.agent_id)

        # If a join token is provided, use it as MQTT username/password.
        user = self.agent_id if self.join_token else None
        password = self.join_token if self.join_token else None

        self._client = MQTTClient(
            client_id,
            self.mqtt_host,
            port=self.mqtt_port,
            user=user,
            password=password,
            keepalive=self.keepalive,
        )

        self._client.set_callback(self._on_message)
        self._client.connect()

        # Subscribe to agent inbox.
        self._client.subscribe("hive/agent/{}/inbox".format(self.agent_id).encode())

        # Subscribe to control channel.
        self._client.subscribe("hive/control/{}".format(self.agent_id).encode())

        # Subscribe to join status.
        self._client.subscribe("hive/join/status/{}".format(self.agent_id).encode())

        # If team_id is set, subscribe to team broadcast.
        if self.team_id:
            self._client.subscribe(
                "hive/team/{}/broadcast".format(self.team_id).encode()
            )

        # Publish join request.
        self._publish_join_request()

    # ------------------------------------------------------------------
    # Capabilities
    # ------------------------------------------------------------------

    def register_capability(self, name, handler_fn):
        """Register a capability with the given name and handler function.

        The handler_fn receives a single argument: the ``inputs`` dict from
        the capability-request payload.  It must return a dict of outputs
        (or raise an Exception on failure).

        After registration, the agent subscribes to the MQTT request topic
        for this capability so that other agents can invoke it.
        """
        self._capabilities[name] = handler_fn

        # Subscribe to capability request topic.
        topic = "hive/capabilities/{}/{}/request".format(self.agent_id, name)
        if self._client is not None:
            self._client.subscribe(topic.encode())

    def on_inbox(self, handler_fn):
        """Set a handler for peer-mode inbox messages.

        handler_fn receives the full decoded envelope dict.
        """
        self._inbox_handler = handler_fn

    def on_broadcast(self, handler_fn):
        """Set a handler for team broadcast messages.

        handler_fn receives the full decoded envelope dict.
        """
        self._broadcast_handler = handler_fn

    # ------------------------------------------------------------------
    # Messaging
    # ------------------------------------------------------------------

    def publish(self, topic, payload):
        """Publish a Hive envelope to the given MQTT topic.

        ``payload`` should be a dict; it is wrapped in the standard Hive
        envelope automatically.
        """
        envelope = self._make_envelope(
            to=topic,
            msg_type="broadcast",
            payload=payload,
        )
        self._client.publish(topic.encode(), ujson.dumps(envelope).encode())

    def send_to_agent(self, target_agent_id, payload, msg_type="task"):
        """Send a message to another agent's inbox."""
        topic = "hive/agent/{}/inbox".format(target_agent_id)
        envelope = self._make_envelope(
            to=target_agent_id,
            msg_type=msg_type,
            payload=payload,
        )
        self._client.publish(topic.encode(), ujson.dumps(envelope).encode())

    def invoke_capability(self, target_agent_id, capability, inputs, timeout_s=30):
        """Invoke a capability on another agent (fire-and-forget).

        For firmware devices the invocation is fire-and-forget; the response
        arrives asynchronously on the agent inbox or via a dedicated response
        subscription.

        Returns the correlation_id so the caller can match a future response.
        """
        correlation_id = _uuid4()
        reply_topic = "hive/capabilities/{}/{}/response".format(
            self.agent_id, capability
        )

        # Ensure we are subscribed to the response topic.
        self._client.subscribe(reply_topic.encode())

        topic = "hive/capabilities/{}/{}/request".format(
            target_agent_id, capability
        )
        envelope = self._make_envelope(
            to=target_agent_id,
            msg_type="capability-request",
            payload={
                "capability": capability,
                "inputs": inputs,
                "timeout": "{}s".format(timeout_s),
            },
            reply_to=reply_topic,
            correlation_id=correlation_id,
        )
        self._client.publish(topic.encode(), ujson.dumps(envelope).encode())
        return correlation_id

    # ------------------------------------------------------------------
    # Health
    # ------------------------------------------------------------------

    def health_report(self):
        """Send a heartbeat / health report to the control plane."""
        gc.collect()

        uptime_ms = ticks_diff(ticks_ms(), self._boot_ticks)
        uptime_s = uptime_ms // 1000

        health_payload = {
            "healthy": True,
            "uptime_seconds": uptime_s,
            "tier": "firmware",
            "free_heap_bytes": gc.mem_free(),
        }

        # Attempt to read RSSI (Wi-Fi signal strength) if available.
        try:
            import network
            sta = network.WLAN(network.STA_IF)
            if sta.isconnected():
                rssi = sta.status("rssi")
                health_payload["rssi"] = rssi
        except Exception:
            pass

        topic = "hive/health/{}".format(self.agent_id)
        envelope = self._make_envelope(
            to="hive.health",
            msg_type="health",
            payload=health_payload,
        )
        self._client.publish(topic.encode(), ujson.dumps(envelope).encode())
        self._last_heartbeat = ticks_ms()

    # ------------------------------------------------------------------
    # Main loop
    # ------------------------------------------------------------------

    def run(self):
        """Enter the main event loop.

        Processes incoming MQTT messages and sends periodic heartbeats.
        Call ``stop()`` from a capability handler or interrupt to exit.
        """
        self._running = True
        # Send initial heartbeat immediately.
        self.health_report()

        while self._running:
            # Process any pending MQTT messages (non-blocking).
            try:
                self._client.check_msg()
            except Exception as e:
                print("hive: MQTT check_msg error:", e)
                self._try_reconnect()

            # Send heartbeat if interval has elapsed.
            now = ticks_ms()
            if ticks_diff(now, self._last_heartbeat) >= self.heartbeat_interval * 1000:
                try:
                    self.health_report()
                except Exception as e:
                    print("hive: heartbeat error:", e)

            # Yield to other tasks / avoid busy-spin.
            sleep_ms(100)

    def stop(self):
        """Stop the event loop and disconnect from MQTT."""
        self._running = False
        if self._client is not None:
            try:
                self._client.disconnect()
            except Exception:
                pass

    # ------------------------------------------------------------------
    # Internal helpers
    # ------------------------------------------------------------------

    def _make_envelope(self, to, msg_type, payload, reply_to=None, correlation_id=None):
        """Build a Hive message envelope dict."""
        env = {
            "id": _uuid4(),
            "from": self.agent_id,
            "to": to,
            "type": msg_type,
            "timestamp": _rfc3339_utc(),
            "payload": payload,
        }
        if reply_to is not None:
            env["reply_to"] = reply_to
        if correlation_id is not None:
            env["correlation_id"] = correlation_id
        return env

    def _publish_join_request(self):
        """Publish a join request so the control plane registers this agent."""
        payload = {
            "agent_id": self.agent_id,
            "tier": "firmware",
            "message_format": "json",
        }
        if self.team_id:
            payload["team_id"] = self.team_id
        if self.join_token:
            payload["join_token"] = self.join_token

        envelope = self._make_envelope(
            to="hive.join",
            msg_type="status",
            payload=payload,
        )
        self._client.publish(
            b"hive/join/request",
            ujson.dumps(envelope).encode(),
        )

    def _on_message(self, topic, msg):
        """MQTT message callback dispatched by umqtt."""
        topic_str = topic.decode() if isinstance(topic, bytes) else topic

        try:
            envelope = ujson.loads(msg)
        except Exception:
            print("hive: failed to decode message on", topic_str)
            return

        # --- Capability request ---
        # Topic pattern: hive/capabilities/{agent_id}/{cap}/request
        cap_prefix = "hive/capabilities/{}/".format(self.agent_id)
        if topic_str.startswith(cap_prefix) and topic_str.endswith("/request"):
            cap_name = topic_str[len(cap_prefix):-len("/request")]
            self._handle_capability_request(cap_name, envelope)
            return

        # --- Agent inbox ---
        inbox_topic = "hive/agent/{}/inbox".format(self.agent_id)
        if topic_str == inbox_topic:
            if self._inbox_handler is not None:
                try:
                    self._inbox_handler(envelope)
                except Exception as e:
                    print("hive: inbox handler error:", e)
            return

        # --- Team broadcast ---
        if self.team_id:
            broadcast_topic = "hive/team/{}/broadcast".format(self.team_id)
            if topic_str == broadcast_topic:
                if self._broadcast_handler is not None:
                    try:
                        self._broadcast_handler(envelope)
                    except Exception as e:
                        print("hive: broadcast handler error:", e)
                return

        # --- Control messages ---
        control_topic = "hive/control/{}".format(self.agent_id)
        if topic_str == control_topic:
            self._handle_control(envelope)
            return

    def _handle_capability_request(self, cap_name, envelope):
        """Execute a registered capability handler and publish the response."""
        start = ticks_ms()
        payload = envelope.get("payload", {})
        inputs = payload.get("inputs", {})
        reply_to = envelope.get("reply_to")
        correlation_id = envelope.get("correlation_id")
        caller = envelope.get("from", "unknown")

        handler = self._capabilities.get(cap_name)
        if handler is None:
            # Capability not found.
            resp_payload = {
                "capability": cap_name,
                "status": "error",
                "error": {
                    "code": "CAPABILITY_NOT_FOUND",
                    "message": "No handler for capability '{}'".format(cap_name),
                    "retryable": False,
                },
                "duration_ms": 0,
            }
        else:
            try:
                outputs = handler(inputs)
                duration = ticks_diff(ticks_ms(), start)
                resp_payload = {
                    "capability": cap_name,
                    "status": "success",
                    "outputs": outputs if outputs is not None else {},
                    "duration_ms": duration,
                }
            except Exception as e:
                duration = ticks_diff(ticks_ms(), start)
                resp_payload = {
                    "capability": cap_name,
                    "status": "error",
                    "error": {
                        "code": "CAPABILITY_FAILED",
                        "message": str(e),
                        "retryable": False,
                    },
                    "duration_ms": duration,
                }

        # Determine response topic.
        if reply_to:
            resp_topic = reply_to
        else:
            resp_topic = "hive/capabilities/{}/{}/response".format(
                self.agent_id, cap_name
            )

        resp_envelope = self._make_envelope(
            to=caller,
            msg_type="capability-response",
            payload=resp_payload,
            correlation_id=correlation_id,
        )
        self._client.publish(
            resp_topic.encode(), ujson.dumps(resp_envelope).encode()
        )

    def _handle_control(self, envelope):
        """Handle control messages from the control plane."""
        payload = envelope.get("payload", {})
        action = payload.get("action", "")
        if action == "stop":
            self.stop()
        elif action == "health":
            self.health_report()

    def _try_reconnect(self):
        """Attempt to reconnect to the MQTT broker after a connection loss."""
        sleep_ms(1000)
        try:
            self._client.connect()
            # Re-subscribe to all topics.
            self._client.subscribe(
                "hive/agent/{}/inbox".format(self.agent_id).encode()
            )
            self._client.subscribe(
                "hive/control/{}".format(self.agent_id).encode()
            )
            if self.team_id:
                self._client.subscribe(
                    "hive/team/{}/broadcast".format(self.team_id).encode()
                )
            for cap_name in self._capabilities:
                topic = "hive/capabilities/{}/{}/request".format(
                    self.agent_id, cap_name
                )
                self._client.subscribe(topic.encode())
            print("hive: reconnected")
        except Exception as e:
            print("hive: reconnect failed:", e)
