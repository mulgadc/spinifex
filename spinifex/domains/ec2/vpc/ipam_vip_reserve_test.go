package vpc

import (
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	vipSubnet = "subnet-vip-reserve"
	vipCIDR   = "10.0.50.0/24"
)

func TestIPAM_ReserveVIPCreatesAndRecovers(t *testing.T) {
	t.Parallel()
	ipam, lagging, realKV := laggingIPAM(t, ipamTestJS(t), false)

	ip, err := ipam.ReserveVIP(t.Context(), vipSubnet, vipCIDR, "lb-0a")
	require.NoError(t, err)
	assert.Equal(t, "10.0.50.4", ip)
	assert.Equal(t, 1, lagging.writes(), "an absent row is created once")
	before := committedIPAMRow(t, realKV, vipSubnet)

	again, err := ipam.ReserveVIP(t.Context(), vipSubnet, "10.0.50.77/24", "lb-0a")
	require.NoError(t, err)
	assert.Equal(t, ip, again, "the token recovers its address, from a CIDR written with host bits")
	assert.Equal(t, 1, lagging.writes(), "a held token writes nothing")
	assert.Equal(t, string(before), string(committedIPAMRow(t, realKV, vipSubnet)))

	other, err := ipam.ReserveVIP(t.Context(), vipSubnet, vipCIDR, "lb-0b")
	require.NoError(t, err)
	assert.Equal(t, "10.0.50.5", other)
	assert.Equal(t, []IPEntry{
		{IP: "10.0.50.4", Purpose: PurposeLBVIP, OwnerID: "lb-0a"},
		{IP: "10.0.50.5", Purpose: PurposeLBVIP, OwnerID: "lb-0b"},
	}, currentIPAMEntries(t, realKV, vipSubnet))
}

func TestIPAM_ReserveVIPSharesTheLedgerWithENIs(t *testing.T) {
	t.Parallel()
	js := ipamTestJS(t)
	ipam, lagging, realKV := laggingIPAM(t, js, false)
	eni, err := NewIPAM(t.Context(), js)
	require.NoError(t, err)

	first, err := eni.AllocateIP(t.Context(), vipSubnet, vipCIDR, PurposeENIPrimary, "eni-0a")
	require.NoError(t, err)
	var raced string
	lagging.beforeUpdate = func(attempt int) {
		if attempt != 1 {
			return
		}
		ip, allocErr := eni.AllocateIP(t.Context(), vipSubnet, vipCIDR, PurposeENIPrimary, "eni-0b")
		require.NoError(t, allocErr)
		raced = ip
	}

	vip, err := ipam.ReserveVIP(t.Context(), vipSubnet, vipCIDR, "lb-0a")
	require.NoError(t, err)
	assert.Equal(t, "10.0.50.5", raced, "the ENI took the address the first CAS chose")
	assert.Equal(t, "10.0.50.6", vip, "the retry chose from the leader's new row")
	_, updates := lagging.counts()
	assert.Equal(t, 2, updates)

	_, err = eni.ClaimIP(t.Context(), vipSubnet, vipCIDR, PurposeENIPrimary, "eni-0c", vip)
	require.ErrorIs(t, err, ErrIPInUse, "an ENI cannot claim the reserved address")
	assert.Equal(t, []IPEntry{
		{IP: first, Purpose: PurposeENIPrimary, OwnerID: "eni-0a"},
		{IP: raced, Purpose: PurposeENIPrimary, OwnerID: "eni-0b"},
		{IP: vip, Purpose: PurposeLBVIP, OwnerID: "lb-0a"},
	}, currentIPAMEntries(t, realKV, vipSubnet))
}

