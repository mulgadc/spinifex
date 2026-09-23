package host

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/vm"
)

// TestIMDSBridgeIsNotBrInt locks in the coexistence decision: the IMDS redirect
// flows live on a dedicated bridge, never br-int. ovn-controller flushes foreign
// br-int flows on restart, so installing them there would break IMDS on every
// ovn-controller lifecycle event.
func TestIMDSBridgeIsNotBrInt(t *testing.T) {
	if IMDSBridge == "br-int" {
		t.Fatalf("IMDSBridge must not be br-int: ovn-controller flushes foreign br-int flows on restart")
	}
	if IMDSBridge == "" {
		t.Fatal("IMDSBridge must be a non-empty bridge name")
	}
}

// TestIMDSBridgeNameMatchesVM keeps the vm-package bridge constant (used by
// IMDSPrimaryTapSpec to place the primary tap) in sync with the host-package
// bridge the datapath installs on. vm cannot import host, so the name is
// duplicated; this test guards against drift.
func TestIMDSBridgeNameMatchesVM(t *testing.T) {
	if vm.IMDSBridgeName != IMDSBridge {
		t.Fatalf("vm.IMDSBridgeName (%q) != host.IMDSBridge (%q): the primary tap would be placed on a different bridge than the datapath", vm.IMDSBridgeName, IMDSBridge)
	}
}

func TestEnsureIMDSBridge(t *testing.T) {
	s := newStubRunner()
	s.expect("ovs-vsctl", nil, nil)
	s.expect("ip", nil, nil)

	if err := EnsureIMDSBridge(context.Background(), s); err != nil {
		t.Fatalf("EnsureIMDSBridge: %v", err)
	}

	for _, want := range []string{
		"ovs-vsctl --may-exist add-br " + IMDSBridge,
		"ovs-vsctl set Bridge " + IMDSBridge + " fail-mode=secure",
		"ip link set " + IMDSBridge + " up",
	} {
		if !s.called(want) {
			t.Errorf("expected command %q; calls: %v", want, s.calls)
		}
	}

	// Coexistence guard: EnsureIMDSBridge must never touch br-int and must never
	// register with OVN, or ovn-controller would adopt and flush the bridge.
	for _, c := range s.calls {
		if strings.Contains(c, "br-int") {
			t.Errorf("EnsureIMDSBridge must not touch br-int: %q", c)
		}
		if strings.Contains(c, "ovn-bridge-mappings") || strings.Contains(c, "external_ids:ovn") {
			t.Errorf("EnsureIMDSBridge must not register the bridge with OVN: %q", c)
		}
	}
}

func TestEnsureIMDSBridgeAddBrError(t *testing.T) {
	s := newStubRunner()
	s.expect("ovs-vsctl --may-exist add-br", nil, errors.New("boom"))

	err := EnsureIMDSBridge(context.Background(), s)
	if err == nil || !strings.Contains(err.Error(), IMDSBridge) {
		t.Fatalf("expected error naming %s, got %v", IMDSBridge, err)
	}
}

func TestRemoveIMDSBridge(t *testing.T) {
	s := newStubRunner()
	s.expect("ovs-vsctl", nil, nil)

	if err := RemoveIMDSBridge(context.Background(), s); err != nil {
		t.Fatalf("RemoveIMDSBridge: %v", err)
	}
	if !s.called("ovs-vsctl --if-exists del-br " + IMDSBridge) {
		t.Errorf("expected del-br; calls: %v", s.calls)
	}
}

func TestInstallIMDSFlow(t *testing.T) {
	s := newStubRunner()
	s.expect("ovs-ofctl", nil, nil)

	spec := "table=0,priority=200,in_port=7,actions=drop"
	if err := installIMDSFlow(context.Background(), s, "0xa1d5cafe", spec); err != nil {
		t.Fatalf("installIMDSFlow: %v", err)
	}

	want := "ovs-ofctl add-flow " + IMDSBridge + " cookie=0xa1d5cafe," + spec
	if !s.called(want) {
		t.Errorf("expected %q; calls: %v", want, s.calls)
	}
}

func TestClearIMDSFlowsByCookie(t *testing.T) {
	s := newStubRunner()
	s.expect("ovs-ofctl", nil, nil)

	if err := clearIMDSFlowsByCookie(context.Background(), s, "0xa1d5cafe"); err != nil {
		t.Fatalf("clearIMDSFlowsByCookie: %v", err)
	}
	// Cookie mask /-1 deletes only this tap's flows, never another tap's.
	want := "ovs-ofctl del-flows " + IMDSBridge + " cookie=0xa1d5cafe/-1"
	if !s.called(want) {
		t.Errorf("expected cookie-scoped del-flows; calls: %v", s.calls)
	}
}

// Oracle's cloud image ships a catch-all REJECT in INPUT, so an appended
// accept never matches and every guest silently loses IMDS and VPC DNS.
func TestEnsureIMDSInputRuleInsertsAtTheHeadOfINPUT(t *testing.T) {
	r := newStubRunner()
	r.expect("iptables -t filter -C", nil, errors.New("no match"))
	r.expect("iptables -t filter -I", nil, nil)

	if err := EnsureIMDSInputRule(context.Background(), r); err != nil {
		t.Fatalf("EnsureIMDSInputRule: %v", err)
	}
	want := "iptables -t filter -I INPUT 1 -i " + IMDSEndpointPrefix +
		"+ -m comment --comment spinifex-imds -j ACCEPT"
	if !r.called(want) {
		t.Errorf("missing insert:\n  want %q\n  got  %v", want, r.calls)
	}
}

