package topology

import "testing"

// IMDS is served by a subnet-switch localport over L2, not option 121, so
// BuildSubnetDHCPOptions must not emit classless_static_route.
func TestBuildSubnetDHCPOptions_NoClasslessStaticRoute(t *testing.T) {
	opts := BuildSubnetDHCPOptions("10.0.1.1", "02:00:00:00:00:01", "{8.8.8.8, 1.1.1.1}", DefaultUnderlayMTU, true)

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
	if got := SubnetMTU(DefaultUnderlayMTU, true); got != 1408 {
		t.Errorf("SubnetMTU(1500, ipsec on) = %d, want 1408 (1500 - 58 geneve - 34 ESP)", got)
	}
	if got := SubnetMTU(DefaultUnderlayMTU, false); got != 1442 {
		t.Errorf("SubnetMTU(1500, ipsec off) = %d, want 1442 (1500 - 58 geneve)", got)
	}
}

// A jumbo underlay must reach the guest rather than being absorbed as headroom:
// the packet-rate saving is the entire point of raising the fabric.
func TestSubnetMTU_JumboUnderlayReachesTheGuest(t *testing.T) {
	if got := SubnetMTU(9000, false); got != 8942 {
		t.Errorf("SubnetMTU(9000, ipsec off) = %d, want 8942", got)
	}
	if got := SubnetMTU(9000, true); got != 8908 {
		t.Errorf("SubnetMTU(9000, ipsec on) = %d, want 8908", got)
	}
}

// An unset or nonsensical underlay must fall back to 1500, not produce a
// negative or tiny MTU that would black-hole the subnet.
func TestSubnetMTU_ClampsImplausibleUnderlay(t *testing.T) {
	for _, underlay := range []int{0, -1, 68, 500} {
		if got := SubnetMTU(underlay, true); got != 1408 {
			t.Errorf("SubnetMTU(%d, ipsec on) = %d, want the 1500 fallback (1408)", underlay, got)
		}
	}
}

func TestBuildSubnetDHCPOptions_MTUFollowsIPSec(t *testing.T) {
	on := BuildSubnetDHCPOptions("10.0.1.1", "02:00:00:00:00:01", "{8.8.8.8}", DefaultUnderlayMTU, true)
	if got := on["mtu"]; got != "1408" {
		t.Errorf("mtu with IPsec on = %q, want \"1408\"", got)
	}
	off := BuildSubnetDHCPOptions("10.0.1.1", "02:00:00:00:00:01", "{8.8.8.8}", DefaultUnderlayMTU, false)
	if got := off["mtu"]; got != "1442" {
		t.Errorf("mtu with IPsec off = %q, want \"1442\"", got)
	}
}
