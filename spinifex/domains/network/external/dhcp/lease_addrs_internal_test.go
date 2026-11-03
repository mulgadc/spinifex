package dhcp

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWireAndKVDecodeSameLeaseAddrs(t *testing.T) {
	wl := &wireLease{
		IP:         "10.0.0.5",
		SubnetMask: "255.255.255.0",
		Routers:    []string{"10.0.0.1", "bogus"},
		DNS:        []string{"1.1.1.1", "8.8.8.8"},
		ServerID:   "10.0.0.2",
		HWAddr:     "02:00:00:00:00:01",
	}
	we := wireEntry{
		ClientID:   "c",
		IP:         wl.IP,
		SubnetMask: wl.SubnetMask,
		Routers:    wl.Routers,
		DNS:        wl.DNS,
		ServerID:   wl.ServerID,
		HWAddr:     wl.HWAddr,
	}

	fromWire, err := fromWireLease(wl)
	require.NoError(t, err)
	fromKV, err := fromWireEntry(we)
	require.NoError(t, err)

	for _, l := range []*Lease{fromWire, fromKV.Lease} {
		assert.True(t, l.IP.Equal(net.ParseIP("10.0.0.5")))
		assert.Equal(t, net.CIDRMask(24, 32), l.SubnetMask)
		require.Len(t, l.Routers, 1, "unparseable router is dropped")
		assert.True(t, l.Routers[0].Equal(net.ParseIP("10.0.0.1")))
		assert.Len(t, l.DNS, 2)
		assert.True(t, l.ServerID.Equal(net.ParseIP("10.0.0.2")))
		assert.Equal(t, "02:00:00:00:00:01", l.HWAddr.String())
	}
}

func TestWireAndKVRejectSameBadLeaseAddrs(t *testing.T) {
	cases := map[string]struct{ mask, hw, want string }{
		"bad mask":   {mask: "not-a-mask", want: `parse subnet_mask "not-a-mask"`},
		"bad hwaddr": {hw: "zz", want: `parse hwaddr "zz"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := fromWireLease(&wireLease{SubnetMask: tc.mask, HWAddr: tc.hw})
			require.ErrorContains(t, err, tc.want)
			_, err = fromWireEntry(wireEntry{SubnetMask: tc.mask, HWAddr: tc.hw})
			require.ErrorContains(t, err, tc.want)
		})
	}
}
