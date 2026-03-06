# Pre-Hardware Validation Report

Generated: 2026-03-04
Build status: All packages compile, `go vet` clean, 22/22 test suites pass with `-race`.

This document catalogs every issue found during deep code review. Each issue has clear acceptance criteria so fixes can be verified independently. Issues are ordered by priority — Tier 1 issues will cause immediate failures on real hardware and must be fixed first.

---

## Tier 1: Hardware Blockers

These will cause immediate failures when running on real Firecracker/KVM hardware.

### T1-01: VMPID Never Set — VMs Cannot Be Stopped or Destroyed

**Location:** `internal/vm/manager.go` (StartAgent), `internal/vm/manager.go:19-36` (Hypervisor interface)

**Problem:** The `Hypervisor` interface's `CreateVM` and `StartVM` methods return only `error`. Neither returns the Firecracker process PID. The `Manager.StartAgent` method never populates `agentState.VMPID`. When `StopAgent` or `DestroyAgent` is called, it passes `VMPID=0` to the hypervisor. `FirecrackerHypervisor.StopVM` rejects PID 0 with an error. `DestroyVM` skips the kill entirely (checks `pid > 0`). Result: VMs become orphan processes that can never be stopped. The `MockHypervisor` hides this because it ignores the PID parameter and keys on socket path instead.

**Acceptance criteria:**
- [ ] `Hypervisor.CreateVM` or `Hypervisor.StartVM` returns the process PID (interface signature change)
- [ ] `MockHypervisor` updated to conform to new interface, returns a synthetic PID
- [ ] `Manager.StartAgent` stores the returned PID in `agentState.VMPID` before calling `store.SetAgent`
- [ ] `agentState.VMPID` is non-zero after a successful `StartAgent` call
- [ ] `StopAgent` and `DestroyAgent` pass the correct PID to the hypervisor
- [ ] Test `TestStartAgent_Success` asserts `VMPID != 0` after start
- [ ] Test `TestStopAgent` verifies the correct PID is passed to `StopVM`

---

### T1-02: Agent Drive Path Is a Directory, Not a Block Device Image

**Location:** `internal/vm/firecracker.go:133-146`

**Problem:** Firecracker's `/drives/` API requires a path to a block device image (ext4 file). The code passes `cfg.AgentDir` which is `clusterRoot/agents/<agentID>/` — a filesystem directory. Firecracker will reject this. Agent-specific files (AGENTS.md, SOUL.md, skills/) will never be available inside the VM.

**Acceptance criteria:**
- [ ] A function exists to create an ext4 disk image from an agent's directory contents
- [ ] `Manager.StartAgent` calls this function to produce a `.img` file before `CreateVM`
- [ ] The `.img` file path (not directory path) is passed to Firecracker's drive API
- [ ] The `.img` file is cleaned up on `DestroyAgent`
- [ ] Test verifies the agent drive path ends in `.img` (or other image extension), not a directory
- [ ] Inside the VM, the sidecar can mount the drive and read agent files (manual hardware verification)

---

### T1-03: Zombie Processes After Failed VM Creation

**Location:** `internal/vm/firecracker.go:75-176`

**Problem:** When `CreateVM` spawns a Firecracker process (`cmd.Start()`) and a subsequent API call fails (boot-source, rootfs, machine-config, vsock), the code calls `cmd.Process.Kill()` but never `cmd.Wait()`. Without `Wait()`, the killed process remains as a zombie in the process table. Same issue when `waitForSocket` times out.

**Acceptance criteria:**
- [ ] Every code path that calls `cmd.Process.Kill()` in `CreateVM` is followed by `cmd.Wait()`
- [ ] The `waitForSocket` timeout path also calls `cmd.Wait()` after kill
- [ ] No zombie processes accumulate after repeated failed VM creations (manual verification or test with process table inspection)

---

### T1-04: CID Reuse After Daemon Restart

**Location:** `internal/vm/manager.go:63-81` (NewManager), `internal/vm/manager.go` (ReconcileOnStartup)

**Problem:** `nextCID` always starts at 3 when a new `Manager` is created. If hived restarts while VMs are still running (or their CIDs are allocated in the kernel), the manager will allocate CIDs 3, 4, 5... again, colliding with existing VMs. `ReconcileOnStartup` does not update `nextCID` from existing agent states.

