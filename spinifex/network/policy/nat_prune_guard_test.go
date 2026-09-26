package policy_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/network/policy"
)

// An empty live set is what a failed intent read looks like from here, and the
// sweep deletes on absence, so it must refuse rather than clear every row.
// Unstamped rows are excluded from the count: they are never swept anyway, and a
// NAT gateway's snat is one of them.
func TestPruneOrphanEIPs_RefusesAnEmptyLiveSet(t *testing.T) {
	m := mock.New()
	nm, err := policy.NewNATManager(m, policy.NATModeDistributed)
	if err != nil {
		t.Fatalf("NewNATManager: %v", err)
	}
	ctx := context.Background()
	if err := m.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{Name: "vpc-a"}); err != nil {
		t.Fatalf("CreateLogicalRouter: %v", err)
	}
	if err := m.AddNAT(ctx, "vpc-a", &nbdb.NAT{
		Type: "dnat_and_snat", ExternalIP: "192.168.1.10", LogicalIP: "10.0.1.10",
		ExternalIDs: map[string]string{"spinifex:logical_port": "port-eni-a"},
	}); err != nil {
		t.Fatalf("AddNAT: %v", err)
	}

	empty := policy.LiveEIPs{Ports: map[string]struct{}{}, ExternalIPs: map[string]struct{}{}}
	pruned, err := nm.PruneOrphanEIPs(ctx, empty)
	if !errors.Is(err, policy.ErrEmptyEIPIntent) {
		t.Fatalf("PruneOrphanEIPs err = %v, want policy.ErrEmptyEIPIntent", err)
	}
	if pruned != 0 {
		t.Errorf("pruned = %d, want 0", pruned)
	}
	nats, err := m.ListNATs(ctx)
	if err != nil {
		t.Fatalf("ListNATs: %v", err)
	}
	if len(nats) != 1 {
		t.Errorf("rows = %d, want the row left intact", len(nats))
	}
}

// With no stamped rows to lose there is nothing to protect, so an empty live set
// is just an idle node and the sweep proceeds normally.
func TestPruneOrphanEIPs_EmptyLiveSetIsFineWithNoStampedRows(t *testing.T) {
	m := mock.New()
	nm, err := policy.NewNATManager(m, policy.NATModeDistributed)
	if err != nil {
		t.Fatalf("NewNATManager: %v", err)
	}
	ctx := context.Background()
	if err := m.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{Name: "vpc-a"}); err != nil {
		t.Fatalf("CreateLogicalRouter: %v", err)
	}
	if err := m.AddNAT(ctx, "vpc-a", &nbdb.NAT{
		Type: "snat", ExternalIP: "192.168.1.240", LogicalIP: "10.99.2.0/24",
	}); err != nil {
		t.Fatalf("AddNAT snat: %v", err)
	}

	empty := policy.LiveEIPs{Ports: map[string]struct{}{}, ExternalIPs: map[string]struct{}{}}
	if _, err := nm.PruneOrphanEIPs(ctx, empty); err != nil {
		t.Fatalf("PruneOrphanEIPs: %v", err)
	}
	nats, err := m.ListNATs(ctx)
	if err != nil {
		t.Fatalf("ListNATs: %v", err)
	}
	if len(nats) != 1 {
		t.Errorf("NAT gateway snat row was touched by the dnat_and_snat sweep")
	}
}
