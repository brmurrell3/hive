// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

// Package capability implements NATS-based capability routing, tool auto-generation, and cross-team invocation.
package capability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/brmurrell3/hive/internal/protocol"
	"github.com/brmurrell3/hive/internal/types"
	"github.com/nats-io/nats.go"
)

var errHandlerTimeout = errors.New("handler execution timed out")

const (
	// defaultTimeout is the default handler execution timeout.
	defaultTimeout = 30 * time.Second

	// dedupTTL is how long message IDs are remembered for deduplication.
	dedupTTL = 2 * time.Minute

	// dedupMaxSize is the maximum number of entries in the dedup cache before
	// a sweep is triggered.
	dedupMaxSize = 10000

	// dedupSweepInterval is how often the periodic dedup sweep runs.
	dedupSweepInterval = 60 * time.Second

	// dedupHardLimit is the absolute cap on dedup map size. If the map exceeds
	// this size, an immediate sweep is forced and the oldest entries are dropped.
	dedupHardLimit = 2 * dedupMaxSize

	// workerPoolSize is the number of worker goroutines that process incoming
	// capability requests from the work channel.
	workerPoolSize = 16

	// workChannelSize is the buffer size for the incoming request channel.
	// When full, new requests receive a "service overloaded" error.
	workChannelSize = 256
)

// Handler is a function that handles a capability invocation.
// The context carries the handler execution timeout so that handlers can
// observe cancellation cooperatively.
type Handler func(ctx context.Context, inputs map[string]interface{}) (map[string]interface{}, error)

// InvocationRequest is the payload for a capability request.
type InvocationRequest struct {
	Capability string                 `json:"capability"`
	Inputs     map[string]interface{} `json:"inputs"`
	Timeout    string                 `json:"timeout,omitempty"` // default "30s"
}

// InvocationResponse is the payload for a capability response.
type InvocationResponse struct {
	Capability string                 `json:"capability"`
	Status     string                 `json:"status"` // success, error, timeout
	Outputs    map[string]interface{} `json:"outputs,omitempty"`
	Error      *InvocationError       `json:"error,omitempty"`
	DurationMs int64                  `json:"duration_ms"`
}

// InvocationError describes an error that occurred during capability invocation.
type InvocationError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// EventPublisher is the interface for publishing capability events.
type EventPublisher interface {
	CapabilityRegistered(agentID, capability string)
	CapabilityInvoked(from, to, capability string)
}

// dedupEntry stores the timestamp when a message ID was first seen.
type dedupEntry struct {
	seen time.Time
}

// workItem represents an incoming capability request queued for processing
// by the worker pool.
type workItem struct {
	msg     *nats.Msg
	capName string
	handler Handler
}

// Router manages capability subscriptions and invocations over NATS.
// Each agent's sidecar creates a Router to expose its capabilities and
// invoke capabilities on other agents.
type Router struct {
	nc       *nats.Conn
	logger   *slog.Logger
	handlers map[string]Handler // capability name -> handler
	subs     []*nats.Subscription
	agentID  string
	mu       sync.RWMutex
	events   EventPublisher // optional event publisher

	// dedupMu protects the dedup map.
	dedupMu sync.Mutex
	// dedup tracks recently seen message IDs to prevent duplicate processing.
	dedup map[string]dedupEntry

	// handlerWg tracks outstanding handler goroutines so they can be waited
	// on during shutdown.
	handlerWg sync.WaitGroup

	// workCh is the buffered channel for incoming requests. NATS callbacks
	// enqueue work items here; the worker pool reads from it.
	workCh chan workItem

	// stopCh is closed when the router is stopping, signaling the dedup
	// sweep goroutine and worker pool to exit.
	stopCh chan struct{}

	// workerWg tracks the worker pool goroutines.
	workerWg sync.WaitGroup

	// stopOnce ensures Stop() is idempotent and safe to call multiple times.
	stopOnce sync.Once

	// started tracks whether Start() has been called, to prevent double-start.
	started bool

	// initErr stores a validation error from NewRouter (e.g., invalid agentID).
	// If set, Start() will return this error immediately.
	initErr error
}

