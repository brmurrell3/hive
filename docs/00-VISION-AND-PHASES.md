HIVE: DECLARATIVE LLM AGENT TEAMS ACROSS HETEROGENEOUS HARDWARE
Version 3.0

vision
  one_liner: Hive is a declarative framework for building agent teams across heterogeneous hardware, from microcontrollers to GPU workstations.

  problem_statement:
    - Existing LLM frameworks assume homogeneous cloud infrastructure
    - Real deployments mix hardware tiers: laptops, RPis, microcontrollers, workstations
    - Manual wiring required for each device/agent pair
    - No standard discovery, provisioning, or lifecycle management across tiers

  solution:
    - Declare agents by capability, not runtime target
    - Declare hardware inventory and constraints
    - Hive provisions, communicates, discovers, manages lifecycle
    - Agents execute identically across tiers with appropriate isolation
    - NATS backbone enables cross-tier async messaging

  core_principles:
    hardware_diversity: Default assumption. Agents transparently run on appropriate tier.
    capability_driven: Agents defined by capabilities they expose, not execution environment.
    plug_and_play: Add hardware. Add agents. Declare. No boilerplate.
    nats_backbone: All inter-agent/inter-node communication via NATS.
    isolation_scales: VM > process > bare metal. Isolation matches capability tier.
    declarative_always: YAML agent specs, hardware inventory, team composition. Markdown for docs.

  non_goals:
    - NOT a general agent framework (orchestrates agents built with OpenClaw, LangChain, custom, firmware)
    - NOT a general container/VM orchestrator (focused on LLM agent lifecycle)
    - NOT tied to any LLM provider (bring your own Claude, Ollama, etc.)

  target_users:
    - Homelab enthusiasts with mixed hardware
    - Researchers with heterogeneous device pools
    - Hobbyists integrating LLMs with physical sensors
    - Small teams avoiding cloud dependency
    - Edge AI deployments with offline-first requirements

