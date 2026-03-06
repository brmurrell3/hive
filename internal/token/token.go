package token

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/brmurrell3/hive/internal/state"
	"github.com/brmurrell3/hive/internal/types"
)

// Generate creates a new join token, stores the hash in the state store,
// and returns the raw token string (which must be given to the joining node).
func Generate(store *state.Store, ttl time.Duration) (string, error) {
	raw, err := generateRandom(32)
	if err != nil {
		return "", fmt.Errorf("generating token: %w", err)
	}

	prefix := raw[:8]
	hash := state.HashToken(raw)

	t := &types.Token{
		Prefix:    prefix,
		Hash:      hash,
		CreatedAt: time.Now().UTC(),
	}

	if ttl > 0 {
		t.ExpiresAt = t.CreatedAt.Add(ttl)
	}

	if err := store.AddToken(t); err != nil {
		return "", fmt.Errorf("storing token: %w", err)
	}

	return raw, nil
}

// Validate checks a raw token against the store and returns the matching token if valid.
func Validate(store *state.Store, rawToken string) (*types.Token, error) {
	t := store.ValidateToken(rawToken)
	if t == nil {
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
