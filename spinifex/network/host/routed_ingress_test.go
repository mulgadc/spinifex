package host

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestEnsureVPCIngressRoute_InstallsRoute(t *testing.T) {
	r := newStubRunner()
	r.expect("ip route replace", nil, nil)

	if err := EnsureVPCIngressRoute(context.Background(), r, "10.0.0.0/16", "100.127.0.10"); err != nil {
		t.Fatalf("EnsureVPCIngressRoute: %v", err)
	}
	want := "ip route replace 10.0.0.0/16 via 100.127.0.10 dev " + NATTransitHostEnd
	if !r.called(want) {
		t.Errorf("missing route call:\n  want %q\n  got  %v", want, r.calls)
	}
}

func TestEnsureVPCIngressRoute_ValidatesArgs(t *testing.T) {
	r := newStubRunner()
	if err := EnsureVPCIngressRoute(context.Background(), r, "", "100.127.0.10"); err == nil {
		t.Error("empty vpcCIDR: expected error")
	}
	if err := EnsureVPCIngressRoute(context.Background(), r, "10.0.0.0/16", ""); err == nil {
		t.Error("empty gwLrpIP: expected error")
	}
	if len(r.calls) != 0 {
		t.Errorf("validation failures must not run commands: %v", r.calls)
	}
}

func TestEnsureVPCIngressRoute_RouteFailure(t *testing.T) {
	r := newStubRunner()
	r.expect("ip route replace", []byte("RTNETLINK answers: no such device"), fmt.Errorf("exit 2"))

	err := EnsureVPCIngressRoute(context.Background(), r, "10.0.0.0/16", "100.127.0.10")
	if err == nil || !strings.Contains(err.Error(), "no such device") {
		t.Fatalf("expected route failure error, got: %v", err)
	}
}

func TestRemoveVPCIngressRoute_DeletesRoute(t *testing.T) {
	r := newStubRunner()
	r.expect("ip route del", nil, nil)

	if err := RemoveVPCIngressRoute(context.Background(), r, "10.0.0.0/16"); err != nil {
		t.Fatalf("RemoveVPCIngressRoute: %v", err)
	}
	want := "ip route del 10.0.0.0/16 dev " + NATTransitHostEnd
	if !r.called(want) {
		t.Errorf("missing route del call:\n  want %q\n  got  %v", want, r.calls)
	}
}

func TestRemoveVPCIngressRoute_MissingRouteNotError(t *testing.T) {
	r := newStubRunner()
	r.expect("ip route del", []byte("RTNETLINK answers: No such process"), fmt.Errorf("exit 2"))

	if err := RemoveVPCIngressRoute(context.Background(), r, "10.0.0.0/16"); err != nil {
		t.Fatalf("missing route must not be an error, got: %v", err)
	}
}
