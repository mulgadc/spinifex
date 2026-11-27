package instancetypes

import "testing"

// A guest NIC gets one queue pair per vCPU, so a customer type below 2 vCPUs
// would expose fewer queues than ENA's smallest. System types are exempt.
func TestCustomerFamilies_HaveAtLeastTwoVCPUs(t *testing.T) {
	for _, fam := range instanceFamilyDefs {
		for _, size := range fam.sizes {
			if IsSystemType(fam.name + "." + size.suffix) {
				continue
			}
			if size.vcpus < 2 {
				t.Errorf("%s.%s has %d vCPU, want at least 2", fam.name, size.suffix, size.vcpus)
			}
		}
	}
	for _, tier := range builtinGPUTiers {
		if tier < 2 {
			t.Errorf("GPU tier of %d vCPU per GPU, want at least 2", tier)
		}
	}
}
