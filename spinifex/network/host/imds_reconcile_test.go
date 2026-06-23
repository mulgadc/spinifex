package host

import (
	"context"
	"errors"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/vm"
)

// ListIMDSTaps must enumerate only the IMDS patch ports (imi-*) on br-int,
// recover the *full* ENI from each port's iface-id ("port-<eniID>"), and pair it
// with the br-imds endpoint name — ignoring guest taps and the br-imds-end patch.
func TestListIMDSTaps_RecoversENIFromPatchIfaceID(t *testing.T) {
	const (
		eniA = "eni-0aaa1111deadbeef"
		eniB = "eni-0bbb2222cafef00d"
	)
	s := newStubRunner()
	// br-int carries the two IMDS patch ports plus a guest tap and the br-imds-end
	// patch name (which lives on br-imds, not br-int, but guards the imi- filter).
	s.expect("ovs-vsctl list-ports br-int",
		[]byte(IMDSIntPatchPort(eniA)+"\ntap0deadbeef\n"+IMDSIntPatchPort(eniB)+"\n"+IMDSPatchPort(eniA)+"\n"), nil)
	// ovs-vsctl quotes the iface-id value and appends a newline.
	s.expect("ovs-vsctl get Interface "+IMDSIntPatchPort(eniA)+" external_ids:iface-id",
		[]byte("\""+vm.OVSIfaceID(eniA)+"\"\n"), nil)
	s.expect("ovs-vsctl get Interface "+IMDSIntPatchPort(eniB)+" external_ids:iface-id",
		[]byte("\""+vm.OVSIfaceID(eniB)+"\"\n"), nil)

	taps, err := ListIMDSTaps(context.Background(), s)
	if err != nil {
		t.Fatalf("ListIMDSTaps: %v", err)
	}

	got := map[string]string{}
	for _, tp := range taps {
		got[tp.ENIID] = tp.Endpoint
	}
	want := map[string]string{
		eniA: IMDSEndpointName(eniA),
		eniB: IMDSEndpointName(eniB),
	}
	if len(got) != len(want) {
		t.Fatalf("ListIMDSTaps returned %d taps, want %d: %v", len(got), len(want), taps)
	}
	for eni, ep := range want {
		if got[eni] != ep {
			t.Errorf("endpoint for %s = %q, want %q", eni, got[eni], ep)
		}
	}
	// The guest tap and the br-imds-end patch must never be queried.
	if s.called("ovs-vsctl get Interface tap0deadbeef") {
		t.Error("guest tap must not be queried for an iface-id")
	}
	if s.called("ovs-vsctl get Interface " + IMDSPatchPort(eniA)) {
		t.Error("br-imds-end patch must not be queried for an iface-id")
	}
}

// A patch port whose iface-id is unreadable or unexpected is skipped, not fatal:
// one malformed port must not stall serving for the rest of the chassis.
func TestListIMDSTaps_SkipsBadPorts(t *testing.T) {
	const (
		eniOK    = "eni-0aaa1111deadbeef"
		eniErr   = "eni-0bbb2222cafef00d"
		eniWrong = "eni-0ccc3333beeff00d"
	)
	s := newStubRunner()
	s.expect("ovs-vsctl list-ports br-int",
		[]byte(IMDSIntPatchPort(eniOK)+"\n"+IMDSIntPatchPort(eniErr)+"\n"+IMDSIntPatchPort(eniWrong)+"\n"), nil)
	s.expect("ovs-vsctl get Interface "+IMDSIntPatchPort(eniOK)+" external_ids:iface-id",
		[]byte("\""+vm.OVSIfaceID(eniOK)+"\"\n"), nil)
	s.expect("ovs-vsctl get Interface "+IMDSIntPatchPort(eniErr)+" external_ids:iface-id",
		nil, errors.New("ovs-vsctl: no key iface-id"))
	// Missing the "port-" prefix — recovered ENI would be wrong, so it is dropped.
	s.expect("ovs-vsctl get Interface "+IMDSIntPatchPort(eniWrong)+" external_ids:iface-id",
		[]byte("\"bogus\"\n"), nil)

	taps, err := ListIMDSTaps(context.Background(), s)
	if err != nil {
		t.Fatalf("ListIMDSTaps: %v", err)
	}
	if len(taps) != 1 || taps[0].ENIID != eniOK {
		t.Fatalf("ListIMDSTaps = %v, want only %s", taps, eniOK)
	}
}

// A failure listing br-int ports is fatal to one reconcile pass (the caller
// retries on the next tick), not silently empty.
func TestListIMDSTaps_ListPortsErrorPropagates(t *testing.T) {
	s := newStubRunner()
	s.expect("ovs-vsctl list-ports br-int", nil, errors.New("ovs down"))
	if _, err := ListIMDSTaps(context.Background(), s); err == nil {
		t.Fatal("expected error when list-ports fails")
	}
}