// NewRouter creates a new capability Router for the given agent.
// The agentID is validated as a NATS subject component; if invalid, Start()
// will return an error.
func NewRouter(agentID string, nc *nats.Conn, logger *slog.Logger) *Router {
	var initErr error
	if err := types.ValidateSubjectComponent("agent_id", agentID); err != nil {
		initErr = fmt.Errorf("invalid agent ID for capability router: %w", err)
	}
	return &Router{
		nc:       nc,
		logger:   logger,
		handlers: make(map[string]Handler),
		agentID:  agentID,
		dedup:    make(map[string]dedupEntry),
		initErr:  initErr,
	}
}

// SetEventPublisher sets an optional event publisher for capability events.
func (r *Router) SetEventPublisher(ep EventPublisher) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = ep
}

// RegisterHandler registers a handler function for the named capability.
// Must be called before Start.
func (r *Router) RegisterHandler(capability string, handler Handler) error {
	if err := types.ValidateSubjectComponent("capability", capability); err != nil {
		return fmt.Errorf("invalid capability name: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		r.logger.Warn("RegisterHandler called after Start; handler will not receive NATS requests", "capability", capability)
	}
	r.handlers[capability] = handler
	if r.events != nil {
		r.events.CapabilityRegistered(r.agentID, capability)
	}
	r.logger.Info("registered capability handler",
		"agent_id", r.agentID,
		"capability", capability,
	)
	return nil
}

// Start subscribes to NATS subjects for each registered capability.
// For each capability, it subscribes to:
//
//	hive.capabilities.{agentID}.{capability}.request
//
// It also starts a worker pool for processing incoming requests and a
// background goroutine for periodic dedup cache sweeps.
func (r *Router) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.initErr != nil {
		return r.initErr
	}

	if r.started {
		return fmt.Errorf("capability router already started")
	}
	r.started = true

	// Initialize the work channel and stop channel.
	r.workCh = make(chan workItem, workChannelSize)
	r.stopCh = make(chan struct{})

	// Start the worker pool.
	for i := 0; i < workerPoolSize; i++ {
		r.workerWg.Add(1)
		go r.worker()
	}

	// Start the periodic dedup sweep goroutine.
	r.workerWg.Add(1)
	go r.dedupSweepLoop()

	for cap, handler := range r.handlers {
		subject := fmt.Sprintf(protocol.FmtCapabilityReq, r.agentID, cap)

		// Capture loop variables for the closure.
		capName := cap
		capHandler := handler

		sub, err := r.nc.Subscribe(subject, func(msg *nats.Msg) {
			r.enqueueRequest(msg, capName, capHandler)
		})
		if err != nil {
			// Unsubscribe any already-created subscriptions on failure.
			for _, s := range r.subs {
				_ = s.Unsubscribe()
			}
			r.subs = nil
			close(r.stopCh)
			r.workerWg.Wait()
			return fmt.Errorf("subscribing to %s: %w", subject, err)
		}

		r.subs = append(r.subs, sub)
		r.logger.Info("subscribed to capability subject",
			"subject", subject,
			"capability", capName,
		)
	}

	return nil
}

