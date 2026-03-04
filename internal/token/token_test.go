// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build unit

package token

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/brmurrell3/hive/internal/state"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func testStore(t *testing.T) *state.Store {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store, err := state.NewStore(path, logger)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store
}

// ---------------------------------------------------------------------------
// Generate creates a valid token with prefix stored
// ---------------------------------------------------------------------------

func TestGenerate_CreatesValidToken(t *testing.T) {
	t.Parallel()
	store := testStore(t)

	raw, err := Generate(store, 24*time.Hour, 0)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Raw token should be a hex-encoded 32-byte value = 64 hex chars.
	if len(raw) != 64 {
		t.Errorf("raw token length = %d, want 64", len(raw))
	}

	// The prefix (first 8 chars) should be stored.
	tokens := store.AllTokens()
	if len(tokens) != 1 {
		t.Fatalf("expected 1 token in store, got %d", len(tokens))
	}

	prefix := raw[:8]
	if tokens[0].Prefix != prefix {
		t.Errorf("stored prefix = %q, want %q", tokens[0].Prefix, prefix)
	}

	// Hash should not be empty.
	if tokens[0].Hash == "" {
		t.Error("stored hash is empty")
	}

	// ExpiresAt should be set (since we passed a TTL).
	if tokens[0].ExpiresAt.IsZero() {
		t.Error("ExpiresAt is zero, expected non-zero with TTL")
	}

	// Token should not be revoked.
	if tokens[0].Revoked {
		t.Error("newly generated token should not be revoked")
	}

	// Token should be valid.
	if !tokens[0].IsValid() {
		t.Error("newly generated token should be valid")
	}
}

func TestGenerate_NoTTL(t *testing.T) {
	t.Parallel()
	store := testStore(t)

	raw, err := Generate(store, 0, 0)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if len(raw) == 0 {
		t.Fatal("raw token is empty")
	}

	tokens := store.AllTokens()
	if len(tokens) != 1 {
		t.Fatalf("expected 1 token in store, got %d", len(tokens))
	}

	// ExpiresAt should be zero when no TTL.
	if !tokens[0].ExpiresAt.IsZero() {
		t.Errorf("ExpiresAt should be zero with no TTL, got %v", tokens[0].ExpiresAt)
	}

	// Token without expiry should still be valid.
	if !tokens[0].IsValid() {
		t.Error("token without TTL should be valid")
	}
}

func TestGenerate_MultipleTokens(t *testing.T) {
	t.Parallel()
	store := testStore(t)

	for i := 0; i < 5; i++ {
		_, err := Generate(store, time.Hour, 0)
		if err != nil {
			t.Fatalf("Generate [%d]: %v", i, err)
		}
	}

	tokens := store.AllTokens()
	if len(tokens) != 5 {
		t.Errorf("expected 5 tokens, got %d", len(tokens))
	}

	// All prefixes should be unique.
	seen := make(map[string]bool)
	for _, tok := range tokens {
		if seen[tok.Prefix] {
			t.Errorf("duplicate prefix: %s", tok.Prefix)
		}
		seen[tok.Prefix] = true
	}
}

// ---------------------------------------------------------------------------
// Validate succeeds with correct token
// ---------------------------------------------------------------------------

func TestValidate_SucceedsWithCorrectToken(t *testing.T) {
	t.Parallel()
	store := testStore(t)

	raw, err := Generate(store, 24*time.Hour, 0)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	tok, err := Validate(store, raw)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if tok == nil {
		t.Fatal("Validate returned nil token")
	}

	if tok.Prefix != raw[:8] {
		t.Errorf("validated token prefix = %q, want %q", tok.Prefix, raw[:8])
	}
}

// ---------------------------------------------------------------------------
// Validate fails with wrong token
// ---------------------------------------------------------------------------

func TestValidate_FailsWithWrongToken(t *testing.T) {
	t.Parallel()
	store := testStore(t)

	_, err := Generate(store, 24*time.Hour, 0)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	tests := []struct {
		name  string
		token string
	}{
		{"completely wrong token", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{"empty token", ""},
		{"partial match", "abcdef"},
		{"random garbage", "not-a-valid-hex-token-at-all"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tok, err := Validate(store, tt.token)
			if err == nil {
				t.Error("expected error for wrong token, got nil")
			}
			if tok != nil {
				t.Errorf("expected nil token for wrong token, got %+v", tok)
			}
		})
	}
}

func TestValidate_FailsWithNoTokensStored(t *testing.T) {
	t.Parallel()
	store := testStore(t)

	tok, err := Validate(store, "some-token-value")
	if err == nil {
		t.Error("expected error with no tokens stored, got nil")
	}
	if tok != nil {
		t.Error("expected nil token with no tokens stored")
	}
}

// ---------------------------------------------------------------------------
// Expired token is rejected
// ---------------------------------------------------------------------------

func TestValidate_ExpiredTokenRejected(t *testing.T) {
	t.Parallel()
	store := testStore(t)

	// Generate with a very short TTL.
	raw, err := Generate(store, 1*time.Millisecond, 0)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Wait for the token to expire.
	time.Sleep(5 * time.Millisecond)

	tok, err := Validate(store, raw)
	if err == nil {
		t.Error("expected error for expired token, got nil")
	}
	if tok != nil {
		t.Error("expected nil token for expired token")
	}
}

// ---------------------------------------------------------------------------
// Revoked token is rejected
// ---------------------------------------------------------------------------

func TestValidate_RevokedTokenRejected(t *testing.T) {
	t.Parallel()
	store := testStore(t)

	raw, err := Generate(store, 24*time.Hour, 0)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Revoke the token by its prefix.
	prefix := raw[:8]
	if err := store.RevokeToken(prefix); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}

	tok, err := Validate(store, raw)
	if err == nil {
		t.Error("expected error for revoked token, got nil")
	}
	if tok != nil {
		t.Error("expected nil token for revoked token")
	}
}

// ---------------------------------------------------------------------------
// Generate produces unique tokens
// ---------------------------------------------------------------------------

func TestGenerate_ProducesUniqueTokens(t *testing.T) {
	t.Parallel()
	store := testStore(t)

	tokens := make(map[string]bool)
	for i := 0; i < 10; i++ {
		raw, err := Generate(store, time.Hour, 0)
		if err != nil {
			t.Fatalf("Generate [%d]: %v", i, err)
		}
		if tokens[raw] {
			t.Fatalf("duplicate token generated at iteration %d", i)
		}
		tokens[raw] = true
	}
}
