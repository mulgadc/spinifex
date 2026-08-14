package topology

import "testing"

// IMDS is served by a subnet-switch localport over L2, not option 121, so
// BuildSubnetDHCPOptions must not emit classless_static_route.
func TestBuildSubnetDHCPOptions_NoClasslessStaticRoute(t *testing.T) {
	opts := BuildSubnetDHCPOptions("10.0.1.1", "02:00:00:00:00:01", "{8.8.8.8, 1.1.1.1}", true)

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

// The advertised MTU must budget for every header the egress path adds, or
// large inbound segments are silently dropped and surface as TLS handshake
// timeouts rather than as anything MTU-shaped.
func TestSubnetMTU_BudgetsGeneveAndESP(t *testing.T) {
	if got := SubnetMTU(true); got != 1408 {
		t.Errorf("SubnetMTU(ipsec on) = %d, want 1408 (1500 - 58 geneve - 34 ESP)", got)
	}
	if got := SubnetMTU(false); got != 1442 {
		t.Errorf("SubnetMTU(ipsec off) = %d, want 1442 (1500 - 58 geneve)", got)
	}
}

func TestBuildSubnetDHCPOptions_MTUFollowsIPSec(t *testing.T) {
	on := BuildSubnetDHCPOptions("10.0.1.1", "02:00:00:00:00:01", "{8.8.8.8}", true)
	if got := on["mtu"]; got != "1408" {
		t.Errorf("mtu with IPsec on = %q, want \"1408\"", got)
	}
	off := BuildSubnetDHCPOptions("10.0.1.1", "02:00:00:00:00:01", "{8.8.8.8}", false)
	if got := off["mtu"]; got != "1442" {
		t.Errorf("mtu with IPsec off = %q, want \"1442\"", got)
	}
}
