package function

import (
	"testing"

	"github.com/mulgadc/bluebottle/pkg/masterkey"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testAEAD(t *testing.T) *masterkey.Key {
	t.Helper()
	k, err := masterkey.New(make([]byte, 32))
	require.NoError(t, err)
	return k
}

func TestEnv_RoundTripAndNoPlaintext(t *testing.T) {
	k := testAEAD(t)
	vars := map[string]string{"DB_PASSWORD": "hunter2", "MODE": "prod"}

	sealed, err := SealEnv(k.AEAD, "000000000001", "fn1", vars)
	require.NoError(t, err)
	assert.NotContains(t, sealed, "hunter2")

	got, err := OpenEnv(k.AEAD, "000000000001", "fn1", sealed)
	require.NoError(t, err)
	assert.Equal(t, vars, got)

	again, err := SealEnv(k.AEAD, "000000000001", "fn1", vars)
	require.NoError(t, err)
	assert.NotEqual(t, sealed, again, "each seal draws a fresh nonce")
}

func TestEnv_BoundToAccountAndFunction(t *testing.T) {
	k := testAEAD(t)
	sealed, err := SealEnv(k.AEAD, "000000000001", "fn1", map[string]string{"A": "b"})
	require.NoError(t, err)

	_, err = OpenEnv(k.AEAD, "000000000001", "fn2", sealed)
	require.Error(t, err)
	_, err = OpenEnv(k.AEAD, "000000000002", "fn1", sealed)
	require.Error(t, err)
	_, err = OpenEnv(k.AEAD, "000000000001", "fn1", "not base64 !!")
	require.Error(t, err)
	_, err = OpenEnv(k.AEAD, "000000000001", "fn1", "AAAA")
	require.Error(t, err)
}

func TestEnv_EmptyIsEmpty(t *testing.T) {
	k := testAEAD(t)
	sealed, err := SealEnv(k.AEAD, "a", "f", nil)
	require.NoError(t, err)
	assert.Empty(t, sealed)
	sealed, err = SealEnv(k.AEAD, "a", "f", map[string]string{})
	require.NoError(t, err)
	assert.Empty(t, sealed)

	got, err := OpenEnv(k.AEAD, "a", "f", "")
	require.NoError(t, err)
	assert.Empty(t, got)
}
