package policy

import (
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const egressTestPG = "sg_test"

// packet is one new IPv4 connection leaving a port in egressTestPG.
type packet struct {
	proto   string // "tcp" or "udp"
	dst     netip.Addr
	srcPort uint16
	dstPort uint16
}

// evalTerm evaluates one conjunct of the matches the compiler emits. Any term
// it does not recognise fails the test, so a new compiler form cannot be
// skipped as if it were true. Address sets are empty: dst belongs to none.
func evalTerm(t *testing.T, term string, p packet) bool {
	t.Helper()
	switch term {
	case "inport == @" + egressTestPG, "ip4":
		return true
	case "tcp", "udp":
		return p.proto == term
	case "arp", "icmp4":
		return false
	}
	lhs, op, rhs, ok := splitTerm(term)
	if !ok {
		t.Fatalf("unrecognised match term %q", term)
	}
	switch lhs {
	case "ip4.dst":
		if strings.HasPrefix(rhs, "$") {
			return false
		}
		require.Equal(t, "==", op, term)
		if pfx, err := netip.ParsePrefix(rhs); err == nil {
			return pfx.Contains(p.dst)
		}
		return netip.MustParseAddr(rhs) == p.dst
	case "tcp.dst", "udp.dst", "tcp.src", "udp.src":
		if !strings.HasPrefix(lhs, p.proto+".") {
			return false
		}
		n, err := strconv.ParseInt(rhs, 10, 64)
		require.NoError(t, err, term)
		v := int64(p.dstPort)
		if strings.HasSuffix(lhs, ".src") {
			v = int64(p.srcPort)
		}
		switch op {
		case "==":
			return v == n
		case ">=":
			return v >= n
		case "<=":
			return v <= n
		}
	case "icmp4.type", "icmp4.code":
		return false
	}
	t.Fatalf("unrecognised match term %q", term)
	return false
}

func splitTerm(term string) (lhs, op, rhs string, ok bool) {
	for _, op := range []string{"==", ">=", "<="} {
		if l, r, found := strings.Cut(term, " "+op+" "); found {
			return l, op, r, true
		}
	}
	return "", "", "", false
}

// aclAllows runs p through the from-lport ACLs a port group really carries,
// tenant rules and infrastructure alike, and applies OVN's highest-priority
// match. Ties never decide differently here: every 1000 ACL allows.
func aclAllows(t *testing.T, egress []Rule, p packet) bool {
	t.Helper()
	tenant, err := RuleACLSpecs(egressTestPG, nil, egress)
	require.NoError(t, err)
	specs := append(InfrastructureACLs(egressTestPG), tenant...)
	sort.SliceStable(specs, func(i, j int) bool { return specs[i].Priority > specs[j].Priority })
	for _, s := range specs {
		if s.Direction != "from-lport" || !matches(t, s, p) {
			continue
		}
		return s.Action == "allow" || s.Action == "allow-related"
	}
	return true // OVN allows on no match; the 800 default-deny matches everything.
}

func matches(t *testing.T, s ovn.ACLSpec, p packet) bool {
	t.Helper()
	for term := range strings.SplitSeq(s.Match, " && ") {
		if !evalTerm(t, term, p) {
			return false
		}
	}
	return true
}

// EgressAllows must decide every connection exactly as the egress ACLs the
// compiler renders from the same rules, since it stands in for them on
// traffic the proxy captures before OVN.
func TestEgressAllows_AgreesWithTheCompiledACLs(t *testing.T) {
	entries := []netip.Addr{
		netip.MustParseAddr("192.0.2.10"),
		netip.MustParseAddr("192.0.2.200"),
		netip.MustParseAddr("198.51.100.7"),
	}
	ruleSets := map[string][]Rule{
		"no rules":                 nil,
		"all traffic anywhere":     {{IPProtocol: "-1", CIDR: "0.0.0.0/0"}},
		"tcp every port, 0-0":      {{IPProtocol: "tcp", FromPort: 0, ToPort: 0, CIDR: "0.0.0.0/0"}},
		"tcp 443 only":             {{IPProtocol: "tcp", FromPort: 443, ToPort: 443, CIDR: "0.0.0.0/0"}},
		"tcp gate port":            {{IPProtocol: "tcp", FromPort: 8443, ToPort: 8443, CIDR: "0.0.0.0/0"}},
		"range excluding the gate": {{IPProtocol: "tcp", FromPort: 1024, ToPort: 8442, CIDR: "0.0.0.0/0"}},
		"range including the gate": {{IPProtocol: "tcp", FromPort: 8000, ToPort: 9000, CIDR: "0.0.0.0/0"}},
		"cidr covering some":       {{IPProtocol: "tcp", FromPort: 8443, ToPort: 8443, CIDR: "192.0.2.0/25"}},
		"single address":           {{IPProtocol: "-1", CIDR: "198.51.100.7/32"}},
		"bare address":             {{IPProtocol: "-1", CIDR: "198.51.100.7"}},
		"udp only":                 {{IPProtocol: "udp", FromPort: 0, ToPort: 0, CIDR: "0.0.0.0/0"}},
		"icmp only":                {{IPProtocol: "icmp", FromPort: -1, ToPort: -1, CIDR: "0.0.0.0/0"}},
		"tcp with -1 ports":        {{IPProtocol: "tcp", FromPort: -1, ToPort: -1, CIDR: "0.0.0.0/0"}},
		"group reference":          {{IPProtocol: "-1", SourceSG: "sg-0123456789abcdef0"}},
		"union of two groups": {
			{IPProtocol: "tcp", FromPort: 443, ToPort: 443, CIDR: "0.0.0.0/0"},
			{IPProtocol: "tcp", FromPort: 8443, ToPort: 8443, CIDR: "198.51.100.0/24"},
		},
	}
	for name, rules := range ruleSets {
		t.Run(name, func(t *testing.T) {
			for _, dst := range entries {
				for _, proto := range []string{"tcp", "udp"} {
					for _, port := range []uint16{443, 8443, 8474} {
						p := packet{proto: proto, dst: dst, srcPort: 40000, dstPort: port}
						assert.Equal(t, aclAllows(t, rules, p), EgressAllows(rules, dst, proto, port),
							"%s to %s:%d", proto, dst, port)
					}
				}
			}
		})
	}
}

// The agreement table must not pass by both sides always refusing.
func TestEgressAllows_Decisions(t *testing.T) {
	gate := netip.MustParseAddr("192.0.2.10")
	assert.False(t, EgressAllows(nil, gate, "tcp", 8443), "no rules")
	assert.True(t, EgressAllows([]Rule{{IPProtocol: "-1", CIDR: "0.0.0.0/0"}}, gate, "tcp", 8443))
	assert.False(t, EgressAllows([]Rule{{IPProtocol: "tcp", FromPort: 443, ToPort: 443, CIDR: "0.0.0.0/0"}}, gate, "tcp", 8443),
		"an egress group allowing only 443 blocks the gate port")
	assert.True(t, EgressAllows([]Rule{{IPProtocol: "tcp", FromPort: 8000, ToPort: 9000, CIDR: "192.0.2.0/24"}}, gate, "tcp", 8443))
	assert.False(t, EgressAllows([]Rule{{IPProtocol: "udp", CIDR: "0.0.0.0/0"}}, gate, "tcp", 8443))
}

// Shapes the compiler would read as "everything" or refuse never widen the
// check: rule conversion drops a rule with no IPv4 source, and the compiler
// fails a whole group on an unknown protocol.
func TestEgressAllows_FailsClosedWhereTheCompilerWouldNotRender(t *testing.T) {
	gate := netip.MustParseAddr("192.0.2.10")
	for name, r := range map[string]Rule{
		"no cidr and no group": {IPProtocol: "-1"},
		"ipv6 cidr":            {IPProtocol: "-1", CIDR: "::/0"},
		"unknown protocol":     {IPProtocol: "sctp", CIDR: "0.0.0.0/0"},
	} {
		assert.False(t, EgressAllows([]Rule{r}, gate, "tcp", 8443), name)
	}
	assert.False(t, EgressAllows([]Rule{{IPProtocol: "-1", CIDR: "0.0.0.0/0"}}, netip.MustParseAddr("2001:db8::1"), "tcp", 8443),
		"an IPv6 destination is never allowed")
}
