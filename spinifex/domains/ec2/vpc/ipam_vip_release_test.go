package vpc

import (
	"testing"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIPAM_ReleaseVIPExactAndReplay(t *testing.T) {
	t.Parallel()
	js := ipamTestJS(t)
	ipam, lagging, realKV := laggingIPAM(t, js, false)
	eni, err := NewIPAM(t.Context(), js)
	require.NoError(t, err)
	_, err = eni.AllocateIP(t.Context(), vipSubnet, vipCIDR, PurposeENIPrimary, "eni-0a")
	require.NoError(t, err)
	vip, err := ipam.ReserveVIP(t.Context(), vipSubnet, vipCIDR, "lb-0a")
	require.NoError(t, err)
	written := lagging.writes()

	require.NoError(t, ipam.ReleaseVIP(t.Context(), vipSubnet, vip, "lb-0a"))
	assert.Equal(t, written+1, lagging.writes())
	assert.Equal(t, []IPEntry{{IP: "10.0.50.4", Purpose: PurposeENIPrimary, OwnerID: "eni-0a"}}, currentIPAMEntries(t, realKV, vipSubnet))

	after := committedIPAMRow(t, realKV, vipSubnet)
	require.NoError(t, ipam.ReleaseVIP(t.Context(), vipSubnet, vip, "lb-0a"), "a replayed release is a no-op")
	assert.Equal(t, written+1, lagging.writes())
	assert.Equal(t, string(after), string(committedIPAMRow(t, realKV, vipSubnet)))
}

func TestIPAM_ReleaseVIPNoOps(t *testing.T) {
	t.Parallel()
	js := ipamTestJS(t)

	t.Run("missing row stays missing", func(t *testing.T) {
		ipam, lagging, realKV := laggingIPAM(t, js, false)
		const subnet = "subnet-vip-release-absent"
		require.NoError(t, ipam.ReleaseVIP(t.Context(), subnet, "10.0.50.4", "lb-0a"))
		assert.Zero(t, lagging.writes())
		_, err := realKV.Get(t.Context(), subnet)
		assert.ErrorIs(t, err, jetstream.ErrKeyNotFound)
	})

	for name, entries := range map[string][]IPEntry{
		"missing entry":     {{IP: "10.0.50.5", Purpose: PurposeLBVIP, OwnerID: "lb-0a"}},
		"different token":   {{IP: "10.0.50.4", Purpose: PurposeLBVIP, OwnerID: "lb-0b"}},
		"different purpose": {{IP: "10.0.50.4", Purpose: PurposeENIPrimary, OwnerID: "lb-0a"}},
	} {
		t.Run(name, func(t *testing.T) {
			ipam, lagging, realKV := laggingIPAM(t, js, false)
			before := putIPAMRow(t, realKV, IPAMRecord{SubnetId: vipSubnet, CidrBlock: vipCIDR, Allocated: entries}).Value()

			require.NoError(t, ipam.ReleaseVIP(t.Context(), vipSubnet, "10.0.50.4", "lb-0a"))
			assert.Zero(t, lagging.writes())
			assert.Equal(t, string(before), string(committedIPAMRow(t, realKV, vipSubnet)))
		})
	}
}

func TestIPAM_ReleaseVIPRefusesBeforeAnyRead(t *testing.T) {
	t.Parallel()
	ipam, lagging, _ := laggingIPAM(t, ipamTestJS(t), true)

	for name, req := range map[string][3]string{
		"wildcard key":     {"subnet-vip.*", "10.0.50.4", "lb-0a"},
		"empty subnet key": {"", "10.0.50.4", "lb-0a"},
		"empty token":      {vipSubnet, "10.0.50.4", ""},
		"empty address":    {vipSubnet, "", "lb-0a"},
		"prefix":           {vipSubnet, "10.0.50.4/24", "lb-0a"},
		"IPv6 address":     {vipSubnet, "2001:db8::4", "lb-0a"},
		"mapped IPv4":      {vipSubnet, "::ffff:10.0.50.4", "lb-0a"},
	} {
		err := ipam.ReleaseVIP(t.Context(), req[0], req[1], req[2])
		assert.ErrorIs(t, err, ErrInvalidVIPRequest, name)
	}
	assert.Zero(t, lagging.writes())
}

func TestIPAM_ReleaseVIPLeaderReadFails(t *testing.T) {
	t.Parallel()
	ipam, lagging, realKV := laggingIPAM(t, ipamTestJS(t), true)
	before := putIPAMRow(t, realKV, IPAMRecord{SubnetId: vipSubnet, CidrBlock: vipCIDR, Allocated: []IPEntry{
		{IP: "10.0.50.4", Purpose: PurposeLBVIP, OwnerID: "lb-0a"},
	}}).Value()

	err := ipam.ReleaseVIP(t.Context(), vipSubnet, "10.0.50.4", "lb-0a")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrInvalidVIPRequest)
	assert.Zero(t, lagging.writes(), "an unread row is never taken as absent or released")
	assert.Equal(t, string(before), string(committedIPAMRow(t, realKV, vipSubnet)))
}

