package invariants

import "testing"

func TestS4_HasOVNPrefix(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"vpc-abc", true},
		{"subnet-1", true},
		{"gw-port-x", true},
		{"ts-az1-vpc1", true},
		{"vpc-", false}, // bare prefix, no ID
		{"vpc", false},  // not prefixed
		{"hello-vpc-x", false},
		{"port-to-br", true}, // matches "port-" — detected by string-prefix
		// note: port-to-br is an ovs-vsctl subcommand, not an OVN object
		// name. The S4 walker exempts network/host/ ovs-vsctl callsites
		// indirectly because they aren't in `<lit> + <ident>` or
		// fmt.Sprintf format-string position. This test confirms the
		// prefix matcher is intentionally broad; precision comes from
		// the AST context, not the matcher.
		{"vpc-cidr-assoc-", false},         // AWS resource ID prefix, not OVN
		{"vpc-cidr-assoc-deadbeef", false}, // AWS resource ID, not OVN
	}
	for _, tc := range cases {
		if got := hasOVNPrefix(tc.in); got != tc.want {
			t.Errorf("hasOVNPrefix(%q) = %v; want %v", tc.in, got, tc.want)
		}
	}
}
