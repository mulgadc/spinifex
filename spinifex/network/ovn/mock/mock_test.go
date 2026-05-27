package mock

import (
	"context"
	"sync"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
)

func TestClient_DeleteAllNATsByExternalIP(t *testing.T) {
	m := New()
	_ = m.Connect(context.Background())
	ctx := context.Background()

	_ = m.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{
		Name:        "vpc-r1",
		ExternalIDs: map[string]string{"spinifex:vpc_id": "r1"},
	})
	_ = m.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{
		Name:        "vpc-r2",
		ExternalIDs: map[string]string{"spinifex:vpc_id": "r2"},
	})

	_ = m.AddNAT(ctx, "vpc-r1", &nbdb.NAT{
		Type: "dnat_and_snat", ExternalIP: "192.168.1.50", LogicalIP: "10.0.0.1",
	})
	_ = m.AddNAT(ctx, "vpc-r2", &nbdb.NAT{
		Type: "dnat_and_snat", ExternalIP: "192.168.1.50", LogicalIP: "10.200.0.1",
	})
	// Unrelated NAT on r1 that should be untouched.
	_ = m.AddNAT(ctx, "vpc-r1", &nbdb.NAT{
		Type: "dnat_and_snat", ExternalIP: "192.168.1.99", LogicalIP: "10.0.0.2",
	})

	removed, err := m.DeleteAllNATsByExternalIP(ctx, "dnat_and_snat", "192.168.1.50")
	if err != nil {
		t.Fatalf("DeleteAllNATsByExternalIP: %v", err)
	}
	if removed != 2 {
		t.Errorf("expected 2 removed, got %d", removed)
	}

	r1, _ := m.GetLogicalRouter(ctx, "vpc-r1")
	if len(r1.NAT) != 1 {
		t.Errorf("r1 should retain 1 unrelated NAT, got %d", len(r1.NAT))
	}

	r2, _ := m.GetLogicalRouter(ctx, "vpc-r2")
	if len(r2.NAT) != 0 {
		t.Errorf("r2 should have 0 NAT rules, got %d", len(r2.NAT))
	}

	removed, err = m.DeleteAllNATsByExternalIP(ctx, "dnat_and_snat", "192.168.1.50")
	if err != nil {
		t.Fatalf("second DeleteAllNATsByExternalIP: %v", err)
	}
	if removed != 0 {
		t.Errorf("expected 0 removed on second call, got %d", removed)
	}
}

func TestSetGatewayChassis_Idempotent(t *testing.T) {
	m := New()
	_ = m.Connect(context.Background())
	ctx := context.Background()

	if err := m.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{Name: "vpc-A"}); err != nil {
		t.Fatalf("CreateLogicalRouter: %v", err)
	}
	if err := m.CreateLogicalRouterPort(ctx, "vpc-A", &nbdb.LogicalRouterPort{Name: "gw-A"}); err != nil {
		t.Fatalf("CreateLogicalRouterPort: %v", err)
	}

	if err := m.SetGatewayChassis(ctx, "gw-A", "chassis-1", 20); err != nil {
		t.Fatalf("SetGatewayChassis (1): %v", err)
	}
	if err := m.SetGatewayChassis(ctx, "gw-A", "chassis-1", 20); err != nil {
		t.Fatalf("SetGatewayChassis (2): %v", err)
	}

	rows, err := m.ListGatewayChassis(ctx)
	if err != nil {
		t.Fatalf("ListGatewayChassis: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d (%v)", len(rows), rows)
	}
	if m.SetGatewayChassisCalls != 1 {
		t.Errorf("expected 1 create call, got %d", m.SetGatewayChassisCalls)
	}
	if m.UpdateGatewayChassisPriorityCalls != 0 {
		t.Errorf("expected 0 priority updates, got %d", m.UpdateGatewayChassisPriorityCalls)
	}
}

func TestSetGatewayChassis_UpdatesPriority(t *testing.T) {
	m := New()
	_ = m.Connect(context.Background())
	ctx := context.Background()

	if err := m.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{Name: "vpc-A"}); err != nil {
		t.Fatalf("CreateLogicalRouter: %v", err)
	}
	if err := m.CreateLogicalRouterPort(ctx, "vpc-A", &nbdb.LogicalRouterPort{Name: "gw-A"}); err != nil {
		t.Fatalf("CreateLogicalRouterPort: %v", err)
	}

	if err := m.SetGatewayChassis(ctx, "gw-A", "chassis-1", 20); err != nil {
		t.Fatalf("SetGatewayChassis at 20: %v", err)
	}
	if err := m.SetGatewayChassis(ctx, "gw-A", "chassis-1", 15); err != nil {
		t.Fatalf("SetGatewayChassis at 15: %v", err)
	}

	rows, err := m.ListGatewayChassis(ctx)
	if err != nil {
		t.Fatalf("ListGatewayChassis: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row (mutated, not duplicated), got %d", len(rows))
	}
	if rows[0].Priority != 15 {
		t.Errorf("expected priority 15 after update, got %d", rows[0].Priority)
	}
	if m.UpdateGatewayChassisPriorityCalls != 1 {
		t.Errorf("expected 1 priority update, got %d", m.UpdateGatewayChassisPriorityCalls)
	}
}

