/*
 * Hive Firmware SDK -- Core Implementation
 *
 * All buffers are statically allocated. No malloc/free calls.
 * JSON construction and parsing are hand-written (no cJSON dependency).
 */

#include "hive.h"

#include <string.h>
#include <stdio.h>

/* ---------------------------------------------------------------------------
 * Internal state
 * --------------------------------------------------------------------------- */

typedef struct {
    char                      name[HIVE_MAX_CAP_NAME_LEN];
    hive_capability_handler_t handler;
} hive_cap_entry_t;

static struct {
    char          agent_id[HIVE_MAX_AGENT_ID_LEN];
    uint32_t      heartbeat_interval_ms;
    hive_mode_t   mode;
    int           initialized;
    int           connected;

    /* capability table */
    hive_cap_entry_t capabilities[HIVE_MAX_CAPABILITIES];
    int              cap_count;

    /* timing */
    uint32_t      boot_ms;
    uint32_t      last_heartbeat_ms;
} g_hive;

/* scratch buffers -- reused across calls, never on the stack */
static char g_topic_buf[HIVE_MAX_TOPIC_LEN];
static char g_envelope_buf[HIVE_ENVELOPE_BUF_LEN];
static char g_output_buf[HIVE_MAX_PAYLOAD_LEN];

/* ---------------------------------------------------------------------------
 * Minimal JSON helpers -- no allocations, writes into caller-supplied buffer
 * --------------------------------------------------------------------------- */

/*
 * json_escape -- Write a JSON-escaped version of src into dst.
 * Returns the number of bytes written (not counting the NUL terminator),
 * or -1 if the buffer is too small.
 */
static int json_escape(char *dst, size_t dst_len, const char *src)
{
    size_t wi = 0;
    if (!src) {
        if (dst_len < 1) return -1;
        dst[0] = '\0';
        return 0;
    }
    for (const char *p = src; *p; p++) {
        char esc = 0;
        switch (*p) {
        case '"':  esc = '"';  break;
        case '\\': esc = '\\'; break;
        case '\n': esc = 'n';  break;
        case '\r': esc = 'r';  break;
        case '\t': esc = 't';  break;
        default:   break;
        }
        if (esc) {
            if (wi + 2 >= dst_len) return -1;
            dst[wi++] = '\\';
            dst[wi++] = esc;
        } else {
            if (wi + 1 >= dst_len) return -1;
            dst[wi++] = *p;
        }
    }
    if (wi >= dst_len) return -1;
    dst[wi] = '\0';
    return (int)wi;
}

/*
 * json_find_value -- Find the value of a key in a flat JSON object.
 *
 * Writes the value (without surrounding quotes for strings) into out_buf.
 * Handles string, number, and boolean values. Does NOT handle nested objects
 * or arrays -- for those, use json_find_object.
 *
 * Returns the length of the value, or -1 if not found.
 */
static int json_find_value(const char *json, const char *key,
                            char *out_buf, size_t out_len)
{
    if (!json || !key) return -1;

    /* build the search pattern: "key": or "key" : */
    char pattern[HIVE_MAX_CAP_NAME_LEN + 4];
    int plen = snprintf(pattern, sizeof(pattern), "\"%s\"", key);
    if (plen < 0 || (size_t)plen >= sizeof(pattern)) return -1;

    const char *pos = strstr(json, pattern);
    if (!pos) return -1;

    /* advance past the key and find the colon */
    pos += plen;
    while (*pos == ' ' || *pos == '\t' || *pos == '\n' || *pos == '\r') pos++;
    if (*pos != ':') return -1;
    pos++;
    while (*pos == ' ' || *pos == '\t' || *pos == '\n' || *pos == '\r') pos++;

    if (*pos == '"') {
        /* string value */
        pos++;
        size_t wi = 0;
        while (*pos && *pos != '"') {
            if (*pos == '\\' && *(pos + 1)) {
                pos++; /* skip escape, take next char */
            }
            if (wi + 1 >= out_len) return -1;
            out_buf[wi++] = *pos++;
        }
        out_buf[wi] = '\0';
        return (int)wi;
    } else {
        /* number, bool, or null */
        size_t wi = 0;
        while (*pos && *pos != ',' && *pos != '}' && *pos != ' '
               && *pos != '\n' && *pos != '\r' && *pos != '\t') {
            if (wi + 1 >= out_len) return -1;
            out_buf[wi++] = *pos++;
        }
        out_buf[wi] = '\0';
        return (int)wi;
    }
}

