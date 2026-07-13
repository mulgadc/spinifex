package gateway_bedrock

import (
	"bytes"
	"context"
	"testing"

	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixedMasterKey returns a deterministic 32-byte AES-256 key for tests.
func fixedMasterKey() []byte {
	return bytes.Repeat([]byte{0x42}, 32)
}

// TestCredentialStore_Resolve_NilJS covers the platform-default and
// vendor-miss paths of a store built with a nil JetStream context (as a
// live NATS server is out of scope here). Resolve must consult
// platformDefaults without dereferencing js.
func TestCredentialStore_Resolve_NilJS(t *testing.T) {
	store := NewCredentialStore(nil, fixedMasterKey(), 1, map[string]string{"anthropic": "sk-platform-default"})

	cases := []struct {
		name    string
		vendor  string
		wantKey string
		wantOK  bool
	}{
		{"platform default hit", "anthropic", "sk-platform-default", true},
		{"vendor miss", "openai", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key, ok, err := store.Resolve(context.Background(), "000000000001", tc.vendor)
			require.NoError(t, err)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantKey, key)
		})
	}
}

func TestCredentialStore_Resolve_NoPlatformDefaults(t *testing.T) {
	store := NewCredentialStore(nil, fixedMasterKey(), 1, nil)

	key, ok, err := store.Resolve(context.Background(), "000000000001", "anthropic")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Empty(t, key)
}

func TestNoopCredentialResolver_ResolvesNothing(t *testing.T) {
	key, ok, err := NoopCredentialResolver.Resolve(context.Background(), "000000000001", "anthropic")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Empty(t, key)
}

// TestCredentialStore_PutAndResolve_KV exercises the real JetStream KV path:
// bucket (lazy create), credentialKey, PutCredential (encrypt+Put), and the
// KV-hit branch of Resolve (Get+decrypt).
func TestCredentialStore_PutAndResolve_KV(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	store := NewCredentialStore(js, fixedMasterKey(), 1, nil)

	ctx := context.Background()
	require.NoError(t, store.PutCredential(ctx, "000000000001", "anthropic", "sk-test"))

	key, ok, err := store.Resolve(ctx, "000000000001", "anthropic")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "sk-test", key)
}

// TestCredentialStore_Resolve_KVMissFallsBackToPlatformDefault covers a KV
// miss (nats.ErrKeyNotFound) that falls through to the platform default.
func TestCredentialStore_Resolve_KVMissFallsBackToPlatformDefault(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	store := NewCredentialStore(js, fixedMasterKey(), 1, map[string]string{"anthropic": "sk-platform-default"})

	key, ok, err := store.Resolve(context.Background(), "999999999999", "anthropic")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "sk-platform-default", key)
}

// TestCredentialStore_Resolve_KVMissNoPlatformDefault covers a KV miss on an
// unknown account with no platform defaults configured: ("", false, nil).
func TestCredentialStore_Resolve_KVMissNoPlatformDefault(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	store := NewCredentialStore(js, fixedMasterKey(), 1, nil)

	key, ok, err := store.Resolve(context.Background(), "999999999999", "anthropic")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Empty(t, key)
}

func TestCredentialKey(t *testing.T) {
	assert.Equal(t, "providers/000000000001/anthropic", credentialKey("000000000001", "anthropic"))
}

func TestEncryptDecryptSecret_RoundTrip(t *testing.T) {
	key := fixedMasterKey()
	plaintext := "sk-ant-test-secret"

	ciphertext, err := handlers_iam.EncryptSecret(plaintext, key)
	require.NoError(t, err)
	assert.NotEqual(t, plaintext, ciphertext)

	decrypted, err := handlers_iam.DecryptSecret(ciphertext, key)
	require.NoError(t, err)
	assert.Equal(t, plaintext, decrypted)
}
