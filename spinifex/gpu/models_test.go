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

func TestLookupModel_KnownAMD(t *testing.T) {
	name, mem := lookupModel("1002", "744c")
	if name != "AMD Radeon RX 7900 XTX" {
		t.Errorf("name = %q, want AMD Radeon RX 7900 XTX", name)
	}
	if mem != 24576 {
		t.Errorf("memoryMiB = %d, want 24576", mem)
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
