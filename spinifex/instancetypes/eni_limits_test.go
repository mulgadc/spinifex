package instancetypes

import "testing"

func TestMaxENIsForType(t *testing.T) {
	tests := []struct {
		instanceType string
		wantMax      int
		wantHotPlug  int
	}{
		// Explicit type overrides
		{"m5.large", 3, 2},
		{"m5.xlarge", 3, 2},
		{"m5.2xlarge", 4, 3},
		{"m5.4xlarge", 8, 7},

		// Family fallback (larger m5 sizes hit the family default of 15)
		{"m5.8xlarge", 15, 14},
		{"m5.16xlarge", 15, 14},

		// Burstable families
		{"t3.nano", 3, 2},
		{"t3.micro", 3, 2},
		{"t3a.2xlarge", 3, 2},
		{"t4g.medium", 3, 2},

		// ARM general-purpose family
		{"m6g.large", 8, 7},

		// x86 general-purpose family default
		{"m7i.4xlarge", 15, 14},

		// Unknown family → default
		{"x9unknown.large", defaultMaxENIs, defaultMaxENIs - 1},

		// Malformed input → default
		{"", defaultMaxENIs, defaultMaxENIs - 1},
		{"nodot", defaultMaxENIs, defaultMaxENIs - 1},
	}

	for _, tt := range tests {
		t.Run(tt.instanceType, func(t *testing.T) {
			if got := MaxENIsForType(tt.instanceType); got != tt.wantMax {
				t.Errorf("MaxENIsForType(%q) = %d, want %d", tt.instanceType, got, tt.wantMax)
			}
			if got := HotPlugENISlotsForType(tt.instanceType); got != tt.wantHotPlug {
				t.Errorf("HotPlugENISlotsForType(%q) = %d, want %d", tt.instanceType, got, tt.wantHotPlug)
			}
		})
	}
}

func TestHotPlugENISlotsForType_NeverNegative(t *testing.T) {
	// Defensive: if a future maxENIsByType entry drops to 0, slot count
	// must clamp at 0 rather than overflow into negative territory.
	maxENIsByType["test.zero"] = 0
	defer delete(maxENIsByType, "test.zero")
	if got := HotPlugENISlotsForType("test.zero"); got != 0 {
		t.Errorf("HotPlugENISlotsForType(zero-cap) = %d, want 0", got)
	}
}