func TestIPAM_ReleaseVIPRefusesAmbiguousOwnership(t *testing.T) {
	t.Parallel()
	js := ipamTestJS(t)
	exact := IPEntry{IP: "10.0.50.4", Purpose: PurposeLBVIP, OwnerID: "lb-0a"}

	for name, row := range map[string]IPAMRecord{
		"another subnet's identity": {SubnetId: "subnet-other", CidrBlock: vipCIDR, Allocated: []IPEntry{exact}},
		"exact entry twice":         {SubnetId: vipSubnet, CidrBlock: vipCIDR, Allocated: []IPEntry{exact, exact}},
		"address shared": {SubnetId: vipSubnet, CidrBlock: vipCIDR, Allocated: []IPEntry{
			exact, {IP: "10.0.50.4", Purpose: PurposeENIPrimary, OwnerID: "eni-0a"},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			ipam, lagging, realKV := laggingIPAM(t, js, false)
			before := putIPAMRowAt(t, realKV, vipSubnet, row).Value()

			err := ipam.ReleaseVIP(t.Context(), vipSubnet, "10.0.50.4", "lb-0a")
			require.ErrorIs(t, err, ErrIPAMInconsistent)
			assert.Zero(t, lagging.writes())
			assert.Equal(t, string(before), string(committedIPAMRow(t, realKV, vipSubnet)))
		})
	}
}

func TestIPAM_ReleaseVIPLostReplyAfterCommit(t *testing.T) {
	t.Parallel()
	js := ipamTestJS(t)
	ipam, lagging, realKV := laggingIPAM(t, js, false)
	vip, err := ipam.ReserveVIP(t.Context(), vipSubnet, vipCIDR, "lb-0a")
	require.NoError(t, err)

	lagging.lostAck = true
	err = ipam.ReleaseVIP(t.Context(), vipSubnet, vip, "lb-0a")
	require.ErrorIs(t, err, errLostAck, "an unknown CAS result is an error, not success")
	assert.Empty(t, currentIPAMEntries(t, realKV, vipSubnet), "the release did commit")

	reopened, err := NewIPAM(t.Context(), js)
	require.NoError(t, err)
	before := committedIPAMRow(t, realKV, vipSubnet)
	require.NoError(t, reopened.ReleaseVIP(t.Context(), vipSubnet, vip, "lb-0a"), "the replay finds nothing to release")
	assert.Equal(t, string(before), string(committedIPAMRow(t, realKV, vipSubnet)))
}

func TestIPAM_ReleaseVIPStaleCASCannotFreeReplacement(t *testing.T) {
	t.Parallel()
	js := ipamTestJS(t)
	stale, lagging, realKV := laggingIPAM(t, js, false)
	other, err := NewIPAM(t.Context(), js)
	require.NoError(t, err)
	vip, err := stale.ReserveVIP(t.Context(), vipSubnet, vipCIDR, "lb-0a")
	require.NoError(t, err)

	var replaced []byte
	lagging.beforeUpdate = func(attempt int) {
		if attempt != 1 {
			return
		}
		require.NoError(t, other.ReleaseVIP(t.Context(), vipSubnet, vip, "lb-0a"))
		reused, reserveErr := other.ReserveVIP(t.Context(), vipSubnet, vipCIDR, "lb-0b")
		require.NoError(t, reserveErr)
		require.Equal(t, vip, reused, "the replacement takes the same address")
		replaced = committedIPAMRow(t, realKV, vipSubnet)
	}

	require.NoError(t, stale.ReleaseVIP(t.Context(), vipSubnet, vip, "lb-0a"), "the resumed release finds A gone")
	_, updates := lagging.counts()
	assert.Equal(t, 1, updates, "the stale CAS lost and the reread wrote nothing")
	assert.Equal(t, string(replaced), string(committedIPAMRow(t, realKV, vipSubnet)), "the replacement is untouched")
	assert.Equal(t, []IPEntry{{IP: vip, Purpose: PurposeLBVIP, OwnerID: "lb-0b"}}, currentIPAMEntries(t, realKV, vipSubnet))

	require.ErrorIs(t, stale.ReleaseIP(t.Context(), vipSubnet, vip), ErrPurposeReserved)
	assert.Equal(t, string(replaced), string(committedIPAMRow(t, realKV, vipSubnet)), "the address-only release refuses it too")
}