// Stop unsubscribes from all capability subjects, shuts down the worker pool,
// and waits for all outstanding handler goroutines to complete.
// Safe to call multiple times; subsequent calls are no-ops.
func (r *Router) Stop() {
	r.stopOnce.Do(func() {
		r.mu.Lock()

		for _, sub := range r.subs {
			if err := sub.Unsubscribe(); err != nil {
				r.logger.Warn("error unsubscribing capability subject",
					"subject", sub.Subject,
					"error", err,
				)
			}
		}

		r.subs = nil

		// Signal the worker pool and dedup sweep to stop.
		// Do NOT close workCh here -- in-flight NATS callbacks in
		// enqueueRequest could race between the stopCh check and the
		// channel send, panicking on a closed channel. Workers exit
		// via stopCh; the GC will reclaim the unreferenced channel.
		if r.stopCh != nil {
			close(r.stopCh)
		}

		r.mu.Unlock()

		// Wait for workers to drain the work channel and finish.
		r.workerWg.Wait()

		// After all workers have exited, drain any remaining items from workCh
		// that were enqueued after the workers' drain loops completed but before
		// all NATS callbacks finished. Respond with a shutdown error so callers
		// get a definitive response instead of timing out.
		for {
			select {
			case item := <-r.workCh:
				r.publishErrorResponse(item.msg, "unknown", item.capName, "SERVICE_UNAVAILABLE",
					"service is shutting down", time.Now())
			default:
				goto drained
			}
		}
	drained:

		// Wait for any outstanding handler goroutines to finish, with a timeout.
		// The timeout here must be >= maxTimeout (currently 5 minutes) because
		// handlers can legitimately run for up to maxTimeout. We add a 60-second
		// grace period on top of maxTimeout to allow for scheduling delays and
		// cleanup. If we time out, handlers are still running but we proceed
		// with shutdown to avoid blocking indefinitely.
		stopTimeout := maxTimeout + 60*time.Second
		done := make(chan struct{})
		go func() {
			r.handlerWg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(stopTimeout):
			r.logger.Warn("timed out waiting for capability handlers to complete",
				"timeout", stopTimeout,
			)
		}

		r.logger.Info("capability router stopped", "agent_id", r.agentID)
	})
}

// worker is a goroutine that reads work items from the work channel and
// processes them. It exits when stopCh is closed, after draining any
// remaining items from workCh. The workCh is never closed; workers rely
// solely on stopCh for shutdown signaling.
func (r *Router) worker() {
	defer r.workerWg.Done()
	for {
		select {
		case item := <-r.workCh:
			r.handleRequest(item.msg, item.capName, item.handler)
		case <-r.stopCh:
			// Drain remaining items from the work channel before exiting.
			// Note: items enqueued after this drain completes but before all
			// workers exit may be dropped. This is acceptable shutdown behavior
			// since NATS subscriptions are already removed and clients will
			// observe a timeout for any in-flight requests.
			for {
				select {
				case item := <-r.workCh:
					r.handleRequest(item.msg, item.capName, item.handler)
				default:
					return
				}
			}
		}
	}
}

// enqueueRequest attempts to enqueue an incoming NATS request into the work
// channel. If the channel is full (backpressure), it immediately responds
// with a "service overloaded" error. If the router is stopping, the request
// is silently dropped to avoid sending on a closed channel.
func (r *Router) enqueueRequest(msg *nats.Msg, capName string, handler Handler) {
	// Check if the router is shutting down before attempting to send on
	// workCh. This guards against in-flight NATS callbacks that entered
	// after Stop() unsubscribed but before workCh was closed.
	select {
	case <-r.stopCh:
		r.logger.Warn("router stopping, dropping capability request",
			"capability", capName,
			"subject", msg.Subject,
		)
		return
	default:
	}

	select {
	case r.workCh <- workItem{msg: msg, capName: capName, handler: handler}:
		// Successfully enqueued.
	case <-r.stopCh:
		// Router is stopping; drop the request to avoid sending on a closed channel.
		r.logger.Warn("router stopping, dropping capability request",
			"capability", capName,
			"subject", msg.Subject,
		)
	default:
		// Channel full -- apply backpressure.
		r.logger.Warn("worker pool full, rejecting capability request",
			"capability", capName,
			"subject", msg.Subject,
		)
		r.publishErrorResponse(msg, "unknown", capName, "SERVICE_OVERLOADED",
			"service is overloaded, please retry later", time.Now())
	}
}

