/*
 * Hive Firmware SDK for C
 *
 * Minimal C SDK for Tier 3 firmware agents. Connects to the Hive control
 * plane via MQTT and provides capability registration, heartbeat publishing,
 * and message envelope construction matching the Hive protocol.
 *
 * Target footprint: ~15-30KB flash, ~5-10KB RAM (excluding MQTT/TLS libs).
 * No dynamic memory allocation -- all buffers are statically sized.
 *
 * Platform targets: ESP-IDF, Arduino, Pico SDK, Zephyr, bare metal ARM.
 */

#ifndef HIVE_H
#define HIVE_H

#ifdef __cplusplus
extern "C" {
#endif

#include <stddef.h>
#include <stdint.h>

/* ---------------------------------------------------------------------------
 * Status codes
 * --------------------------------------------------------------------------- */

#define HIVE_OK            0
#define HIVE_ERR_CONNECT  -1
#define HIVE_ERR_PUBLISH  -2
#define HIVE_ERR_INVALID  -3
#define HIVE_ERR_FULL     -4   /* capability table full */
#define HIVE_ERR_NOMATCH  -5   /* no handler for capability */
#define HIVE_ERR_OVERFLOW -6   /* output buffer overflow */

/* ---------------------------------------------------------------------------
 * Limits -- override at compile time with -D if needed
 * --------------------------------------------------------------------------- */

#ifndef HIVE_MAX_CAPABILITIES
#define HIVE_MAX_CAPABILITIES 8
#endif

#ifndef HIVE_MAX_TOPIC_LEN
#define HIVE_MAX_TOPIC_LEN 128
#endif

#ifndef HIVE_MAX_PAYLOAD_LEN
#define HIVE_MAX_PAYLOAD_LEN 1024
#endif

#ifndef HIVE_MAX_AGENT_ID_LEN
#define HIVE_MAX_AGENT_ID_LEN 64
#endif

#ifndef HIVE_MAX_CAP_NAME_LEN
#define HIVE_MAX_CAP_NAME_LEN 48
#endif

#ifndef HIVE_ENVELOPE_BUF_LEN
#define HIVE_ENVELOPE_BUF_LEN 2048
#endif

/* ---------------------------------------------------------------------------
 * Agent operating mode
 * --------------------------------------------------------------------------- */

typedef enum {
    HIVE_MODE_TOOL = 0,  /* responds to capability invocations only */
    HIVE_MODE_PEER = 1   /* can initiate messages and run autonomous logic */
} hive_mode_t;

/* ---------------------------------------------------------------------------
 * Configuration
 * --------------------------------------------------------------------------- */

typedef struct {
    const char *agent_id;             /* required, e.g. "temp-sensor-01" */
    const char *mqtt_host;            /* required, e.g. "192.168.1.10" */
    uint16_t    mqtt_port;            /* default 1883 */
    const char *mqtt_user;            /* optional, NULL if unused */
    const char *mqtt_pass;            /* optional, NULL if unused */
    uint32_t    heartbeat_interval_ms;/* default 30000 (30s) */
    hive_mode_t mode;                 /* HIVE_MODE_TOOL or HIVE_MODE_PEER */
} hive_config_t;

/* ---------------------------------------------------------------------------
 * Capability handler
 *
 * Called when a capability-request message arrives for this agent.
 *   inputs_json  -- the "inputs" object from the request, as a JSON string
 *   outputs_json -- buffer to write the "outputs" JSON object into
 *   max_output_len -- size of outputs_json buffer
 *
 * Return HIVE_OK on success, or a HIVE_ERR_* code on failure.
 * On success the SDK publishes a capability-response with status "success".
 * On failure the SDK publishes a capability-response with status "error".
 * --------------------------------------------------------------------------- */

typedef int (*hive_capability_handler_t)(const char *inputs_json,
                                         char       *outputs_json,
                                         size_t      max_output_len);

/* ---------------------------------------------------------------------------
 * SDK lifecycle
 * --------------------------------------------------------------------------- */

/*
 * hive_init -- Initialize the SDK and connect to the MQTT broker.
 *
 * Must be called before any other hive_* function. The config struct is
 * copied internally; the caller may free it after this call returns.
 *
 * Returns HIVE_OK on success, HIVE_ERR_CONNECT on connection failure,
 * or HIVE_ERR_INVALID if required config fields are missing.
 */
int hive_init(const hive_config_t *config);

/*
 * hive_register_capability -- Register a named capability with a handler.
 *
 * The SDK subscribes to the MQTT topic for capability requests and dispatches
 * incoming requests to the handler. Up to HIVE_MAX_CAPABILITIES may be
 * registered.
 *
 * Returns HIVE_OK on success, HIVE_ERR_FULL if the table is full, or
 * HIVE_ERR_INVALID if name or handler is NULL.
 */
int hive_register_capability(const char *name,
                              hive_capability_handler_t handler);

/*
 * hive_publish -- Publish a raw payload to an MQTT topic.
 *
 * The payload is sent as-is (no envelope wrapping). Use this for custom
 * topics or when you have already constructed the envelope JSON.
 *
 * Returns HIVE_OK on success, HIVE_ERR_PUBLISH on failure, or
 * HIVE_ERR_INVALID if topic or payload is NULL.
 */
int hive_publish(const char *topic, const char *payload);

/*
 * hive_loop -- Run one iteration of the MQTT event loop.
 *
 * Processes incoming messages, sends keepalive packets, and publishes
 * heartbeats when the heartbeat interval has elapsed. Call this from
 * your main loop.
 *
 * Returns HIVE_OK normally, or HIVE_ERR_CONNECT if the connection was lost.
 */
int hive_loop(void);

/*
 * hive_health_report -- Immediately publish a heartbeat message.
 *
 * Sends a health envelope to hive/health/{agent_id} with uptime,
 * free heap, and tier="firmware". Normally called automatically by
 * hive_loop() at the configured interval, but can be called manually.
 *
 * Returns HIVE_OK on success or HIVE_ERR_PUBLISH on failure.
 */
int hive_health_report(void);

/*
 * hive_destroy -- Disconnect from the broker and release resources.
 *
 * After this call, no other hive_* functions may be called until
 * hive_init() is called again.
 */
void hive_destroy(void);

/* ---------------------------------------------------------------------------
 * Peer mode helpers (only meaningful in HIVE_MODE_PEER)
 * --------------------------------------------------------------------------- */

/*
 * hive_send_to_agent -- Send an envelope-wrapped message to another agent.
 *
 * Constructs a full Hive envelope and publishes to
 * hive/agent/{to_agent_id}/inbox.
 *
 * Returns HIVE_OK on success.
 */
int hive_send_to_agent(const char *to_agent_id,
                        const char *type,
                        const char *payload_json);

/*
 * hive_team_broadcast -- Publish a broadcast message to the team.
 *
 * Requires team_id to be set (derived from agent manifest on the control
 * plane side; the firmware agent must know its team ID).
 *
 * Returns HIVE_OK on success.
 */
int hive_team_broadcast(const char *team_id, const char *content_json);

/* ---------------------------------------------------------------------------
 * Platform hooks -- implement these in your platform layer
 *
 * The SDK calls these functions for MQTT I/O. You must provide
 * implementations that match your platform's MQTT client library.
 * --------------------------------------------------------------------------- */

/*
 * hive_platform_mqtt_connect -- Connect to the MQTT broker.
 * Return 0 on success, non-zero on failure.
 */
extern int hive_platform_mqtt_connect(const char *host, uint16_t port,
                                       const char *client_id,
                                       const char *user, const char *pass);

/*
 * hive_platform_mqtt_publish -- Publish a message.
 * Return 0 on success, non-zero on failure.
 */
extern int hive_platform_mqtt_publish(const char *topic,
                                       const char *payload, size_t len);

/*
 * hive_platform_mqtt_subscribe -- Subscribe to a topic.
 * Return 0 on success, non-zero on failure.
 */
extern int hive_platform_mqtt_subscribe(const char *topic);

/*
 * hive_platform_mqtt_loop -- Process MQTT events (keepalive, receive).
 * Return 0 on success, non-zero on connection loss.
 */
extern int hive_platform_mqtt_loop(void);

/*
 * hive_platform_mqtt_disconnect -- Disconnect from the broker.
 */
extern void hive_platform_mqtt_disconnect(void);

/*
 * hive_platform_mqtt_set_callback -- Register the message receive callback.
 *
 * The SDK provides a callback with signature:
 *   void callback(const char *topic, const char *payload, size_t len)
 *
 * The platform layer must call this callback whenever a message arrives
 * on a subscribed topic.
 */
typedef void (*hive_mqtt_msg_callback_t)(const char *topic,
                                          const char *payload,
                                          size_t      len);

extern void hive_platform_mqtt_set_callback(hive_mqtt_msg_callback_t cb);

/* ---------------------------------------------------------------------------
 * Platform hooks -- timing and device metrics
 * --------------------------------------------------------------------------- */

/*
 * hive_platform_millis -- Return milliseconds since boot.
 */
extern uint32_t hive_platform_millis(void);

/*
 * hive_platform_free_heap -- Return free heap memory in bytes.
 * Return 0 if not available on this platform.
 */
extern uint32_t hive_platform_free_heap(void);

/*
 * hive_platform_random -- Return a pseudo-random 32-bit value.
 * Used for UUID generation. Does not need to be cryptographic.
 */
extern uint32_t hive_platform_random(void);

#ifdef __cplusplus
}
#endif

#endif /* HIVE_H */
