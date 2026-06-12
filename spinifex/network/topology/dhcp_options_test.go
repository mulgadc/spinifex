package topology

import "testing"

// IMDS is served by a subnet-switch localport over L2, not option 121, so
// BuildSubnetDHCPOptions must not emit classless_static_route.
func TestBuildSubnetDHCPOptions_NoClasslessStaticRoute(t *testing.T) {
	opts := BuildSubnetDHCPOptions("10.0.1.1", "02:00:00:00:00:01", "{8.8.8.8, 1.1.1.1}")

	if _, ok := opts["classless_static_route"]; ok {
		t.Errorf("classless_static_route must be absent (IMDS is served by a subnet-switch localport over L2, not DHCP option 121); got %q", opts["classless_static_route"])
	}
	if got := opts["router"]; got != "10.0.1.1" {
		t.Errorf("router = %q, want %q", got, "10.0.1.1")
	}
	if got := opts["dns_server"]; got != "{8.8.8.8, 1.1.1.1}" {
		t.Errorf("dns_server = %q, want %q", got, "{8.8.8.8, 1.1.1.1}")
	}
	if got := opts["server_id"]; got != "10.0.1.1" {
		t.Errorf("server_id = %q, want %q", got, "10.0.1.1")
	}
}
