package ovn

import "testing"

func TestNamedUUID(t *testing.T) {
	tests := []struct {
		name     string
		prefix   string
		input    string
		expected string
	}{
		{"hyphen replacement", "sw_", "vpc-abc123", "sw_vpc_abc123"},
		{"slash and dot replacement", "rp_", "10.0.0.1/24", "rp_10_0_0_1_24"},
		{"no special characters", "lr_", "vpcabc", "lr_vpcabc"},
		{"empty name", "sw_", "", "sw_"},
		{"empty prefix", "", "vpc-1", "vpc_1"},
		{"both empty", "", "", ""},
		{"multiple consecutive hyphens", "x_", "a--b", "x_a__b"},
		{"mixed special chars", "p_", "a.b-c/d", "p_a_b_c_d"},
		// ACL match expressions embed @, =, &, space; OVSDB rejects unsanitised
		// named-uuids with "Type mismatch for member 'uuid-name'".
		{"acl match at sign", "acl_", "outport == @pg-sg-XYZ && ip4", "acl_outport_____pg_sg_XYZ____ip4"},
		{"space replacement", "acl_", "a b c", "acl_a_b_c"},
		{"equals replacement", "acl_", "a==b", "acl_a__b"},
		{"ampersand replacement", "acl_", "a&&b", "acl_a__b"},
		{"colon replacement", "acl_", "ip6.src == fe80::1", "acl_ip6_src____fe80__1"},
		{"parens and quotes", "acl_", `ip4.src == "10.0.0.1"`, "acl_ip4_src_____10_0_0_1_"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := namedUUID(tt.prefix, tt.input)
			if got != tt.expected {
				t.Errorf("namedUUID(%q, %q) = %q, want %q", tt.prefix, tt.input, got, tt.expected)
			}
		})
	}
}
