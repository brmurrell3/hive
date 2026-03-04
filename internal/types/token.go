package types

import "time"

// Token represents a join token stored in state.
// The raw token is never stored; only the SHA-256 hash.
type Token struct {
	Prefix    string    `json:"prefix"`
	Hash      string    `json:"hash"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	LastUsed  time.Time `json:"last_used,omitempty"`
	Revoked   bool      `json:"revoked"`
}

// IsExpired returns true if the token has a TTL and has expired.
func (t Token) IsExpired() bool {
	if t.ExpiresAt.IsZero() {
		return false
	}
	return time.Now().After(t.ExpiresAt)
}

// IsValid returns true if the token is not expired and not revoked.
func (t Token) IsValid() bool {
	return !t.Revoked && !t.IsExpired()
}
