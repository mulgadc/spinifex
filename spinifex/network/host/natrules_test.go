package host

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestEnsureNATEgressRules_InstallsAllWhenMissing(t *testing.T) {
	r := newStubRunner()
	r.expect("iptables -t nat -C", nil, fmt.Errorf("no match"))
	r.expect("iptables -t filter -C", nil, fmt.Errorf("no match"))
	r.expect("iptables -t nat -A", nil, nil)
	r.expect("iptables -t filter -A", nil, nil)

	if err := EnsureNATEgressRules(context.Background(), r); err != nil {
		t.Fatalf("EnsureNATEgressRules: %v", err)
	}
	wantAppends := []string{
		"iptables -t nat -A POSTROUTING -s " + NATTransitCIDR + " ! -d " + NATTransitCIDR +
			" -m comment --comment spinifex-nat-egress -j MASQUERADE",
		"iptables -t filter -A FORWARD -i " + NATTransitHostEnd + " -s " + NATTransitCIDR +
			" -m comment --comment spinifex-nat-egress -j ACCEPT",
		"iptables -t filter -A FORWARD -o " + NATTransitHostEnd + " -m conntrack --ctstate RELATED,ESTABLISHED" +
			" -m comment --comment spinifex-nat-egress -j ACCEPT",
	}
	for _, want := range wantAppends {
		if !r.called(want) {
			t.Errorf("missing append call:\n  want %q\n  got  %v", want, r.calls)
		}
	}
}

func TestEnsureNATEgressRules_SkipsWhenPresent(t *testing.T) {
	r := newStubRunner()
	r.expect("iptables -t nat -C", nil, nil)
	r.expect("iptables -t filter -C", nil, nil)

	if err := EnsureNATEgressRules(context.Background(), r); err != nil {
		t.Fatalf("EnsureNATEgressRules: %v", err)
	}
	for _, c := range r.calls {
		if strings.Contains(c, " -A ") {
			t.Errorf("unexpected append when rule present: %q", c)
		}
	}
}

func TestEnsureNATEgressRules_AppendFailure(t *testing.T) {
	r := newStubRunner()
	r.expect("iptables -t nat -C", nil, fmt.Errorf("no match"))
	r.expect("iptables -t nat -A", []byte("iptables: permission denied"), fmt.Errorf("exit 4"))

	err := EnsureNATEgressRules(context.Background(), r)
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("expected append failure error, got: %v", err)
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
