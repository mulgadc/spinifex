package host

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestEnsureNATEgressRules_InstallsAllAtTheHeadOfTheChain(t *testing.T) {
	r := newStubRunner()
	r.expect("iptables -t nat -D", nil, fmt.Errorf("no match"))
	r.expect("iptables -t filter -D", nil, fmt.Errorf("no match"))
	r.expect("iptables -t nat -I", nil, nil)
	r.expect("iptables -t filter -I", nil, nil)

	if err := EnsureNATEgressRules(context.Background(), r); err != nil {
		t.Fatalf("EnsureNATEgressRules: %v", err)
	}
	// Position 1, not -A: Ubuntu ships a catch-all REJECT in FORWARD that an
	// appended ACCEPT sits uselessly behind.
	wantInserts := []string{
		"iptables -t nat -I POSTROUTING 1 -s " + NATTransitCIDR + " ! -d " + NATTransitCIDR +
			" -m comment --comment spinifex-nat-egress -j MASQUERADE",
		"iptables -t filter -I FORWARD 1 -i " + NATTransitHostEnd + " -s " + NATTransitCIDR +
			" -m comment --comment spinifex-nat-egress -j ACCEPT",
		"iptables -t filter -I FORWARD 1 -o " + NATTransitHostEnd + " -m conntrack --ctstate RELATED,ESTABLISHED" +
			" -m comment --comment spinifex-nat-egress -j ACCEPT",
	}
	for _, want := range wantInserts {
		if !r.called(want) {
			t.Errorf("missing insert call:\n  want %q\n  got  %v", want, r.calls)
		}
	}
	for _, c := range r.calls {
		if strings.Contains(c, " -A ") {
			t.Errorf("appended instead of inserted: %q", c)
		}
	}
}

// A rule an earlier build appended satisfies -C, so probing would leave it
// stranded behind the REJECT forever. Ensure has to delete and re-insert.
func TestEnsureNATEgressRules_RepositionsAnAlreadyPresentRule(t *testing.T) {
	r := newStubRunner()
	r.expect("iptables -t nat -D", nil, nil)
	r.expect("iptables -t filter -D", nil, nil)
	r.expect("iptables -t nat -I", nil, nil)
	r.expect("iptables -t filter -I", nil, nil)

	if err := EnsureNATEgressRules(context.Background(), r); err != nil {
		t.Fatalf("EnsureNATEgressRules: %v", err)
	}
	if !r.called("iptables -t filter -I FORWARD 1 -i " + NATTransitHostEnd + " -s " + NATTransitCIDR +
		" -m comment --comment spinifex-nat-egress -j ACCEPT") {
		t.Errorf("present rule was not re-inserted: %v", r.calls)
	}
	// A delete that keeps succeeding must not loop forever.
	var deletes int
	for _, c := range r.calls {
		if strings.Contains(c, " -D ") {
			deletes++
		}
	}
	if deletes > len(natEgressRules)*maxDuplicateRules {
		t.Errorf("drain loop is unbounded: %d deletes", deletes)
	}
}

func TestEnsureNATEgressRules_InsertFailure(t *testing.T) {
	r := newStubRunner()
	r.expect("iptables -t nat -D", nil, fmt.Errorf("no match"))
	r.expect("iptables -t nat -I", []byte("iptables: permission denied"), fmt.Errorf("exit 4"))

	err := EnsureNATEgressRules(context.Background(), r)
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("expected insert failure error, got: %v", err)
	}
}

func TestRemoveNATEgressRules_IgnoresMissing(t *testing.T) {
	r := newStubRunner()
	r.expect("iptables -t nat -D", nil, fmt.Errorf("no match"))
	r.expect("iptables -t filter -D", nil, nil)

	RemoveNATEgressRules(context.Background(), r)
	if !r.called("iptables -t nat -D POSTROUTING") {
		t.Errorf("expected POSTROUTING delete attempt, got %v", r.calls)
	}
}
