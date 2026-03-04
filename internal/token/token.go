// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

// Package token handles generation and validation of cryptographic join tokens for node authentication.
package token

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/brmurrell3/hive/internal/auth"
	"github.com/brmurrell3/hive/internal/state"
	"github.com/brmurrell3/hive/internal/types"
)

// Generate creates a new join token, stores the hash in the state store,
// and returns the raw token string (which must be given to the joining node).
// maxUses limits how many times the token can be used (0 = unlimited).
func Generate(store *state.Store, ttl time.Duration, maxUses int) (string, error) {
	raw, err := generateRandom(32)
	if err != nil {
		return "", fmt.Errorf("generating token: %w", err)
	}

	prefix := raw[:8]
	hash := auth.HashToken(raw)

	t := &types.Token{
		Prefix:    prefix,
		Hash:      hash,
		CreatedAt: time.Now().UTC(),
		MaxUses:   maxUses,
	}

	if ttl > 0 {
		t.ExpiresAt = t.CreatedAt.Add(ttl)
	}

	if err := store.AddToken(t); err != nil {
		return "", fmt.Errorf("storing token: %w", err)
	}

	return raw, nil
}

// tokenFailureDelay is a constant delay applied on token validation failure
// to mitigate timing side-channel attacks. The store's ValidateToken uses
// constant-time comparison, but returning immediately on "not found" can
// still leak timing information about whether any tokens exist.
const tokenFailureDelay = 50 * time.Millisecond

// Validate checks a raw token against the store and returns the matching token if valid.
// Failed validations are logged at Info level so production deployments can
// see failed validation attempts. A constant delay is applied on failure to
// mitigate timing side-channel attacks.
//
// Note: Intentionally uses package-level slog rather than an injected logger.
// This is acceptable here since token validation is a low-frequency operation
// and the log output is purely informational. Consider injecting a logger if
// this function is used in contexts requiring structured log routing.
func Validate(store *state.Store, rawToken string) (*types.Token, error) {
	if len(rawToken) == 0 || len(rawToken) > 1024 {
		time.Sleep(tokenFailureDelay)
		return nil, fmt.Errorf("invalid or expired token")
	}

	t := store.ValidateToken(rawToken)
	if t == nil {
		slog.Info("token validation failed: not found")
		time.Sleep(tokenFailureDelay)
		return nil, fmt.Errorf("invalid or expired token")
	}
	return t, nil
}

// generateRandom returns a hex-encoded random string of the specified byte length.
func generateRandom(byteLen int) (string, error) {
	b := make([]byte, byteLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
