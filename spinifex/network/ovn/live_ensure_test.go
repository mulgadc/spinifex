package ovn_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/ovntest"
)

func liveClient(t *testing.T) (*ovn.LiveClient, context.Context) {
	t.Helper()
	nb := ovntest.StartNB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	cli := ovn.NewLiveClient(nb.Endpoint)
	if err := cli.Connect(ctx); err != nil {
		t.Fatalf("LiveClient.Connect: %v", err)
	}
	t.Cleanup(cli.Close)
	return cli, ctx
}

// EnsureLogicalRouter must return the persisted UUID (not the client-side
// named-uuid) on the insert path and report created; a second call reuses it.
func TestEnsureLogicalRouter_InsertResolvesRealUUID(t *testing.T) {
	cli, ctx := liveClient(t)

	got, created, err := cli.EnsureLogicalRouter(ctx, &nbdb.LogicalRouter{Name: "lr-x"})
	if err != nil {
		t.Fatalf("EnsureLogicalRouter #1: %v", err)
	}
	if !created {
		t.Fatalf("first call: created = false, want true")
	}
	if got.UUID == "" || got.UUID == "lr_lr_x" {
		t.Fatalf("insert returned non-persisted UUID %q", got.UUID)
	}

	again, created, err := cli.EnsureLogicalRouter(ctx, &nbdb.LogicalRouter{Name: "lr-x"})
	if err != nil {
		t.Fatalf("EnsureLogicalRouter #2: %v", err)
	}
	if created {
		t.Fatalf("second call: created = true, want false")
	}
	if again.UUID != got.UUID {
		t.Fatalf("second call UUID %q != first %q", again.UUID, got.UUID)
	}
}

func TestEnsurePortGroup_InsertResolvesRealUUID(t *testing.T) {
	cli, ctx := liveClient(t)

	got, created, err := cli.EnsurePortGroup(ctx, "pg-x", nil)
	if err != nil {
		t.Fatalf("EnsurePortGroup #1: %v", err)
	}
	if !created {
		t.Fatalf("first call: created = false, want true")
	}
	if got.UUID == "" || got.UUID == "pg_pg_x" {
		t.Fatalf("insert returned non-persisted UUID %q", got.UUID)
	}

	_, created, err = cli.EnsurePortGroup(ctx, "pg-x", nil)
	if err != nil {
		t.Fatalf("EnsurePortGroup #2: %v", err)
	}
	if created {
		t.Fatalf("second call: created = true, want false")
	}
}

// ReplaceACLs must no-op when the desired set already matches, keeping ACL row
// UUIDs stable, and swap when the set changes.
func TestReplaceACLs_IdempotentNoChurn(t *testing.T) {
	cli, ctx := liveClient(t)

	if _, _, err := cli.EnsurePortGroup(ctx, "pg-acl", nil); err != nil {
		t.Fatalf("EnsurePortGroup: %v", err)
	}
	specs := []ovn.ACLSpec{
		{Direction: "to-lport", Priority: 1001, Match: "ip4", Action: "allow-related"},
		{Direction: "from-lport", Priority: 1002, Match: "ip4", Action: "allow-related"},
	}

	if err := cli.ReplaceACLs(ctx, "pg-acl", specs); err != nil {
		t.Fatalf("ReplaceACLs #1: %v", err)
	}
	before := aclUUIDs(t, ctx, cli, "pg-acl")
	if len(before) != len(specs) {
		t.Fatalf("expected %d ACLs, got %d", len(specs), len(before))
	}

	if err := cli.ReplaceACLs(ctx, "pg-acl", specs); err != nil {
		t.Fatalf("ReplaceACLs #2 (identical): %v", err)
	}
	after := aclUUIDs(t, ctx, cli, "pg-acl")
	if !equalStringSet(before, after) {
		t.Fatalf("identical ReplaceACLs churned ACL UUIDs: %v → %v", before, after)
	}

	changed := append(specs, ovn.ACLSpec{Direction: "to-lport", Priority: 2000, Match: "ip6", Action: "drop"})
	if err := cli.ReplaceACLs(ctx, "pg-acl", changed); err != nil {
		t.Fatalf("ReplaceACLs #3 (changed): %v", err)
	}
	if got := aclUUIDs(t, ctx, cli, "pg-acl"); len(got) != len(changed) {
		t.Fatalf("changed ReplaceACLs: expected %d ACLs, got %d", len(changed), len(got))
	}
}