// dedupSweepLoop runs a periodic sweep of the dedup cache every
// dedupSweepInterval. It exits when stopCh is closed.
func (r *Router) dedupSweepLoop() {
	defer r.workerWg.Done()
	ticker := time.NewTicker(dedupSweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			r.sweepDedup()
		case <-r.stopCh:
			return
		}
	}
}

// sweepDedup removes expired entries from the dedup map. If the map still
// exceeds dedupHardLimit after removing expired entries, the oldest entries
// are dropped to bring it down to dedupMaxSize.
func (r *Router) sweepDedup() {
	r.dedupMu.Lock()
	defer r.dedupMu.Unlock()

	now := time.Now()

	for id, entry := range r.dedup {
		if now.Sub(entry.seen) > dedupTTL {
			delete(r.dedup, id)
		}
	}

	// If still over the hard limit, drop the oldest entries to bring it
	// down to dedupMaxSize.
	if len(r.dedup) > dedupHardLimit {
		r.evictOldestLocked(len(r.dedup) - dedupMaxSize)
	}
}

// evictOldestLocked removes the n oldest entries from the dedup map.
// Caller must hold dedupMu.
func (r *Router) evictOldestLocked(n int) {
	if n <= 0 {
		return
	}

	type idTime struct {
		id   string
		seen time.Time
	}

	entries := make([]idTime, 0, len(r.dedup))
	for id, entry := range r.dedup {
		entries = append(entries, idTime{id: id, seen: entry.seen})
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].seen.Before(entries[j].seen)
	})

	if n > len(entries) {
		n = len(entries)
	}
	for i := 0; i < n; i++ {
		delete(r.dedup, entries[i].id)
	}
}

// isDuplicate checks whether a message ID has been seen recently. If not,
// it records the ID and returns false. If the ID was already seen within
// the TTL window, it returns true.
//
// If the dedup map exceeds dedupHardLimit, an immediate sweep is triggered
// and the oldest entries are dropped.
func (r *Router) isDuplicate(messageID string) bool {
	if messageID == "" {
		return false
	}

	r.dedupMu.Lock()
	defer r.dedupMu.Unlock()

	now := time.Now()

	// If the map has exceeded the hard limit, force an immediate sweep
	// and evict oldest entries to bring it down to dedupMaxSize.
	if len(r.dedup) >= dedupHardLimit {
		for id, entry := range r.dedup {
			if now.Sub(entry.seen) > dedupTTL {
				delete(r.dedup, id)
			}
		}
		if len(r.dedup) >= dedupHardLimit {
			r.evictOldestLocked(len(r.dedup) - dedupMaxSize)
		}
	}

	if entry, exists := r.dedup[messageID]; exists {
		if now.Sub(entry.seen) <= dedupTTL {
			return true
		}
	}

	r.dedup[messageID] = dedupEntry{seen: now}
	return false
}

// marshalPayload marshals an arbitrary value into json.RawMessage suitable
// for embedding in an Envelope.
func marshalPayload(v interface{}) (json.RawMessage, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(data), nil
}

