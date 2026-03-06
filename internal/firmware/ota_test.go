//go:build unit

package firmware

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestComputeSHA256(t *testing.T) {
	t.Helper()

	tests := []struct {
		name string
		data []byte
	}{
		{"empty", []byte{}},
		{"hello", []byte("hello world")},
		{"binary", []byte{0x00, 0xFF, 0x7F, 0x80, 0x01}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeSHA256(tt.data)

			// Verify against standard library directly.
			expected := sha256.Sum256(tt.data)
			want := hex.EncodeToString(expected[:])

			if got != want {
				t.Errorf("ComputeSHA256(%q) = %q, want %q", tt.data, got, want)
			}

			// Hash should be 64 hex characters.
			if len(got) != 64 {
				t.Errorf("expected 64-char hex hash, got %d chars", len(got))
			}
		})
	}
}

func TestComputeSHA256_Deterministic(t *testing.T) {
	t.Helper()

	data := []byte("deterministic test data 12345")
	hash1 := ComputeSHA256(data)
	hash2 := ComputeSHA256(data)

	if hash1 != hash2 {
		t.Errorf("same data should produce same hash: %q != %q", hash1, hash2)
	}
}

func TestSplitChunks(t *testing.T) {
	t.Helper()

	tests := []struct {
		name       string
		dataLen    int
		chunkSize  int
		wantChunks int
		lastSize   int
	}{
		{"exact multiple", 12, 4, 3, 4},
		{"with remainder", 10, 4, 3, 2},
		{"single chunk", 3, 4, 1, 3},
		{"exact single", 4, 4, 1, 4},
		{"large data default chunk", 10000, DefaultChunkSize, 3, 10000 - 2*DefaultChunkSize},
		{"empty data", 0, 4, 0, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := make([]byte, tt.dataLen)
			for i := range data {
				data[i] = byte(i % 256)
			}

			chunks := SplitChunks(data, tt.chunkSize)

			if len(chunks) != tt.wantChunks {
				t.Errorf("expected %d chunks, got %d", tt.wantChunks, len(chunks))
			}

			if tt.wantChunks > 0 && len(chunks[len(chunks)-1]) != tt.lastSize {
				t.Errorf("expected last chunk size %d, got %d", tt.lastSize, len(chunks[len(chunks)-1]))
			}

			// Verify reassembly.
			var reassembled []byte
			for _, c := range chunks {
				reassembled = append(reassembled, c...)
			}
			if len(reassembled) != tt.dataLen {
				t.Errorf("reassembled data length %d != original %d", len(reassembled), tt.dataLen)
			}
			for i := range data {
				if reassembled[i] != data[i] {
					t.Errorf("reassembled data differs at index %d", i)
					break
				}
			}
		})
	}
}

func TestSplitChunks_DefaultChunkSize(t *testing.T) {
	t.Helper()

	data := make([]byte, DefaultChunkSize*2+100)
	chunks := SplitChunks(data, 0) // 0 should use default

	if len(chunks) != 3 {
		t.Errorf("expected 3 chunks with default size, got %d", len(chunks))
	}
	if len(chunks[0]) != DefaultChunkSize {
		t.Errorf("expected first chunk %d bytes, got %d", DefaultChunkSize, len(chunks[0]))
	}
	if len(chunks[2]) != 100 {
		t.Errorf("expected last chunk 100 bytes, got %d", len(chunks[2]))
	}
}

func TestSplitChunks_IndependentCopies(t *testing.T) {
	t.Helper()

	data := []byte("abcdefghij")
	chunks := SplitChunks(data, 3)

	// Modify original data and verify chunks are unaffected.
	data[0] = 'X'
	if chunks[0][0] == 'X' {
		t.Error("chunks should be independent copies of the data")
	}
}

func TestGenerateManifest(t *testing.T) {
	t.Helper()

	tests := []struct {
		name        string
		dataLen     int
		version     string
		chunkSize   int
		wantChunks  int
	}{
		{"standard", 10000, "1.0.0", DefaultChunkSize, 3},
		{"exact", 8192, "2.0.0", DefaultChunkSize, 2},
		{"small", 100, "0.1.0", DefaultChunkSize, 1},
		{"custom chunk", 10000, "1.0.0", 1000, 10},
		{"with remainder", 10001, "1.0.0", 1000, 11},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := make([]byte, tt.dataLen)
			for i := range data {
				data[i] = byte(i % 256)
			}

			manifest := GenerateManifest(data, tt.version, tt.chunkSize)

			if manifest.FirmwareVersion != tt.version {
				t.Errorf("expected version %q, got %q", tt.version, manifest.FirmwareVersion)
			}

			if manifest.TotalSize != tt.dataLen {
				t.Errorf("expected total size %d, got %d", tt.dataLen, manifest.TotalSize)
			}

			if manifest.TotalChunks != tt.wantChunks {
				t.Errorf("expected %d chunks, got %d", tt.wantChunks, manifest.TotalChunks)
			}

			expectedChunkSize := tt.chunkSize
			if expectedChunkSize <= 0 {
				expectedChunkSize = DefaultChunkSize
			}
			if manifest.ChunkSize != expectedChunkSize {
				t.Errorf("expected chunk size %d, got %d", expectedChunkSize, manifest.ChunkSize)
			}

			expectedHash := ComputeSHA256(data)
			if manifest.SHA256 != expectedHash {
				t.Errorf("expected hash %q, got %q", expectedHash, manifest.SHA256)
			}
		})
	}
}

func TestGenerateManifest_DefaultChunkSize(t *testing.T) {
	t.Helper()

	data := make([]byte, 5000)
	manifest := GenerateManifest(data, "1.0.0", 0)

	if manifest.ChunkSize != DefaultChunkSize {
		t.Errorf("expected default chunk size %d, got %d", DefaultChunkSize, manifest.ChunkSize)
	}
}

func TestSplitChunks_AllChunksSizeValid(t *testing.T) {
	t.Helper()

	data := make([]byte, 10000)
	chunkSize := 3000
	chunks := SplitChunks(data, chunkSize)

	for i, c := range chunks {
		if i < len(chunks)-1 {
			// All chunks except the last must be exactly chunkSize.
			if len(c) != chunkSize {
				t.Errorf("chunk %d: expected size %d, got %d", i, chunkSize, len(c))
			}
		} else {
			// Last chunk can be <= chunkSize.
			if len(c) > chunkSize || len(c) == 0 {
				t.Errorf("last chunk: expected 1-%d bytes, got %d", chunkSize, len(c))
			}
		}
	}
}
