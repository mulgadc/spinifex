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

// TestVersionStateMachine covers unset→0, first write, idempotent same write, upgrade, and no-downgrade.
// One bucket is reused so each step runs against the prior state — the only way to catch unconditional-overwrite regressions.
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

// streamReplicas returns the replica count of the JetStream stream backing a
// KV bucket, so tests can assert on the config actually sent to the server.
func streamReplicas(t *testing.T, js nats.JetStreamContext, bucket string) int {
	t.Helper()
	si, err := js.StreamInfo("KV_" + bucket)
	require.NoError(t, err)
	return si.Config.Replicas
}

func TestGetOrCreateKVBucket_CreatesAtReplicaOne(t *testing.T) {
	_, js := startJSNATSServer(t)

	kv, err := GetOrCreateKVBucket(js, "regression-bucket", 5)
	require.NoError(t, err)
	require.NotNil(t, kv)
	assert.Equal(t, 1, streamReplicas(t, js, "regression-bucket"))
}

func TestGetOrCreateKVBucketWithReplicas_CreatesAtRequestedReplicas(t *testing.T) {
	_, js := startJSNATSServer(t)

	// The embedded single-node test server rejects Replicas > 1
	// ("replicas > 1 not supported in non-clustered mode"), so the only
	// replica count exercisable end-to-end here is 1; the clamping logic
	// (max(replicas, 1)) is covered separately below.
	kv, err := GetOrCreateKVBucketWithReplicas(js, "replicated-bucket", 5, 1)
	require.NoError(t, err)
	require.NotNil(t, kv)
	assert.Equal(t, 1, streamReplicas(t, js, "replicated-bucket"))
}

func TestGetOrCreateKVBucketWithReplicas_ClampsBelowOne(t *testing.T) {
	_, js := startJSNATSServer(t)

	kv, err := GetOrCreateKVBucketWithReplicas(js, "clamped-zero", 1, 0)
	require.NoError(t, err)
	require.NotNil(t, kv)
	assert.Equal(t, 1, streamReplicas(t, js, "clamped-zero"))

	kv, err = GetOrCreateKVBucketWithReplicas(js, "clamped-negative", 1, -3)
	require.NoError(t, err)
	require.NotNil(t, kv)
	assert.Equal(t, 1, streamReplicas(t, js, "clamped-negative"))
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
