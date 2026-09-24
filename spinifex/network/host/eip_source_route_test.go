package host

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// The reference OCI topology: br-wan holds the secondary VNIC's own address in
// a /23, and the host source-routes replies from it through table 200. enp0s9
// is the primary VNIC and carries the default route.
const (
	ociAddrShow = `2: enp0s9    inet 10.200.0.215/23 brd 10.200.1.255 scope global enp0s9\       valid_lft forever preferred_lft forever
3: br-wan    inet 10.200.1.119/23 brd 10.200.1.255 scope global br-wan\       valid_lft forever preferred_lft forever
`
	ociRuleShow = `0:	from all lookup local
1000:	from 10.200.1.119 lookup 200 proto static
32766:	from all lookup main
32767:	from all lookup default
`
	// A host with no policy routing at all, which is prod and dev-prod.
	plainAddrShow = `2: eth0    inet 192.168.1.42/24 brd 192.168.1.255 scope global eth0\       valid_lft forever preferred_lft forever
`
	plainRuleShow = `0:	from all lookup local
32766:	from all lookup main
32767:	from all lookup default
`
)

func newSourceRouteRunner(addrShow, ruleShow string) *stubRunner {
	r := newStubRunner()
	r.expect("ip -4 -o addr show scope global", []byte(addrShow), nil)
	r.expect("ip rule show", []byte(ruleShow), nil)
	r.expect("ip rule del", nil, nil)
	r.expect("ip rule add", nil, nil)
	return r
}

func TestEnsureEIPSourceRoute_MirrorsTheUplinkRule(t *testing.T) {
	r := newSourceRouteRunner(ociAddrShow, ociRuleShow)
	if err := EnsureEIPSourceRoute(context.Background(), r, "10.200.1.207"); err != nil {
		t.Fatalf("EnsureEIPSourceRoute: %v", err)
	}
	// Deleted before added: `ip rule add` appends a duplicate every call, and
	// this runs on every bind and every reconcile pass.
	for _, want := range []string{
		"ip rule del from 10.200.1.207/32 lookup 200",
		"ip rule add from 10.200.1.207/32 lookup 200 priority 1000",
	} {
		if !r.called(want) {
			t.Errorf("missing call:\n  want %q\n  got  %v", want, r.calls)
		}
	}
}

func TestEnsureEIPSourceRoute_NoRuleToMirrorIsANoOp(t *testing.T) {
	r := newSourceRouteRunner(plainAddrShow, plainRuleShow)
	if err := EnsureEIPSourceRoute(context.Background(), r, "192.168.1.200"); err != nil {
		t.Fatalf("EnsureEIPSourceRoute: %v", err)
	}
	if r.called("ip rule add") || r.called("ip rule del") {
		t.Errorf("touched ip rule on a host with no source routing: %v", r.calls)
	}
}

// Two source rules covering the same subnet name two different interfaces, and
// picking one would send the replies out an interface that does not hold the
// address — which the provider drops without a word.
func TestEnsureEIPSourceRoute_AmbiguousRulesRefuse(t *testing.T) {
	const twoRules = `0:	from all lookup local
1000:	from 10.200.1.119 lookup 200 proto static
1001:	from 10.200.0.215 lookup 201 proto static
32766:	from all lookup main
`
	r := newSourceRouteRunner(ociAddrShow, twoRules)
	err := EnsureEIPSourceRoute(context.Background(), r, "10.200.1.207")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected an ambiguity error, got: %v", err)
	}
	if r.called("ip rule add") {
		t.Errorf("added a rule despite the ambiguity: %v", r.calls)
	}
}

// An EIP outside every local subnet belongs to no interface here, so there is
// nothing to mirror — and guessing an interface would send its replies out the
// wrong VNIC, which is the failure this whole file exists to prevent.
func TestEnsureEIPSourceRoute_ForeignSubnetIsANoOp(t *testing.T) {
	r := newSourceRouteRunner(ociAddrShow, ociRuleShow)
	if err := EnsureEIPSourceRoute(context.Background(), r, "203.0.113.7"); err != nil {
		t.Fatalf("EnsureEIPSourceRoute: %v", err)
	}
	if r.called("ip rule add") {
		t.Errorf("added a rule for an address on no local subnet: %v", r.calls)
	}
}

func TestRemoveEIPSourceRoute_DeletesWithoutAdding(t *testing.T) {
	r := newSourceRouteRunner(ociAddrShow, ociRuleShow)
	if err := RemoveEIPSourceRoute(context.Background(), r, "10.200.1.207"); err != nil {
		t.Fatalf("RemoveEIPSourceRoute: %v", err)
	}
	if !r.called("ip rule del from 10.200.1.207/32 lookup 200") {
		t.Errorf("rule not deleted: %v", r.calls)
	}
	if r.called("ip rule add") {
		t.Errorf("teardown added a rule: %v", r.calls)
	}
}

func TestEnsureEIPSourceRoute_AddFailureIsFatal(t *testing.T) {
	r := newSourceRouteRunner(ociAddrShow, ociRuleShow)
	r.expect("ip rule add", []byte("RTNETLINK answers: File exists"), fmt.Errorf("exit 2"))
	err := EnsureEIPSourceRoute(context.Background(), r, "10.200.1.207")
	if err == nil || !strings.Contains(err.Error(), "File exists") {
		t.Fatalf("expected the kernel's message in the error, got: %v", err)
	}
}

func TestEnsureEIPSourceRoute_UnparseableEIP(t *testing.T) {
	r := newSourceRouteRunner(ociAddrShow, ociRuleShow)
	if err := EnsureEIPSourceRoute(context.Background(), r, "not-an-ip"); err == nil {
		t.Fatal("expected a parse error")
	}
}

func TestParseIPRule(t *testing.T) {
	tests := []struct {
		name                          string
		line                          string
		wantPrio, wantFrom, wantTable string
		wantOK                        bool
	}{
		{name: "source rule", line: "1000:\tfrom 10.200.1.119 lookup 200 proto static",
			wantPrio: "1000", wantFrom: "10.200.1.119", wantTable: "200", wantOK: true},
		{name: "prefix form", line: "1000:\tfrom 10.200.1.207/32 lookup 200",
			wantPrio: "1000", wantFrom: "10.200.1.207", wantTable: "200", wantOK: true},
		{name: "named table", line: "1000:\tfrom 10.200.1.119 lookup wan",
			wantPrio: "1000", wantFrom: "10.200.1.119", wantTable: "wan", wantOK: true},
		{name: "from all", line: "32766:\tfrom all lookup main"},
		{name: "oif rule", line: "998:\tfrom all oif ime-6a0b60c5 lookup 1140435084"},
		{name: "blank", line: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prio, from, table, ok := parseIPRule(tt.line)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if prio != tt.wantPrio || from != tt.wantFrom || table != tt.wantTable {
				t.Errorf("got (%q, %q, %q), want (%q, %q, %q)",
					prio, from, table, tt.wantPrio, tt.wantFrom, tt.wantTable)
			}
		})
	}
}
