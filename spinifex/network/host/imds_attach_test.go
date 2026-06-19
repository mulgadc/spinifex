package host

import (
	"context"
	"slices"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
)

func TestIMDSDatapathSpec(t *testing.T) {
	const (
		eniID    = "eni-0abc1234deadbeef"
		mac      = "02:00:00:00:01:05"
		subnetID = "subnet-0fedcba9"
	)
	d := imdsDatapathSpec(eniID, mac, subnetID)

	if d.Tap != vm.TapDeviceName(eniID) {
		t.Errorf("Tap = %q, want %q", d.Tap, vm.TapDeviceName(eniID))
	}
	if d.Endpoint != IMDSEndpointName(eniID) {
		t.Errorf("Endpoint = %q, want %q", d.Endpoint, IMDSEndpointName(eniID))
	}
	if d.EndpointMAC != IMDSEndpointMAC(eniID) {
		t.Errorf("EndpointMAC = %q, want %q", d.EndpointMAC, IMDSEndpointMAC(eniID))
	}
	if d.GuestMAC != mac {
		t.Errorf("GuestMAC = %q, want %q", d.GuestMAC, mac)
	}
	// GatewayMAC must match the subnet's OVN router-port MAC so the egress flow
	// restores the reply source to the gateway the guest expects.
	if want := utils.HashMAC(subnetID); d.GatewayMAC != want {
		t.Errorf("GatewayMAC = %q, want %q (utils.HashMAC(subnetID))", d.GatewayMAC, want)
	}
	if err := d.validate(); err != nil {
		t.Errorf("derived spec must be valid: %v", err)
	}
}

func TestInstallIMDSDatapath(t *testing.T) {
	s := newStubRunner()
	s.expect("ovs-vsctl", nil, nil)
	s.expect("ip", nil, nil)
	s.expect("sysctl", nil, nil)
	s.expect("ovs-ofctl", nil, nil)

	d := imdsDatapathSpec("eni-0abc1234", "02:00:00:00:01:05", "subnet-0fedcba9")
	if err := installIMDSDatapath(context.Background(), s, d); err != nil {
		t.Fatalf("installIMDSDatapath: %v", err)
	}

	for _, w := range []string{
		"ovs-vsctl --may-exist add-br " + IMDSBridge,                      // EnsureIMDSBridge
		"ovs-vsctl --may-exist add-port " + IMDSBridge + " " + d.Endpoint, // InstallTapDatapath endpoint
		"ovs-ofctl add-flow " + IMDSBridge,                                // demux/egress flow
		"ip route replace default via " + imdsReplyNexthop,                // InstallTapReplyRouting route
		"ip rule add oif " + d.Endpoint,                                   // InstallTapReplyRouting rule
	} {
		if !s.called(w) {
			t.Errorf("missing command:\n  %q\ncalls: %v", w, s.calls)
		}
	}

	// Ordering: the bridge must exist before its endpoint, and the endpoint
	// before the reply rule that selects out it.
	bridge := cmdIndex(s.calls, "ovs-vsctl --may-exist add-br "+IMDSBridge)
	endpoint := cmdIndex(s.calls, "ovs-vsctl --may-exist add-port "+IMDSBridge+" "+d.Endpoint)
	rule := cmdIndex(s.calls, "ip rule add oif "+d.Endpoint)
	if bridge >= endpoint || endpoint >= rule {
		t.Errorf("install order wrong: bridge=%d endpoint=%d rule=%d (want bridge<endpoint<rule)", bridge, endpoint, rule)
	}
}

// cmdIndex returns the index of the first recorded call with the given prefix,
// or -1 if none.
func cmdIndex(calls []string, prefix string) int {
	return slices.IndexFunc(calls, func(c string) bool {
		return len(c) >= len(prefix) && c[:len(prefix)] == prefix
	})
}
