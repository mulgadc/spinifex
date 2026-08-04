package gpu

import "testing"

func TestLookupModel_KnownNVIDIA(t *testing.T) {
	name, mem := lookupModel("10de", "2236")
	if name != "NVIDIA A10" {
		t.Errorf("name = %q, want NVIDIA A10", name)
	}
	if mem != 23028 {
		t.Errorf("memoryMiB = %d, want 23028", mem)
	}
}

func TestLookupModel_RTXPro6000BlackwellSE(t *testing.T) {
	name, mem := lookupModel("10de", "2bb5")
	if name != "NVIDIA RTX Pro 6000 Blackwell Server Edition" {
		t.Errorf("name = %q, want NVIDIA RTX Pro 6000 Blackwell Server Edition", name)
	}
	if mem != 98304 {
		t.Errorf("memoryMiB = %d, want 98304 (96 GiB)", mem)
	}
}

func TestLookupModel_KnownAMD(t *testing.T) {
	name, mem := lookupModel("1002", "740f")
	if name != "AMD Instinct MI210" {
		t.Errorf("name = %q, want AMD Instinct MI210", name)
	}
	if mem != 65536 {
		t.Errorf("memoryMiB = %d, want 65536", mem)
	}
}

// TestLookupModel_AmbiguityRule pins 1002:744c to the smallest of the four
// Radeon RX 7900 SKUs (16/16/20/24GB) the ID covers, per the table's
// documented under-report convention for shared PCI IDs.
func TestLookupModel_AmbiguityRule(t *testing.T) {
	name, mem := lookupModel("1002", "744c")
	if name != "AMD Radeon RX 7900 XT/XTX/GRE/7900M" {
		t.Errorf("name = %q, want AMD Radeon RX 7900 XT/XTX/GRE/7900M", name)
	}
	if mem != 16384 {
		t.Errorf("memoryMiB = %d, want 16384 (smallest covered SKU)", mem)
	}
}

// TestLookupModel_CorrectedEntries pins the corrected table entries by name
// and VRAM, so a future edit cannot silently reintroduce a card labelled as
// hardware it is not.
func TestLookupModel_CorrectedEntries(t *testing.T) {
	tests := []struct {
		vendorID, deviceID string
		wantName           string
		wantMemMiB         int64
	}{
		{"1002", "7448", "AMD Radeon Pro W7900", 49152},
		{"1002", "74a1", "AMD Instinct MI300X", 196608},
		{"10de", "2233", "NVIDIA RTX A5500", 24576},
		{"10de", "20b7", "NVIDIA A30 PCIe", 24576},
		{"10de", "20b2", "NVIDIA A100 SXM4 80GB", 81920},
		{"10de", "20b0", "NVIDIA A100 SXM4 40GB", 40960},
		{"10de", "20b5", "NVIDIA A100 PCIe 80GB", 81920},
		{"10de", "20f3", "NVIDIA A800-SXM4-80GB", 81920},
		{"10de", "233a", "NVIDIA H800L 94GB", 94208},
		{"10de", "25b0", "NVIDIA RTX A1000", 8188},
	}
	for _, tc := range tests {
		t.Run(tc.vendorID+":"+tc.deviceID, func(t *testing.T) {
			name, mem := lookupModel(tc.vendorID, tc.deviceID)
			if name != tc.wantName {
				t.Errorf("name = %q, want %q", name, tc.wantName)
			}
			if mem != tc.wantMemMiB {
				t.Errorf("memoryMiB = %d, want %d", mem, tc.wantMemMiB)
			}
		})
	}
}

// TestModels_CorrectedMembership asserts the corrected entries landed in the
// right compute/MIG sets: the wattle A1000 and the newly-real A30 are
// compute+MIG where applicable, while the corrected W7900/A5500/RX7900 stay
// out of both since they're display-capable workstation/consumer cards.
func TestModels_CorrectedMembership(t *testing.T) {
	tests := []struct {
		vendorID, deviceID   string
		wantCompute, wantMIG bool
	}{
		{"1002", "7448", false, false}, // W7900: workstation, has display
		{"1002", "74a1", true, false},  // MI300X: compute, AMD has no MIG
		{"10de", "2233", false, false}, // A5500: workstation, has display
		{"10de", "20b7", true, true},   // A30 PCIe: compute + MIG-capable
		{"10de", "20b2", true, true},   // A100 SXM4 80GB: compute + MIG-capable
		{"10de", "25b0", false, false}, // A1000 (wattle): workstation, has display
		{"1002", "744c", false, false}, // RX 7900 family: consumer, has display
	}
	for _, tc := range tests {
		t.Run(tc.vendorID+":"+tc.deviceID, func(t *testing.T) {
			if got := IsComputeGPU(tc.vendorID, tc.deviceID); got != tc.wantCompute {
				t.Errorf("IsComputeGPU(%s,%s) = %v, want %v", tc.vendorID, tc.deviceID, got, tc.wantCompute)
			}
			if got := IsMIGCapable(tc.vendorID, tc.deviceID); got != tc.wantMIG {
				t.Errorf("IsMIGCapable(%s,%s) = %v, want %v", tc.vendorID, tc.deviceID, got, tc.wantMIG)
			}
		})
	}
}

// TestModels_NonMIGCapableCompute pins the plan's explicit exception list:
// A10, A10G, L4, L40S and T4 are compute GPUs but do not support MIG.
func TestModels_NonMIGCapableCompute(t *testing.T) {
	nonMIG := []struct{ vendorID, deviceID string }{
		{"10de", "2236"}, // A10
		{"10de", "2237"}, // A10G
		{"10de", "27b8"}, // L4
		{"10de", "26b9"}, // L40S
		{"10de", "1eb8"}, // T4
	}
	for _, tc := range nonMIG {
		if !IsComputeGPU(tc.vendorID, tc.deviceID) {
			t.Errorf("%s:%s expected to be a compute GPU", tc.vendorID, tc.deviceID)
		}
		if IsMIGCapable(tc.vendorID, tc.deviceID) {
			t.Errorf("%s:%s expected NOT to be MIG-capable", tc.vendorID, tc.deviceID)
		}
	}
}

func TestLookupModel_Unknown(t *testing.T) {
	name, mem := lookupModel("10de", "ffff")
	if name != "" {
		t.Errorf("name = %q, want empty for unknown device", name)
	}
	if mem != 0 {
		t.Errorf("memoryMiB = %d, want 0 for unknown device", mem)
	}
}

func TestLookupModel_UnknownVendor(t *testing.T) {
	name, mem := lookupModel("dead", "beef")
	if name != "" || mem != 0 {
		t.Errorf("want empty for completely unknown vendor:device, got name=%q mem=%d", name, mem)
	}
}
