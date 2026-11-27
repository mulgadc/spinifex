package vpc

import (
	"encoding/json"
	"testing"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An allocator built over a bare KV handle has no stream leader to ask, so
// each leader-read path reports an error and leaves the stored row alone,
// while an ordinary address release still works through the same handle.
func TestIPAM_KVOnlyRefusesLeaderReads(t *testing.T) {
	const subnet = "subnet-kv-only"
	kv, err := ipamTestJS(t).CreateOrUpdateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: "test-ipam-kv-only"})
	require.NoError(t, err)
	data, err := json.Marshal(IPAMRecord{
		SubnetId:  subnet,
		CidrBlock: vipCIDR,
		Allocated: []IPEntry{
			{IP: "10.0.50.4", Purpose: PurposeENIPrimary, OwnerID: "eni-0a"},
			{IP: "10.0.50.5", Purpose: PurposeLBVIP, OwnerID: "lb-0a"},
		},
	})
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), subnet, data)
	require.NoError(t, err)
	ipam := NewIPAMWithKV(kv)

	for name, call := range map[string]func() error{
		"address-only release of the VIP": func() error { return ipam.ReleaseIP(t.Context(), subnet, "10.0.50.5") },
		"reserve":                         func() error { _, err := ipam.ReserveVIP(t.Context(), subnet, vipCIDR, "lb-0b"); return err },
		"token release":                   func() error { return ipam.ReleaseVIP(t.Context(), subnet, "10.0.50.5", "lb-0a") },
	} {
		t.Run(name, func(t *testing.T) {
			before, err := kv.Get(t.Context(), subnet)
			require.NoError(t, err)
			var callErr error
			assert.NotPanics(t, func() { callErr = call() })
			require.ErrorIs(t, callErr, errNoIPAMLeader)
			after, err := kv.Get(t.Context(), subnet)
			require.NoError(t, err)
			assert.Equal(t, before.Revision(), after.Revision(), "no write")
			assert.Equal(t, string(before.Value()), string(after.Value()))
		})
	}

	t.Run("ordinary ENI release", func(t *testing.T) {
		require.NoError(t, ipam.ReleaseIP(t.Context(), subnet, "10.0.50.4"))
		assert.Equal(t, []IPEntry{{IP: "10.0.50.5", Purpose: PurposeLBVIP, OwnerID: "lb-0a"}}, currentIPAMEntries(t, kv, subnet))
	})
}
