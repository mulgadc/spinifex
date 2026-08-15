package listenerinventory_test

import (
	"testing"

	networkconnections "github.com/mulgadc/spinifex/docs/security/network-connections"
	"github.com/mulgadc/spinifex/spinifex/network/listenerinventory"
)

func TestParse_Synthetic(t *testing.T) {
	const doc = `# doc

## 1. Inbound Listeners

Some prose before the table.

| Port | Service | Protocol | Scope | Purpose | Auth / Verification |
|------|---------|----------|-------|---------|--------------------|
| 9999 | gw | HTTPS | External | public API | TLS |
| 5300 | ns | DNS | Cluster | Forward target. Binds the wildcard by design. | none |
| 6641 | ovn-nb | OVSDB | Cluster | Binds 127.0.0.1 plus lan, never the wildcard address. | TLS |
| 7777 | mystery | UDP | Cluster | Mentions the wildcard address here, unrelated to any exception. | none |
| 500, 4500 | charon | IKEv2 | Encap | Binds the wildcard by design (upstream default). | cert |
| — | ESP | IP proto 50 | Encap | payload | cert |
| 169.254.169.254:80 | vpcd | HTTP | Guest | imds | tokens |
| socket / dynamic TCP | nbdkit | NBD | Host-local / cluster | block transport | unix |

## 2. Outbound Connections

not a table row
`
	table, err := listenerinventory.Parse(doc)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(table.Rows) != 8 {
		t.Fatalf("got %d rows, want 8", len(table.Rows))
	}

	if !table.KnownPort(9999) {
		t.Fatalf("expected 9999 known")
	}
	if table.KnownPort(1234) {
		t.Fatalf("expected 1234 unknown")
	}

	for _, r := range table.RowsForPort(5300) {
		if r.Scope != listenerinventory.ScopeCluster {
			t.Fatalf("5300 scope = %v, want Cluster", r.Scope)
		}
		if !r.WildcardOK() {
			t.Fatalf("5300 should declare a wildcard exception")
		}
	}

	// 6641's mention is negated ("never the wildcard") and 7777's is plain
	// prose with no negation at all — both must resolve to no exception, for
	// the same structural reason: neither row contains the exact marker
	// phrase. Fail-closed means a bare mention never grants an exception on
	// its own, negated or not.
	for _, r := range table.RowsForPort(6641) {
		if r.WildcardOK() {
			t.Fatalf("6641 says \"never the wildcard\" and has no marker phrase; should not read as an exception")
		}
	}
	for _, r := range table.RowsForPort(7777) {
		if r.WildcardOK() {
			t.Fatalf("7777 mentions \"wildcard\" with no marker phrase and no negation; should still not read as an exception")
		}
	}

	for _, port := range []int{500, 4500} {
		found := false
		for _, r := range table.RowsForPort(port) {
			found = true
			if r.Scope != listenerinventory.ScopeEncap {
				t.Fatalf("%d scope = %v, want Encap", port, r.Scope)
			}
			if !r.WildcardOK() {
				t.Fatalf("%d should declare a wildcard exception", port)
			}
		}
		if !found {
			t.Fatalf("expected %d to parse out of the \"500, 4500\" cell", port)
		}
	}

	espRows := table.Rows
	sawESP := false
	for _, r := range espRows {
		if r.Service == "ESP" {
			sawESP = true
			if len(r.Ports) != 0 {
				t.Fatalf("ESP row should have no numeric ports, got %v", r.Ports)
			}
		}
	}
	if !sawESP {
		t.Fatalf("expected an ESP row")
	}

	imds := table.RowsForPort(80)
	if len(imds) != 1 || imds[0].Addr != "169.254.169.254" || imds[0].Scope != listenerinventory.ScopeGuest {
		t.Fatalf("imds row parsed as %+v", imds)
	}

	for _, r := range table.Rows {
		if r.Service == "nbdkit" && len(r.Ports) != 0 {
			t.Fatalf("dynamic-port row should have no numeric ports, got %v", r.Ports)
		}
	}
}

func TestIsWildcardAddr(t *testing.T) {
	cases := map[string]bool{
		"0.0.0.0":   true,
		"::":        true,
		"*":         true,
		"[::]":      true,
		"":          true,
		"10.0.1.2":  false,
		"127.0.0.1": false,
	}
	for addr, want := range cases {
		if got := listenerinventory.IsWildcardAddr(addr); got != want {
			t.Errorf("IsWildcardAddr(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestParse_RealDoc(t *testing.T) {
	table, err := listenerinventory.Parse(string(networkconnections.README()))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(table.Rows) < 15 {
		t.Fatalf("got %d rows from the real inventory, expected considerably more", len(table.Rows))
	}

	wantWildcardOK := map[int]listenerinventory.Scope{
		5300: listenerinventory.ScopeCluster,
		500:  listenerinventory.ScopeEncap,
		4500: listenerinventory.ScopeEncap,
	}
	for port, scope := range wantWildcardOK {
		rows := table.RowsForPort(port)
		if len(rows) == 0 {
			t.Fatalf("port %d not found in the real inventory", port)
		}
		found := false
		for _, r := range rows {
			if r.Scope == scope {
				found = true
				if !r.WildcardOK() {
					t.Errorf("port %d (%v) should declare a wildcard exception in the real doc", port, scope)
				}
			}
		}
		if !found {
			t.Errorf("port %d has no %v row in the real inventory", port, scope)
		}
	}

	// 6641/6642 say "never the wildcard address" — read as no exception
	// because the marker phrase is absent, not because "never" was spotted.
	wantNoException := []int{6641, 6642}
	for _, port := range wantNoException {
		for _, r := range table.RowsForPort(port) {
			if r.Scope == listenerinventory.ScopeCluster && r.WildcardOK() {
				t.Errorf("port %d should NOT declare a wildcard exception (no marker phrase present)", port)
			}
		}
	}
}
