# Hive Firmware C SDK -- CMake Build Configuration
#
# Usage with ESP-IDF:
#   Add this directory as a component in your ESP-IDF project:
#     components/hive/ -> symlink or copy of this sdk/firmware-c/ directory
#   ESP-IDF will pick it up automatically via its component system.
#
# Usage with CMake (standalone or Pico SDK):
#   add_subdirectory(path/to/sdk/firmware-c)
#   target_link_libraries(your_target PRIVATE hive_firmware)
#
# Usage with Zephyr:
#   Add to your west.yml or CMakeLists.txt as a Zephyr module.

cmake_minimum_required(VERSION 3.13)

# ---------------------------------------------------------------------------
# Detect build environment
# ---------------------------------------------------------------------------

if(DEFINED ENV{IDF_PATH} OR IDF_PATH)
    # --- ESP-IDF Component Mode ---
    # When building as an ESP-IDF component, use the IDF component API.
    idf_component_register(
        SRCS "src/hive.c"
        INCLUDE_DIRS "include"
    )

elseif(DEFINED CONFIG_ZEPHYR_MODULE)
    # --- Zephyr Module Mode ---
    zephyr_library_named(hive_firmware)
    zephyr_library_sources(src/hive.c)
    zephyr_include_directories(include)

else()
    # --- Standalone CMake (Pico SDK, generic ARM, host tests) ---
    project(hive_firmware C)

    add_library(hive_firmware STATIC
        src/hive.c
    )

    target_include_directories(hive_firmware PUBLIC
        ${CMAKE_CURRENT_SOURCE_DIR}/include
    )

    # C99 for designated initializers and mixed declarations
    target_compile_features(hive_firmware PUBLIC c_std_99)

    # Size optimizations suitable for embedded targets
    if(CMAKE_C_COMPILER_ID MATCHES "GNU|Clang")
        target_compile_options(hive_firmware PRIVATE
            -Wall
            -Wextra
            -Wpedantic
            -Wno-unused-parameter
            -Os
            -ffunction-sections
            -fdata-sections
        )
    endif()

    # --- Example: ESP32 Sensor ---
    # Only build examples when this is the top-level project
    if(CMAKE_SOURCE_DIR STREQUAL CMAKE_CURRENT_SOURCE_DIR)
        # Examples require a platform implementation (MQTT stubs, timing, etc.)
        # This target is for reference; it will not link without platform code.
        add_executable(esp32_sensor_example
            examples/esp32-sensor/main.c
        )
        target_link_libraries(esp32_sensor_example PRIVATE hive_firmware)
        set_target_properties(esp32_sensor_example PROPERTIES
            EXCLUDE_FROM_ALL TRUE
        )
    endif()
endif()
