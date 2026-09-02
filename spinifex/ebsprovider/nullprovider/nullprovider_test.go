package nullprovider_test

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider/natsserve"
	"github.com/mulgadc/spinifex/spinifex/ebsprovider/nullprovider"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewAdvertisesEveryOptionalCapability is what makes the floor a floor: a
// capability left off is a verb that returns early instead of being measured,
// so a baseline taken against a partly-capable provider prices less work than
// the provider it is compared against.
func TestNewAdvertisesEveryOptionalCapability(t *testing.T) {
	caps, err := nullprovider.New().GetCapabilities(t.Context(), ebsprovider.GetCapabilitiesRequest{Versioned: ebsprovider.NewVersioned()})
	require.NoError(t, err)

	assert.Equal(t, nullprovider.Capabilities, caps.Capabilities)
	assert.True(t, caps.Capabilities.OnlineExpansion)
	assert.True(t, caps.Capabilities.VolumeSeeding)
	assert.True(t, caps.Capabilities.VolumeEnumeration)
	assert.True(t, caps.Capabilities.SnapshotEnumeration)
	assert.True(t, caps.Capabilities.ReadOnlyPublish)
	assert.False(t, caps.Capabilities.Exclusion.SingleWriter(), "an in-process map excludes within a node, not across a cluster")
}

// TestServeRoundTripsOverNATS covers the composition Serve exists to name: the
// returned client must actually reach the served provider, since a measurement
// taken against a client wired to nothing would report only its own timeout.
func TestServeRoundTripsOverNATS(t *testing.T) {
	_, conn := testutil.StartTestNATS(t)
	client, stop, err := nullprovider.Serve(t.Context(), conn, natsserve.Options{NoQueueGroup: true})
	require.NoError(t, err)
	t.Cleanup(stop)

	volume, err := client.CreateVolume(t.Context(), ebsprovider.CreateVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: "vol-null",
		CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
	})
	require.NoError(t, err)
	assert.Equal(t, "memory://volume/vol-null", volume.Handle)

	listed, err := client.ListVolumes(t.Context(), ebsprovider.ListVolumesRequest{Versioned: ebsprovider.NewVersioned()})
	require.NoError(t, err)
	assert.Equal(t, []ebsprovider.VolumeRef{{ID: "vol-null", Handle: volume.Handle}}, listed.Volumes)
}

// TestServeFailsOnClosedConnection covers the wrapped failure path: a caller
// that cannot stand the provider up must get an error rather than a client
// pointed at a subject nothing serves.
func TestServeFailsOnClosedConnection(t *testing.T) {
	_, conn := testutil.StartTestNATS(t)
	conn.Close()

	client, stop, err := nullprovider.Serve(t.Context(), conn, natsserve.Options{NoQueueGroup: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "serve null provider")
	assert.Nil(t, client)
	assert.Nil(t, stop)
}
