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
	want := "ip route replace 10.0.0.0/16 via 100.127.0.10 dev " + NATTransitHostEnd + " proto " + VPCIngressRouteProto
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

// The list is what the per-node pass prunes from, so it must carry only the
// tagged routes and key them exactly as intent renders a VPC CIDR.
func TestListVPCIngressRoutes_ParsesTaggedRoutes(t *testing.T) {
	r := newStubRunner()
	r.expect("ip -o route show", []byte("172.31.0.0/16 via 100.127.0.16 \n10.1.0.0/16 via 100.127.0.17 \n"), nil)

	routes, err := ListVPCIngressRoutes(context.Background(), r)
	if err != nil {
		t.Fatalf("ListVPCIngressRoutes: %v", err)
	}
	want := map[string]string{"172.31.0.0/16": "100.127.0.16", "10.1.0.0/16": "100.127.0.17"}
	if len(routes) != len(want) {
		t.Fatalf("routes = %v, want %v", routes, want)
	}
	for cidr, via := range want {
		if routes[cidr] != via {
			t.Errorf("routes[%s] = %q, want %q", cidr, routes[cidr], via)
		}
	}
	call := "ip -o route show dev " + NATTransitHostEnd + " proto " + VPCIngressRouteProto
	if !r.called(call) {
		t.Errorf("missing list call:\n  want %q\n  got  %v", call, r.calls)
	}
}

func TestListVPCIngressRoutes_Empty(t *testing.T) {
	r := newStubRunner()
	r.expect("ip -o route show", nil, nil)

	routes, err := ListVPCIngressRoutes(context.Background(), r)
	if err != nil {
		t.Fatalf("ListVPCIngressRoutes: %v", err)
	}
	if len(routes) != 0 {
		t.Errorf("routes = %v, want none", routes)
	}
}

// A failed list must surface: read as empty it would prune nothing, but read as
// a partial table the caller could not tell what it was missing.
func TestListVPCIngressRoutes_Failure(t *testing.T) {
	r := newStubRunner()
	r.expect("ip -o route show", []byte("Cannot find device"), fmt.Errorf("exit 1"))

	if _, err := ListVPCIngressRoutes(context.Background(), r); err == nil {
		t.Fatal("expected list failure to be returned")
	}
}

func TestRemoveVPCIngressRoute_DeletesRoute(t *testing.T) {
	r := newStubRunner()
	r.expect("ip route del", nil, nil)

	if err := RemoveVPCIngressRoute(context.Background(), r, "10.0.0.0/16"); err != nil {
		t.Fatalf("RemoveVPCIngressRoute: %v", err)
	}
	want := "ip route del 10.0.0.0/16 dev " + NATTransitHostEnd + " proto " + VPCIngressRouteProto
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
