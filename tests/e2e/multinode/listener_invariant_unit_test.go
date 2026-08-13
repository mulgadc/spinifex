//go:build e2e

package multinode

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/network/listenerinventory"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// TestCheckSocketScope covers the four outcomes checkSocketScope must
// produce: the wildcard rule (with and without a row-declared exception)
// and the WAN-vs-peer-address rule (single-homed passes, a genuinely
// distinct WAN bind fails).
func TestCheckSocketScope(t *testing.T) {
	clusterRow := listenerinventory.Row{Scope: listenerinventory.ScopeCluster}
	wildcardOKRow := listenerinventory.Row{
		Scope:   listenerinventory.ScopeCluster,
		Purpose: "Binds the wildcard by design.",
	}

	tests := []struct {
		name        string
		node        harness.Node
		sock        ssSocket
		row         listenerinventory.Row
		wanAddr     string
		wantMessage bool
	}{
		{
			name:        "wildcard bind with declared exception passes",
			node:        harness.Node{Name: "node1", Addr: "10.2.0.5"},
			sock:        ssSocket{proto: "tcp", addr: "0.0.0.0", port: 5300},
			row:         wildcardOKRow,
			wanAddr:     "",
			wantMessage: false,
		},
		{
			name:        "wildcard bind without a declared exception fails",
			node:        harness.Node{Name: "node1", Addr: "10.2.0.5"},
			sock:        ssSocket{proto: "tcp", addr: "0.0.0.0", port: 6641},
			row:         clusterRow,
			wanAddr:     "",
			wantMessage: true,
		},
		{
			name:        "single-homed node bound to its only address passes",
			node:        harness.Node{Name: "node1", Addr: "192.168.0.22"},
			sock:        ssSocket{proto: "tcp", addr: "192.168.0.22", port: 4432},
			row:         clusterRow,
			wanAddr:     "",
			wantMessage: false,
		},
		{
			name:        "multi-homed node bound to a distinct WAN address fails",
			node:        harness.Node{Name: "node4", Addr: "10.2.0.5"},
			sock:        ssSocket{proto: "tcp", addr: "72.52.77.231", port: 6641},
			row:         clusterRow,
			wanAddr:     "72.52.77.231",
			wantMessage: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := checkSocketScope(tc.node, tc.sock, tc.row, tc.wanAddr)
			if tc.wantMessage && got == "" {
				t.Fatalf("checkSocketScope() = %q, want a violation message", got)
			}
			if !tc.wantMessage && got != "" {
				t.Fatalf("checkSocketScope() = %q, want no violation", got)
			}
			t.Logf("checkSocketScope() = %q", got)
		})
	}
}