/*
 * json_find_object -- Extract a nested JSON object value for a key.
 *
 * Copies the full object (including braces) into out_buf.
 * Returns the length or -1 on failure.
 */
static int json_find_object(const char *json, const char *key,
                              char *out_buf, size_t out_len)
{
    if (!json || !key) return -1;

    char pattern[HIVE_MAX_CAP_NAME_LEN + 4];
    int plen = snprintf(pattern, sizeof(pattern), "\"%s\"", key);
    if (plen < 0 || (size_t)plen >= sizeof(pattern)) return -1;

    const char *pos = strstr(json, pattern);
    if (!pos) return -1;

    pos += plen;
    while (*pos == ' ' || *pos == '\t' || *pos == '\n' || *pos == '\r') pos++;
    if (*pos != ':') return -1;
    pos++;
    while (*pos == ' ' || *pos == '\t' || *pos == '\n' || *pos == '\r') pos++;

    if (*pos != '{') return -1;

    /* brace-counting copy */
    int depth = 0;
    size_t wi = 0;
    for (; *pos; pos++) {
        if (*pos == '{') depth++;
        else if (*pos == '}') depth--;
        if (wi + 1 >= out_len) return -1;
        out_buf[wi++] = *pos;
        if (depth == 0) break;
    }
    if (wi >= out_len) return -1;
    out_buf[wi] = '\0';
    return (int)wi;
}

/* ---------------------------------------------------------------------------
 * UUID generation -- pseudo-random v4 UUID (not cryptographic)
 * --------------------------------------------------------------------------- */

static void uuid_v4(char *buf, size_t len)
{
    /* UUID v4: xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx */
    if (len < 37) {
        if (len > 0) buf[0] = '\0';
        return;
    }

    static const char hex[] = "0123456789abcdef";
    uint32_t r;
    int ri = 0;

    for (int i = 0; i < 36; i++) {
        if (i == 8 || i == 13 || i == 18 || i == 23) {
            buf[i] = '-';
            continue;
        }
        if (i == 14) {
            buf[i] = '4'; /* version */
            continue;
        }
        if (ri == 0) {
            r = hive_platform_random();
            ri = 8; /* 8 hex digits per uint32 */
        }
        uint8_t nibble = r & 0x0F;
        r >>= 4;
        ri--;

        if (i == 19) {
            nibble = (nibble & 0x03) | 0x08; /* variant bits */
        }
        buf[i] = hex[nibble];
    }
    buf[36] = '\0';
}

/* ---------------------------------------------------------------------------
 * Timestamp -- seconds since boot, formatted as a simple ISO-ish string.
 *
 * Firmware devices typically lack an RTC. We produce a relative timestamp
 * "1970-01-01T00:00:00Z" offset by uptime_seconds. The control plane should
 * re-stamp messages with the actual wall time when received via the MQTT
 * bridge. If the platform provides real time, override this with a custom
 * implementation.
 * --------------------------------------------------------------------------- */

static void format_timestamp(char *buf, size_t len, uint32_t uptime_s)
{
    /* Produce a minimal RFC3339 timestamp based on uptime.
     * For embedded devices without an RTC, this is acceptable --
     * the control plane re-stamps with real time. */
    uint32_t s = uptime_s % 60;
    uint32_t m = (uptime_s / 60) % 60;
    uint32_t h = (uptime_s / 3600) % 24;
    uint32_t d = uptime_s / 86400;

    /* days since epoch -- for firmware we just add to 1970-01-01 */
    snprintf(buf, len, "1970-01-%02uT%02u:%02u:%02uZ",
             (unsigned)(1 + (d % 28)), (unsigned)h, (unsigned)m, (unsigned)s);
}

