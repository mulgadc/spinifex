package host

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"testing"

	networkids "github.com/mulgadc/spinifex/spinifex/domains/network/identifiers"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	hostcommand "github.com/mulgadc/spinifex/spinifex/runtime/host/command"
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
	if want := networkids.HashMAC(subnetID); d.GatewayMAC != want {
		t.Errorf("GatewayMAC = %q, want %q (networkids.HashMAC(subnetID))", d.GatewayMAC, want)
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
	s.expect(hostcommand.EndpointSysctlHelper, nil, nil)
	s.expect("ovs-ofctl", nil, nil)

	d := imdsDatapathSpec("eni-0abc1234", "02:00:00:00:01:05", "subnet-0fedcba9")
	if err := installIMDSDatapath(context.Background(), s, d); err != nil {
		t.Fatalf("installIMDSDatapath: %v", err)
	}

	endpoint := cmdIndex(s.calls, "ovs-vsctl --may-exist add-port "+IMDSBridge+" "+d.Endpoint)
	patch := cmdIndex(s.calls, "ovs-vsctl --may-exist add-port "+IMDSBridge+" "+d.PatchIMDS)
	route := cmdIndex(s.calls, "ip route replace default via "+imdsReplyNexthop)
	rule := cmdIndex(s.calls, "ip rule add oif "+d.Endpoint)
	bundle := cmdIndex(s.calls, "ovs-ofctl --bundle add-flows "+IMDSBridge+" ")
	marker := cmdIndex(s.calls, hostcommand.EndpointSysctlHelper+" "+d.Endpoint+" accept_local 1")
	for name, i := range map[string]int{"endpoint": endpoint, "patch": patch, "route": route, "rule": rule, "bundle": bundle, "marker": marker} {
		if i < 0 {
			t.Errorf("missing %s step; calls: %v", name, s.calls)
		}
	}
	// The endpoint exists before the patch that advertises the tap and before the
	// reply rule that selects out of it; the marker follows everything it stands for.
	if endpoint >= patch || patch >= route || rule >= bundle || bundle >= marker {
		t.Errorf("install order wrong: endpoint=%d patch=%d route=%d rule=%d bundle=%d marker=%d",
			endpoint, patch, route, rule, bundle, marker)
	}
	if s.called("ovs-ofctl add-flow") || s.called("ovs-ofctl del-flows") {
		t.Errorf("flows must go in the one bundle only; calls: %v", s.calls)
	}
}

// A connectivity-critical failure (here the patch) must surface bare, so the
// caller does not treat it as a serving-only failure.
func TestInstallIMDSDatapath_ConnectivityFailureNotWrapped(t *testing.T) {
	d := imdsDatapathSpec("eni-0abc1234", "02:00:00:00:01:05", "subnet-0fedcba9")
	s := newStubRunner()
	s.expect("ovs-vsctl --may-exist add-port "+IMDSBridge+" "+d.Endpoint, nil, nil)
	s.expect("ip", nil, nil)
	s.expect("ovs-vsctl --may-exist add-port "+IMDSBridge+" "+d.PatchIMDS, nil, errors.New("ovsdb lock held"))

	err := installIMDSDatapath(context.Background(), s, d)
	if err == nil {
		t.Fatal("expected the patch failure to surface")
	}
	if errors.Is(err, vm.ErrIMDSServingDegraded) {
		t.Errorf("connectivity-critical failure must not be ErrIMDSServingDegraded: %v", err)
	}
}

// A serving-stage failure (after the endpoint and patch are in place) must be
// wrapped as ErrIMDSServingDegraded, and it leaves the drift marker unset.
func TestInstallIMDSDatapath_ServingFailureWrapped(t *testing.T) {
	d := imdsDatapathSpec("eni-0abc1234", "02:00:00:00:01:05", "subnet-0fedcba9")

	s := newStubRunner()
	s.expect("ovs-vsctl", nil, nil)
	s.expect("ip", nil, nil)
	s.expect(hostcommand.EndpointSysctlHelper, nil, nil)
	s.expect("ovs-ofctl --bundle", nil, errors.New("OFPBFC_MSG_FAILED"))

	err := installIMDSDatapath(context.Background(), s, d)
	if err == nil {
		t.Fatal("expected the bundle failure to surface")
	}
	if !errors.Is(err, vm.ErrIMDSServingDegraded) {
		t.Errorf("serving-stage failure must be ErrIMDSServingDegraded, got %v", err)
	}
	// The endpoint (vpcd's bind target) and the patch were installed before the
	// failure, so vpcd never binds a missing device.
	if !s.called("ovs-vsctl --may-exist add-port " + IMDSBridge + " " + d.Endpoint) {
		t.Errorf("endpoint must be installed before the serving stage; calls: %v", s.calls)
	}
	if cmdIndex(s.calls, "ovs-vsctl --may-exist add-port "+IMDSBridge+" "+d.PatchIMDS) < 0 {
		t.Errorf("patch must be installed before the serving stage; calls: %v", s.calls)
	}
	if s.called(hostcommand.EndpointSysctlHelper) {
		t.Errorf("sysctls set after a failed bundle; calls: %v", s.calls)
	}
}

// The endpoint is vpcd's bind target and is installed before the patch that
// advertises the tap to ListIMDSTaps, so an endpoint-create failure is
// connectivity-critical: it surfaces bare (not ErrIMDSServingDegraded) and the patch
// is never installed, so the caller rolls the tap back to br-int instead of stranding
// a patch with no endpoint that vpcd would bind against forever.
func TestInstallIMDSDatapath_EndpointFailureIsConnectivityCritical(t *testing.T) {
	d := imdsDatapathSpec("eni-0abc1234", "02:00:00:00:01:05", "subnet-0fedcba9")

	s := newStubRunner()
	s.expect("ovs-vsctl --may-exist add-port "+IMDSBridge+" "+d.Endpoint, nil, errors.New("ovsdb busy"))

	err := installIMDSDatapath(context.Background(), s, d)
	if err == nil {
		t.Fatal("expected the endpoint failure to surface")
	}
	if errors.Is(err, vm.ErrIMDSServingDegraded) {
		t.Errorf("endpoint failure must be connectivity-critical (bare), got %v", err)
	}
	if len(s.calls) != 1 {
		t.Errorf("nothing may follow a failed endpoint; calls: %v", s.calls)
	}
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
