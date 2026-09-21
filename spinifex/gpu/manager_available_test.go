//test:in-package — reuses the in-package sysfs and MIG fixtures
package gpu

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A pool holding whole GPUs and carved slices together must not report one kind
// as the other. Available counts both, which is why admission uses neither it
// nor a division of it.
func TestAvailableWholeAndSlicesCountSeparately(t *testing.T) {
	root := t.TempDir()

	m := NewManager([]GPUDevice{
		{PCIAddress: "0000:01:00.0", IOMMUGroup: 1},
		{PCIAddress: "0000:02:00.0", IOMMUGroup: 2},
	})
	carved := newMIGDevice("0000:03:00.0")
	m.AddMIGInstances(carved, []MIGInstance{
		{GIID: 1, MdevPath: makeMdevDir(t, root, "uuid-gi1"), Profile: MIGProfile{Name: "1g.10gb", MemoryMiB: 10240}},
		{GIID: 2, MdevPath: makeMdevDir(t, root, "uuid-gi2"), Profile: MIGProfile{Name: "1g.10gb", MemoryMiB: 10240}},
		{GIID: 3, MdevPath: makeMdevDir(t, root, "uuid-gi3"), Profile: MIGProfile{Name: "3g.40gb", MemoryMiB: 40960}},
	})

	assert.Equal(t, 5, m.Available(), "Available counts every free entry")
	assert.Equal(t, 2, m.AvailableWhole(), "a carved GPU is not a whole GPU")
	assert.Equal(t, 2, m.AvailableSlices("1g.10gb"))
	assert.Equal(t, 1, m.AvailableSlices("3g.40gb"))
	assert.Zero(t, m.AvailableSlices("7g.80gb"), "a profile with no slices carved is not available")
}

// An un-carved MIG-capable GPU yields at least one slice of any profile, but
// the carve happens at claim time so its full yield is not known yet.
func TestAvailableSlicesCountsUncarvedGPUsAsOne(t *testing.T) {
	m := NewManager(nil)
	m.AddMIGGPU(newMIGDevice("0000:01:00.0"))
	m.AddMIGGPU(newMIGDevice("0000:02:00.0"))

	assert.Equal(t, 2, m.AvailableSlices("1g.10gb"))
	assert.Zero(t, m.AvailableWhole(), "a MIG-capable GPU held for carving is not offerable whole")
	assert.Zero(t, m.Available(), "freeMIGGPUs are not pool entries yet")
}

// The whole-GPU claim path already skips slices; only the arithmetic was wrong.
func TestAvailableWholeDropsAClaimedGPU(t *testing.T) {
	root, dev := buildManagerSysfs(t)
	m := newManagerForTest([]GPUDevice{dev}, root)

	assert.Equal(t, 1, m.AvailableWhole())

	if _, _, err := m.Claim("i-1", ""); err != nil {
		t.Fatalf("claim: %v", err)
	}
	assert.Zero(t, m.AvailableWhole())

	if err := m.Release("i-1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	assert.Equal(t, 1, m.AvailableWhole())
}