/* ---------------------------------------------------------------------------
 * Envelope construction
 * --------------------------------------------------------------------------- */

/*
 * build_envelope -- Construct a Hive message envelope JSON string.
 *
 * The payload_json argument should be a valid JSON object string (e.g.
 * {"healthy":true}). It is embedded verbatim (not escaped) as the
 * "payload" field value.
 *
 * Returns the length written, or -1 on overflow.
 */
static int build_envelope(char *buf, size_t buf_len,
                           const char *from, const char *to,
                           const char *type, const char *payload_json,
                           const char *reply_to, const char *correlation_id)
{
    char uuid[37];
    uuid_v4(uuid, sizeof(uuid));

    uint32_t now_ms = hive_platform_millis();
    uint32_t uptime_s = (now_ms - g_hive.boot_ms) / 1000;

    char ts[32];
    format_timestamp(ts, sizeof(ts), uptime_s);

    char escaped_from[HIVE_MAX_AGENT_ID_LEN * 2];
    char escaped_to[HIVE_MAX_AGENT_ID_LEN * 2];
    json_escape(escaped_from, sizeof(escaped_from), from);
    json_escape(escaped_to, sizeof(escaped_to), to);

    int n;
    if (reply_to && correlation_id) {
        char escaped_reply[HIVE_MAX_TOPIC_LEN * 2];
        char escaped_corr[48];
        json_escape(escaped_reply, sizeof(escaped_reply), reply_to);
        json_escape(escaped_corr, sizeof(escaped_corr), correlation_id);

        n = snprintf(buf, buf_len,
            "{\"id\":\"%s\","
            "\"from\":\"%s\","
            "\"to\":\"%s\","
            "\"type\":\"%s\","
            "\"timestamp\":\"%s\","
            "\"payload\":%s,"
            "\"reply_to\":\"%s\","
            "\"correlation_id\":\"%s\"}",
            uuid, escaped_from, escaped_to, type, ts,
            payload_json ? payload_json : "{}",
            escaped_reply, escaped_corr);
    } else {
        n = snprintf(buf, buf_len,
            "{\"id\":\"%s\","
            "\"from\":\"%s\","
            "\"to\":\"%s\","
            "\"type\":\"%s\","
            "\"timestamp\":\"%s\","
            "\"payload\":%s}",
            uuid, escaped_from, escaped_to, type, ts,
            payload_json ? payload_json : "{}");
    }

    if (n < 0 || (size_t)n >= buf_len) return -1;
    return n;
}

/* ---------------------------------------------------------------------------
 * Internal message dispatcher
 * --------------------------------------------------------------------------- */

static void on_message(const char *topic, const char *payload, size_t len);

/* ---------------------------------------------------------------------------
 * Public API implementation
 * --------------------------------------------------------------------------- */

