// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package protocol

import (
	"encoding/json"
	"fmt"

	"github.com/brmurrell3/hive/internal/state"
)

// CtlRequest is the wire-format payload sent from hivectl to hived
// on control subjects.
type CtlRequest struct {
	AgentID string `json:"agent_id"`
}

// CtlResponse is the wire-format payload returned from hived to hivectl
// on control subjects.
type CtlResponse struct {
	Success bool                `json:"success"`
	Error   string              `json:"error,omitempty"`
	Agent   *state.AgentState   `json:"agent,omitempty"`
	Agents  []*state.AgentState `json:"agents,omitempty"`
	Data    json.RawMessage     `json:"data,omitempty"`
}

// Err returns a non-nil error if the response indicates failure.
func (r *CtlResponse) Err() error {
	if !r.Success {
		return fmt.Errorf("%s", r.Error)
	}
	return nil
}
