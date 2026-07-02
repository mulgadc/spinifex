package host

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

func TestIMDSReplyTablePerTap(t *testing.T) {
	t1 := imdsReplyTable("ime-11111111")
	t2 := imdsReplyTable("ime-22222222")
	if t1 == t2 {
		t.Errorf("distinct endpoints share a reply table: %d", t1)
	}
	if t1 != imdsReplyTable("ime-11111111") {
		t.Errorf("reply table not deterministic for the same endpoint")
	}
	// Must clear the reserved tables (unspec/default/main/local) and stay in
	// iproute2's positive range.
	for _, tbl := range []int{t1, t2} {
		if tbl < 256 || tbl >= 1<<31 {
			t.Errorf("reply table %d outside [256, 2^31)", tbl)
		}
	}
}

func TestInstallTapReplyRoutingValidate(t *testing.T) {
	s := newStubRunner()
	d := testDatapath()
	d.Endpoint = ""
	if err := InstallTapReplyRouting(context.Background(), s, d); err == nil ||
		!strings.Contains(err.Error(), "Endpoint") {
		t.Fatalf("expected Endpoint validation error, got %v", err)
	}
	if len(s.calls) != 0 {
		t.Errorf("validation must fail before issuing commands; calls: %v", s.calls)
	}
}

func TestInstallTapReplyRouting(t *testing.T) {
	s := newStubRunner()
	s.expect("ip", nil, nil)

	d := testDatapath()
	if err := InstallTapReplyRouting(context.Background(), s, d); err != nil {
		t.Fatalf("InstallTapReplyRouting: %v", err)
	}
	table := strconv.Itoa(imdsReplyTable(d.Endpoint))

	want := []string{
		// Per-tap table: one onlink default route out the endpoint.
		"ip route replace default via " + imdsReplyNexthop + " dev " + d.Endpoint + " onlink table " + table,
		// Static neigh so the kernel emits without ARPing the secure bridge.
		"ip neigh replace " + imdsReplyNexthop + " lladdr " + d.GuestMAC + " dev " + d.Endpoint + " nud permanent",
		// del-before-add keeps exactly one oif rule selecting the per-tap table.
		"ip rule del oif " + d.Endpoint + " lookup " + table,
		"ip rule add oif " + d.Endpoint + " lookup " + table,
	}
	for _, w := range want {
		if !s.called(w) {
			t.Errorf("missing command:\n  %q\ncalls: %v", w, s.calls)
		}
	}
}

func TestRemoveTapReplyRouting(t *testing.T) {
	s := newStubRunner()
	s.expect("ip", nil, nil)

	d := testDatapath()
	if err := RemoveTapReplyRouting(context.Background(), s, d); err != nil {
		t.Fatalf("RemoveTapReplyRouting: %v", err)
	}
	table := strconv.Itoa(imdsReplyTable(d.Endpoint))
	for _, w := range []string{
		"ip rule del oif " + d.Endpoint + " lookup " + table,
		"ip route flush table " + table,
		"ip neigh del " + imdsReplyNexthop + " dev " + d.Endpoint,
	} {
		if !s.called(w) {
			t.Errorf("missing command %q; calls: %v", w, s.calls)
		}
	}
}

// TestRemoveTapReplyRoutingBestEffort confirms teardown reports success even
// when individual ip commands fail (the endpoint delete clears most state).
func TestRemoveTapReplyRoutingBestEffort(t *testing.T) {
	s := newStubRunner()
	// No expectations registered: every ip command returns an error.
	if err := RemoveTapReplyRouting(context.Background(), s, testDatapath()); err != nil {
		t.Fatalf("RemoveTapReplyRouting must be best-effort, got: %v", err)
	}
}