func TestIPAM_ReserveVIPSameTokenRacesToOneEntry(t *testing.T) {
	js := ipamTestJS(t)

	t.Run("create lost to the same token", func(t *testing.T) {
		ipam, lagging, realKV := laggingIPAM(t, js, false)
		winner, err := NewIPAM(t.Context(), js)
		require.NoError(t, err)
		const subnet = "subnet-vip-same-create"
		var won string
		lagging.beforeCreate = func(attempt int) {
			if attempt != 1 {
				return
			}
			ip, reserveErr := winner.ReserveVIP(t.Context(), subnet, vipCIDR, "lb-0a")
			require.NoError(t, reserveErr)
			won = ip
		}

		ip, err := ipam.ReserveVIP(t.Context(), subnet, vipCIDR, "lb-0a")
		require.NoError(t, err)
		assert.Equal(t, won, ip)
		assert.Equal(t, 1, lagging.writes(), "the lost create is not followed by another write")
		assert.Equal(t, []IPEntry{{IP: ip, Purpose: PurposeLBVIP, OwnerID: "lb-0a"}}, currentIPAMEntries(t, realKV, subnet))
	})

	t.Run("concurrent handles", func(t *testing.T) {
		const subnet, callers = "subnet-vip-same-many", 8
		_, _, realKV := laggingIPAM(t, js, false)
		results := make([]string, callers)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range callers {
			handle, err := NewIPAM(t.Context(), js)
			require.NoError(t, err)
			wg.Go(func() {
				<-start
				ip, err := handle.ReserveVIP(t.Context(), subnet, vipCIDR, "lb-0a")
				assert.NoError(t, err, "caller %d", i)
				results[i] = ip
			})
		}
		close(start)
		wg.Wait()

		for i, ip := range results {
			assert.Equal(t, results[0], ip, "caller %d", i)
		}
		assert.Equal(t, []IPEntry{{IP: results[0], Purpose: PurposeLBVIP, OwnerID: "lb-0a"}}, currentIPAMEntries(t, realKV, subnet))
	})
}

func TestIPAM_ReserveVIPLostAckRecoversOneEntry(t *testing.T) {
	t.Parallel()
	js := ipamTestJS(t)
	ipam, lagging, realKV := laggingIPAM(t, js, false)
	eni, err := NewIPAM(t.Context(), js)
	require.NoError(t, err)

	lagging.lostAck = true
	_, err = ipam.ReserveVIP(t.Context(), vipSubnet, vipCIDR, "lb-0a")
	require.ErrorIs(t, err, errLostAck, "an unknown create result is an error")

	_, err = eni.AllocateIP(t.Context(), vipSubnet, vipCIDR, PurposeENIPrimary, "eni-0a")
	require.NoError(t, err)
	_, err = ipam.ReserveVIP(t.Context(), vipSubnet, vipCIDR, "lb-0b")
	require.ErrorIs(t, err, errLostAck, "an unknown CAS result is an error")

	reopened, err := NewIPAM(t.Context(), js)
	require.NoError(t, err)
	before := committedIPAMRow(t, realKV, vipSubnet)
	a, err := reopened.ReserveVIP(t.Context(), vipSubnet, vipCIDR, "lb-0a")
	require.NoError(t, err)
	b, err := reopened.ReserveVIP(t.Context(), vipSubnet, vipCIDR, "lb-0b")
	require.NoError(t, err)

	assert.Equal(t, "10.0.50.4", a)
	assert.Equal(t, "10.0.50.6", b)
	assert.Equal(t, string(before), string(committedIPAMRow(t, realKV, vipSubnet)), "recovery writes nothing")
	assert.Equal(t, []IPEntry{
		{IP: "10.0.50.4", Purpose: PurposeLBVIP, OwnerID: "lb-0a"},
		{IP: "10.0.50.5", Purpose: PurposeENIPrimary, OwnerID: "eni-0a"},
		{IP: "10.0.50.6", Purpose: PurposeLBVIP, OwnerID: "lb-0b"},
	}, currentIPAMEntries(t, realKV, vipSubnet))
}

func TestIPAM_ReserveVIPExhausted(t *testing.T) {
	t.Parallel()
	ipam, lagging, realKV := laggingIPAM(t, ipamTestJS(t), false)
	const cidr = "10.0.51.0/28"
	var full []IPEntry
	for i := 4; i < 15; i++ {
		full = append(full, IPEntry{IP: "10.0.51." + strconv.Itoa(i), Purpose: PurposeENIPrimary, OwnerID: "eni-" + strconv.Itoa(i)})
	}
	before := putIPAMRow(t, realKV, IPAMRecord{SubnetId: vipSubnet, CidrBlock: cidr, Allocated: full}).Value()

	_, err := ipam.ReserveVIP(t.Context(), vipSubnet, cidr, "lb-0a")
	assert.ErrorContains(t, err, "exhausted")
	assert.Zero(t, lagging.writes())
	assert.Equal(t, string(before), string(committedIPAMRow(t, realKV, vipSubnet)))
}

