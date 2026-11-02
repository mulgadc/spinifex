package servicestate

import (
	"context"
	"errors"
	"testing"
	"time"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
)

func TestSet_DefaultDisabledEnableIsIdempotentAndPinnedToItsNode(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	ctx := context.Background()
	st, err := Open(ctx, js)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0)

	rec, err := st.Get(ctx, "ap-southeast-2")
	if err != nil || rec.Desired != Disabled || rec.Generation != 0 {
		t.Fatalf("absent record = %+v, %v; want disabled", rec, err)
	}
	on, err := st.Set(ctx, "ap-southeast-2", Enabled, "node1", now)
	if err != nil || on.Desired != Enabled || on.Node != "node1" || on.Generation != 1 {
		t.Fatalf("enable = %+v, %v", on, err)
	}
	again, err := st.Set(ctx, "ap-southeast-2", Enabled, "node1", now.Add(time.Hour))
	if err != nil || again != on {
		t.Fatalf("repeated enable = %+v, %v; want unchanged %+v", again, err, on)
	}
	if _, err := st.Set(ctx, "ap-southeast-2", Enabled, "node2", now); !errors.Is(err, ErrNodeConflict) {
		t.Fatalf("enable on another node = %v; want ErrNodeConflict", err)
	}

	off, err := st.Set(ctx, "ap-southeast-2", Disabled, "", now)
	if err != nil || off.Desired != Disabled || off.Node != "node1" || off.Generation != 2 {
		t.Fatalf("disable = %+v, %v; want node kept", off, err)
	}
	if _, err := st.Set(ctx, "ap-southeast-2", Enabled, "node2", now); !errors.Is(err, ErrNodeConflict) {
		t.Fatalf("re-enable elsewhere = %v; the volumes stay on node1", err)
	}
	if rec, _ := st.Get(ctx, "us-east-1"); rec.Desired != Disabled {
		t.Fatalf("another Region = %+v; want disabled", rec)
	}
}

func TestGate_FollowsTheRecordAndFailsClosed(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)
	g := &Gate{JS: js, Region: "ap-southeast-2", TTL: time.Second, Now: func() time.Time { return now }}
	if g.Open(ctx) {
		t.Fatal("gate open with no bucket")
	}
	st, err := Open(ctx, js)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Set(ctx, "ap-southeast-2", Enabled, "node1", now); err != nil {
		t.Fatal(err)
	}
	if g.Open(ctx) {
		t.Fatal("gate reread before its TTL")
	}
	now = now.Add(time.Second)
	if !g.Open(ctx) {
		t.Fatal("gate closed after enable")
	}
	if _, err := st.Set(ctx, "ap-southeast-2", Disabled, "", now); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if g.Open(ctx) {
		t.Fatal("gate open after disable")
	}
}
