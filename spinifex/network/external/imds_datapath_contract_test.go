package external

import (
	"context"
	"net/netip"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	handlers_imds "github.com/mulgadc/spinifex/spinifex/handlers/imds"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/network/policy"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
)

// IMDS datapath contract: 169.254.169.254 is served by a localport on the
// subnet switch (one L2 hop, never enters lr_in_*). No IMDS LRP, /32 route,
// or dedicated switch on the VPC router — the LR pipeline cannot shadow it.

const imdsContractCIDR = "10.211.0.0/16"

func TestIMDSDatapathContract_AnsweredAtL2_OverlappingCIDRs(t *testing.T) {
	ctx := context.Background()
	m := mock.New()

	// Two VPCs on the IDENTICAL CIDR, each with internet egress (the broad
	// 0.0.0.0/0 reroute + drop that historically threatened IMDS) and a subnet
	// switch carrying the IMDS localport.
	vpcs := []struct{ vpcID, subnetID string }{
		{"vpc-aaaa1111", "subnet-aaaa1111"},
		{"vpc-bbbb2222", "subnet-bbbb2222"},
	}

	imdsMgr, _ := newIMDSManager(t, m)
	igwMgr, _ := newTestIGWManager(t, m, policy.NATModeDistributed,
		&ExternalPoolConfig{Name: "p", Gateway: "192.168.1.1", PrefixLen: 24},
		LinkLocalAllocator{}, []string{"chassis-a"})

	defaultRoute := netip.MustParsePrefix("0.0.0.0/0")

	for _, v := range vpcs {
		seedVPCRouter(t, m, v.vpcID, imdsContractCIDR)
		seedSubnetSwitch(t, m, v.subnetID)
		require.NoError(t, igwMgr.AttachIGW(ctx, IGWSpec{VPCID: v.vpcID, InternetGatewayID: "igw-" + v.vpcID}))
		// The broad 0.0.0.0/0 egress reroute + drop — the pipeline stages that
		// hijacked / killed the IMDS /32 in v1 before #19 excluded link-local.
		require.NoError(t, igwMgr.EnsureSubnetEgress(ctx, v.vpcID, v.subnetID, defaultRoute))
		require.NoError(t, igwMgr.EnsureSubnetEgressDrop(ctx, v.vpcID, v.subnetID, defaultRoute))
		// The IMDS localport on the subnet switch.
		_, err := imdsMgr.EnsureForSubnet(ctx, v.subnetID, v.vpcID, netip.MustParsePrefix(imdsContractCIDR))
		require.NoError(t, err)
	}

	for _, v := range vpcs {
		assertIMDSAnsweredAtL2(t, m, v.vpcID, v.subnetID)
	}
}

// TestIMDSDatapathContract_OracleHasTeeth proves the LR-pipeline simulator
// catches the pre-#19 bug: a 0.0.0.0/0 reroute without link-local exclusion
// must be detected as diverting IMDS, keeping the oracle non-vacuous.
func TestIMDSDatapathContract_OracleHasTeeth(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	const vpcID, subnetID = "vpc-cccc3333", "subnet-cccc3333"
	seedVPCRouter(t, m, vpcID, imdsContractCIDR)

	rm := policy.NewRouteManager(m)
	gw := "169.254.0.2"
	// Reroute excluding ONLY the VPC CIDR — link-local omitted, reproducing the
	// bug #19 fixed. IMDS (169.254.169.254) is in 0.0.0.0/0 and not in the VPC
	// CIDR, so this policy would hijack it had IMDS ever been routed.
	require.NoError(t, rm.AddSubnetEgress(ctx, vpcID, policy.SubnetEgressSpec{
		SubnetID:     subnetID,
		Prefix:       netip.MustParsePrefix("0.0.0.0/0"),
		Nexthop:      gw,
		OutputPort:   topology.GatewayRouterPort(vpcID),
		Priority:     policy.SubnetEgressPriorityIGW,
		ExcludeCIDRs: []netip.Prefix{netip.MustParsePrefix(imdsContractCIDR)},
	}))

	res := resolveEgress(t, m, vpcID, subnetID, handlers_imds.MetaDataServerIP)
	assert.True(t, res.diverted,
		"simulator must detect a link-local-unaware reroute as hijacking IMDS; if this fails the oracle is blind")
}

