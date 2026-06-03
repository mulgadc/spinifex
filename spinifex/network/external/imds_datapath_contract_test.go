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

// IMDS datapath contract — desk-speed pipeline oracle.
//
// Every IMDS datapath bug to date (the egress-reroute hijack of the /32, the
// reply-path break) lived inside the VPC logical-router pipeline and was only
// caught by a full VM-boot E2E run, one bug per deploy. This test makes the
// load-bearing half of that pipeline assertable in `go test`: it builds the
// REAL managers (IGW egress reroute + drop, IMDS topology) against the OVN mock
// for two VPCs that share the *identical* CIDR — the overlapping-CIDR case that
// is the whole point of per-VPC IMDS identity — then runs a faithful, minimal
// simulation of OVN's two ingress stages (lr_in_policy, then lr_in_ip_routing)
// and asserts a guest packet to 169.254.169.254 reaches the IMDS LRP rather
// than being rerouted out the WAN or dropped.
//
// A true round-trip oracle is ovn-trace against a live SB DB; that belongs in
// the e2e harness. This is its desk-speed counterpart over the same NB rows —
// it runs on every `make preflight`, so a future broad lr_in_policy rule that
// forgets to spare link-local fails here instead of in a deploy.

const (
	imdsContractGuestIP = "10.211.0.5"
	imdsContractCIDR    = "10.211.0.0/16"
)

func TestIMDSDatapathContract_SurvivesVPCPipeline_OverlappingCIDRs(t *testing.T) {
	ctx := context.Background()
	m := mock.New()

	// Two VPCs on the IDENTICAL CIDR, each with internet egress and a subnet
	// whose first guest gets the same private IP. The OVN NB rows for the two
	// must each independently route IMDS correctly.
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
		require.NoError(t, igwMgr.AttachIGW(ctx, IGWSpec{VPCID: v.vpcID, InternetGatewayID: "igw-" + v.vpcID}))
		// The broad 0.0.0.0/0 egress reroute + drop — the pipeline stages that
		// previously hijacked / killed the IMDS /32 before #19 excluded link-local.
		require.NoError(t, igwMgr.EnsureSubnetEgress(ctx, v.vpcID, v.subnetID, defaultRoute))
		require.NoError(t, igwMgr.EnsureSubnetEgressDrop(ctx, v.vpcID, v.subnetID, defaultRoute))
		// The IMDS /32 static route + imds-lrp.
		_, err := imdsMgr.EnsureForVPC(ctx, v.vpcID)
		require.NoError(t, err)
	}

	for _, v := range vpcs {
		assertIMDSDatapathContract(t, m, v.vpcID, v.subnetID, imdsContractGuestIP)
	}
}

// TestIMDSDatapathContract_OracleHasTeeth proves the simulator actually detects
// the regression it guards against: a 0.0.0.0/0 egress reroute that does NOT
// exclude link-local (the pre-#19 bug) must be reported as diverting IMDS.
// Without this, a vacuously-passing contract test would give false confidence.
func TestIMDSDatapathContract_OracleHasTeeth(t *testing.T) {
	ctx := context.Background()
	m := mock.New()
	const vpcID, subnetID = "vpc-cccc3333", "subnet-cccc3333"
	seedVPCRouter(t, m, vpcID, imdsContractCIDR)

	rm := policy.NewRouteManager(m)
	gw := "169.254.0.2"
	// Reroute excluding ONLY the VPC CIDR — link-local omitted, reproducing the
	// bug #19 fixed. IMDS (169.254.169.254) is in 0.0.0.0/0 and not in the VPC
	// CIDR, so this policy hijacks it.
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

// assertIMDSDatapathContract is the contract: a guest packet to
// 169.254.169.254 must reach the IMDS LRP, and the IMDS reply must not be
// caught by any egress policy.
func assertIMDSDatapathContract(t *testing.T, m *mock.Client, vpcID, subnetID, guestIP string) {
	t.Helper()
	imdsLRP := topology.IMDSRouterPort(vpcID)

	// Request: guest -> 169.254.169.254, ingressing on the subnet LRP.
	req := resolveEgress(t, m, vpcID, subnetID, handlers_imds.MetaDataServerIP)
	assert.Falsef(t, req.diverted,
		"IMDS contract (%s): an lr_in_policy reroute on 0.0.0.0/0 hijacks 169.254.169.254 out the WAN; "+
			"every broad egress reroute MUST exclude 169.254.0.0/16 (see decision #19)", vpcID)
	assert.Falsef(t, req.dropped,
		"IMDS contract (%s): an lr_in_policy drop kills 169.254.169.254; "+
			"the per-subnet drop policy MUST exclude 169.254.0.0/16", vpcID)
	assert.Equalf(t, "169.254.169.254/32", req.routePrefix,
		"IMDS contract (%s): 169.254.169.254 must be resolved by the /32 IMDS static route, not a broader route", vpcID)
	assert.Equalf(t, imdsLRP, req.outputPort,
		"IMDS contract (%s): the IMDS /32 must egress via %s", vpcID, imdsLRP)

	// Reply: 169.254.169.254 -> guest, ingressing on the IMDS LRP. The egress
	// policies are inport-scoped to the subnet LRP, so none may catch the reply;
	// a future inport-less broad policy would break IMDS replies and fail here.
	reply := resolveEgressFrom(t, m, vpcID, imdsLRP, guestIP)
	assert.Falsef(t, reply.diverted,
		"IMDS contract (%s): an lr_in_policy must not divert the IMDS reply to %s (policies must stay inport-scoped)", vpcID, guestIP)
	assert.Falsef(t, reply.dropped,
		"IMDS contract (%s): an lr_in_policy must not drop the IMDS reply to %s", vpcID, guestIP)
}

// --- minimal OVN LR ingress-pipeline model ------------------------------------

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

// resolveEgressFrom models OVN's two ingress stages in order:
//
//	lr_in_policy      — Logical_Router_Policy, highest Priority first, first
//	                    applicable wins (reroute => diverted, drop => dropped).
//	lr_in_ip_routing  — longest-prefix-match over static routes.
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
	for _, clause := range strings.Split(match, "&&") {
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
