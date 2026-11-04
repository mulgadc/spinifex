package testutil

import (
	"testing"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

func TestSeedKV_ExistingBucket(t *testing.T) {
	_, _, js := StartTestJetStream(t)
	_, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: "seeded", History: 5})
	require.NoError(t, err)

	kv := SeedKV(t, js, "seeded", map[string][]byte{"k": []byte("v")})
	entry, err := kv.Get(t.Context(), "k")
	require.NoError(t, err)
	require.Equal(t, []byte("v"), entry.Value())
}