int hive_init(const hive_config_t *config)
{
    if (!config || !config->agent_id || !config->mqtt_host) {
        return HIVE_ERR_INVALID;
    }

    memset(&g_hive, 0, sizeof(g_hive));

    size_t id_len = strlen(config->agent_id);
    if (id_len == 0 || id_len >= HIVE_MAX_AGENT_ID_LEN) {
        return HIVE_ERR_INVALID;
    }
    memcpy(g_hive.agent_id, config->agent_id, id_len + 1);

    g_hive.heartbeat_interval_ms = config->heartbeat_interval_ms;
    if (g_hive.heartbeat_interval_ms == 0) {
        g_hive.heartbeat_interval_ms = 30000; /* default 30s */
    }
    g_hive.mode = config->mode;

    uint16_t port = config->mqtt_port;
    if (port == 0) port = 1883;

    /* set up the receive callback before connecting */
    hive_platform_mqtt_set_callback(on_message);

    int rc = hive_platform_mqtt_connect(
        config->mqtt_host, port,
        g_hive.agent_id,
        config->mqtt_user, config->mqtt_pass);

    if (rc != 0) {
        return HIVE_ERR_CONNECT;
    }

    g_hive.connected = 1;
    g_hive.initialized = 1;
    g_hive.boot_ms = hive_platform_millis();
    g_hive.last_heartbeat_ms = 0; /* force immediate first heartbeat */

    /* subscribe to the agent inbox */
    snprintf(g_topic_buf, sizeof(g_topic_buf),
             "hive/agent/%s/inbox", g_hive.agent_id);
    hive_platform_mqtt_subscribe(g_topic_buf);

    return HIVE_OK;
}

int hive_register_capability(const char *name,
                              hive_capability_handler_t handler)
{
    if (!name || !handler) return HIVE_ERR_INVALID;
    if (g_hive.cap_count >= HIVE_MAX_CAPABILITIES) return HIVE_ERR_FULL;

    size_t nlen = strlen(name);
    if (nlen == 0 || nlen >= HIVE_MAX_CAP_NAME_LEN) return HIVE_ERR_INVALID;

    hive_cap_entry_t *entry = &g_hive.capabilities[g_hive.cap_count];
    memcpy(entry->name, name, nlen + 1);
    entry->handler = handler;
    g_hive.cap_count++;

    /* subscribe to capability request topic */
    snprintf(g_topic_buf, sizeof(g_topic_buf),
             "hive/capabilities/%s/%s/request",
             g_hive.agent_id, name);
    hive_platform_mqtt_subscribe(g_topic_buf);

    return HIVE_OK;
}

int hive_publish(const char *topic, const char *payload)
{
    if (!topic || !payload) return HIVE_ERR_INVALID;
    if (!g_hive.connected) return HIVE_ERR_CONNECT;

    int rc = hive_platform_mqtt_publish(topic, payload, strlen(payload));
    return (rc == 0) ? HIVE_OK : HIVE_ERR_PUBLISH;
}

int hive_loop(void)
{
    if (!g_hive.initialized) return HIVE_ERR_INVALID;

    int rc = hive_platform_mqtt_loop();
    if (rc != 0) {
        g_hive.connected = 0;
        return HIVE_ERR_CONNECT;
    }

    /* periodic heartbeat */
    uint32_t now = hive_platform_millis();
    if (now - g_hive.last_heartbeat_ms >= g_hive.heartbeat_interval_ms) {
        g_hive.last_heartbeat_ms = now;
        hive_health_report();
    }

    return HIVE_OK;
}

int hive_health_report(void)
{
    if (!g_hive.connected) return HIVE_ERR_PUBLISH;

    uint32_t now_ms = hive_platform_millis();
    uint32_t uptime_s = (now_ms - g_hive.boot_ms) / 1000;
    uint32_t free_heap = hive_platform_free_heap();

    /* build health payload */
    char payload[256];
    int plen = snprintf(payload, sizeof(payload),
        "{\"healthy\":true,"
        "\"uptime_seconds\":%u,"
        "\"tier\":\"firmware\","
        "\"free_heap_bytes\":%u}",
        (unsigned)uptime_s, (unsigned)free_heap);

    if (plen < 0 || (size_t)plen >= sizeof(payload)) return HIVE_ERR_OVERFLOW;

    /* build envelope */
    int elen = build_envelope(g_envelope_buf, sizeof(g_envelope_buf),
                               g_hive.agent_id, "hive",
                               "health", payload, NULL, NULL);
    if (elen < 0) return HIVE_ERR_OVERFLOW;

    /* publish to health topic */
    snprintf(g_topic_buf, sizeof(g_topic_buf),
             "hive/health/%s", g_hive.agent_id);

    return hive_publish(g_topic_buf, g_envelope_buf);
}