**Acceptance criteria:**
- [ ] `ReconcileOnStartup` (or `NewManager`) scans all existing agent states for their `VMCID` values
- [ ] `nextCID` is set to `max(existing CIDs) + 1` (minimum 3)
- [ ] Test: create a manager, simulate agents with CIDs 3-7 in state, verify next allocation is CID 8
- [ ] Test: empty state still starts at CID 3

---

### T1-05: Rootfs Copies Leaked on VM Creation Failure

**Location:** `internal/vm/manager.go:140-178`

**Problem:** A rootfs copy is created at line 140 (`copyFile`). If `CreateVM` fails (line 159) or `StartVM` fails (line 177), the rootfs copy remains on disk. `StartAgent` creates a fresh `AgentState` without the old `RootfsCopyPath`, so the orphaned copy can never be cleaned up. Repeated failures will fill the disk.

**Acceptance criteria:**
- [ ] If `CreateVM` fails after rootfs copy, the copy is removed before returning the error
- [ ] If `StartVM` fails after rootfs copy, the copy is removed before returning the error
- [ ] CID is also freed on these failure paths (currently leaked — see also T1-04)
- [ ] Test: simulate CreateVM failure, verify rootfs copy does not exist on disk after StartAgent returns

---

### T1-06: Sidecar Has No NATS Connection Retry on Startup

**Location:** `internal/sidecar/sidecar.go:128-131`

**Problem:** `Start()` calls `connectNATS()` which fails immediately if NATS is unreachable. There is no retry loop. On real hardware, the sidecar starts inside the VM potentially before hived's embedded NATS server is ready (boot race condition). A failed start means the VM is permanently useless since the sidecar runs as PID 1 or init-launched.

**Acceptance criteria:**
- [ ] `connectNATS` retries with exponential backoff (e.g., 1s, 2s, 4s... up to 30s)
- [ ] A maximum retry count or total timeout is configurable (default: retry for at least 60s)
- [ ] Each retry attempt is logged at warn level
- [ ] If all retries are exhausted, the error is returned with context
- [ ] Test: mock a NATS server that becomes available after N connection attempts, verify sidecar connects

---

### T1-07: Sidecar HTTP Server Bind Failure Is Silently Swallowed

**Location:** `internal/sidecar/health.go:32-58`

**Problem:** `startHTTPServer` launches `ListenAndServe` in a goroutine and immediately returns `nil`. If port 9100 is already in use (e.g., node_exporter uses 9100), the error is only logged inside the goroutine. The caller thinks the server started successfully.

**Acceptance criteria:**
- [ ] `startHTTPServer` verifies the listener actually binds before returning (e.g., use `net.Listen` first, then pass the listener to `http.Serve`)
- [ ] If binding fails, `startHTTPServer` returns the error
- [ ] `Sidecar.Start()` propagates this error and fails the startup
- [ ] Test: attempt to start HTTP on an occupied port, verify `Start()` returns an error

---

### T1-08: MQTT Bridge Assumes One Packet Per TCP Read

**Location:** `internal/mqtt/bridge.go:161-183`

**Problem:** The read loop does `conn.Read(buf)` and passes the result to `handlePacket`, assuming exactly one complete MQTT packet per read. TCP is a stream protocol — reads can return partial packets, multiple concatenated packets, or packets split across reads. This will cause parse failures with real MQTT devices, especially with payloads approaching 4096 bytes.

**Acceptance criteria:**
- [ ] MQTT packet reading uses a proper framing layer: read the fixed header to determine remaining length, then read exactly that many more bytes
- [ ] Handles partial reads (buffering until a complete packet is assembled)
- [ ] Handles multiple packets in a single read
- [ ] Test: send two small MQTT packets concatenated in a single TCP write, verify both are processed
- [ ] Test: send a packet larger than 4096 bytes, verify it is received completely

---

### T1-09: MQTT PUBACK Packet ID Calculation Is Wrong

**Location:** `internal/mqtt/bridge.go:354-358`

**Problem:** The QoS 1 PUBACK requires the packet identifier from the PUBLISH message. The code tries to reverse-compute the position from the final `offset` value using arithmetic that produces wrong results for most packet sizes. The packet ID should be saved at the point it is read, before `offset` is advanced past it.