// Re-inserting on every launch would blip IMDS for guests already running, so
// a rule that is already there is left alone.
func TestEnsureIMDSInputRuleLeavesAnExistingRuleAlone(t *testing.T) {
	r := newStubRunner()
	r.expect("iptables -t filter -C", nil, nil)

	if err := EnsureIMDSInputRule(context.Background(), r); err != nil {
		t.Fatalf("EnsureIMDSInputRule: %v", err)
	}
	for _, c := range r.calls {
		if strings.Contains(c, " -I ") {
			t.Errorf("re-inserted an existing rule: %q", c)
		}
	}
}

// One wildcard rule has to cover every per-instance endpoint; a per-device rule
// would leak one INPUT entry per launch.
func TestIMDSInputRuleWildcardMatchesEveryEndpointName(t *testing.T) {
	name := IMDSEndpointName("eni-0123456789abcdef0")
	if !strings.HasPrefix(name, IMDSEndpointPrefix) {
		t.Fatalf("endpoint %q does not carry prefix %q", name, IMDSEndpointPrefix)
	}
}

// withIMDSHostAddrs moves the endpoint addresses for one test and restores
// them, so a remapped case cannot leak into the default-path tests.
func withIMDSHostAddrs(t *testing.T, meta, dns string) {
	t.Helper()
	prev := imdsHostAddrs
	t.Cleanup(func() { imdsHostAddrs = prev })
	SetIMDSHostAddrs(meta, dns)
}

// The default is no remap at all: a bare-metal host owns .254/.253 outright
// and a DNAT there would be pure overhead.
func TestEnsureIMDSRemapRulesInstallsNothingByDefault(t *testing.T) {
	r := newStubRunner()

	if err := EnsureIMDSRemapRules(context.Background(), r); err != nil {
		t.Fatalf("EnsureIMDSRemapRules: %v", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("expected no rules on an unremapped host; calls: %v", r.calls)
	}
}

// The guest must keep addressing 169.254.169.254 whatever the host binds, so
// the DNAT matches the standard address and rewrites to the endpoint's.
func TestEnsureIMDSRemapRulesTranslatesTheGuestFacingPair(t *testing.T) {
	withIMDSHostAddrs(t, "169.254.42.254", "169.254.42.253")
	r := newStubRunner()
	r.expect("iptables -t nat -C", nil, errors.New("no match"))
	r.expect("iptables -t nat -I", nil, nil)
	r.expect("iptables -t nat -C", nil, errors.New("no match"))
	r.expect("iptables -t nat -I", nil, nil)

	if err := EnsureIMDSRemapRules(context.Background(), r); err != nil {
		t.Fatalf("EnsureIMDSRemapRules: %v", err)
	}
	for _, want := range []string{
		"iptables -t nat -I PREROUTING 1 -i " + IMDSEndpointPrefix +
			"+ -d " + imdsMetaAddr + " -m comment --comment spinifex-imds-remap -j DNAT --to-destination 169.254.42.254",
		"iptables -t nat -I PREROUTING 1 -i " + IMDSEndpointPrefix +
			"+ -d " + imdsDNSAddr + " -m comment --comment spinifex-imds-remap -j DNAT --to-destination 169.254.42.253",
	} {
		if !r.called(want) {
			t.Errorf("missing rule:\n  want %q\n  got  %v", want, r.calls)
		}
	}
}

// Half a remap binds one address the host has already taken, so an incomplete
// pair is ignored rather than half-applied.
func TestSetIMDSHostAddrsIgnoresAHalfPair(t *testing.T) {
	withIMDSHostAddrs(t, "169.254.42.254", "169.254.42.253")
	SetIMDSHostAddrs("169.254.42.1", "")

	meta, dns := IMDSHostAddrs()
	if meta != "169.254.42.254" || dns != "169.254.42.253" {
		t.Errorf("half a pair was applied: meta=%q dns=%q", meta, dns)
	}
}

// The endpoint owns the bind addresses; the flows still match what the guest
// sends, or the demux would never fire.
func TestIMDSEndpointOwnsTheHostAddressesNotTheGuestOnes(t *testing.T) {
	withIMDSHostAddrs(t, "169.254.42.254", "169.254.42.253")
	r := newStubRunner()
	for range 7 {
		r.expect("", nil, nil)
	}

	d := IMDSTapDatapath{
		Tap: "tap0", Endpoint: "ime-abcd1234", EndpointMAC: "02:00:00:00:00:01",
		GuestMAC: "02:00:00:00:00:02", GatewayMAC: "02:00:00:00:00:03",
	}
	if err := ensureIMDSEndpoint(context.Background(), r, d); err != nil {
		t.Fatalf("ensureIMDSEndpoint: %v", err)
	}
	if !r.called("ip addr replace 169.254.42.254/32 dev ime-abcd1234") {
		t.Errorf("endpoint did not take the remapped metadata address; calls: %v", r.calls)
	}
	for _, c := range r.calls {
		if strings.Contains(c, "addr replace "+imdsMetaAddr) {
			t.Errorf("endpoint took %s, shadowing the host's own metadata route: %q", imdsMetaAddr, c)
		}
	}
}
