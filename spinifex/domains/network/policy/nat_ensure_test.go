package policy

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn/mock"
)

// ociDatapath maps each public address to a private one on the wire, as the
// OCI resolver does, so rows hold an address the intent never names.
func ociDatapath(_ context.Context, ip string) (string, error) {
	return strings.Replace(ip, "203.0.113.", "10.200.0.", 1), nil
}

func liveEIP() EIPSpec {
	return EIPSpec{VPCID: "vpc-1", ExternalIP: "203.0.113.20", LogicalIP: "10.0.1.5",
		PortName: "port-eni-1", MAC: "52:54:00:aa:bb:cc"}
}

// EnsureEIP only creates. Every change AddEIP would make to a live row is
// refused, and the row stays exactly as it was.
func TestNATManager_EnsureEIPNeverReplacesALiveRow(t *testing.T) {
	cases := []struct {
		name   string
		change func(EIPSpec) EIPSpec
	}{
		{"another external IP on the same private IP", func(e EIPSpec) EIPSpec { e.ExternalIP = "203.0.113.10"; return e }},
		{"another owning ENI on the same address pair", func(e EIPSpec) EIPSpec { e.PortName = "port-eni-2"; return e }},
		{"another private IP on the same external IP", func(e EIPSpec) EIPSpec { e.LogicalIP = "10.0.1.6"; return e }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := mock.New()
			seedRouter(t, m, "vpc-1")
			mgr, err := NewNATManager(m, NATModeDistributed, WithDatapathResolver(ociDatapath))
			require.NoError(t, err)
			ctx := context.Background()
			require.NoError(t, mgr.AddEIP(ctx, liveEIP()))
			before, err := m.FindNATByExternalIP(ctx, "dnat_and_snat", "10.200.0.20")
			require.NoError(t, err)
			require.NotNil(t, before, "the live row is stored under its datapath address")
			snapshot := *before

			err = mgr.EnsureEIP(ctx, tc.change(liveEIP()))

			assert.ErrorIs(t, err, ErrEIPWouldReplace)
			after, err := m.FindNATByExternalIP(ctx, "dnat_and_snat", "10.200.0.20")
			require.NoError(t, err)
			require.NotNil(t, after, "the live row was removed")
			assert.Equal(t, snapshot.LogicalIP, after.LogicalIP)
			assert.Equal(t, snapshot.ExternalIDs, after.ExternalIDs)
		})
	}
}

// A matching row and a missing row are both fine: EnsureEIP keeps the first
// and creates the second.
func TestNATManager_EnsureEIPKeepsAMatchingRowAndCreatesAMissingOne(t *testing.T) {
	m := mock.New()
	seedRouter(t, m, "vpc-1")
	mgr, err := NewNATManager(m, NATModeDistributed, WithDatapathResolver(ociDatapath))
	require.NoError(t, err)
	ctx := context.Background()

	require.NoError(t, mgr.EnsureEIP(ctx, liveEIP()), "a missing row is created")
	require.NoError(t, mgr.EnsureEIP(ctx, liveEIP()), "a matching row is kept")
	row, err := m.FindNATByExternalIP(ctx, "dnat_and_snat", "10.200.0.20")
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, "10.0.1.5", row.LogicalIP)
}