void hive_destroy(void)
{
    if (g_hive.connected) {
        hive_platform_mqtt_disconnect();
    }
    memset(&g_hive, 0, sizeof(g_hive));
}

/* ---------------------------------------------------------------------------
 * Peer mode helpers
 * --------------------------------------------------------------------------- */

int hive_send_to_agent(const char *to_agent_id,
                        const char *type,
                        const char *payload_json)
{
    if (!to_agent_id || !type) return HIVE_ERR_INVALID;
    if (g_hive.mode != HIVE_MODE_PEER) return HIVE_ERR_INVALID;

    int elen = build_envelope(g_envelope_buf, sizeof(g_envelope_buf),
                               g_hive.agent_id, to_agent_id,
                               type, payload_json, NULL, NULL);
    if (elen < 0) return HIVE_ERR_OVERFLOW;

    snprintf(g_topic_buf, sizeof(g_topic_buf),
             "hive/agent/%s/inbox", to_agent_id);

    return hive_publish(g_topic_buf, g_envelope_buf);
}

int hive_team_broadcast(const char *team_id, const char *content_json)
{
    if (!team_id) return HIVE_ERR_INVALID;
    if (g_hive.mode != HIVE_MODE_PEER) return HIVE_ERR_INVALID;

    /* wrap content in broadcast payload */
    char payload[HIVE_MAX_PAYLOAD_LEN];
    int plen = snprintf(payload, sizeof(payload),
                         "{\"content\":%s}", content_json ? content_json : "{}");
    if (plen < 0 || (size_t)plen >= sizeof(payload)) return HIVE_ERR_OVERFLOW;

    int elen = build_envelope(g_envelope_buf, sizeof(g_envelope_buf),
                               g_hive.agent_id, team_id,
                               "broadcast", payload, NULL, NULL);
    if (elen < 0) return HIVE_ERR_OVERFLOW;

    snprintf(g_topic_buf, sizeof(g_topic_buf),
             "hive/team/%s/broadcast", team_id);

    return hive_publish(g_topic_buf, g_envelope_buf);
}

/* ---------------------------------------------------------------------------
 * Capability request dispatch
 * --------------------------------------------------------------------------- */

/*
 * Extract the capability name from an MQTT topic.
 * Expected format: hive/capabilities/{agent_id}/{capability_name}/request
 * Writes the capability name into out_buf. Returns length or -1.
 */
static int extract_capability_from_topic(const char *topic,
                                          char *out_buf, size_t out_len)
{
    /* skip "hive/capabilities/" */
    const char *prefix = "hive/capabilities/";
    size_t prefix_len = strlen(prefix);
    if (strncmp(topic, prefix, prefix_len) != 0) return -1;

    const char *rest = topic + prefix_len;

    /* skip agent_id (up to next '/') */
    const char *slash = strchr(rest, '/');
    if (!slash) return -1;
    slash++; /* move past the slash */

    /* find the next '/' (end of capability name) */
    const char *end = strchr(slash, '/');
    if (!end) return -1;

    size_t cap_len = (size_t)(end - slash);
    if (cap_len == 0 || cap_len >= out_len) return -1;

    memcpy(out_buf, slash, cap_len);
    out_buf[cap_len] = '\0';
    return (int)cap_len;
}

/*
 * Find the handler for a capability name. Returns NULL if not found.
 */
static hive_capability_handler_t find_handler(const char *cap_name)
{
    for (int i = 0; i < g_hive.cap_count; i++) {
        if (strcmp(g_hive.capabilities[i].name, cap_name) == 0) {
            return g_hive.capabilities[i].handler;
        }
    }
    return NULL;
}

/*
 * Handle an incoming capability request.
 */