// assertIMDSAnsweredAtL2 enforces the L2 contract: IMDS localport must exist on
// the subnet switch, and no IMDS LRP/route may exist on the VPC router.
func assertIMDSAnsweredAtL2(t *testing.T, m *mock.Client, vpcID, subnetID string) {
	t.Helper()
	spec := IMDSSpecForSubnet(subnetID)

	lsp, err := m.GetLogicalSwitchPort(context.Background(), spec.LSPName)
	require.NoErrorf(t, err, "IMDS contract (%s): no localport on the subnet switch %s", vpcID, spec.LSName)
	assert.Equalf(t, "localport", lsp.Type,
		"IMDS contract (%s): the IMDS port must be a localport so every chassis self-serves it over L2", vpcID)
	assert.Emptyf(t, lsp.PortSecurity,
		"IMDS contract (%s): the IMDS localport must carry no port_security (the host sources the frames)", vpcID)
	assert.Equalf(t, []string{spec.LSPMAC + " " + handlers_imds.MetaDataServerIP}, lsp.Addresses,
		"IMDS contract (%s): the localport must claim %s on the subnet switch", vpcID, handlers_imds.MetaDataServerIP)

	// No IMDS object on the router: the request never enters lr_in_*.
	assert.Nilf(t, findIMDSRoute(m, handlers_imds.MetaDataServerIP+"/32"),
		"IMDS contract (%s): no %s/32 static route may exist — IMDS must not transit the VPC router",
		vpcID, handlers_imds.MetaDataServerIP)
	for name := range m.RouterPorts {
		assert.Falsef(t, strings.HasPrefix(name, "imds-"),
			"IMDS contract (%s): found IMDS LRP %q — IMDS must not transit the VPC router", vpcID, name)
	}
}

// --- minimal OVN LR ingress-pipeline model (used by OracleHasTeeth) -----------

type egressResult struct {
	diverted    bool   // a reroute lr_in_policy matched
	dropped     bool   // a drop lr_in_policy matched
	outputPort  string // lr_in_ip_routing winner's output port (empty if a policy already decided)
	routePrefix string // lr_in_ip_routing winner's prefix
}

// resolveEgress simulates the VPC LR ingress pipeline for a packet arriving on
// the subnet LRP destined to dstIP.
func resolveEgress(t *testing.T, m *mock.Client, vpcID, subnetID, dstIP string) egressResult {
	t.Helper()
	return resolveEgressFrom(t, m, vpcID, topology.SubnetRouterPort(subnetID), dstIP)
}

// resolveEgressFrom models OVN lr_in_policy (highest-priority first) then
// lr_in_ip_routing (longest-prefix-match).
func resolveEgressFrom(t *testing.T, m *mock.Client, vpcID, inport, dstIP string) egressResult {
	t.Helper()
	dst, err := netip.ParseAddr(dstIP)
	require.NoError(t, err)
	router := topology.VPCRouter(vpcID)

	// Stage 1: lr_in_policy.
	policies, err := m.ListLogicalRouterPolicies(context.Background(), router)
	require.NoError(t, err)
	sort.SliceStable(policies, func(i, j int) bool { return policies[i].Priority > policies[j].Priority })
	for _, p := range policies {
		mt, ok := parsePolicyMatch(p.Match)
		if !ok || !mt.applies(inport, dst) {
			continue
		}
		switch p.Action {
		case "reroute":
			return egressResult{diverted: true}
		case "drop":
			return egressResult{dropped: true}
		case "allow":
			// falls through to routing
		}
		break
	}

	// Stage 2: lr_in_ip_routing (longest-prefix-match).
	lr, err := m.GetLogicalRouter(context.Background(), router)
	require.NoError(t, err)
	var best egressResult
	bestBits := -1
	for _, uuid := range lr.StaticRoutes {
		r := m.StaticRoutes[uuid]
		if r == nil {
			continue
		}
		pfx, err := netip.ParsePrefix(r.IPPrefix)
		if err != nil || !pfx.Contains(dst) {
			continue
		}
		if pfx.Bits() > bestBits {
			bestBits = pfx.Bits()
			out := ""
			if r.OutputPort != nil {
				out = *r.OutputPort
			}
			best = egressResult{outputPort: out, routePrefix: r.IPPrefix}
		}
	}
	return best
}

type policyMatch struct {
	inport   string // "" => no inport constraint
	dst      netip.Prefix
	excludes []netip.Prefix
}

func (mt policyMatch) applies(inport string, dst netip.Addr) bool {
	if mt.inport != "" && mt.inport != inport {
		return false
	}
	if mt.dst.IsValid() && !mt.dst.Contains(dst) {
		return false
	}
	for _, ex := range mt.excludes {
		if ex.Contains(dst) {
			return false
		}
	}
	return true
}

// parsePolicyMatch parses the match strings produced by subnetEgressMatch:
//
//	inport == "rtr-subnet-x" && ip4.dst == 0.0.0.0/0 && ip4.dst != 10.0.0.0/16 && ip4.dst != 169.254.0.0/16
func parsePolicyMatch(match string) (policyMatch, bool) {
	var mt policyMatch
	for clause := range strings.SplitSeq(match, "&&") {
		clause = strings.TrimSpace(clause)
		switch {
		case strings.HasPrefix(clause, "inport =="):
			mt.inport = strings.Trim(strings.TrimSpace(strings.TrimPrefix(clause, "inport ==")), `"`)
		case strings.HasPrefix(clause, "ip4.dst =="):
			if p, err := netip.ParsePrefix(strings.TrimSpace(strings.TrimPrefix(clause, "ip4.dst =="))); err == nil {
				mt.dst = p
			}
		case strings.HasPrefix(clause, "ip4.dst !="):
			if p, err := netip.ParsePrefix(strings.TrimSpace(strings.TrimPrefix(clause, "ip4.dst !="))); err == nil {
				mt.excludes = append(mt.excludes, p)
			}
		}
	}
	return mt, true
}
