package capability

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hivehq/hive/internal/types"
	"github.com/nats-io/nats.go"
)

// Handler is a function that handles a capability invocation.
type Handler func(inputs map[string]interface{}) (map[string]interface{}, error)

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
}

// NewRouter creates a new capability Router for the given agent.
func NewRouter(agentID string, nc *nats.Conn, logger *slog.Logger) *Router {
	return &Router{
		nc:       nc,
		logger:   logger,
		handlers: make(map[string]Handler),
		agentID:  agentID,
	}
}

// RegisterHandler registers a handler function for the named capability.
// Must be called before Start.
func (r *Router) RegisterHandler(capability string, handler Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[capability] = handler
	r.logger.Info("registered capability handler",
		"agent_id", r.agentID,
		"capability", capability,
	)
}

// Start subscribes to NATS subjects for each registered capability.
// For each capability, it subscribes to:
//
//	hive.capabilities.{agentID}.{capability}.request
func (r *Router) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	for cap, handler := range r.handlers {
		subject := fmt.Sprintf("hive.capabilities.%s.%s.request", r.agentID, cap)

		// Capture loop variables for the closure.
		capName := cap
		capHandler := handler

		sub, err := r.nc.Subscribe(subject, func(msg *nats.Msg) {
			r.handleRequest(msg, capName, capHandler)
		})
		if err != nil {
			// Unsubscribe any already-created subscriptions on failure.
			for _, s := range r.subs {
				_ = s.Unsubscribe()
			}
			r.subs = nil
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

// Stop unsubscribes from all capability subjects.
func (r *Router) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, sub := range r.subs {
		if err := sub.Unsubscribe(); err != nil {
			r.logger.Warn("error unsubscribing capability subject",
				"subject", sub.Subject,
				"error", err,
			)
		}
	}

	r.subs = nil
	r.logger.Info("capability router stopped", "agent_id", r.agentID)
}

// Invoke calls a capability on a remote agent and waits for the response.
// It publishes an InvocationRequest wrapped in an Envelope to
// hive.capabilities.{targetAgentID}.{capability}.request and waits for
// the response on an auto-created reply subject.
func (r *Router) Invoke(targetAgentID, capability string, inputs map[string]interface{}, timeout time.Duration) (*InvocationResponse, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	subject := fmt.Sprintf("hive.capabilities.%s.%s.request", targetAgentID, capability)

	req := InvocationRequest{
		Capability: capability,
		Inputs:     inputs,
		Timeout:    timeout.String(),
	}

	env := types.Envelope{
		ID:        newUUID(),
		From:      r.agentID,
		To:        targetAgentID,
		Type:      types.MessageTypeCapabilityRequest,
		Timestamp: time.Now().UTC(),
		Payload:   req,
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
	msg, err := r.nc.Request(subject, data, timeout)
	if err != nil {
		if err == nats.ErrTimeout {
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

	// Unmarshal the response envelope.
	var respEnv types.Envelope
	if err := json.Unmarshal(msg.Data, &respEnv); err != nil {
		return nil, fmt.Errorf("unmarshaling response envelope: %w", err)
	}

	// The Payload in the envelope is a generic interface{}, so re-marshal
	// and unmarshal to get the typed InvocationResponse.
	payloadBytes, err := json.Marshal(respEnv.Payload)
	if err != nil {
		return nil, fmt.Errorf("re-marshaling response payload: %w", err)
	}

	var resp InvocationResponse
	if err := json.Unmarshal(payloadBytes, &resp); err != nil {
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

// handleRequest processes an incoming capability request message.
func (r *Router) handleRequest(msg *nats.Msg, capName string, handler Handler) {
	start := time.Now()

	r.logger.Info("received capability request",
		"capability", capName,
		"subject", msg.Subject,
		"reply", msg.Reply,
		"size", len(msg.Data),
	)

	// Unmarshal the incoming envelope.
	var env types.Envelope
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		r.logger.Error("failed to unmarshal capability request envelope",
			"capability", capName,
			"error", err,
		)
		r.publishErrorResponse(msg, capName, "INVALID_REQUEST", "failed to unmarshal request envelope", start)
		return
	}

	// Extract the InvocationRequest from the payload.
	payloadBytes, err := json.Marshal(env.Payload)
	if err != nil {
		r.logger.Error("failed to re-marshal request payload",
			"capability", capName,
			"error", err,
		)
		r.publishErrorResponse(msg, capName, "INVALID_PAYLOAD", "failed to process request payload", start)
		return
	}

	var req InvocationRequest
	if err := json.Unmarshal(payloadBytes, &req); err != nil {
		r.logger.Error("failed to unmarshal invocation request",
			"capability", capName,
			"error", err,
		)
		r.publishErrorResponse(msg, capName, "INVALID_PAYLOAD", "failed to unmarshal invocation request", start)
		return
	}

	// Call the registered handler.
	outputs, handlerErr := handler(req.Inputs)
	durationMs := time.Since(start).Milliseconds()

	var resp InvocationResponse
	if handlerErr != nil {
		r.logger.Warn("capability handler returned error",
			"capability", capName,
			"error", handlerErr,
			"duration_ms", durationMs,
		)
		resp = InvocationResponse{
			Capability: capName,
			Status:     "error",
			Error: &InvocationError{
				Code:      "HANDLER_ERROR",
				Message:   handlerErr.Error(),
				Retryable: false,
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
	}

	// Wrap the response in an envelope and publish to the reply subject.
	respEnv := types.Envelope{
		ID:            newUUID(),
		From:          r.agentID,
		To:            env.From,
		Type:          types.MessageTypeCapabilityResponse,
		Timestamp:     time.Now().UTC(),
		Payload:       resp,
		CorrelationID: env.ID,
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

// publishErrorResponse sends an error response envelope to the reply subject.
func (r *Router) publishErrorResponse(msg *nats.Msg, capName, code, message string, start time.Time) {
	if msg.Reply == "" {
		return
	}

	resp := InvocationResponse{
		Capability: capName,
		Status:     "error",
		Error: &InvocationError{
			Code:      code,
			Message:   message,
			Retryable: false,
		},
		DurationMs: time.Since(start).Milliseconds(),
	}

	env := types.Envelope{
		ID:        newUUID(),
		From:      r.agentID,
		Type:      types.MessageTypeCapabilityResponse,
		Timestamp: time.Now().UTC(),
		Payload:   resp,
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

// newUUID generates a UUID v4 string using crypto/rand.
func newUUID() string {
	var uuid [16]byte
	if _, err := rand.Read(uuid[:]); err != nil {
		// crypto/rand should never fail on supported platforms.
		// Fall back to a zero UUID rather than panicking.
		return "00000000-0000-0000-0000-000000000000"
	}

	// Set version 4 (bits 12-15 of time_hi_and_version).
	uuid[6] = (uuid[6] & 0x0f) | 0x40
	// Set variant to RFC 4122 (bits 6-7 of clock_seq_hi_and_reserved).
	uuid[8] = (uuid[8] & 0x3f) | 0x80

	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		uuid[0:4],
		uuid[4:6],
		uuid[6:8],
		uuid[8:10],
		uuid[10:16],
	)
}
