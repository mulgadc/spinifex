package topology

import "testing"

func TestBuildSubnetDHCPOptions(t *testing.T) {
	const (
		gwIP      = "10.0.1.1"
		routerMAC = "02:00:00:00:00:01"
		dnsServer = "{8.8.8.8, 1.1.1.1}"
	)

	opts := BuildSubnetDHCPOptions(gwIP, routerMAC, dnsServer)

	want := map[string]string{
		"server_id":  gwIP,
		"server_mac": routerMAC,
		"lease_time": "3600",
		"router":     gwIP,
		"dns_server": dnsServer,
		"mtu":        "1442",
	}

	if len(opts) != len(want) {
		t.Fatalf("option count = %d, want %d (%v)", len(opts), len(want), opts)
	}
	for k, v := range want {
		if got := opts[k]; got != v {
			t.Errorf("opts[%q] = %q, want %q", k, got, v)
		}
	}
}
