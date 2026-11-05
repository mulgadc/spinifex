package daemon_test

import (
	"testing"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/runtime/daemon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The daemon creates the store-record bucket at startup and hands out the
// store it opened, so later readers share one handle.
func TestInitUEFIStoreBucket_CreatesBucketAndServesTheStore(t *testing.T) {
	t.Parallel()
	_, nc, js := testutil.StartTestJetStream(t)
	m, err := daemon.NewJetStreamManager(nc)
	require.NoError(t, err)

	require.NoError(t, m.InitUEFIStoreBucket())

	_, err = js.KeyValue(t.Context(), uefistore.BucketName)
	require.NoError(t, err, "init must create the bucket rather than leave it to first use")

	rec := &uefistore.UEFIStoreRecord{InstanceID: "i-0123456789abcdef0", State: uefistore.StateLive}
	rev, err := m.UEFIStores().Create(t.Context(), rec)
	require.NoError(t, err)
	got, gotRev, err := m.UEFIStores().Get(t.Context(), rec.InstanceID)
	require.NoError(t, err)
	assert.Equal(t, rev, gotRev)
	assert.Equal(t, rec, got)

	_, err = m.UEFIStores().Create(t.Context(), rec)
	require.ErrorIs(t, err, kvstore.ErrExists)
}
