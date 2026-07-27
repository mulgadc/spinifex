package ovn

import (
	"context"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/ovntest"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
)

func ptr(s string) *string { return &s }

func TestACLSetEqual(t *testing.T) {
	specs := []ACLSpec{
		{Direction: "to-lport", Priority: 1001, Match: "ip4", Action: "allow-related", Name: "a", Severity: "info"},
		{Direction: "from-lport", Priority: 1002, Match: "ip6", Action: "drop"},
	}
	rows := []nbdb.ACL{
		{Direction: "from-lport", Priority: 1002, Match: "ip6", Action: "drop"},
		{Direction: "to-lport", Priority: 1001, Match: "ip4", Action: "allow-related", Name: ptr("a"), Severity: ptr("info")},
	}

	if !ACLSetEqual(rows, specs) {
		t.Errorf("reordered equal set reported unequal")
	}
	if ACLSetEqual(rows[:1], specs) {
		t.Errorf("count mismatch reported equal")
	}

	diff := []nbdb.ACL{rows[0], {Direction: "to-lport", Priority: 1001, Match: "ip4", Action: "drop"}}
	if ACLSetEqual(diff, specs) {
		t.Errorf("content mismatch reported equal")
	}
}

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

// TestIsEnsureProbeTimeout pins which failed transaction results are the
// idempotent ensure path rather than a fault, since only the former is
// downgraded out of the ERROR log.
func TestIsEnsureProbeTimeout(t *testing.T) {
	ops := []ovsdb.Operation{
		{Op: ovsdb.OperationWait, Table: "Logical_Router"},
		{Op: ovsdb.OperationInsert, Table: "Logical_Router"},
	}

	tests := []struct {
		name      string
		index     int
		resultErr string
		want      bool
	}{
		{"ensure probe found existing row", 0, "timed out", true},
		{"wait op failed for another reason", 0, "constraint violation", false},
		{"insert op timed out", 1, "timed out", false},
		{"index past the op list", 5, "timed out", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isEnsureProbeTimeout(ops, tt.index, tt.resultErr); got != tt.want {
				t.Errorf("isEnsureProbeTimeout(ops, %d, %q) = %v, want %v", tt.index, tt.resultErr, got, tt.want)
			}
		})
	}
}

// TestTransactOps_FailedResults drives transactOps over a real NB DB for both
// kinds of failed result, since the two are logged at different levels and
// only one of them is a genuine fault.
func TestTransactOps_FailedResults(t *testing.T) {
	nb := ovntest.StartNB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	cli := NewLiveClient(nb.Endpoint)
	if err := cli.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(cli.Close)

	if _, _, err := cli.EnsureLogicalRouter(ctx, &nbdb.LogicalRouter{Name: "lr-dup"}); err != nil {
		t.Fatalf("seed EnsureLogicalRouter: %v", err)
	}

	// Re-ensuring an existing row is what the caller's cache lookup normally
	// absorbs; going straight to transactOps reproduces the case where it did
	// not, and the guard probe fires instead.
	t.Run("ensure probe on existing row", func(t *testing.T) {
		ops, err := cli.ensureNamedRowOps("Logical_Router", "lr-dup", &nbdb.LogicalRouter{
			UUID: namedUUID("lr_", "lr-dup"),
			Name: "lr-dup",
		})
		if err != nil {
			t.Fatalf("ensureNamedRowOps: %v", err)
		}
		if err := cli.transactOps(ctx, ops); err == nil {
			t.Fatal("transactOps succeeded, want the probe to report the row already exists")
		}
	})

	// Two same-named inserts in one transaction violate the schema's name
	// index — a real fault, and the path that must stay at ERROR.
	t.Run("genuine constraint violation", func(t *testing.T) {
		var ops []ovsdb.Operation
		for _, uuid := range []string{"ls_a", "ls_b"} {
			createOps, err := cli.client.Create(&nbdb.LogicalSwitch{UUID: uuid, Name: "ls-clash"})
			if err != nil {
				t.Fatalf("build create ops: %v", err)
			}
			ops = append(ops, createOps...)
		}
		if err := cli.transactOps(ctx, ops); err == nil {
			t.Fatal("transactOps succeeded, want a constraint violation")
		}
	})
}
