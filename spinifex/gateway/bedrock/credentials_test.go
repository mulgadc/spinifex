package gateway_bedrock

import (
	"bytes"
	"context"
	"testing"

	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
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