implementation_phases

  phase_1:
    name: Single-Node MVP
    duration_estimate: 6-8 weeks
    scope_summary: Single control plane node. VM-tier agents only. One team. MVP CLI.

    hardware:
      control_plane_tier: 1 (NixOS)
      agent_tiers_supported: [1]
      execution_targets: [firecracker_vms]
      scaling: Single node, multi-VM capacity

    agents:
      lead_agent: Required. Orchestrates team, receives user input, delegates.
      tool_agents: Optional. Tool-mode agents execute single capabilities.
      peer_agents: Not yet supported.

    communication:
      backbone: NATS embedded in hived daemon (NOT external nats.service)
      resolves_issue: "#6 - NATS embedded, no external dependency Phase 1"
      transport: In-process (localhost), NATS subjects for inter-agent routing
      discovery: Single node, static NATS subject registration

    runtimes:
      openclaw: Full support. Python-native OpenClaw agents in VMs.
      custom: entrypoint.sh wrapper. Agents define init_command + listen_port.
      future: LangChain, LlamaIndex agents in later phases.

    cli_commands:
      init: Create team, initialize local cluster state
      status: Cluster health, agent states, NATS connectivity
      validate: Spec syntax, capability routing, YAML schema
      agents_list: Running agents, state, resource usage
      agents_logs: Stream or tail agent logs
      agents_restart: Soft restart (SIGTERM + spawn)
      agents_stop: Graceful shutdown
      agents_start: Resume agent
      agents_destroy: Kill + remove state
      teams_list: Teams in cluster
      teams_status: Team-level aggregated health

    features:
      capability_routing: Agents declare capabilities. Lead agent routes requests within team.
      hot_reload: Config change detected, agent restarted, state preserved in NATS.
      cold_reload: Full cluster state reset.
      health_checks: Agent liveness via NATS heartbeat, restart policy (always/on-failure/never).
      scheduler: Single-node VM scheduler, CPU/memory constraints, bin-packing.
      resource_limits: CPU cores, RAM, disk per agent (Firecracker cgroup limits).
      log_collection: Agent stdout/stderr captured, available via CLI or WebSocket (Phase 4).
      isolation: Each VM isolated via Firecracker, separate namespace/cgroups.

    specification_format:
      team_spec: YAML. Agents array, capabilities, routing rules.
      agent_spec: YAML. Name, runtime, entrypoint, capabilities (name, input schema, output schema), constraints.
      hardware_spec: Not yet used. Placeholder for Phase 2.
      documentation: Markdown embedded in specs, extracted for Phase 4 dashboard.

    deliverables:
      - hived daemon (Go/Rust, NATS-embedded)
      - hivectl CLI (Go/Rust)
      - team.yaml schema and validator
      - agent.yaml schema and validator
      - Example team: simple lead + 2 tools
      - NixOS flake for reproducible dev environment
      - Phase 2 architecture doc (hardware tiers, NATS bridge design)

    success_criteria:
      - Single team with lead + tool agents runs and communicates
      - Agent restart and health checks work
      - hivectl can list, start, stop, logs agents
      - Firecracker VM overhead < 100ms coldstart, < 50MB baseline
      - Config hot reload preserves state

  phase_2:
    name: Multi-Tier Hardware Support
    duration_estimate: 8-10 weeks
    scope_summary: Tier 2 native agents. Tier 3 firmware. Cross-tier capability invocation. Pre-built device images.

    hardware:
      control_plane_tier: 1 (NixOS)
      agent_tiers_supported: [1, 2, 3]
      execution_targets: [firecracker_vms, native_binaries, firmware_esp32, firmware_rpi_pico]
      tier_2_devices: [x86_laptop, rpi_4, rpi_5, beaglebone, jetson_nano]
      tier_3_devices: [esp32, esp8266, rpi_pico, stm32, arduino]

    agents:
      tier_1_agents: VMs (unchanged from Phase 1)
      tier_2_agents: Native hive-agent binary (Go/Rust). Run on bare metal or RPi. Single process per agent.
      tier_3_agents: Firmware agents (C SDK or MicroPython SDK). Run on microcontrollers. Peer or tool mode only.
      agent_modes: lead (T1 only) | tool (all tiers) | peer (T2, T3, request-response pairs)

    communication:
      backbone: NATS still embedded in T1 hived. MQTT bridge on T1 relays to T3 devices (IP or serial).
      discovery: Node join protocol. T2 devices detect T1 via mDNS or explicit URL. Register with join token.
      resolves_issue: "#6 continuation - NATS embedded in T1, MQTT bridge for T3"
      heartbeat: All agents send liveness to NATS. T1 monitors cluster state.

    runtimes:
      openclaw: T1 VMs only (unchanged)
      hive_native_agent: T2 agents. Compiled Go/Rust binary. Listens on HTTP + NATS subject.
      c_sdk: T3 agents. Thin wrapper around TCP/serial to MQTT bridge. Single capability.
      micropython_sdk: T3 agents. MicroPython library wrapping device logic. MQTT publish/subscribe.
      custom: T2 agents can use custom wrappers (entrypoint.sh style).

    hardware_inventory:
      spec_format: hardware.yaml. Global inventory file.
      fields_per_device: name, tier, device_type, ip_address, capabilities_available, constraints (CPU, RAM, power).
      discovery_automation: T2 devices self-register on join. T1 stores inventory in local SQLite.
      resolves_issue: "#7 - Hardware inventory collection and querying"

    provisioning:
      pre_built_images:
        rpi_3: Raspbian-based image with hive-agent, systemd unit, auto-join script
        rpi_4: Same
        rpi_5: Same
        rpi_zero2w: Minimal image, hive-agent binary
      flash_mechanism: hivectl firmware flash <image> <device> <join_token>. Writes SD card or UART.
      ota_updates: hivectl firmware update <agent_name>. Pull new binary from T1, restart.

    cross_tier_capabilities:
      invocation: Lead agent requests "take-photo" from camera-agent on T2. NATS routes transparently.
      schema_validation: Input/output schemas enforced at capability definition. Hive serializes JSON over NATS.
      timeout: Configurable per capability. T3 sensor reads may be slow.
      retry_policy: Exponential backoff. Fail-open for non-critical capabilities.

    join_token_management:
      generation: hivectl create token --tier 2 --device rpi_4. Returns token string.
      verification: T2 agent presents token to T1. T1 verifies against pre-generated list. Single-use.
      revocation: hivectl revoke token <token_id>.
      ttl: Default 24h. Configurable.

    cli_commands_new:
      firmware_build: hivectl firmware build --sdk=c --device=esp32 --source=<path>
      firmware_flash: hivectl firmware flash <image> --port=/dev/ttyUSB0
      firmware_update: hivectl firmware update <agent_name>
      hardware_list: List all discovered hardware. Tiers, device types, IPs.
      hardware_inventory: Dump full inventory (JSON or YAML).
      join_token_create: Generate join token.
      join_token_revoke: Revoke token.
      agent_pair: Pair a discovered T2 device as new agent in team.

    deliverables:
      - hive-agent binary (Go/Rust, runs on T2 devices)
      - C SDK for T3 agents (simple MQTT wrapper)
      - MicroPython SDK for T3 agents (MicroPython-compatible library)
      - MQTT bridge daemon (runs on T1, relays to T3 over IP or serial)
      - Pre-built RPi images (3, 4, 5, Zero2W)
      - hardware.yaml schema and auto-discovery logic
      - Join token lifecycle management
      - Cross-tier capability invocation spec
      - Example teams: RPi camera + ESP32 sensors + VM orchestrator

    success_criteria:
      - T2 native agent on RPi joins cluster and executes capabilities
      - T3 firmware agent (ESP32) read-sensor capability invoked from T1 lead agent
      - Cross-tier routing latency < 200ms
      - Pre-built RPi image boots, joins, and registers capabilities in < 2 min
      - OTA firmware update works end-to-end

  phase_3:
    name: Multi-Node Cluster and Organization
    duration_estimate: 10-12 weeks
    scope_summary: Multiple T1 nodes form cluster. NATS clustering. Org-level multi-team. Cluster root sync.

    hardware:
      control_plane_tier: Multiple T1 nodes. One is authoritative root. Others are workers.
      cluster_topology: Star topology. Root is authoritative. Workers replica read-only cluster state.
      resolves_issue: "#5 - Cluster root sync strategy"

    cluster_state:
      authority_model: Root node is single source of truth for team, agent, hardware specs.
      replication: Root publishes state changes via NATS cluster topic. Workers subscribe and cache locally.
      conflict_resolution: Root always wins. Workers reject local modifications (error).
      persistence: Root uses SQLite. Workers use in-memory + NATS subscription.

    multi_node_nats:
      transition: Phase 1-2 embedded NATS -> Phase 3 external NATS cluster.
      resolves_issue: "#6 - NATS transitions from embedded to external for clustering"
      nats_cluster_setup: 3+ NATS nodes with routes. Root hived connects as client. Worker hiveds connect as clients.
      subject_hierarchy: hive.org.team.agent.capability. Cross-team subjects: hive.org.cross_team.*

    scheduler:
      multi_node_vm_scheduling: Agents with VM targets distributed across T1 nodes.
      constraints: Resource requests (CPU, RAM) matched against node availability.
      bin_packing: Greedy first-fit or best-fit strategy.
      affinity_rules: Co-locate agents on same node or spread.
      node_drain: hivectl node drain <node_id>. Migrate VMs off node for maintenance.
      node_cordon: hivectl node cordon <node_id>. Mark unhealthy, no new VMs scheduled.

    multi_team:
      org_structure: Organization contains multiple teams. Each team has separate agent namespace.
      cross_team_communication: Teams invoke capabilities across boundaries via cross_team subject.
      capability_exposure: Team can explicitly expose a capability for org-level use.
      routing: Cross-team capability request routed through root node NATS.

    director_agent:
      purpose: Optional. Org-level coordinator. Manages multi-team workflows.
      scope: Runs on root T1 node. Single instance.
      capabilities: Discover all teams, invoke cross-team capabilities, aggregate results.
      use_case: Multi-team data pipelines, complex workflows.

    access_control:
      user_roles: admin (all nodes), team_lead (own team), guest (read-only).
      authentication: mTLS between nodes. JWT tokens for CLI access.
      authorization: RBAC enforced by root node. Workers trust root decisions.
      team_isolation: Users assigned to teams. Cannot see other team agents/state.

    node_discovery:
      mechanism: mDNS or explicit seed list. New T1 node announces. Root accepts or rejects.
      join_flow: New node sends join request. Root verifies identity (cert). New node becomes worker.
      seed_list: Static list of known root IPs, used if mDNS unavailable.

    cli_commands_new:
      node_list: List all T1 nodes in cluster. Status, role (root/worker), uptime.
      node_drain: Drain VMs from node.
      node_cordon: Mark node unhealthy.
      node_promote: Promote worker to root (dangerous, requires confirmation).
      cluster_status: Cluster health, NATS connectivity, replication lag.
      team_create: Create new team in org.
      team_list: List teams.
      team_switch: Set active team for CLI commands.
      capability_expose: Expose team capability to org level.
      director_invoke: Invoke cross-team capability via director.

    deliverables:
      - Multi-node Hive cluster architecture doc
      - NATS cluster setup guide (3+ node minimal)
      - Root/worker node sync logic (SQLite + NATS subscription)
      - Multi-team agent namespace and routing
      - Org-level cross-team capability spec
      - Director agent template
      - Node drain and cordon implementation
      - mTLS cert generation and rotation
      - JWT token lifecycle management
      - RBAC policy engine
      - Example: 2 root + 3 worker nodes, 4 teams, cross-team capability invocation

    success_criteria:
      - 3 T1 nodes form cluster with root and 2 workers
      - Agent created on root, VM scheduled on worker node
      - Cross-team capability invocation works end-to-end
      - Node drain triggers VM migration to healthy node
      - Replication lag < 500ms under normal load
      - CLI correctly enforces user RBAC

  phase_4:
    name: Observability and UX
    duration_estimate: 6-8 weeks
    scope_summary: Web dashboard. Log collection and rotation. Metrics. Message flow viz. Chat interface.

    web_dashboard:
      architecture: React SPA. REST/WebSocket API from hived (or root hived in multi-node).
      components:
        cluster_view: Node list, health, resource usage (CPU, RAM, disk)
        team_view: Teams, agents, capabilities, status
        agent_details: Agent logs (live stream), metrics, restart history, resource limits
        capability_browser: Searchable list of all org capabilities. Input/output schemas.
        message_flow: Visualization of inter-agent messages (DAG). Timestamp, latency.
        settings: Access control, NATS config, backup/restore
      connectivity: WebSocket for live updates. REST for queries. Auth via JWT.

    log_collection:
      resolves_issue: "#15 - Log collection, storage, rotation"
      agent_logs: Captured from agent stdout/stderr. Streamed to T1 log collector.
      log_storage: SQLite (small deployments) or external LTSV/JSON files (rotated daily).
      retention_policy: 30 days default. Configurable per team/agent.
      rotation: Daily or size-based (100MB). Old logs compressed and archived.
      api: hivectl logs <agent_name> --tail=100 --follow. Dashboard streams live.

    metrics:
      collection: Prometheus-compatible /metrics endpoint from each hived.
      scraped_by: Prometheus (optional, not bundled). Grafana for long-term dashboards.
      metrics_types:
        agent_state: running, stopped, restarting, failed (gauge)
        api_latency: capability invocation latency (histogram)
        nats_messages: message count per subject (counter)
        vm_resources: CPU%, RAM%, disk% per VM (gauge)
        cluster_replication_lag: time since root publish (gauge for multi-node)
      retention: In-process ringbuffer for 24h (local metrics). Prometheus for long-term.

    message_flow_visualization:
      purpose: Understand agent interactions and bottlenecks.
      implementation: Trace messages through NATS. Record sender, receiver, timestamp, latency.
      ui: DAG-style timeline. Click on message to see payload (if not binary).
      filtering: By agent, capability, time range.
      export: PNG/SVG export for debugging.

    click_to_chat:
      purpose: QA and testing. Request capability directly from dashboard.
      flow: Select agent/capability. Input JSON in form. Send. Results in modal.
      use_case: Test agent integration, debug capability output.
      auth: User must have team_lead or higher role.

    web_api:
      rest_endpoints:
        /api/cluster/status: Cluster health, node list
        /api/teams: List teams
        /api/teams/{id}/agents: Agents in team
        /api/agents/{id}/logs: Agent logs (paginated, queryable)
        /api/agents/{id}/metrics: Agent metrics
        /api/capabilities: Org-wide capability catalog
        /api/capabilities/{id}/invoke: Invoke capability (POST with input JSON)
      websocket: /ws/cluster, /ws/agent/{id}/logs, /ws/messages (message flow)

    ui_features:
      search: Search agents, capabilities, teams across org
      favorites: Pin frequently-used agents/capabilities
      alerts: Agent state change, high resource usage, NATS connectivity loss
      dark_mode: Toggle
      export: Download logs, metrics, DAG as JSON/CSV

    cli_enhancements:
      logs_query: hivectl logs <agent_name> --tail=<n> --follow --grep=<pattern>
      metrics_export: hivectl metrics export --format=prometheus <filename>
      watch: hivectl watch agents (live agent state table)

    deliverables:
      - Web dashboard (React + TypeScript)
      - REST API server (integrated into hived)
      - WebSocket handlers (logs, metrics, message flow)
      - Prometheus metrics exporter
      - Log rotation and compression logic
      - Message flow tracer (NATS interception)
      - Capability browser and invocation UI
      - Example dashboards (Grafana JSON)
      - Documentation: API spec (OpenAPI), dashboard user guide

    success_criteria:
      - Dashboard displays 10+ node cluster with 50+ agents in real-time
      - Log streaming latency < 100ms
      - Message flow DAG renders in < 1s for 100-message flow
      - Capability invocation via UI works with schema validation
      - Log retention policy correctly expires old logs