func aclUUIDs(t *testing.T, ctx context.Context, cli *ovn.LiveClient, pgName string) map[string]struct{} {
	t.Helper()
	pg, err := cli.GetPortGroup(ctx, pgName)
	if err != nil {
		t.Fatalf("GetPortGroup: %v", err)
	}
	out := make(map[string]struct{}, len(pg.ACLs))
	for _, u := range pg.ACLs {
		out[u] = struct{}{}
	}
	return out
}

func equalStringSet(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

// EnsureAddressSet must return the persisted UUID on insert, keep it stable on
// re-ensure, and converge the address list.
func TestEnsureAddressSet_InsertAndConverge(t *testing.T) {
	cli, ctx := liveClient(t)

	uuid1, err := cli.EnsureAddressSet(ctx, "spinifex_nat_exempt", []string{"100.127.0.0/24"})
	if err != nil {
		t.Fatalf("EnsureAddressSet #1: %v", err)
	}
	if uuid1 == "" || uuid1 == "as_spinifex_nat_exempt" {
		t.Fatalf("insert returned non-persisted UUID %q", uuid1)
	}

	uuid2, err := cli.EnsureAddressSet(ctx, "spinifex_nat_exempt", []string{"100.127.0.0/24", "192.168.1.0/24"})
	if err != nil {
		t.Fatalf("EnsureAddressSet #2: %v", err)
	}
	if uuid2 != uuid1 {
		t.Fatalf("UUID changed on re-ensure: %q → %q", uuid1, uuid2)
	}

	as, err := cli.GetAddressSet(ctx, "spinifex_nat_exempt")
	if err != nil {
		t.Fatalf("GetAddressSet: %v", err)
	}
	if len(as.Addresses) != 2 {
		t.Fatalf("addresses not converged: %v", as.Addresses)
	}

	if _, err := cli.GetAddressSet(ctx, "no-such-set"); !errors.Is(err, ovn.ErrAddressSetNotFound) {
		t.Fatalf("missing set: err = %v, want ErrAddressSetNotFound", err)
	}
}

// SetNATExemptedExtIPs must stamp/clear the strong Address_Set ref in place on
// the matching NAT row and return ErrNATNotFound for absent rules.
func TestSetNATExemptedExtIPs_Live(t *testing.T) {
	cli, ctx := liveClient(t)

	if _, _, err := cli.EnsureLogicalRouter(ctx, &nbdb.LogicalRouter{Name: "vpc-r1"}); err != nil {
		t.Fatalf("EnsureLogicalRouter: %v", err)
	}
	if err := cli.AddNAT(ctx, "vpc-r1", &nbdb.NAT{
		Type: "snat", ExternalIP: "100.127.0.10", LogicalIP: "10.0.0.0/16",
	}); err != nil {
		t.Fatalf("AddNAT: %v", err)
	}
	setUUID, err := cli.EnsureAddressSet(ctx, "spinifex_nat_exempt", []string{"100.127.0.0/24"})
	if err != nil {
		t.Fatalf("EnsureAddressSet: %v", err)
	}

	if err := cli.SetNATExemptedExtIPs(ctx, "vpc-r1", "snat", "10.0.0.0/16", &setUUID); err != nil {
		t.Fatalf("SetNATExemptedExtIPs: %v", err)
	}
	nat, err := cli.FindNATByLogicalIP(ctx, "vpc-r1", "snat", "10.0.0.0/16")
	if err != nil || nat == nil {
		t.Fatalf("FindNATByLogicalIP: nat=%v err=%v", nat, err)
	}
	if nat.ExemptedExtIps == nil || *nat.ExemptedExtIps != setUUID {
		t.Fatalf("ExemptedExtIps = %v, want %q", nat.ExemptedExtIps, setUUID)
	}

	if err := cli.SetNATExemptedExtIPs(ctx, "vpc-r1", "snat", "10.0.0.0/16", nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	nat, _ = cli.FindNATByLogicalIP(ctx, "vpc-r1", "snat", "10.0.0.0/16")
	if nat.ExemptedExtIps != nil {
		t.Fatalf("ExemptedExtIps not cleared: %v", *nat.ExemptedExtIps)
	}

	err = cli.SetNATExemptedExtIPs(ctx, "vpc-r1", "snat", "10.99.0.0/16", &setUUID)
	if !errors.Is(err, ovn.ErrNATNotFound) {
		t.Fatalf("missing NAT: err = %v, want ErrNATNotFound", err)
	}
}
