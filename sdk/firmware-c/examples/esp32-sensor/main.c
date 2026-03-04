/*
 * Hive Firmware SDK -- ESP32 Sensor Example
 *
 * Demonstrates a Tier 3 firmware agent running on an ESP32 with ESP-IDF.
 * Registers a "temperature-read" capability that returns mock sensor data.
 * Shows both HIVE_MODE_TOOL (respond-only) and HIVE_MODE_PEER (can initiate)
 * usage patterns.
 *
 * To build with ESP-IDF:
 *   idf.py set-target esp32
 *   idf.py build
 *   idf.py flash --port /dev/ttyUSB0
 */

#include <stdio.h>
#include <string.h>
#include "hive.h"

/* ---- Platform configuration ---- */

#define MQTT_HOST "192.168.1.10"
#define MQTT_PORT 1883
#define AGENT_ID  "temp-sensor-01"
#define TEAM_ID   "sensor-team"

/* ---- Mock sensor data ---- */

static int g_reading_count = 0;

/*
 * Mock temperature reading. On real hardware this would read from
 * a DHT22, BME280, DS18B20, or the ESP32's internal temperature sensor.
 */
static float read_temperature(void)
{
    /* simulate temperature between 20.0 and 25.0 C */
    float base = 22.5f;
    float variation = (float)(g_reading_count % 50) * 0.1f - 2.5f;
    g_reading_count++;
    return base + variation;
}

static float read_humidity(void)
{
    /* simulate humidity between 40% and 60% */
    float base = 50.0f;
    float variation = (float)(g_reading_count % 40) * 0.5f - 10.0f;
    return base + variation;
}

/* ---- Capability handler ---- */

/*
 * temperature-read capability handler.
 *
 * Input:  {"unit": "celsius"} (optional, defaults to celsius)
 * Output: {"temperature": 22.5, "humidity": 50.0, "unit": "celsius"}
 */
static int handle_temperature_read(const char *inputs_json,
                                    char *outputs_json,
                                    size_t max_output_len)
{
    (void)inputs_json; /* unit selection could be parsed here */

    float temp = read_temperature();
    float hum  = read_humidity();

    /*
     * Format the integer and fractional parts separately to avoid
     * pulling in floating-point printf on constrained platforms.
     */
    int temp_int  = (int)temp;
    int temp_frac = (int)((temp - (float)temp_int) * 10.0f);
    if (temp_frac < 0) temp_frac = -temp_frac;

    int hum_int  = (int)hum;
    int hum_frac = (int)((hum - (float)hum_int) * 10.0f);
    if (hum_frac < 0) hum_frac = -hum_frac;

    int n = snprintf(outputs_json, max_output_len,
        "{\"temperature\":%d.%d,"
        "\"humidity\":%d.%d,"
        "\"unit\":\"celsius\","
        "\"reading_number\":%d}",
        temp_int, temp_frac,
        hum_int, hum_frac,
        g_reading_count);

    if (n < 0 || (size_t)n >= max_output_len) {
        return HIVE_ERR_OVERFLOW;
    }

    return HIVE_OK;
}

/* ---- Example 1: Tool Mode (respond to invocations only) ---- */

static void run_tool_mode(void)
{
    printf("[hive] Starting in TOOL mode\n");

    hive_config_t config = {
        .agent_id             = AGENT_ID,
        .mqtt_host            = MQTT_HOST,
        .mqtt_port            = MQTT_PORT,
        .mqtt_user            = NULL,
        .mqtt_pass            = NULL,
        .heartbeat_interval_ms = 30000,
        .mode                 = HIVE_MODE_TOOL,
    };

    int rc = hive_init(&config);
    if (rc != HIVE_OK) {
        printf("[hive] Init failed: %d\n", rc);
        return;
    }

    rc = hive_register_capability("temperature-read", handle_temperature_read);
    if (rc != HIVE_OK) {
        printf("[hive] Capability registration failed: %d\n", rc);
        hive_destroy();
        return;
    }

    printf("[hive] Agent '%s' connected, capability registered\n", AGENT_ID);
    printf("[hive] Waiting for capability requests...\n");

    /* main loop -- runs forever in tool mode */
    while (1) {
        rc = hive_loop();
        if (rc == HIVE_ERR_CONNECT) {
            printf("[hive] Connection lost, attempting reconnect...\n");
            hive_destroy();

            /* simple retry with delay */
            /* platform_delay_ms(5000); */
            rc = hive_init(&config);
            if (rc == HIVE_OK) {
                hive_register_capability("temperature-read",
                                          handle_temperature_read);
                printf("[hive] Reconnected\n");
            }
        }

        /*
         * On ESP-IDF: vTaskDelay(pdMS_TO_TICKS(100));
         * On Arduino: delay(100);
         * On bare metal: your own delay function.
         */
    }
}

/* ---- Example 2: Peer Mode (can initiate messages) ---- */

static void run_peer_mode(void)
{
    printf("[hive] Starting in PEER mode\n");

    hive_config_t config = {
        .agent_id             = AGENT_ID,
        .mqtt_host            = MQTT_HOST,
        .mqtt_port            = MQTT_PORT,
        .mqtt_user            = NULL,
        .mqtt_pass            = NULL,
        .heartbeat_interval_ms = 30000,
        .mode                 = HIVE_MODE_PEER,
    };

    int rc = hive_init(&config);
    if (rc != HIVE_OK) {
        printf("[hive] Init failed: %d\n", rc);
        return;
    }

    /* register capability -- peer mode agents can also respond to requests */
    hive_register_capability("temperature-read", handle_temperature_read);

    printf("[hive] Agent '%s' connected in peer mode\n", AGENT_ID);

    int loop_count = 0;

    while (1) {
        rc = hive_loop();
        if (rc == HIVE_ERR_CONNECT) {
            printf("[hive] Connection lost\n");
            break;
        }

        /* every 60 seconds, broadcast a reading to the team */
        loop_count++;
        if (loop_count >= 600) { /* assuming ~100ms loop delay */
            loop_count = 0;

            float temp = read_temperature();
            int temp_int  = (int)temp;
            int temp_frac = (int)((temp - (float)temp_int) * 10.0f);
            if (temp_frac < 0) temp_frac = -temp_frac;

            char content[128];
            snprintf(content, sizeof(content),
                "{\"type\":\"sensor-reading\","
                "\"sensor\":\"temperature\","
                "\"value\":%d.%d,"
                "\"unit\":\"celsius\"}",
                temp_int, temp_frac);

            rc = hive_team_broadcast(TEAM_ID, content);
            if (rc == HIVE_OK) {
                printf("[hive] Broadcast sensor reading to team\n");
            }
        }

        /*
         * On ESP-IDF: vTaskDelay(pdMS_TO_TICKS(100));
         * On Arduino: delay(100);
         */
    }

    hive_destroy();
}

/* ---- Entry point ---- */

/*
 * On ESP-IDF this would be called from app_main().
 * The USE_PEER_MODE preprocessor define selects the operating mode.
 */
#ifndef USE_PEER_MODE
#define USE_PEER_MODE 0
#endif

void app_main(void)
{
    printf("[hive] ESP32 Sensor Agent starting\n");

    /*
     * Select operating mode at compile time with -DUSE_PEER_MODE=1.
     * Tool mode: responds to capability requests only.
     * Peer mode: can also initiate messages and broadcast to the team.
     */
#if USE_PEER_MODE
    run_peer_mode();
#else
    run_tool_mode();
    (void)run_peer_mode; /* suppress unused-function warning */
#endif
}
