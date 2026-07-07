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

func TestVPCIngressRouteVia_ParsesHolder(t *testing.T) {
	r := newStubRunner()
	r.expect("ip route show", []byte("172.31.0.0/16 via 100.127.0.239 dev spx-nat-host \n"), nil)

	via, err := VPCIngressRouteVia(context.Background(), r, "172.31.0.0/16")
	if err != nil {
		t.Fatalf("VPCIngressRouteVia: %v", err)
	}
	if via != "100.127.0.239" {
		t.Errorf("via = %q, want 100.127.0.239", via)
	}
	want := "ip route show 172.31.0.0/16 dev " + NATTransitHostEnd
	if !r.called(want) {
		t.Errorf("missing route show call:\n  want %q\n  got  %v", want, r.calls)
	}
}

func TestVPCIngressRouteVia_NoRoute(t *testing.T) {
	r := newStubRunner()
	r.expect("ip route show", nil, nil)

	via, err := VPCIngressRouteVia(context.Background(), r, "172.31.0.0/16")
	if err != nil {
		t.Fatalf("VPCIngressRouteVia: %v", err)
	}
	if via != "" {
		t.Errorf("via = %q, want empty for absent route", via)
	}
}

func TestVPCIngressRouteVia_ValidatesArgs(t *testing.T) {
	r := newStubRunner()
	if _, err := VPCIngressRouteVia(context.Background(), r, ""); err == nil {
		t.Error("empty vpcCIDR: expected error")
	}
	if len(r.calls) != 0 {
		t.Errorf("validation failures must not run commands: %v", r.calls)
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
