// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build unit

package types

import (
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// ClassifyTier tests
// ---------------------------------------------------------------------------

func TestClassifyTier(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		resources NodeResources
		want      NodeTier
	}{
		{
			name: "KVM available with 4GB = Tier 1",
			resources: NodeResources{
				KVMAvail:    true,
				MemoryTotal: 4 * 1024 * 1024 * 1024, // exactly 4 GB
				CPUCount:    4,
			},
			want: NodeTier1,
		},
		{
			name: "KVM available with 8GB = Tier 1",
			resources: NodeResources{
				KVMAvail:    true,
				MemoryTotal: 8 * 1024 * 1024 * 1024,
				CPUCount:    8,
			},
			want: NodeTier1,
		},
		{
			name: "KVM available with 16GB = Tier 1",
			resources: NodeResources{
				KVMAvail:    true,
				MemoryTotal: 16 * 1024 * 1024 * 1024,
				CPUCount:    16,
			},
			want: NodeTier1,
		},
		{
			name: "no KVM with 4GB = Tier 2",
			resources: NodeResources{
				KVMAvail:    false,
				MemoryTotal: 4 * 1024 * 1024 * 1024,
				CPUCount:    4,
			},
			want: NodeTier2,
		},
		{
			name: "no KVM with 8GB = Tier 2",
			resources: NodeResources{
				KVMAvail:    false,
				MemoryTotal: 8 * 1024 * 1024 * 1024,
				CPUCount:    8,
			},
			want: NodeTier2,
		},
		{
			name: "KVM available with less than 4GB = Tier 2",
			resources: NodeResources{
				KVMAvail:    true,
				MemoryTotal: 2 * 1024 * 1024 * 1024,
				CPUCount:    2,
			},
			want: NodeTier2,
		},
		{
			name: "KVM with 3.9GB = Tier 2",
			resources: NodeResources{
				KVMAvail:    true,
				MemoryTotal: 4*1024*1024*1024 - 1, // one byte short of 4GB
				CPUCount:    4,
			},
			want: NodeTier2,
		},
		{
			name: "no KVM with 0 memory = Tier 2",
			resources: NodeResources{
				KVMAvail:    false,
				MemoryTotal: 0,
				CPUCount:    0,
			},
			want: NodeTier2,
		},
		{
			name:      "very low memory 256KB = Tier 3 (microcontroller)",
			resources: NodeResources{MemoryTotal: 256 * 1024, CPUCount: 1},
			want:      NodeTier3,
		},
		{
			name:      "512KB boundary = Tier 2 (not Tier 3)",
			resources: NodeResources{MemoryTotal: 512 * 1024, CPUCount: 1},
			want:      NodeTier2,
		},
		{
			name:      "just under 512KB = Tier 3",
			resources: NodeResources{MemoryTotal: 512*1024 - 1, CPUCount: 1},
			want:      NodeTier3,
		},
		{
			name: "no KVM with 1GB = Tier 2 (Pi-like)",
			resources: NodeResources{
				KVMAvail:    false,
				MemoryTotal: 1 * 1024 * 1024 * 1024,
				CPUCount:    4,
			},
			want: NodeTier2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ClassifyTier(tt.resources)
			if got != tt.want {
				t.Errorf("ClassifyTier(%+v) = %d, want %d", tt.resources, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Token.IsExpired tests
// ---------------------------------------------------------------------------

func TestToken_IsExpired(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		expiresAt time.Time
		want      bool
	}{
		{
			name:      "zero ExpiresAt is never expired",
			expiresAt: time.Time{},
			want:      false,
		},
		{
			name:      "future ExpiresAt is not expired",
			expiresAt: time.Now().Add(24 * time.Hour),
			want:      false,
		},
		{
			name:      "past ExpiresAt is expired",
			expiresAt: time.Now().Add(-1 * time.Second),
			want:      true,
		},
		{
			name:      "far past ExpiresAt is expired",
			expiresAt: time.Now().Add(-24 * time.Hour),
			want:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tok := Token{
				Prefix:    "test1234",
				Hash:      "somehash",
				CreatedAt: time.Now().Add(-1 * time.Hour),
				ExpiresAt: tt.expiresAt,
			}
			got := tok.IsExpired()
			if got != tt.want {
				t.Errorf("IsExpired() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Token.IsValid tests
// ---------------------------------------------------------------------------

func TestToken_IsValid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		expiresAt time.Time
		revoked   bool
		want      bool
	}{
		{
			name:      "valid token: not expired, not revoked",
			expiresAt: time.Now().Add(24 * time.Hour),
			revoked:   false,
			want:      true,
		},
		{
			name:      "valid token: no expiry, not revoked",
			expiresAt: time.Time{},
			revoked:   false,
			want:      true,
		},
		{
			name:      "invalid: expired",
			expiresAt: time.Now().Add(-1 * time.Second),
			revoked:   false,
			want:      false,
		},
		{
			name:      "invalid: revoked",
			expiresAt: time.Now().Add(24 * time.Hour),
			revoked:   true,
			want:      false,
		},
		{
			name:      "invalid: both expired and revoked",
			expiresAt: time.Now().Add(-1 * time.Hour),
			revoked:   true,
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tok := Token{
				Prefix:    "test1234",
				Hash:      "somehash",
				CreatedAt: time.Now().Add(-1 * time.Hour),
				ExpiresAt: tt.expiresAt,
				Revoked:   tt.revoked,
			}
			got := tok.IsValid()
			if got != tt.want {
				t.Errorf("IsValid() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// NodeTier constants
// ---------------------------------------------------------------------------

func TestNodeTierConstants(t *testing.T) {
	t.Parallel()
	if NodeTierUnknown != 0 {
		t.Errorf("NodeTierUnknown = %d, want 0", NodeTierUnknown)
	}
	if NodeTier1 != 1 {
		t.Errorf("NodeTier1 = %d, want 1", NodeTier1)
	}
	if NodeTier2 != 2 {
		t.Errorf("NodeTier2 = %d, want 2", NodeTier2)
	}
	if NodeTier3 != 3 {
		t.Errorf("NodeTier3 = %d, want 3", NodeTier3)
	}
}

// ---------------------------------------------------------------------------
// NodeStatus constants
// ---------------------------------------------------------------------------

func TestNodeStatusConstants(t *testing.T) {
	t.Parallel()
	tests := []struct {
		status NodeStatus
		want   string
	}{
		{NodeStatusOnline, "online"},
		{NodeStatusOffline, "offline"},
		{NodeStatusPending, "pending"},
		{NodeStatusDraining, "draining"},
		{NodeStatusCordoned, "cordoned"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			t.Parallel()
			if string(tt.status) != tt.want {
				t.Errorf("NodeStatus = %q, want %q", tt.status, tt.want)
			}
		})
	}
}
