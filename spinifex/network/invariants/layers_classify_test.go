package invariants

import "testing"

func TestS1_ClassifyTable(t *testing.T) {
	cases := []struct {
		name     string
		from, to packageKind
		wantBad  bool
	}{
		{"L3 imports L0 (downward, permitted)", kindL3Policy, kindL0Host, false},
		{"L5 imports L1 (downward skip, permitted per ADR L5 description)", kindL5External, kindL1OVN, false},
		{"L0 imports L1 (upward, forbidden)", kindL0Host, kindL1OVN, true},
		{"L2 imports L3 (upward, forbidden)", kindL2Topology, kindL3Policy, true},
		{"reconcile imports L1 (orchestrator, permitted)", kindCrossCutter, kindL1OVN, false},
		{"L2 imports reconcile (layer pulling orchestrator, forbidden)", kindL2Topology, kindCrossCutter, true},
		{"L1 imports L1 (same layer, permitted)", kindL1OVN, kindL1OVN, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(tc.from, tc.to)
			if (got != "") != tc.wantBad {
				t.Fatalf("classify(%v, %v) = %q; wantBad=%v", tc.from, tc.to, got, tc.wantBad)
			}
		})
	}
}
