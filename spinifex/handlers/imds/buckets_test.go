package handlers_imds

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/require"
)

func TestInitENIByIPBucket(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	kv, err := InitENIByIPBucket(js, 1)
	require.NoError(t, err)
	require.NotNil(t, kv)
}

// InitBuckets clamps replicas < 1 to 1, and a second call converges on the
// open-existing fallback rather than failing on the already-created bucket.
func TestInitBuckets_ReplicaClampAndIdempotent(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)

	vpcVeth, eniByIP, err := InitBuckets(js, 0)
	require.NoError(t, err)
	require.NotNil(t, vpcVeth)
	require.NotNil(t, eniByIP)

	vpcVeth2, eniByIP2, err := InitBuckets(js, 1)
	require.NoError(t, err)
	require.NotNil(t, vpcVeth2)
	require.NotNil(t, eniByIP2)
}