// N concurrent EnsureLogicalRouter calls for the same Name must converge to
// a single row.
func TestEnsureLogicalRouter_ConcurrentSingleSurvivor(t *testing.T) {
	const callers = 50
	m := New()
	_ = m.Connect(context.Background())
	ctx := context.Background()

	uuids := make([]string, callers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			lr, err := m.EnsureLogicalRouter(ctx, &nbdb.LogicalRouter{
				Name:        "vpc-vpc-X",
				ExternalIDs: map[string]string{"spinifex:vpc_id": "vpc-X"},
			})
			if err != nil {
				t.Errorf("EnsureLogicalRouter[%d]: %v", i, err)
				return
			}
			uuids[i] = lr.UUID
		}(i)
	}
	close(start)
	wg.Wait()

	first := uuids[0]
	if first == "" {
		t.Fatalf("caller 0 got empty UUID")
	}
	for i, u := range uuids {
		if u != first {
			t.Errorf("caller %d UUID mismatch: %q vs %q", i, u, first)
		}
	}

	rows, err := m.ListLogicalRouters(ctx)
	if err != nil {
		t.Fatalf("ListLogicalRouters: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 router after concurrent EnsureLogicalRouter, got %d", len(rows))
	}
}

// Sequential second EnsureLogicalRouter for an existing Name returns the
// pre-existing row.
func TestEnsureLogicalRouter_ReturnsExistingOnSecondCall(t *testing.T) {
	m := New()
	_ = m.Connect(context.Background())
	ctx := context.Background()

	first, err := m.EnsureLogicalRouter(ctx, &nbdb.LogicalRouter{
		Name:        "vpc-vpc-Y",
		ExternalIDs: map[string]string{"spinifex:vpc_id": "vpc-Y", "spinifex:cidr": ""},
	})
	if err != nil {
		t.Fatalf("EnsureLogicalRouter first call: %v", err)
	}
	second, err := m.EnsureLogicalRouter(ctx, &nbdb.LogicalRouter{
		Name:        "vpc-vpc-Y",
		ExternalIDs: map[string]string{"spinifex:vpc_id": "vpc-Y", "spinifex:cidr": "10.0.0.0/16"},
	})
	if err != nil {
		t.Fatalf("EnsureLogicalRouter second call: %v", err)
	}
	if first.UUID != second.UUID {
		t.Errorf("second call returned different UUID: %q vs %q", second.UUID, first.UUID)
	}
}

func TestEnsureLogicalSwitch_ConcurrentSingleSurvivor(t *testing.T) {
	const callers = 30
	m := New()
	_ = m.Connect(context.Background())
	ctx := context.Background()

	uuids := make([]string, callers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ls, err := m.EnsureLogicalSwitch(ctx, &nbdb.LogicalSwitch{
				Name:        "subnet-subnet-Z",
				ExternalIDs: map[string]string{"spinifex:subnet_id": "subnet-Z"},
			})
			if err != nil {
				t.Errorf("EnsureLogicalSwitch[%d]: %v", i, err)
				return
			}
			uuids[i] = ls.UUID
		}(i)
	}
	close(start)
	wg.Wait()

	first := uuids[0]
	for i, u := range uuids {
		if u != first {
			t.Errorf("caller %d UUID mismatch: %q vs %q", i, u, first)
		}
	}
	rows, err := m.ListLogicalSwitches(ctx)
	if err != nil {
		t.Fatalf("ListLogicalSwitches: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 switch, got %d", len(rows))
	}
}

func TestEnsurePortGroup_ConcurrentSingleSurvivor(t *testing.T) {
	const callers = 30
	m := New()
	_ = m.Connect(context.Background())
	ctx := context.Background()

	uuids := make([]string, callers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			pg, err := m.EnsurePortGroup(ctx, "sg-pg-A", nil)
			if err != nil {
				t.Errorf("EnsurePortGroup[%d]: %v", i, err)
				return
			}
			uuids[i] = pg.UUID
		}(i)
	}
	close(start)
	wg.Wait()

	first := uuids[0]
	for i, u := range uuids {
		if u != first {
			t.Errorf("caller %d UUID mismatch: %q vs %q", i, u, first)
		}
	}
	rows, err := m.ListPortGroups(ctx)
	if err != nil {
		t.Fatalf("ListPortGroups: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 port group, got %d", len(rows))
	}
}
