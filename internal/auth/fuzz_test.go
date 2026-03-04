// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build unit

package auth

import (
	"log/slog"
	"os"
	"testing"
)

func FuzzAuthenticate(f *testing.F) {
	// Valid token (will match the test user)
	f.Add("valid-token-abc123")

	// Invalid tokens
	f.Add("")
	f.Add("wrong-token")
	f.Add("a")
	f.Add(string(make([]byte, 10000))) // very long token

	// Injection / boundary attempts from previous audit passes
	f.Add("\x00")                        // null byte
	f.Add("\n")                          // newline
	f.Add("token with spaces")           // spaces
	f.Add("token\ttab")                  // tab
	f.Add("../../../etc/passwd")         // path traversal (shouldn't matter but test anyway)
	f.Add("admin")                       // common word
	f.Add("valid-token-abc123\x00extra") // null byte after valid token

	f.Fuzz(func(t *testing.T, token string) {
		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

		// Set up authorizer with a known user
		knownToken := "valid-token-abc123"
		users := []User{
			{
				ID:        "test-user",
				Name:      "Test User",
				Role:      RoleAdmin,
				TokenHash: HashToken(knownToken),
				Teams:     []string{"team1"},
			},
		}
		auth := NewAuthorizer(users, logger)

		// Must not panic
		user, err := auth.Authenticate(token)

		if err != nil {
			// Invariant: error message must always be generic "invalid credentials"
			// (never leak info about why auth failed)
			if err.Error() != "invalid credentials" {
				t.Errorf("Authenticate(%q) error = %q, want %q", token, err.Error(), "invalid credentials")
			}
			// Invariant: user must be nil on error
			if user != nil {
				t.Errorf("Authenticate(%q) returned non-nil user with error", token)
			}
			return
		}

		// Invariant: successful auth must return valid user
		if user == nil {
			t.Errorf("Authenticate(%q) returned nil user without error", token)
			return
		}
		if user.ID == "" {
			t.Errorf("Authenticate(%q) returned user with empty ID", token)
		}
		if !validRoles[user.Role] {
			t.Errorf("Authenticate(%q) returned user with invalid role %q", token, user.Role)
		}
	})
}