example_deployments

  deployment_1:
    name: Smart Greenhouse Monitor
    scale: Single node (homelab)
    team_name: greenhouse-monitor

    hardware:
      control_plane: 1 MacBook Air (Tier 1)
      camera_node: 1 RPi4 (Tier 2)
      sensors: 3 ESP32 (Tier 3 sensors)
      actuator: 1 ESP32 (Tier 3 relay)

    agents:
      greenhouse_brain:
        name: greenhouse-brain
        tier: 1
        runtime: openclaw
        base_image: claude-openclaw-v1
        capabilities:
          - analyze_environment: Receive sensor readings, decide on irrigation
          - capture_image: Request photo from camera agent
        constraints:
          cpu: 2
          ram_mb: 1024

      greenhouse_camera:
        name: greenhouse-camera
        tier: 2
        runtime: hive_native_agent
        device: rpi4_front
        capabilities:
          - capture_image:
              input_schema: {exposure_ms: int, resolution: string}
              output_schema: {image_base64: string, timestamp: int}
              timeout_sec: 5
        constraints:
          cpu: 1
          ram_mb: 256

      zone_1_sensor:
        name: zone-1-sensor
        tier: 3
        runtime: micropython_sdk
        device: esp32_zone1
        mode: peer
        capabilities:
          - read_temperature:
              output_schema: {celsius: float}
              timeout_sec: 2
          - read_humidity:
              output_schema: {percent: float}
              timeout_sec: 2

      zone_2_sensor:
        name: zone-2-sensor
        tier: 3
        runtime: micropython_sdk
        device: esp32_zone2
        mode: peer
        capabilities:
          - read_temperature:
              output_schema: {celsius: float}
          - read_humidity:
              output_schema: {percent: float}

      zone_3_sensor:
        name: zone-3-sensor
        tier: 3
        runtime: micropython_sdk
        device: esp32_zone3
        mode: peer
        capabilities:
          - read_temperature:
              output_schema: {celsius: float}
          - read_humidity:
              output_schema: {percent: float}

      irrigation_controller:
        name: irrigation-controller
        tier: 3
        runtime: c_sdk
        device: esp32_relay
        mode: tool
        capabilities:
          - activate_irrigation:
              input_schema: {duration_sec: int}
              output_schema: {success: bool}
          - get_valve_status:
              output_schema: {open: bool, water_flow_lpm: float}

    team_spec:
      name: greenhouse-monitor
      lead_agent: greenhouse-brain
      agents: [greenhouse-brain, greenhouse-camera, zone-1-sensor, zone-2-sensor, zone-3-sensor, irrigation-controller]
      routing_rules:
        - source: greenhouse-brain
          capability: analyze_environment
          target: zone_1_sensor.read_temperature, zone_1_sensor.read_humidity, zone_2_sensor.read_temperature, zone_2_sensor.read_humidity, zone_3_sensor.read_temperature, zone_3_sensor.read_humidity
        - source: greenhouse-brain
          capability: capture_image
          target: greenhouse-camera.capture_image
        - source: greenhouse-brain
          capability: irrigation_decision
          target: irrigation_controller.activate_irrigation

    workflow_example:
      step_1: User: "How is the greenhouse doing?"
      step_2: greenhouse-brain invokes read_temperature and read_humidity on zone_1/2/3
      step_3: greenhouse-brain captures image from greenhouse-camera
      step_4: greenhouse-brain analyzes sensor data and image
      step_5: If too dry, greenhouse-brain activates irrigation via irrigation-controller
      step_6: greenhouse-brain returns status to user

  deployment_2:
    name: Quant Research Lab
    scale: Multi-node, multi-team

    hardware:
      cluster_nodes:
        workstation_1: Tier 1 (root, 16-core, 64GB RAM)
        workstation_2: Tier 1 (worker, 16-core, 64GB RAM)
      external_services:
        ollama_local: On workstation_1, accessible to all agents
        anthropic_api: Cloud-based Claude access

    teams:
      momentum_research:
        agents: 4 VM agents (OpenClaw)
          - market_data_fetcher: Pulls OHLC data from APIs
          - momentum_analyzer: Identifies momentum signals
          - risk_evaluator: Computes position sizing and drawdown risks
          - report_generator: Summarizes findings for report
        shared_volumes:
          market_data:
            path: /data/market_data
            mode: read-only
            access: All agents in team
          momentum_results:
            path: /data/momentum_results
            mode: read-write
            access: momentum_analyzer and report_generator

      risk_analysis:
        agents: 2 VM agents (OpenClaw)
          - portfolio_optimizer: VaR, Sharpe ratio, correlation analysis
          - hedge_simulator: Backtest hedge strategies
        shared_volumes:
          market_data:
            path: /data/market_data
            mode: read-only
          hedge_results:
            path: /data/hedge_results
            mode: read-write

    cross_team_capabilities:
      exposure_rule_1:
        team: momentum_research
        capability: momentum_analyzer.get_signals
        exposed_to: risk_analysis (risk_evaluator may call to retrieve signals)
      exposure_rule_2:
        team: risk_analysis
        capability: portfolio_optimizer.compute_var
        exposed_to: momentum_research

    director_agent:
      enabled: true
      orchestrates:
        - Fetch market data once, route to both teams
        - Momentum team produces signals
        - Risk team evaluates signals
        - Merge results into final portfolio recommendation

    cluster_setup:
      nats_cluster: 3 NATS nodes (HA setup)
      root_node: workstation_1
      worker_node: workstation_2
      communication: All inter-team requests routed through root NATS

    workflow_example:
      trigger: Morning market open
      step_1: Director invokes market_data_fetcher.fetch (momentum_research)
      step_2: market_data_fetcher writes to /data/market_data
      step_3: momentum_analyzer reads market_data, computes signals
      step_4: Director retrieves signals via cross-team capability
      step_5: Director invokes portfolio_optimizer.compute_var (risk_analysis) with signal data
      step_6: risk_analysis team outputs hedge recommendations
      step_7: report_generator merges momentum + risk results
      step_8: Final report written to /data/momentum_results/report.json

  deployment_3:
    name: Hybrid Greenhouse + Quant Lab
    scale: Combined single cluster
    description: Merges deployments 1 and 2. Separate teams. One cluster.

    cluster:
      nodes: 2 Tier 1 (1 root + 1 worker)
      teams:
        - greenhouse-monitor (single node, homelab-style)
        - momentum_research (multi-node, research-grade)
        - risk_analysis (multi-node, research-grade)

    team_isolation:
      each team operates independently
      NATS subject hierarchy enforces isolation
      users assigned to teams, cannot see other team agents

    resource_constraints:
      root_node: 4 cores reserved for management, remainder for VM scheduling
      momentum_research: 8 cores, 32GB RAM
      risk_analysis: 4 cores, 16GB RAM
      greenhouse_monitor: 1 core, 2GB RAM

    cross_cluster_features:
      director: Optional. If enabled, orchestrates across teams.
      shared_data: Market data volume could be exposed to greenhouse team (for future ML features)
      monitoring: Single dashboard views all teams. Users filtered by role/team.

    scheduling:
      momentum and risk agents scheduled on worker node (high CPU)
      greenhouse agents can run on root or worker (low CPU)
      constraint: market data must be local to workstation cluster for low latency

