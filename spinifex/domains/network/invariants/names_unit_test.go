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
		{"port-to-br", true}, // ovs-vsctl subcommand; precision comes from AST context, not matcher
		{"vpc-cidr-assoc-", false},
		{"vpc-cidr-assoc-deadbeef", false},
	}
	for _, tc := range cases {
		if got := hasOVNPrefix(tc.in); got != tc.want {
			t.Errorf("hasOVNPrefix(%q) = %v; want %v", tc.in, got, tc.want)
		}
	}
}
