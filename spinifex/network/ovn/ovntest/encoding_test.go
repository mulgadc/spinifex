package ovntest

import (
	"context"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
)

// TestStartNB_ColumnEncoding exercises the trickier NB column kinds end-to-end:
// a pointer *bool (Enabled), sets (Addresses/PortSecurity) and the Ensure
// wait-op idempotency path. A schema/model optionality mismatch would surface
// here as an OVSDB transaction error rather than at model construction.
func TestStartNB_ColumnEncoding(t *testing.T) {
	nb := StartNB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cli := ovn.NewLiveClient(nb.Endpoint)
	if err := cli.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer cli.Close()

	// EnsureLogicalSwitch twice must converge to a single row, not insert a
	// duplicate. The insert path reports created=true and the second call
	// created=false; both return the persisted UUID.
	ls := &nbdb.LogicalSwitch{Name: "ls-enc"}
	if _, created, err := cli.EnsureLogicalSwitch(ctx, ls); err != nil {
		t.Fatalf("EnsureLogicalSwitch #1: %v", err)
	} else if !created {
		t.Fatalf("EnsureLogicalSwitch #1: created = false, want true")
	}
	if _, created, err := cli.EnsureLogicalSwitch(ctx, &nbdb.LogicalSwitch{Name: "ls-enc"}); err != nil {
		t.Fatalf("EnsureLogicalSwitch #2: %v", err)
	} else if created {
		t.Fatalf("EnsureLogicalSwitch #2: created = true, want false")
	}
	switches, err := cli.ListLogicalSwitches(ctx)
	if err != nil {
		t.Fatalf("ListLogicalSwitches: %v", err)
	}
	if n := countByName(switches, "ls-enc"); n != 1 {
		t.Fatalf("ls-enc row count = %d, want 1 (Ensure not idempotent)", n)
	}

	enabled := true
	lsp := &nbdb.LogicalSwitchPort{
		Name:         "lsp-enc",
		Type:         "",
		Addresses:    []string{"02:00:00:00:00:01 10.0.0.5"},
		PortSecurity: []string{"02:00:00:00:00:01 10.0.0.5"},
		Enabled:      &enabled,
	}
	if err := cli.CreateLogicalSwitchPort(ctx, "ls-enc", lsp); err != nil {
		t.Fatalf("CreateLogicalSwitchPort: %v", err)
	}

	got, err := cli.GetLogicalSwitchPort(ctx, "lsp-enc")
	if err != nil {
		t.Fatalf("GetLogicalSwitchPort: %v", err)
	}
	if got.Enabled == nil || !*got.Enabled {
		t.Fatalf("Enabled = %v, want true", got.Enabled)
	}
	if len(got.Addresses) != 1 || got.Addresses[0] != "02:00:00:00:00:01 10.0.0.5" {
		t.Fatalf("Addresses = %v, want single MAC/IP", got.Addresses)
	}
}

// countByName returns how many switches carry the given name.
func countByName(switches []nbdb.LogicalSwitch, name string) int {
	n := 0
	for i := range switches {
		if switches[i].Name == name {
			n++
		}
	}
	return n
}
