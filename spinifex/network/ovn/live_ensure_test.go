package ovn_test

import (
	"context"
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
