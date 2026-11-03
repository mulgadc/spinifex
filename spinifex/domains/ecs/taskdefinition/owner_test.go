package taskdefinition

import (
	"testing"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJSONHelpers_RoundTripAndListSortedUnderPrefix(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	kv := testutil.SeedKV(t, js, "ecs-helpers", nil)
	ctx := t.Context()

	keys, err := KeysWithPrefix(ctx, kv, "a/")
	require.NoError(t, err)
	assert.Empty(t, keys, "an empty bucket lists no keys rather than erroring")

	var missing Record
	found, err := GetJSON(ctx, kv, "a/absent", &missing)
	require.NoError(t, err)
	assert.False(t, found)

	for _, k := range []string{"a/2", "b/1", "a/1"} {
		require.NoError(t, PutJSON(ctx, kv, k, Record{ARN: k}))
	}

	var got Record
	found, err = GetJSON(ctx, kv, "a/2", &got)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "a/2", got.ARN)

	keys, err = KeysWithPrefix(ctx, kv, "a/")
	require.NoError(t, err)
	assert.Equal(t, []string{"a/1", "a/2"}, keys)
}
