package vpcd

import (
	"context"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/services/vpcd/nbdb"
)

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
		// ACL match expressions embed these characters; OVSDB rejects unsanitised
		// named-uuids with "Type mismatch for member 'uuid-name'", which would
		// silently break every default-SG ACL transaction.
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


func TestMockOVNClient_DeleteAllNATsByExternalIP(t *testing.T) {
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	// Create two routers with NAT rules sharing the same external IP.
	_ = mock.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{
		Name:        "vpc-r1",
		ExternalIDs: map[string]string{"spinifex:vpc_id": "r1"},
	})
	_ = mock.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{
		Name:        "vpc-r2",
		ExternalIDs: map[string]string{"spinifex:vpc_id": "r2"},
	})

	_ = mock.AddNAT(ctx, "vpc-r1", &nbdb.NAT{
		Type: "dnat_and_snat", ExternalIP: "192.168.1.50", LogicalIP: "10.0.0.1",
	})
	_ = mock.AddNAT(ctx, "vpc-r2", &nbdb.NAT{
		Type: "dnat_and_snat", ExternalIP: "192.168.1.50", LogicalIP: "10.200.0.1",
	})
	// Unrelated NAT on r1 that should be untouched.
	_ = mock.AddNAT(ctx, "vpc-r1", &nbdb.NAT{
		Type: "dnat_and_snat", ExternalIP: "192.168.1.99", LogicalIP: "10.0.0.2",
	})

	removed, err := mock.DeleteAllNATsByExternalIP(ctx, "dnat_and_snat", "192.168.1.50")
	if err != nil {
		t.Fatalf("DeleteAllNATsByExternalIP: %v", err)
	}
	if removed != 2 {
		t.Errorf("expected 2 removed, got %d", removed)
	}

	r1, _ := mock.GetLogicalRouter(ctx, "vpc-r1")
	if len(r1.NAT) != 1 {
		t.Errorf("r1 should retain 1 unrelated NAT, got %d", len(r1.NAT))
	}

	r2, _ := mock.GetLogicalRouter(ctx, "vpc-r2")
	if len(r2.NAT) != 0 {
		t.Errorf("r2 should have 0 NAT rules, got %d", len(r2.NAT))
	}

	// Deleting again should be a no-op.
	removed, err = mock.DeleteAllNATsByExternalIP(ctx, "dnat_and_snat", "192.168.1.50")
	if err != nil {
		t.Fatalf("second DeleteAllNATsByExternalIP: %v", err)
	}
	if removed != 0 {
		t.Errorf("expected 0 removed on second call, got %d", removed)
	}
}


// --- SetGatewayChassis idempotency (mulga-999) ---

func TestSetGatewayChassis_Idempotent(t *testing.T) {
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	if err := mock.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{Name: "vpc-A"}); err != nil {
		t.Fatalf("CreateLogicalRouter: %v", err)
	}
	if err := mock.CreateLogicalRouterPort(ctx, "vpc-A", &nbdb.LogicalRouterPort{Name: "gw-A"}); err != nil {
		t.Fatalf("CreateLogicalRouterPort: %v", err)
	}

	if err := mock.SetGatewayChassis(ctx, "gw-A", "chassis-1", 20); err != nil {
		t.Fatalf("SetGatewayChassis (1): %v", err)
	}
	if err := mock.SetGatewayChassis(ctx, "gw-A", "chassis-1", 20); err != nil {
		t.Fatalf("SetGatewayChassis (2): %v", err)
	}

	rows, err := mock.ListGatewayChassis(ctx)
	if err != nil {
		t.Fatalf("ListGatewayChassis: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d (%v)", len(rows), rows)
	}
	if mock.SetGatewayChassisCalls != 1 {
		t.Errorf("expected 1 create call, got %d", mock.SetGatewayChassisCalls)
	}
	if mock.UpdateGatewayChassisPriorityCalls != 0 {
		t.Errorf("expected 0 priority updates, got %d", mock.UpdateGatewayChassisPriorityCalls)
	}
}

func TestSetGatewayChassis_UpdatesPriority(t *testing.T) {
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	if err := mock.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{Name: "vpc-A"}); err != nil {
		t.Fatalf("CreateLogicalRouter: %v", err)
	}
	if err := mock.CreateLogicalRouterPort(ctx, "vpc-A", &nbdb.LogicalRouterPort{Name: "gw-A"}); err != nil {
		t.Fatalf("CreateLogicalRouterPort: %v", err)
	}

	if err := mock.SetGatewayChassis(ctx, "gw-A", "chassis-1", 20); err != nil {
		t.Fatalf("SetGatewayChassis at 20: %v", err)
	}
	if err := mock.SetGatewayChassis(ctx, "gw-A", "chassis-1", 15); err != nil {
		t.Fatalf("SetGatewayChassis at 15: %v", err)
	}

	rows, err := mock.ListGatewayChassis(ctx)
	if err != nil {
		t.Fatalf("ListGatewayChassis: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row (mutated, not duplicated), got %d", len(rows))
	}
	if rows[0].Priority != 15 {
		t.Errorf("expected priority 15 after update, got %d", rows[0].Priority)
	}
	if mock.UpdateGatewayChassisPriorityCalls != 1 {
		t.Errorf("expected 1 priority update, got %d", mock.UpdateGatewayChassisPriorityCalls)
	}
}