static void handle_capability_request(const char *cap_name,
                                       const char *payload,
                                       size_t payload_len)
{
    (void)payload_len;

    hive_capability_handler_t handler = find_handler(cap_name);
    if (!handler) return;

    /* extract "inputs" from the payload envelope */
    /* first find the envelope payload object */
    char envelope_payload[HIVE_MAX_PAYLOAD_LEN];
    int found = json_find_object(payload, "payload", envelope_payload,
                                  sizeof(envelope_payload));

    /* extract the "inputs" from the envelope payload */
    char inputs[HIVE_MAX_PAYLOAD_LEN];
    if (found > 0) {
        int ifound = json_find_object(envelope_payload, "inputs", inputs,
                                       sizeof(inputs));
        if (ifound < 0) {
            /* inputs might not be an object, try as value */
            inputs[0] = '{';
            inputs[1] = '}';
            inputs[2] = '\0';
        }
    } else {
        inputs[0] = '{';
        inputs[1] = '}';
        inputs[2] = '\0';
    }

    /* extract correlation_id and reply_to from the envelope */
    char correlation_id[48] = {0};
    char reply_to[HIVE_MAX_TOPIC_LEN] = {0};
    json_find_value(payload, "correlation_id", correlation_id,
                     sizeof(correlation_id));
    json_find_value(payload, "reply_to", reply_to, sizeof(reply_to));

    /* extract the sender */
    char from[HIVE_MAX_AGENT_ID_LEN] = {0};
    json_find_value(payload, "from", from, sizeof(from));

    /* call the handler */
    memset(g_output_buf, 0, sizeof(g_output_buf));
    int rc = handler(inputs, g_output_buf, sizeof(g_output_buf));

    /* build response payload */
    char resp_payload[HIVE_MAX_PAYLOAD_LEN];
    char escaped_cap[HIVE_MAX_CAP_NAME_LEN * 2];
    json_escape(escaped_cap, sizeof(escaped_cap), cap_name);

    if (rc == HIVE_OK) {
        snprintf(resp_payload, sizeof(resp_payload),
            "{\"capability\":\"%s\","
            "\"status\":\"success\","
            "\"outputs\":%s}",
            escaped_cap,
            g_output_buf[0] ? g_output_buf : "{}");
    } else {
        snprintf(resp_payload, sizeof(resp_payload),
            "{\"capability\":\"%s\","
            "\"status\":\"error\","
            "\"error\":{\"code\":\"CAPABILITY_FAILED\","
            "\"message\":\"handler returned error\","
            "\"retryable\":false}}",
            escaped_cap);
    }

    /* build the response envelope */
    const char *resp_to = from[0] ? from : "hive";

    int elen = build_envelope(g_envelope_buf, sizeof(g_envelope_buf),
                               g_hive.agent_id, resp_to,
                               "capability-response", resp_payload,
                               reply_to[0] ? reply_to : NULL,
                               correlation_id[0] ? correlation_id : NULL);
    if (elen < 0) return;

    /* publish to the response topic (or reply_to if specified) */
    if (reply_to[0]) {
        /* MQTT reply_to topics use / separators */
        hive_publish(reply_to, g_envelope_buf);
    } else {
        snprintf(g_topic_buf, sizeof(g_topic_buf),
                 "hive/capabilities/%s/%s/response",
                 g_hive.agent_id, cap_name);
        hive_publish(g_topic_buf, g_envelope_buf);
    }
}

/* ---------------------------------------------------------------------------
 * MQTT message callback
 * --------------------------------------------------------------------------- */

static void on_message(const char *topic, const char *payload, size_t len)
{
    (void)len;

    if (!topic || !payload) return;

    /* check if this is a capability request */
    char cap_name[HIVE_MAX_CAP_NAME_LEN];
    int clen = extract_capability_from_topic(topic, cap_name, sizeof(cap_name));
    if (clen > 0) {
        handle_capability_request(cap_name, payload, len);
        return;
    }

    /*
     * Other message types (inbox, broadcast) could be dispatched here.
     * For M7 the firmware SDK focuses on capability handling and heartbeats.
     * Additional dispatch logic can be added as needed.
     */
}
