package utils

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAccountKey(t *testing.T) {
	tests := []struct {
		accountID  string
		resourceID string
		want       string
	}{
		{"000000000000", "vpc-123", "000000000000.vpc-123"},
		{"123456789012", "igw-abc", "123456789012.igw-abc"},
		{"", "vol-1", ".vol-1"},
	}
	for _, tt := range tests {
		got := AccountKey(tt.accountID, tt.resourceID)
		if got != tt.want {
			t.Errorf("AccountKey(%q, %q) = %q, want %q", tt.accountID, tt.resourceID, got, tt.want)
		}
	}
}

func TestIsAccountID(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"000000000000", true},
		{"123456789012", true},
		{"999999999999", true},
		{"self", false},
		{"spinifex", false},
		{"", false},
		{"12345678901", false},   // 11 digits
		{"1234567890123", false}, // 13 digits
		{"12345678901a", false},  // non-digit
		{"abcdefghijkl", false},
	}
	for _, tt := range tests {
		got := IsAccountID(tt.input)
		if got != tt.want {
			t.Errorf("IsAccountID(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestGlobalAccountID(t *testing.T) {
	if GlobalAccountID != "000000000000" {
		t.Errorf("GlobalAccountID = %q, want %q", GlobalAccountID, "000000000000")
	}
	if !IsAccountID(GlobalAccountID) {
		t.Error("GlobalAccountID should be a valid account ID")
	}
}

// startJSNATSServer starts an embedded JetStream-enabled NATS server for testing.
func startJSNATSServer(t *testing.T) (*nats.Conn, nats.JetStreamContext) {
	t.Helper()
	_, nc, js := testutil.StartTestJetStream(t)
	return nc, js
}

// TestVersionStateMachine walks WriteVersion/ReadVersion through every state
// transition the migration helper guarantees:
//   - unset → ReadVersion returns 0 with no error
//   - first write persists the value
//   - same version is a no-op (idempotent)
//   - higher version replaces the stored value (upgrade)
//   - lower version is a no-op (no downgrade)
//
// One bucket is reused across steps so each Write is exercised against the
// state the prior Write left behind — that's the only way to catch a
// regression that, say, made WriteVersion unconditionally overwrite.
func TestVersionStateMachine(t *testing.T) {
	_, js := startJSNATSServer(t)
	kv, err := js.CreateKeyValue(&nats.KeyValueConfig{Bucket: "test-version-fsm"})
	require.NoError(t, err)

	// Unset → 0.
	v, err := ReadVersion(kv)
	require.NoError(t, err)
	assert.Equal(t, 0, v, "ReadVersion on unset bucket")

	steps := []struct {
		name  string
		write int
		want  int // expected ReadVersion after the write
	}{
		{"first write persists", 1, 1},
		{"same version is no-op", 1, 1},
		{"higher version upgrades", 2, 2},
		{"lower version is no-op (no downgrade)", 1, 2},
		{"larger jump upgrades", 5, 5},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			require.NoError(t, WriteVersion(kv, step.write))
			v, err := ReadVersion(kv)
			require.NoError(t, err)
			assert.Equal(t, step.want, v)
		})
	}

	// Round-trip the raw KV value to confirm the encoding is what readers
	// outside this package would expect.
	entry, err := kv.Get(VersionKey)
	require.NoError(t, err)
	assert.Equal(t, "5", string(entry.Value()))
}

func TestDeleteKVBucketIfExists(t *testing.T) {
	_, js := startJSNATSServer(t)

	// Missing bucket is a no-op.
	require.NoError(t, DeleteKVBucketIfExists(js, "ghost-bucket"))

	// Existing bucket gets deleted.
	_, err := js.CreateKeyValue(&nats.KeyValueConfig{Bucket: "doomed-bucket"})
	require.NoError(t, err)
	require.NoError(t, DeleteKVBucketIfExists(js, "doomed-bucket"))

	_, err = js.KeyValue("doomed-bucket")
	require.Error(t, err, "bucket should be gone")

	// Calling again on the now-missing bucket is still a no-op (idempotent).
	require.NoError(t, DeleteKVBucketIfExists(js, "doomed-bucket"))
}
