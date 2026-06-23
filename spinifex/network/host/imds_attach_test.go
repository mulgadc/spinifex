package host

import (
	"context"
	"errors"
	"slices"
	"strconv"
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
	if d.PatchIMDS != IMDSPatchPort(eniID) {
		t.Errorf("PatchIMDS = %q, want %q", d.PatchIMDS, IMDSPatchPort(eniID))
	}
	if d.PatchInt != IMDSIntPatchPort(eniID) {
		t.Errorf("PatchInt = %q, want %q", d.PatchInt, IMDSIntPatchPort(eniID))
	}
	// IfaceID must equal the OVN LSP name so ovn-controller binds the guest LSP
	// to the patch's br-int end exactly as it bound the tap.
	if want := vm.OVSIfaceID(eniID); d.IfaceID != want {
		t.Errorf("IfaceID = %q, want %q (vm.OVSIfaceID(eniID))", d.IfaceID, want)
	}
	if err := d.validate(); err != nil {
		t.Errorf("derived spec must be valid: %v", err)
	}
	if err := d.validatePatch(); err != nil {
		t.Errorf("derived spec must pass patch validation: %v", err)
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
		"ovs-vsctl --may-exist add-br " + IMDSBridge,                       // EnsureIMDSBridge
		"ovs-vsctl --may-exist add-port " + IMDSBridge + " " + d.PatchIMDS, // installTapPatch (br-imds end)
		"ovs-vsctl --may-exist add-port br-int " + d.PatchInt,              // installTapPatch (br-int end)
		"ovs-vsctl --may-exist add-port " + IMDSBridge + " " + d.Endpoint,  // InstallTapDatapath endpoint
		"ovs-ofctl add-flow " + IMDSBridge,                                 // demux/egress/forward flow
		"ip route replace default via " + imdsReplyNexthop,                 // InstallTapReplyRouting route
		"ip rule add oif " + d.Endpoint,                                    // InstallTapReplyRouting rule
	} {
		if !s.called(w) {
			t.Errorf("missing command:\n  %q\ncalls: %v", w, s.calls)
		}
	}

	// Ordering: the bridge must exist before its ports (patch, endpoint), and the
	// endpoint before the reply rule that selects out it.
	bridge := cmdIndex(s.calls, "ovs-vsctl --may-exist add-br "+IMDSBridge)
	patch := cmdIndex(s.calls, "ovs-vsctl --may-exist add-port "+IMDSBridge+" "+d.PatchIMDS)
	endpoint := cmdIndex(s.calls, "ovs-vsctl --may-exist add-port "+IMDSBridge+" "+d.Endpoint)
	rule := cmdIndex(s.calls, "ip rule add oif "+d.Endpoint)
	if bridge >= patch || bridge >= endpoint || endpoint >= rule {
		t.Errorf("install order wrong: bridge=%d patch=%d endpoint=%d rule=%d (want bridge<patch, bridge<endpoint<rule)", bridge, patch, endpoint, rule)
	}

	// Regression: the forward, demux, and egress flows all share the tap's cookie,
	// so the single up-front clear must precede every add-flow. A clear inside any
	// installer would wipe an earlier installer's flows under the same cookie —
	// notably the patch's forward flows, leaving the guest with no non-IMDS path.
	cookie := imdsFlowCookie(d.Endpoint)
	clearIdx := cmdIndex(s.calls, "ovs-ofctl del-flows "+IMDSBridge+" cookie="+cookie+"/-1")
	firstAdd := cmdIndex(s.calls, "ovs-ofctl add-flow")
	if clearIdx < 0 {
		t.Errorf("missing up-front cookie clear; calls: %v", s.calls)
	}
	if firstAdd < 0 || clearIdx >= firstAdd {
		t.Errorf("cookie clear must precede all add-flows: clear=%d firstAdd=%d (calls: %v)", clearIdx, firstAdd, s.calls)
	}
	// And exactly one clear: a second would mean an installer re-cleared the cookie.
	if n := countCalls(s.calls, "ovs-ofctl del-flows "+IMDSBridge+" cookie="+cookie+"/-1"); n != 1 {
		t.Errorf("cookie cleared %d times, want exactly 1 (calls: %v)", n, s.calls)
	}

	// Both forward (priority=100, from installTapPatch) and demux (priority=200,
	// from InstallTapDatapath) flows must survive the shared-cookie clear.
	forward := "ovs-ofctl add-flow " + IMDSBridge + " cookie=" + cookie + ",table=0,priority=100,in_port=" + d.Tap + ",actions=output:" + d.PatchIMDS
	demux := "ovs-ofctl add-flow " + IMDSBridge + " cookie=" + cookie + ",table=0,priority=200,in_port=" + d.Tap + ",ip,nw_dst=" + imdsMetaAddr + ",actions=mod_dl_dst:" + d.EndpointMAC + ",output:" + d.Endpoint
	for _, w := range []string{forward, demux} {
		if !s.called(w) {
			t.Errorf("missing flow after shared-cookie clear:\n  %q\ncalls: %v", w, s.calls)
		}
	}
}

// A connectivity-critical failure (here the bridge create) must surface bare, so
// the caller rolls the tap back to br-int rather than treating it as best-effort.
func TestInstallIMDSDatapath_ConnectivityFailureNotWrapped(t *testing.T) {
	s := newStubRunner()
	s.expect("ovs-vsctl --may-exist add-br", nil, errors.New("ovsdb lock held"))

	d := imdsDatapathSpec("eni-0abc1234", "02:00:00:00:01:05", "subnet-0fedcba9")
	err := installIMDSDatapath(context.Background(), s, d)
	if err == nil {
		t.Fatal("expected the bridge failure to surface")
	}
	if errors.Is(err, vm.ErrIMDSServingDegraded) {
		t.Errorf("connectivity-critical failure must not be ErrIMDSServingDegraded: %v", err)
	}
}