**Acceptance criteria:**
- [ ] The packet ID is captured into a local variable immediately when read (before advancing offset)
- [ ] PUBACK uses this captured packet ID
- [ ] Test: send a QoS 1 PUBLISH with a known packet ID, verify the PUBACK contains the same packet ID
- [ ] Test with varying topic name lengths and payload sizes to exercise different offset calculations

---

### T1-10: MQTT NATS Subscriptions Leak on Client Disconnect

**Location:** `internal/mqtt/bridge.go:433-437`

**Problem:** `subscribeForClient` discards the `*nats.Subscription` return value. When an MQTT client disconnects, its NATS subscriptions remain active forever. The subscription handler holds a reference to the dead `Client` and attempts to write to a closed TCP connection, causing errors. Over time, this leaks subscriptions, goroutines, and memory.

**Acceptance criteria:**
- [ ] NATS subscriptions created for a client are stored (on the `Client` struct or a map)
- [ ] When a client disconnects, all its NATS subscriptions are unsubscribed
- [ ] Test: connect an MQTT client, subscribe to a topic, disconnect, verify NATS subscription count returns to zero
- [ ] No errors logged from attempting to write to disconnected clients

---

### T1-11: processExists Returns False on EPERM (PID Reuse Detection)

**Location:** `internal/production/process.go:11-19`

**Problem:** `processExists` sends `Signal(0)` to check if a process is alive. If the PID has been reused by a process owned by a different user (e.g., root), `Signal(0)` returns `EPERM` (permission denied), which means the process exists but `processExists` returns `false`. During crash recovery, this causes hived to think the agent's VM is dead and start a duplicate, even though the original is still running.

