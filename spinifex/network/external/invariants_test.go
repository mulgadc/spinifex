package external

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	handlers_imds "github.com/mulgadc/spinifex/spinifex/handlers/imds"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/mock"
)

// IMDS-datapath invariants. These guard the OVN topology
// the IMDS handler trusts as a security + availability boundary. A regression
// in either is a privilege-escalation or single-point-of-failure vector, so the
// failure messages quote the offending design clause verbatim.

// TestI2_IMDSLSPMustBeLocalportNoPortSecurity asserts the host-owned
// imds-port-{vpcID} LSP is a localport with no port_security. The host — not a
// guest — sources 169.254.169.254 frames on this port, so port_security would
// be actively harmful (it pins the allowed src MAC to the LSP's claimed MAC,
// and ovn-controller would drop reply frames egressing from the host veth's
// MAC). A regular (non-localport) LSP would bind to a single chassis, defeating
// the per-chassis self-serve design.
func TestI2_IMDSLSPMustBeLocalportNoPortSecurity(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedVPCRouter(t, m, "vpc-0a1b2c3d4e5f6789", "10.0.0.0/16")
	mgr, _ := newIMDSManager(t, m)

	spec, err := mgr.EnsureForVPC(ctx, "vpc-0a1b2c3d4e5f6789")
	require.NoError(t, err)

	lsp, err := m.GetLogicalSwitchPort(ctx, spec.LSPName)
	require.NoError(t, err)

	if lsp.Type != "localport" {
		t.Errorf("imds LSP %s Type = %q, want \"localport\": a regular LSP binds to "+
			"one chassis only, forcing IMDS through Geneve and creating a single point "+
			"of failure per VPC", spec.LSPName, lsp.Type)
	}
	if len(lsp.PortSecurity) != 0 {
		t.Errorf("imds LSP %s PortSecurity = %v, want empty: port_security set on the "+
			"host-owned LSP would cause ovn-controller to drop reply frames from the "+
			"host veth's MAC", spec.LSPName, lsp.PortSecurity)
	}
}

// TestI3_IMDSStaticRouteShape asserts the static route on vpc-{vpcID} that
// diverts 169.254.169.254 off the WAN default and out the IMDS LRP has the
// exact shape the datapath depends on. A drifted prefix, nexthop, or output
// port silently sends IMDS traffic to the WAN, where it disappears.
func TestI3_IMDSStaticRouteShape(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	seedVPCRouter(t, m, "vpc-0a1b2c3d4e5f6789", "10.0.0.0/16")
	mgr, _ := newIMDSManager(t, m)

	spec, err := mgr.EnsureForVPC(ctx, "vpc-0a1b2c3d4e5f6789")
	require.NoError(t, err)

	route := findIMDSRoute(m, handlers_imds.MetaDataServerIP+"/32")
	require.NotNil(t, route, "no static route for %s/32 on the VPC router — IMDS traffic "+
		"falls through to the WAN default", handlers_imds.MetaDataServerIP)

	assert.Equal(t, handlers_imds.MetaDataServerIP+"/32", route.IPPrefix)
	assert.Equal(t, handlers_imds.MetaDataServerIP, route.Nexthop)
	require.NotNil(t, route.OutputPort, "IMDS static route has no output port — OVN cannot "+
		"resolve which LRP to send 169.254.169.254 traffic out of")
	assert.Equal(t, spec.LRPName, *route.OutputPort)
}
