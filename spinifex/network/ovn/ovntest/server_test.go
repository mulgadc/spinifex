package ovntest

import (
	"context"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
)

// TestStartNB_RoundTrip proves the in-process NB server accepts our NB schema,
// a real LiveClient connects to it, and a write round-trips through OVSDB.
func TestStartNB_RoundTrip(t *testing.T) {
	nb := StartNB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cli := ovn.NewLiveClient(nb.Endpoint)
	if err := cli.Connect(ctx); err != nil {
		t.Fatalf("LiveClient.Connect: %v", err)
	}
	defer cli.Close()

	want := &nbdb.LogicalSwitch{
		Name:        "ls-test",
		ExternalIDs: map[string]string{"spinifex:subnet_id": "subnet-abc"},
	}
	if err := cli.CreateLogicalSwitch(ctx, want); err != nil {
		t.Fatalf("CreateLogicalSwitch: %v", err)
	}

	got, err := cli.GetLogicalSwitch(ctx, "ls-test")
	if err != nil {
		t.Fatalf("GetLogicalSwitch: %v", err)
	}
	if got.Name != "ls-test" {
		t.Fatalf("Name = %q, want ls-test", got.Name)
	}
	if got.ExternalIDs["spinifex:subnet_id"] != "subnet-abc" {
		t.Fatalf("ExternalIDs = %v, want spinifex:subnet_id=subnet-abc", got.ExternalIDs)
	}
}