// A serving-stage failure (after the patch + forward flows are in place) must be
// wrapped as ErrIMDSServingDegraded — the guest is connected, only IMDS is down,
// so the caller logs and continues instead of rolling the tap back.
func TestInstallIMDSDatapath_ServingFailureWrapped(t *testing.T) {
	d := imdsDatapathSpec("eni-0abc1234", "02:00:00:00:01:05", "subnet-0fedcba9")

	s := newStubRunner()
	s.expect("ovs-vsctl --may-exist add-br", nil, nil)
	s.expect("ovs-vsctl set Bridge", nil, nil)
	s.expect("ip", nil, nil)
	s.expect("ovs-ofctl", nil, nil)
	s.expect("ovs-vsctl --may-exist add-port "+IMDSBridge+" "+d.PatchIMDS, nil, nil)
	s.expect("ovs-vsctl --may-exist add-port br-int "+d.PatchInt, nil, nil)
	// The endpoint create is the first command past the connectivity-critical
	// patch — fail it to land in the serving stage.
	s.expect("ovs-vsctl --may-exist add-port "+IMDSBridge+" "+d.Endpoint, nil, errors.New("ovsdb busy"))

	err := installIMDSDatapath(context.Background(), s, d)
	if err == nil {
		t.Fatal("expected the endpoint failure to surface")
	}
	if !errors.Is(err, vm.ErrIMDSServingDegraded) {
		t.Errorf("serving-stage failure must be ErrIMDSServingDegraded, got %v", err)
	}
	// Connectivity (the patch's br-int end carrying the OVN binding) was installed
	// before the failure, proving the guest keeps its path to OVN.
	if !s.called("ovs-vsctl --may-exist add-port br-int " + d.PatchInt) {
		t.Errorf("patch br-int end must be installed before the serving stage; calls: %v", s.calls)
	}
}

// countCalls returns how many recorded calls start with prefix.
func countCalls(calls []string, prefix string) int {
	n := 0
	for _, c := range calls {
		if len(c) >= len(prefix) && c[:len(prefix)] == prefix {
			n++
		}
	}
	return n
}

func TestIMDSDetachSpec(t *testing.T) {
	const eniID = "eni-0abc1234deadbeef"
	d := imdsDetachSpec(eniID)

	// Teardown keys off the endpoint (flow cookie + reply table) and the patch
	// pair, all derived from the ENI — matching what the attach spec installs.
	if d.Endpoint != IMDSEndpointName(eniID) {
		t.Errorf("Endpoint = %q, want %q", d.Endpoint, IMDSEndpointName(eniID))
	}
	if d.PatchIMDS != IMDSPatchPort(eniID) {
		t.Errorf("PatchIMDS = %q, want %q", d.PatchIMDS, IMDSPatchPort(eniID))
	}
	if d.PatchInt != IMDSIntPatchPort(eniID) {
		t.Errorf("PatchInt = %q, want %q", d.PatchInt, IMDSIntPatchPort(eniID))
	}
}

func TestRemoveIMDSDatapath(t *testing.T) {
	s := newStubRunner()
	s.expect("ip", nil, nil)
	s.expect("ovs-ofctl", nil, nil)
	s.expect("ovs-vsctl", nil, nil)

	d := imdsDetachSpec("eni-0abc1234")
	if err := removeIMDSDatapath(context.Background(), s, d); err != nil {
		t.Fatalf("removeIMDSDatapath: %v", err)
	}
	cookie := imdsFlowCookie(d.Endpoint)
	table := strconv.Itoa(imdsReplyTable(d.Endpoint))

	for _, w := range []string{
		// Reply routing removed first (rule keyed by endpoint, table flushed).
		"ip rule del oif " + d.Endpoint + " lookup " + table,
		"ip route flush table " + table,
		// Then flows (by cookie), the patch pair, and the endpoint port.
		"ovs-ofctl del-flows " + IMDSBridge + " cookie=" + cookie + "/-1",
		"ovs-vsctl --if-exists del-port " + IMDSBridge + " " + d.PatchIMDS,
		"ovs-vsctl --if-exists del-port br-int " + d.PatchInt,
		"ovs-vsctl --if-exists del-port " + IMDSBridge + " " + d.Endpoint,
	} {
		if !s.called(w) {
			t.Errorf("missing command:\n  %q\ncalls: %v", w, s.calls)
		}
	}

	// Ordering: the reply rule must be dropped before the endpoint port is
	// deleted (the rule is keyed by the endpoint name).
	rule := cmdIndex(s.calls, "ip rule del oif "+d.Endpoint)
	endpoint := cmdIndex(s.calls, "ovs-vsctl --if-exists del-port "+IMDSBridge+" "+d.Endpoint)
	if rule < 0 || endpoint < 0 || rule >= endpoint {
		t.Errorf("teardown order wrong: rule=%d endpoint=%d (want rule<endpoint)", rule, endpoint)
	}
}

// cmdIndex returns the index of the first recorded call with the given prefix,
// or -1 if none.
func cmdIndex(calls []string, prefix string) int {
	return slices.IndexFunc(calls, func(c string) bool {
		return len(c) >= len(prefix) && c[:len(prefix)] == prefix
	})
}