// Invoke calls a capability on a remote agent and waits for the response.
// It publishes an InvocationRequest wrapped in an Envelope to
// hive.capabilities.{targetAgentID}.{capability}.request and waits for
// the response on an auto-created reply subject.
func (r *Router) Invoke(targetAgentID, capability string, inputs map[string]interface{}, timeout time.Duration) (*InvocationResponse, error) {
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	if err := types.ValidateSubjectComponent("target_agent_id", targetAgentID); err != nil {
		return nil, fmt.Errorf("invalid target agent ID: %w", err)
	}
	if err := types.ValidateSubjectComponent("capability", capability); err != nil {
		return nil, fmt.Errorf("invalid capability name: %w", err)
	}

	subject := fmt.Sprintf(protocol.FmtCapabilityReq, targetAgentID, capability)

	req := InvocationRequest{
		Capability: capability,
		Inputs:     inputs,
		Timeout:    timeout.String(),
	}

	payloadBytes, err := marshalPayload(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling invocation request payload: %w", err)
	}

	env := types.Envelope{
		ID:        types.NewUUID(),
		From:      r.agentID,
		To:        targetAgentID,
		Type:      types.MessageTypeCapabilityRequest,
		Timestamp: time.Now().UTC(),
		Payload:   payloadBytes,
	}

	if err := env.Validate(); err != nil {
		return nil, fmt.Errorf("envelope validation failed: %w", err)
	}

	data, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("marshaling invocation request: %w", err)
	}

	r.logger.Info("invoking capability",
		"target", targetAgentID,
		"capability", capability,
		"subject", subject,
		"timeout", timeout,
	)

	// NATS Request creates a temporary reply subject and waits for a response.
	// Add a buffer to the NATS request timeout so the handler has time to
	// process and respond before the NATS-level timeout fires. Without this,
	// both the handler timeout and the NATS timeout race, potentially causing
	// the caller to see a NATS timeout when the handler was about to respond.
	natsTimeout := timeout + 5*time.Second
	msg, err := r.nc.Request(subject, data, natsTimeout)
	if err != nil {
		if errors.Is(err, nats.ErrTimeout) {
			return &InvocationResponse{
				Capability: capability,
				Status:     "timeout",
				Error: &InvocationError{
					Code:      "TIMEOUT",
					Message:   fmt.Sprintf("capability invocation timed out after %s", timeout),
					Retryable: true,
				},
			}, nil
		}
		return nil, fmt.Errorf("invoking capability %s on %s: %w", capability, targetAgentID, err)
	}

	var respEnv types.Envelope
	if err := json.Unmarshal(msg.Data, &respEnv); err != nil {
		return nil, fmt.Errorf("unmarshaling response envelope: %w", err)
	}

	var resp InvocationResponse
	if err := json.Unmarshal(respEnv.Payload, &resp); err != nil {
		return nil, fmt.Errorf("unmarshaling invocation response: %w", err)
	}

	r.logger.Info("capability invocation complete",
		"target", targetAgentID,
		"capability", capability,
		"status", resp.Status,
		"duration_ms", resp.DurationMs,
	)

	return &resp, nil
}

// executeHandler runs a handler function with panic recovery and a timeout.
// If the handler panics, the panic is caught and returned as an error.
// If the handler does not complete within the given timeout, a timeout error
// is returned. A context with timeout is passed to the handler so that
// well-behaved handlers can observe cancellation cooperatively. The handler
// goroutine is tracked via handlerWg to prevent zombie goroutines on shutdown.
//
// NOTE: Handlers MUST be responsive or Stop() will block indefinitely waiting
// for handlerWg. When the timeout fires, executeHandler returns a timeout
// error to the caller, but the handler goroutine continues running until it
// completes naturally (it is tracked by handlerWg). This is inherent to the
// design: Go does not support forcible goroutine cancellation, and handler
// authors must ensure their handlers respect context cancellation.
func (r *Router) executeHandler(handler Handler, inputs map[string]interface{}, timeout time.Duration, capName string) (result map[string]interface{}, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	type handlerResult struct {
		outputs map[string]interface{}
		err     error
	}

	done := make(chan handlerResult, 2)

	r.handlerWg.Add(1)
	go func() {
		defer r.handlerWg.Done()

		// Panic recovery MUST be the first defer set up in this goroutine
		// (so it is the last to execute) to guarantee it catches all panics,
		// including any that might occur in other deferred functions. The
		// sent flag ensures we always send exactly one result on the done
		// channel -- either the normal result or the panic error -- so the
		// caller never hangs waiting on the channel.
		sent := false
		defer func() {
			if rec := recover(); rec != nil {
				sent = true
				done <- handlerResult{err: fmt.Errorf("handler panicked: %v", rec)}
			}
			if !sent {
				// This should not happen in normal operation, but guards
				// against edge cases where the handler returned but the
				// normal send on done was somehow skipped.
				done <- handlerResult{err: fmt.Errorf("handler completed without producing a result")}
			}
		}()

		out, herr := handler(ctx, inputs)
		sent = true
		done <- handlerResult{outputs: out, err: herr}
	}()

	select {
	case res := <-done:
		return res.outputs, res.err
	case <-ctx.Done():
		r.logger.Warn("capability handler timed out, goroutine may leak", "capability", capName)
		return nil, fmt.Errorf("%w after %s", errHandlerTimeout, timeout)
	}
}

