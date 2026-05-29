package topology

import "testing"

// The classless_static_route (RFC 3442 option 121) must carry the default
// route AND the IMDS /32, both via the subnet gateway. A client honouring
// option 121 ignores option 3 (router), so omitting the default route strips
// the gateway; omitting the /32 leaves guests unable to reach 169.254.169.254.
func TestBuildSubnetDHCPOptions_ClasslessStaticRoute(t *testing.T) {
	opts := BuildSubnetDHCPOptions("10.0.1.1", "02:00:00:00:00:01", "{8.8.8.8, 1.1.1.1}")

	got := opts["classless_static_route"]
	want := "{0.0.0.0/0,10.0.1.1, 169.254.169.254/32,10.0.1.1}"
	if got != want {
		t.Errorf("classless_static_route = %q, want %q", got, want)
	}
}
