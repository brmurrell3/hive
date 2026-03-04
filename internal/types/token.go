// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package types

import "time"

// Token represents a join token stored in state.
// The raw token is never stored; only the SHA-256 hash.
type Token struct {
	Prefix     string    `json:"prefix"`
	Hash       string    `json:"hash"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at,omitempty"`
	LastUsed   time.Time `json:"last_used,omitempty"`
	Revoked    bool      `json:"revoked"`
	MaxUses    int       `json:"max_uses,omitempty"`
	UsageCount int       `json:"usage_count,omitempty"`
}

// IsExpired returns true if the token has a TTL and has expired.
// Note: Uses wall-clock time. NTP adjustments could briefly affect expiry checks.
func (t Token) IsExpired() bool {
	if t.ExpiresAt.IsZero() {
		return false
	}
	return time.Now().After(t.ExpiresAt)
}

// IsValid returns true if the token is not expired, not revoked, and has
// remaining uses (if MaxUses is set). MaxUses of 0 means unlimited.
func (t Token) IsValid() bool {
	if t.Revoked || t.IsExpired() {
		return false
	}
	if t.MaxUses > 0 && t.UsageCount >= t.MaxUses {
		return false
	}
	return true
}
