package host

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestInstallTapPatchValidate(t *testing.T) {
	s := newStubRunner()
	d := testDatapath()
	d.IfaceID = ""
	if err := installTapPatch(context.Background(), s, d); err == nil ||
		!strings.Contains(err.Error(), "IfaceID") {
		t.Fatalf("expected IfaceID validation error, got %v", err)
	}
	if len(s.calls) != 0 {
		t.Errorf("validation must fail before issuing commands; calls: %v", s.calls)
	}
}

func TestInstallTapPatch(t *testing.T) {
	s := newStubRunner()
	s.expect("ovs-vsctl", nil, nil)
	s.expect("ovs-ofctl", nil, nil)

	d := testDatapath()
	if err := installTapPatch(context.Background(), s, d); err != nil {
		t.Fatalf("installTapPatch: %v", err)
	}
	cookie := imdsFlowCookie(d.Endpoint)

	want := []string{
		// IMDSBridge end of the patch, peered to the br-int end.
		"ovs-vsctl --may-exist add-port " + IMDSBridge + " " + d.PatchIMDS + " -- set Interface " + d.PatchIMDS + " type=patch options:peer=" + d.PatchInt,
		// br-int end: carries the OVN iface-id + attached-mac binding.
		"ovs-vsctl --may-exist add-port br-int " + d.PatchInt + " -- set Interface " + d.PatchInt + " type=patch options:peer=" + d.PatchIMDS + " external_ids:iface-id=" + d.IfaceID + " external_ids:attached-mac=" + d.GuestMAC,
		// Forward flows tap<->patch, below the demux priority.
		"ovs-ofctl add-flow " + IMDSBridge + " cookie=" + cookie + fmt.Sprintf(",table=0,priority=%d,in_port=%s,actions=output:%s", imdsForwardPriority, d.Tap, d.PatchIMDS),
		"ovs-ofctl add-flow " + IMDSBridge + " cookie=" + cookie + fmt.Sprintf(",table=0,priority=%d,in_port=%s,actions=output:%s", imdsForwardPriority, d.PatchIMDS, d.Tap),
	}
	for _, w := range want {
		if !s.called(w) {
			t.Errorf("missing command:\n  %q\ncalls: %v", w, s.calls)
		}
	}
}

// TestForwardBelowDemux locks in the priority ordering: .254/.253 must be
// intercepted by the demux flows before the catch-all forward flows bridge the
// rest to br-int. A regression here would either black-hole guest traffic or
// leak IMDS requests past the responder.
func TestForwardBelowDemux(t *testing.T) {
	if imdsForwardPriority >= imdsDemuxPriority {
		t.Fatalf("forward priority (%d) must be below demux priority (%d)", imdsForwardPriority, imdsDemuxPriority)
	}
}

// TestInstallTapPatchTouchesBrInt asserts the br-int end is created on br-int —
// the whole point of the patch is to bind the guest LSP via ovn-controller.
func TestInstallTapPatchTouchesBrInt(t *testing.T) {
	s := newStubRunner()
	s.expect("ovs-vsctl", nil, nil)
	s.expect("ovs-ofctl", nil, nil)

	d := testDatapath()
	if err := installTapPatch(context.Background(), s, d); err != nil {
		t.Fatalf("installTapPatch: %v", err)
	}
	var sawBrIntPatch bool
	for _, c := range s.calls {
		if strings.HasPrefix(c, "ovs-vsctl --may-exist add-port br-int "+d.PatchInt) &&
			strings.Contains(c, "external_ids:iface-id="+d.IfaceID) {
			sawBrIntPatch = true
		}
	}
	if !sawBrIntPatch {
		t.Errorf("br-int patch end with iface-id not created; calls: %v", s.calls)
	}
}