// maxTimeout is the maximum allowed handler timeout to prevent callers from
// requesting unbounded execution times.
const maxTimeout = 5 * time.Minute

// parseTimeout parses a timeout string from an InvocationRequest and returns
// the duration. Falls back to defaultTimeout if the string is empty or invalid.
// The result is capped at maxTimeout (5 minutes).
func parseTimeout(s string) time.Duration {
	if s == "" {
		return defaultTimeout
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return defaultTimeout
	}
	if d > maxTimeout {
		d = maxTimeout
	}
	return d
}

// handleRequest processes an incoming capability request message.
func (r *Router) handleRequest(msg *nats.Msg, capName string, handler Handler) {
	start := time.Now()

	// Snapshot the events publisher under the lock so we can use it safely
	// later without holding the lock during request processing.
	r.mu.RLock()
	events := r.events
	r.mu.RUnlock()

	r.logger.Info("received capability request",
		"capability", capName,
		"subject", msg.Subject,
		"reply", msg.Reply,
		"size", len(msg.Data),
	)

	var env types.Envelope
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		r.logger.Error("failed to unmarshal capability request envelope",
			"capability", capName,
			"error", err,
		)
		r.publishErrorResponse(msg, "unknown", capName, "INVALID_REQUEST", "failed to unmarshal request envelope", start)
		return
	}

	if r.isDuplicate(env.ID) {
		r.logger.Warn("dropping duplicate capability request",
			"capability", capName,
			"message_id", env.ID,
			"from", env.From,
		)
		return
	}

	if err := env.Validate(); err != nil {
		r.logger.Error("incoming capability request envelope validation failed",
			"capability", capName,
			"error", err,
		)
		r.publishErrorResponse(msg, env.From, capName, "INVALID_REQUEST", "invalid request envelope", start)
		return
	}

	var req InvocationRequest
	if err := json.Unmarshal(env.Payload, &req); err != nil {
		r.logger.Error("failed to unmarshal invocation request",
			"capability", capName,
			"error", err,
		)
		r.publishErrorResponse(msg, env.From, capName, "INVALID_PAYLOAD", "failed to unmarshal invocation request", start)
		return
	}

	timeout := parseTimeout(req.Timeout)

	outputs, handlerErr := r.executeHandler(handler, req.Inputs, timeout, capName)
	durationMs := time.Since(start).Milliseconds()

	var resp InvocationResponse
	if handlerErr != nil {
		r.logger.Warn("capability handler returned error",
			"capability", capName,
			"error", handlerErr,
			"duration_ms", durationMs,
		)
		status := "error"
		code := "HANDLER_ERROR"
		retryable := false
		if errors.Is(handlerErr, errHandlerTimeout) {
			status = "timeout"
			code = "HANDLER_TIMEOUT"
			retryable = true
		}
		resp = InvocationResponse{
			Capability: capName,
			Status:     status,
			Error: &InvocationError{
				Code:      code,
				Message:   "handler execution failed",
				Retryable: retryable,
			},
			DurationMs: durationMs,
		}
	} else {
		resp = InvocationResponse{
			Capability: capName,
			Status:     "success",
			Outputs:    outputs,
			DurationMs: durationMs,
		}
		if events != nil {
			events.CapabilityInvoked(env.From, r.agentID, capName)
		}
	}

	respPayload, err := marshalPayload(resp)
	if err != nil {
		r.logger.Error("failed to marshal capability response payload",
			"capability", capName,
			"error", err,
		)
		return
	}

	respEnv := types.Envelope{
		ID:            types.NewUUID(),
		From:          r.agentID,
		To:            env.From,
		Type:          types.MessageTypeCapabilityResponse,
		Timestamp:     time.Now().UTC(),
		Payload:       respPayload,
		CorrelationID: env.ID,
	}

	if err := respEnv.Validate(); err != nil {
		r.logger.Error("response envelope validation failed, dropping response",
			"capability", capName,
			"error", err,
		)
		return
	}

	respData, err := json.Marshal(respEnv)
	if err != nil {
		r.logger.Error("failed to marshal capability response",
			"capability", capName,
			"error", err,
		)
		return
	}

	if msg.Reply == "" {
		r.logger.Warn("no reply subject on capability request, dropping response",
			"capability", capName,
		)
		return
	}

	if err := r.nc.Publish(msg.Reply, respData); err != nil {
		r.logger.Error("failed to publish capability response",
			"capability", capName,
			"reply", msg.Reply,
			"error", err,
		)
		return
	}

	r.logger.Info("capability response published",
		"capability", capName,
		"status", resp.Status,
		"duration_ms", durationMs,
	)
}