func TestIPAM_ReserveVIPRefusesBeforeAnyRead(t *testing.T) {
	t.Parallel()
	ipam, lagging, _ := laggingIPAM(t, ipamTestJS(t), true)

	for name, req := range map[string][3]string{
		"wildcard key":     {"subnet-vip.*", vipCIDR, "lb-0a"},
		"full wildcard":    {">", vipCIDR, "lb-0a"},
		"empty token":      {vipSubnet, vipCIDR, ""},
		"malformed CIDR":   {vipSubnet, "10.0.50.0/33", "lb-0a"},
		"IPv6 CIDR":        {vipSubnet, "2001:db8::/64", "lb-0a"},
		"empty subnet key": {"", vipCIDR, "lb-0a"},
	} {
		_, err := ipam.ReserveVIP(t.Context(), req[0], req[1], req[2])
		assert.ErrorIs(t, err, ErrInvalidVIPRequest, name)
	}
	staleReads, _ := lagging.counts()
	assert.Zero(t, staleReads)
	assert.Zero(t, lagging.writes())
}

func TestIPAM_ReserveVIPLeaderReadFails(t *testing.T) {
	t.Parallel()
	ipam, lagging, realKV := laggingIPAM(t, ipamTestJS(t), true)
	before := putIPAMRow(t, realKV, IPAMRecord{SubnetId: vipSubnet, CidrBlock: vipCIDR}).Value()

	_, err := ipam.ReserveVIP(t.Context(), vipSubnet, vipCIDR, "lb-0a")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrInvalidVIPRequest)
	assert.Zero(t, lagging.writes(), "an unread row is never taken as absent")
	assert.Equal(t, string(before), string(committedIPAMRow(t, realKV, vipSubnet)))
}

func TestIPAM_ReserveVIPRefusesInconsistentRow(t *testing.T) {
	js := ipamTestJS(t)
	vip := func(ip, owner string) IPEntry { return IPEntry{IP: ip, Purpose: PurposeLBVIP, OwnerID: owner} }

	for name, row := range map[string]IPAMRecord{
		"another subnet's identity":   {SubnetId: "subnet-other", CidrBlock: vipCIDR},
		"another CIDR":                {SubnetId: vipSubnet, CidrBlock: "10.0.52.0/24"},
		"unparsable stored CIDR":      {SubnetId: vipSubnet, CidrBlock: "not-a-cidr"},
		"token holds two addresses":   {SubnetId: vipSubnet, CidrBlock: vipCIDR, Allocated: []IPEntry{vip("10.0.50.4", "lb-0a"), vip("10.0.50.5", "lb-0a")}},
		"token at a reserved address": {SubnetId: vipSubnet, CidrBlock: vipCIDR, Allocated: []IPEntry{vip("10.0.50.1", "lb-0a")}},
		"token outside the CIDR":      {SubnetId: vipSubnet, CidrBlock: vipCIDR, Allocated: []IPEntry{vip("10.0.60.4", "lb-0a")}},
		"token at an empty address":   {SubnetId: vipSubnet, CidrBlock: vipCIDR, Allocated: []IPEntry{vip("", "lb-0a")}},
		"empty token entry before a valid one": {SubnetId: vipSubnet, CidrBlock: vipCIDR, Allocated: []IPEntry{
			vip("", "lb-0a"), vip("10.0.50.4", "lb-0a"),
		}},
		"token address shared": {SubnetId: vipSubnet, CidrBlock: vipCIDR, Allocated: []IPEntry{
			vip("10.0.50.4", "lb-0a"), {IP: "10.0.50.4", Purpose: PurposeENIPrimary, OwnerID: "eni-0a"},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			ipam, lagging, realKV := laggingIPAM(t, js, false)
			before := putIPAMRowAt(t, realKV, vipSubnet, row).Value()

			_, err := ipam.ReserveVIP(t.Context(), vipSubnet, vipCIDR, "lb-0a")
			require.ErrorIs(t, err, ErrIPAMInconsistent)
			assert.Zero(t, lagging.writes())
			assert.Equal(t, string(before), string(committedIPAMRow(t, realKV, vipSubnet)))
		})
	}
}