notes_for_implementation
  phase_1_blocking_issues:
    "#6": "NATS embedded in hived (NOT external service for Phase 1). Multi-node external NATS is Phase 3."
    "#5": "Not applicable Phase 1. Single node. Addressed in Phase 3 cluster sync."
    "#15": "Log collection basic in Phase 1 (agent stdout). Full log rotation in Phase 4."

  phase_2_blocking_issues:
    "#6": "MQTT bridge for Tier 3 devices. NATS embedded unchanged."
    "#7": "Hardware inventory collection and SQLite schema."

  phase_3_blocking_issues:
    "#5": "Cluster root as authoritative source. Workers sync via NATS cluster."
    "#6": "Transition embedded NATS to external NATS cluster (3+ nodes)."

  phase_4_blocking_issues:
    "#15": "Full log rotation, compression, long-term storage."

  testing_strategy:
    unit: Agent routing, spec validation, capability schema matching
    integration: Single-node team with multiple agents, cross-tier invocation
    e2e: Full deployment examples, multi-node cluster, director orchestration
    performance: Agent startup latency, message throughput, replication lag
    chaos: Node failure, NATS partition, VM OOM, network latency injection

  documentation_required:
    spec_docs: Agent YAML schema, team YAML schema, hardware YAML schema, team spec format
    api_docs: NATS subject hierarchy, REST API (Phase 4), WebSocket API (Phase 4)
    examples: Per-phase example teams (greenhouse, quant lab, hybrid)
    operator_guide: Cluster setup, multi-node, backup/restore, monitoring
    developer_guide: Writing agents (OpenClaw, custom, firmware), SDKs, testing
    architecture: Component diagram, message flow, state sync, cross-tier communication
