package reconcile

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAcquireLeader_GetOrCreateAcrossCalls pins the get-or-create fix: the
// first acquire creates the bucket and wins, a second acquire while the lock is
// held must attach to the now-existing bucket (not hang on "stream name already
// in use") and lose cleanly, and once the winner releases a later acquire wins
// again.
func TestAcquireLeader_GetOrCreateAcrossCalls(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)

	release, ok := AcquireLeader(nc, "node-1")
	require.True(t, ok, "first acquire must win and create the bucket")
	require.NotNil(t, release)

	// Bucket now exists; the second acquire must reach the Create-key contention
	// path (and lose) rather than dead-ending on CreateKeyValue.
	loserRelease, ok := AcquireLeader(nc, "node-2")
	assert.False(t, ok, "second acquire must lose while the lock is held")
	assert.Nil(t, loserRelease)

	// Releasing frees the key so a subsequent acquire can take over.
	release()
	release2, ok := AcquireLeader(nc, "node-3")
	require.True(t, ok, "acquire after release must win")
	require.NotNil(t, release2)
	release2()
}