**Acceptance criteria:**
- [ ] `processExists` returns `true` when `Signal(0)` returns `EPERM` (process exists, just can't signal it)
- [ ] `processExists` returns `true` when `Signal(0)` returns `nil` (process exists and we can signal it)
- [ ] `processExists` returns `false` only when `Signal(0)` returns `ESRCH` or other "no such process" errors
- [ ] Test: verify the function handles `EPERM` correctly (may require a test helper or mock)

---

## Tier 2: Correctness Bugs

Wrong behavior that won't crash but will cause incorrect system state or confuse operators.

### T2-01: Health Monitor Never Triggers Restart or Status Change

**Location:** `internal/health/monitor.go:160-171`

**Problem:** When an agent's heartbeat times out, `markUnhealthy` sets `agent.Error` to a message string but leaves `agent.Status` as `AgentStatusRunning`. The health monitor has no integration with the `RestartManager`. The agent remains "running" in state despite being unresponsive. The `RestartManager` exists but is never invoked by the monitor.

**Acceptance criteria:**
- [ ] `Monitor` accepts a `RestartManager` (or a callback) during construction
- [ ] When `markUnhealthy` detects a timed-out agent, it either: (a) changes status to a distinct unhealthy state, or (b) invokes the `RestartManager.HandleUnhealthy`
- [ ] The restart policy from the agent manifest is respected (always/on-failure/never)
- [ ] Test: simulate heartbeat timeout, verify `HandleUnhealthy` is called or status transitions from RUNNING

---

### T2-02: State Store Has No Atomic Read-Modify-Write API

**Location:** `internal/state/store.go` (all callers do Get→Modify→Set pattern)

**Problem:** Every caller does `agent := store.GetAgent(id)` (returns a copy, releases lock), modifies the copy, then `store.SetAgent(agent)` (acquires lock, overwrites). Between Get and Set, another goroutine can modify the same agent, and those changes are silently overwritten. This affects `vm/manager.go`, `health/restart.go`, `health/monitor.go`, and `production/hardening.go`.

**Acceptance criteria:**
- [ ] Store provides a `ModifyAgent(id string, fn func(*AgentState) error) error` method that holds the lock across the read-modify-write
- [ ] The callback receives the current state, mutates it, and the store persists atomically
- [ ] Key callers (VM Manager, RestartManager, Health Monitor) are migrated to use `ModifyAgent`
- [ ] Test: concurrent `ModifyAgent` calls on the same agent, verify no lost updates with `-race`

---

### T2-03: ValidateToken Mutates But Doesn't Persist, Returns Direct Pointer

**Location:** `internal/state/store.go:339-351`

**Problem:** Two issues: (a) `t.LastUsed = time.Now()` mutates the token in memory but never calls `saveLocked()`, so the timestamp is lost on restart. (b) Returns `t` directly — a pointer into the store's internal slice. The caller can corrupt state by mutating the returned token.

**Acceptance criteria:**
- [ ] `ValidateToken` calls `saveLocked()` after updating `LastUsed`
- [ ] `ValidateToken` returns a copy of the token, not a direct pointer
- [ ] Test: call `ValidateToken`, mutate the returned token, verify store's internal token is unchanged
- [ ] Test: call `ValidateToken`, restart store from disk, verify `LastUsed` was persisted

---

### T2-04: AddToken Stores Caller's Pointer Without Copy

**Location:** `internal/state/store.go:328`

**Problem:** `AddToken` appends the caller's pointer directly. The caller retains a live pointer into the store's slice. Mutations bypass locking and persistence. Every other mutation method (`SetAgent`, `AddUser`) makes a defensive copy.

**Acceptance criteria:**
- [ ] `AddToken` makes a copy of the token before storing: `cp := *token; s.state.Tokens = append(s.state.Tokens, &cp)`
- [ ] Test: add a token, mutate the original pointer, verify store's token is unchanged

---

### T2-05: Shallow Copies Share Maps and Slices

**Location:** `internal/state/store.go` — `GetNode` (lines 268-270), `SetNode` (278-279), `GetUser/AddUser/AllUsers/UpdateUser` (lines 457-535), `GetCapabilityRegistry` (407-412)

**Problem:** Struct value copies (`cp := *node`) share the underlying data for map and slice fields. `NodeState.Labels` (map), `NodeState.Agents` (slice), `User.Teams` (slice), `User.Agents` (slice), `CapabilityRegistryEntry.Capabilities` (slice) are all shared between the store's internal state and the returned copy.

**Acceptance criteria:**
- [ ] `GetNode` deep-copies `Labels` map and `Agents` slice
- [ ] `SetNode` deep-copies `Labels` map and `Agents` slice
- [ ] `GetUser`/`AllUsers` deep-copy `Teams` and `Agents` slices
- [ ] `GetCapabilityRegistry` deep-copies `Capabilities` slice (and nested `Inputs`/`Outputs`)
- [ ] Test: `GetNode`, mutate returned `Labels` map, verify store's node is unchanged
- [ ] Test: `GetUser`, mutate returned `Teams` slice, verify store's user is unchanged

---

### T2-06: State Machine Contradiction — Cannot Stop a STARTING Agent

**Location:** `internal/vm/manager.go:208-215`, `internal/state/store.go:33-41`

**Problem:** `StopAgent` has a guard that allows STARTING status (line 208), but then calls `ValidateTransition(STARTING, STOPPING)` which fails because the valid transitions table only allows STARTING → {RUNNING, FAILED}. Result: `hivectl agents stop` on a STARTING agent always returns an error even though the guard explicitly permits it.

**Acceptance criteria:**
- [ ] Either: add STOPPING to the valid transitions from STARTING, OR: update the guard to reject STARTING agents
- [ ] The chosen behavior is consistent — if an operator can't stop a starting agent, the error message should say so clearly
- [ ] Test: attempt to stop a STARTING agent, verify it either succeeds or returns a clear error

---

### T2-07: Reconciler Concurrent Trigger + Poll Loop Race

**Location:** `internal/reconciler/reconciler.go:114-116`

**Problem:** `Trigger()` calls `r.runOnce()` directly. The periodic 5s loop also calls `r.runOnce()`. These can run concurrently if `Trigger()` is called from a fsnotify handler while the timer fires. Both invocations detect the same drift and produce duplicate create/destroy/restart actions.

**Acceptance criteria:**
- [ ] `runOnce()` is protected by a mutex (or `Trigger` sends to a channel that the loop selects on)
- [ ] Concurrent calls to `Trigger` during a running reconciliation are coalesced (one pending trigger, not queued)
- [ ] Test: call `Trigger()` while a reconciliation is in progress, verify the action handler is not called with duplicate actions

---

### T2-08: Manifest Hash Non-Deterministic Due to Map Iteration

**Location:** `internal/reconciler/reconciler.go:236-243`

**Problem:** `manifestHash` uses `json.Marshal(manifest)` to produce a hash. `AgentManifest` contains `map[string]string` fields (`Labels`, `Env`, `NodeLabels`, `Custom`) whose JSON key order is non-deterministic. The same manifest produces different hashes on different runs, causing spurious restart actions.

**Acceptance criteria:**
- [ ] `manifestHash` produces a stable hash regardless of map iteration order (e.g., sort keys before hashing, or use a canonical JSON encoder)
- [ ] `manifestHash` returns an error instead of empty string on marshal failure
- [ ] Test: hash the same manifest twice with non-empty map fields, verify identical hashes
- [ ] Test: verify marshal failure returns error, not empty string

---

### T2-09: Cross-Team Response Format Inconsistency

**Location:** `internal/crossteam/router.go:209-218` (success) vs `228-264` (error)

**Problem:** On success, the router forwards the raw response bytes from the internal agent. On error, it wraps the response in a `types.Envelope`. Callers must handle two completely different response formats depending on whether the call succeeded or failed.

**Acceptance criteria:**
- [ ] Both success and error responses use the same format (either both raw or both Envelope-wrapped)
- [ ] Test: verify a successful cross-team invocation and a failed one return the same top-level structure

---

### T2-10: NATS Subject Prefix Mismatch With Spec

**Location:** `internal/dashboard/api.go:349`, `internal/director/director.go:318`, `internal/dashboard/api.go:468`

**Problem:** The spec (`03-COMMUNICATION.md`) defines subjects like `agent.{ID}.inbox`. The code uses `hive.agent.{ID}.inbox` (with `hive.` prefix). If any consumer subscribes to the spec-defined subject, it will never receive messages from the code. This needs a project-wide decision on which is correct, then consistency.

**Acceptance criteria:**
- [ ] A single canonical subject prefix convention is chosen and documented
- [ ] All publishers and subscribers use the same convention
- [ ] Either the spec is updated to match the code, or the code is updated to match the spec
- [ ] Grep for `hive.agent.` and `"agent.` in the codebase confirms no mismatches remain

---

### T2-11: Agent Defaults Never Merged From Cluster Config

**Location:** `internal/config/loader.go:181-233`

**Problem:** `ParseAgent` applies no defaults. Cluster-level defaults (health interval 30s, timeout 5s, max failures 3, restart policy "on-failure", backoff 5s) are never inherited by agents that omit these fields. An agent with no `health:` section will have `Interval: 0`, `Timeout: 0`, `MaxFailures: 0` — all nonsensical for actual health checking.

**Acceptance criteria:**
- [ ] After loading agents and the cluster config, agent-level health/restart fields are populated from cluster defaults when not explicitly set
- [ ] This merging happens in `LoadDesiredState` or a dedicated function called after both are loaded
- [ ] Test: load an agent with no `health:` section, verify it inherits the cluster defaults
- [ ] Test: load an agent with explicit `health:` values, verify they are NOT overwritten by cluster defaults

---

### T2-12: Duplicate Agent/Team IDs Silently Overwritten

**Location:** `internal/config/loader.go:174` (agents), `internal/config/loader.go:271-307` (teams)

**Problem:** When two directories contain manifests with the same `metadata.id`, the second silently overwrites the first in the map. The spec requires globally unique IDs (rules 4 and 5). An operator with a typo in an ID will never know they have a shadow conflict.

**Acceptance criteria:**
- [ ] `LoadAgents` detects duplicate agent IDs and returns an error listing both file paths
- [ ] `LoadTeams` detects duplicate team IDs and returns an error listing both file paths
- [ ] Test: two agent directories with the same `metadata.id`, verify error is returned
- [ ] Test: two team files with the same `metadata.id`, verify error is returned

---

### T2-13: hived Daemon Is a NATS-Only No-Op

**Location:** `cmd/hived/main.go:32-77`

**Problem:** The daemon only starts the embedded NATS server and waits for shutdown. It does not initialize or run: state store, VM manager, health monitor, reconciler, capability router, node registry, join handler, scheduler, or any NATS subscription handlers. All agent management is performed by CLI commands that directly manipulate `state.json`, which is architecturally incorrect — hivectl should communicate with the running daemon.

**Acceptance criteria:**
- [ ] `hived` initializes the state store, loads existing state
- [ ] `hived` starts the health monitor, reconciler, and capability router
- [ ] `hived` subscribes to NATS subjects for control-plane operations (join requests, health heartbeats, capability routing)
- [ ] `hivectl` commands communicate with hived over NATS (or a control API) instead of directly manipulating state.json
- [ ] Test: start hived, use hivectl to start an agent, verify hived processes the request

*Note: This is the largest single item and may need to be broken into sub-tasks.*

---

### T2-14: Dashboard Uses Non-UUID Envelope IDs

**Location:** `internal/dashboard/api.go:334`

**Problem:** Uses `fmt.Sprintf("dashboard-%d", time.Now().UnixNano())` instead of UUID v4. Not unique under concurrent requests within the same nanosecond. Violates the envelope spec.

**Acceptance criteria:**
- [ ] Dashboard envelope IDs use the same `newUUID()` function (or a shared one from `internal/types`)
- [ ] Test: verify envelope ID matches UUID v4 format regex

---

### T2-15: Dashboard Envelope Timestamp Not UTC

**Location:** `internal/dashboard/api.go:338`

**Problem:** Uses `time.Now()` (local timezone) while every other envelope in the codebase uses `time.Now().UTC()`.

**Acceptance criteria:**
- [ ] Dashboard envelopes use `time.Now().UTC()`
- [ ] Grep the codebase for `Timestamp: time.Now()` (without `.UTC()`) — should find zero matches in envelope construction

---

## Tier 3: Safety and Robustness

Issues that could cause panics, resource leaks, or security problems under certain conditions.

### T3-01: Double-Close Panics in Monitor, Sidecar, and Reconciler

**Location:** `internal/health/monitor.go:63-68`, `internal/sidecar/sidecar.go:201`, `internal/reconciler/reconciler.go:107-110`

**Problem:** `Stop()` calls `close(stopCh)` without a guard. Calling `Stop()` twice panics. Signal handlers and graceful shutdown paths can trigger this.

**Acceptance criteria:**
- [ ] All three `Stop()` methods use `sync.Once` to ensure the channel is closed at most once
- [ ] Test: call `Stop()` twice, verify no panic

---

### T3-02: Monitor and Reconciler Start() Can Be Called Multiple Times

**Location:** `internal/health/monitor.go:49-60`, `internal/reconciler/reconciler.go`

**Problem:** No guard prevents `Start()` from being called multiple times. Each call creates a new NATS subscription (leaking the old one) and spawns a new goroutine.

**Acceptance criteria:**
- [ ] `Start()` returns an error or is a no-op if already started
- [ ] Test: call `Start()` twice, verify no duplicate subscriptions or goroutines

---

### T3-03: MQTT Bridge Allows Unauthenticated Connections

**Location:** `internal/mqtt/bridge.go:275-282`

**Problem:** If an MQTT client connects with no password, the token validation is skipped entirely. Any device on the network can connect and publish/subscribe freely.

**Acceptance criteria:**
- [ ] Decide: either require authentication always, or make it configurable with a clear default
- [ ] If auth is required: empty password connections are rejected with return code 5
- [ ] If auth is optional: document this explicitly and add a config flag to require it
- [ ] Test: connect with no password when auth is required, verify connection is rejected

---

### T3-04: Dashboard CORS Allows All Origins

**Location:** `internal/dashboard/api.go:147-149`

**Problem:** `Access-Control-Allow-Origin: *` allows any website to make API requests to the dashboard. Combined with future authentication, this enables CSRF attacks.

**Acceptance criteria:**
- [ ] CORS origin is configurable (default: same-origin or `localhost`)
- [ ] `*` is only used when explicitly configured for development
- [ ] Test: request with foreign `Origin` header is rejected when CORS is restricted

---

### T3-05: Dashboard WebSocket Has No Authentication

**Location:** `internal/dashboard/api.go:498-568`

**Problem:** Any client that can reach the dashboard port can connect via WebSocket and receive real-time events (agent state, heartbeats, logs). No token or session verification.

**Acceptance criteria:**
- [ ] WebSocket upgrade requires an auth token (query param, header, or initial message)
- [ ] Unauthenticated WebSocket connections are rejected with 401
- [ ] Test: attempt WebSocket connection without auth, verify 401 response

---

### T3-06: Director Has No Auth on Tool Invocations

**Location:** `internal/director/director.go:57-92`

**Problem:** Any NATS client can publish to `hive.director.*.request` and invoke director tools (list all agents, broadcast to all, invoke capabilities). No identity verification or RBAC check.

**Acceptance criteria:**
- [ ] Director validates the `From` field in the envelope against the auth system
- [ ] Only admin/operator roles can invoke director tools
- [ ] Test: send a director request with an unauthorized `From`, verify it is rejected

---

### T3-07: Metrics Latency Observations Grow Unbounded

**Location:** `internal/metrics/metrics.go:78-88`

**Problem:** Every `ObserveInvocationLatency` call appends to a slice that is never trimmed. Over time this consumes unbounded memory and makes quantile computation O(n log n) per render.

**Acceptance criteria:**
- [ ] Latency observations use a bounded data structure (ring buffer, time-windowed reservoir, or periodic reset)
- [ ] Memory usage is bounded regardless of how many observations are recorded
- [ ] Quantile accuracy is maintained for recent observations
- [ ] Test: record 100,000 observations, verify memory usage is bounded

---

### T3-08: Log Aggregator Opens/Closes File Per Entry

**Location:** `internal/logs/aggregator.go:224-234`

**Problem:** Every single log entry opens the file, writes one line, and closes it. Under high log volume this creates massive file descriptor churn and contention.

**Acceptance criteria:**
- [ ] Log files are opened once and reused (file handle pool or per-agent writer)
- [ ] Handles are flushed periodically and closed on shutdown
- [ ] File rotation still works correctly with persistent handles (reopen after rotation)
- [ ] Test: write 1000 log entries rapidly, verify no errors and reasonable performance

---

### T3-09: Log Follower Double-Close Panic

**Location:** `internal/logs/aggregator.go:446-461`

**Problem:** The `cancel` function from `Follow` calls `close(ch)`. If `Stop()` also closes the same channel (via iterating `a.followers`), the deferred `cancel()` panics.

**Acceptance criteria:**
- [ ] `cancel` uses `sync.Once` to prevent double-close
- [ ] `Stop()` and `cancel()` can both be called without panic in any order
- [ ] Test: call `Follow()`, then `Stop()`, then `cancel()`, verify no panic

---

### T3-10: OTA Push Blocks Caller for Up to 5 Minutes

**Location:** `internal/ota/ota.go:225-265`

**Problem:** `Push()` calls `completeSub.NextMsg(5 * time.Minute)` synchronously. No context support, no cancellation, no progress reporting.

**Acceptance criteria:**
- [ ] `Push` accepts a `context.Context` parameter for cancellation
- [ ] Progress is reported via a callback or channel (chunk count, bytes sent)
- [ ] Context cancellation stops the transfer cleanly
- [ ] Test: cancel a push mid-transfer, verify it returns promptly with a context error

---

### T3-11: State Store Does Not Initialize Tokens/Users Slices

**Location:** `internal/state/store.go:71-77` (newState), `internal/state/store.go:120-136` (Load)

**Problem:** Neither `newState()` nor `Load()` initializes `Tokens` or `Users` — they remain `nil`. While Go's nil-slice semantics prevent panics, this is inconsistent with `Agents`, `Nodes`, and `Capabilities` which are all initialized.

**Acceptance criteria:**
- [ ] `newState()` initializes `Tokens: []*types.Token{}` and `Users: []*auth.User{}`
- [ ] `Load()` nil-checks and initializes `Tokens` and `Users` like it does for `Agents`, `Nodes`, and `Capabilities`
- [ ] Test: load state from JSON without `tokens`/`users` keys, verify they are empty slices not nil

---

### T3-12: hive-agent join Accepts Empty Agent ID

**Location:** `cmd/hive-agent/main.go:79`

**Problem:** `--agent-id` flag is not marked as required. If omitted, `agentID` is empty string, which flows through to NATS subjects and state as an empty ID.

**Acceptance criteria:**
- [ ] `--agent-id` is marked as a required flag via cobra's `MarkFlagRequired`
- [ ] If cobra's flag validation is insufficient, an explicit check rejects empty string with a clear error
- [ ] Test: run `hive-agent join` without `--agent-id`, verify it exits with a usage error

---

### T3-13: newUUID() Duplicated in 4 Packages

**Location:** `internal/sidecar/nats.go:186-206`, `internal/capability/router.go:368-389`, `internal/crossteam/router.go:312-329`, `internal/director/director.go:637-654`

**Problem:** Identical function copy-pasted four times. A bug fix must be applied in four places.

**Acceptance criteria:**
- [ ] A single `NewUUID()` function exists in `internal/types/envelope.go` (or `internal/uuid/`)
- [ ] All four packages import and use the shared function
- [ ] The duplicated functions are deleted
- [ ] Test: verify the shared function produces valid UUID v4 format strings

---

## Tier 4: Validation and Spec Conformance

Missing validation rules and CLI commands that should be addressed before release.

### T4-01: Missing Validation Rules

The following validation rules from `02-SCHEMAS.md` are not enforced:

- [ ] `AgentVolume.Access` must be `read-only` or `read-write` (validate.go:181-199)
- [ ] `SharedVolume.Access` must be `read-only` or `read-write` (validate.go:230-255)
- [ ] `Agent.Network.Egress` must be `none`, `restricted`, or `full` (validate.go:201-204)
- [ ] `CapabilityParam.Type` must be `string`, `int`, `float`, `bool`, or `bytes` (validate.go:165-179)
- [ ] Secret refs (`${SECRET_NAME}`) must resolve to `spec.secrets` entries
- [ ] Director `agentId` must reference an existing agent with no team
- [ ] Agent IDs validated against `[a-z0-9][a-z0-9-]{0,62}` in CLI commands
- [ ] `.yml` files should be loaded (or warned about) in addition to `.yaml`

### T4-02: Missing CLI Commands

The following commands from `07-CLI-AND-INTERACTION.md` are not implemented:

- [ ] `hivectl status` — cluster overview
- [ ] `hivectl teams list` / `teams status` / `teams capabilities`
- [ ] `hivectl agents logs AGENT_ID`
- [ ] `hivectl agents exec AGENT_ID -- COMMAND`
- [ ] `hivectl agents capabilities AGENT_ID`
- [ ] `hivectl connect AGENT_ID` — interactive agent connection
- [ ] `hivectl nodes approve NODE_ID` / `nodes remove NODE_ID`
- [ ] `hivectl users rotate USER_ID`
- [ ] `hivectl firmware update` / `firmware sign`
- [ ] `hivectl messages send` / `messages subscribe`
- [ ] `hivectl capabilities invoke`

### T4-03: Missing CLI Global Flags

- [ ] `--output json` for machine-readable output
- [ ] `--control-plane ADDRESS` for remote daemon
- [ ] `HIVE_CONFIG` environment variable support
- [ ] `--user USER_ID --token TOKEN` auth flags

### T4-04: hivectl init Is Not Idempotent

**Location:** `cmd/hivectl/main.go:85-112`

**Problem:** Running `hivectl init PATH` twice overwrites existing `cluster.yaml` and manifest files. The spec says it should be idempotent.

**Acceptance criteria:**
- [ ] `hivectl init` skips files that already exist (or uses a merge strategy)
- [ ] Existing user-customized configs are never overwritten
- [ ] Missing files are created
- [ ] Test: init once, modify cluster.yaml, init again, verify modifications are preserved

### T4-05: Incomplete AgentFirmware Type

**Location:** `internal/types/agent.go:112-115`

**Problem:** `AgentFirmware` only has `Platform` and `Board`. Missing: `PartitionTable`, `ExtraLibs`, `BuildFlags`, `FlashMethod`, `FlashBaud` per spec.

**Acceptance criteria:**
- [ ] All fields from `02-SCHEMAS.md` lines 163-168 are present in the struct with correct types and yaml tags
- [ ] Validation enforces required fields when tier is `firmware`

---

## Test Coverage Gaps

These are areas where tests should be added regardless of which bugs are fixed:

- [ ] **RestartManager** — zero test coverage (`internal/health/restart.go`)
- [ ] **Concurrent state store access** — no test for multiple goroutines doing Get/Set with `-race`
- [ ] **LoadTeams** — no dedicated test
- [ ] **LoadDesiredState** — no dedicated test
- [ ] **Sidecar full Start() flow** — tests only exercise individual methods, never the full startup sequence
- [ ] **VMPID population** — no test verifies VMPID is set after StartAgent
- [ ] **Overlapping capabilities** — no test for two agents with the same capability name
- [ ] **Empty/zero-byte state file recovery** — only corrupt JSON is tested, not empty files
- [ ] **Cluster restart safety** — no test for Stop() then Start() on cluster