// CallLocal invokes a locally registered handler directly, bypassing NATS.
// This is used by the sidecar to execute capabilities without publishing back
// to the same NATS subject (which would cause an infinite loop).
// Returns an InvocationResponse with status, outputs, and timing information.
func (r *Router) CallLocal(capability string, inputs map[string]interface{}) *InvocationResponse {
	r.mu.RLock()
	handler, ok := r.handlers[capability]
	r.mu.RUnlock()

	if !ok {
		return &InvocationResponse{
			Capability: capability,
			Status:     "error",
			Error: &InvocationError{
				Code:      "NOT_FOUND",
				Message:   fmt.Sprintf("no handler registered for capability %q", capability),
				Retryable: false,
			},
		}
	}

	start := time.Now()
	outputs, err := r.executeHandler(handler, inputs, defaultTimeout, capability)
	durationMs := time.Since(start).Milliseconds()

	if err != nil {
		return &InvocationResponse{
			Capability: capability,
			Status:     "error",
			Error: &InvocationError{
				Code:      "HANDLER_ERROR",
				Message:   err.Error(),
				Retryable: false,
			},
			DurationMs: durationMs,
		}
	}

	return &InvocationResponse{
		Capability: capability,
		Status:     "success",
		Outputs:    outputs,
		DurationMs: durationMs,
	}
}

// publishErrorResponse sends an error response envelope to the reply subject.
// replyTo is the identity of the original sender (used as the To field in the
// response envelope). Pass "unknown" if the sender could not be determined.
func (r *Router) publishErrorResponse(msg *nats.Msg, replyTo, capName, code, message string, start time.Time) {
	if msg.Reply == "" {
		return
	}

	resp := InvocationResponse{
		Capability: capName,
		Status:     "error",
		Error: &InvocationError{
			Code:      code,
			Message:   message,
			Retryable: code == "SERVICE_OVERLOADED" || code == "TIMEOUT",
		},
		DurationMs: time.Since(start).Milliseconds(),
	}

	respPayload, err := marshalPayload(resp)
	if err != nil {
		r.logger.Error("failed to marshal error response payload", "error", err)
		return
	}

	env := types.Envelope{
		ID:        types.NewUUID(),
		From:      r.agentID,
		To:        replyTo,
		Type:      types.MessageTypeCapabilityResponse,
		Timestamp: time.Now().UTC(),
		Payload:   respPayload,
	}

	if err := env.Validate(); err != nil {
		r.logger.Warn("dropping error response due to invalid envelope",
			"capability", capName,
			"error", err,
		)
		return
	}

	data, err := json.Marshal(env)
	if err != nil {
		r.logger.Error("failed to marshal error response", "error", err)
		return
	}

	if err := r.nc.Publish(msg.Reply, data); err != nil {
		r.logger.Error("failed to publish error response",
			"reply", msg.Reply,
			"error", err,
		)
	}
}
