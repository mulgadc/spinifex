package gpu

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeMdevDir creates a fake mdev device directory under root and returns the
// full path. The directory's existence is enough to satisfy os.Stat checks in
// Claim and ReclaimByMdev; no sysfs attributes are needed for these unit tests.
func makeMdevDir(t *testing.T, root, uuid string) string {
	t.Helper()
	p := filepath.Join(root, uuid)
	require.NoError(t, os.MkdirAll(p, 0755))
	return p
}

// newMIGDevice returns a minimal GPUDevice configured as MIG-capable.
func newMIGDevice(pciAddr string) GPUDevice {
	return GPUDevice{
		PCIAddress: pciAddr,
		Vendor:     VendorNVIDIA,
		VendorID:   "10de",
		DeviceID:   "20b5", // A100 80GB PCIe
		Model:      "NVIDIA A100-PCIE-80GB",
		MemoryMiB:  81920,
		IOMMUGroup: -1,
		MIGCapable: true,
		MIGEnabled: true,
	}
}

// --- AddMIGInstances ---

func TestAddMIGInstances_PopulatesPool(t *testing.T) {
	root := t.TempDir()
	mdev1 := makeMdevDir(t, root, "uuid-gi1")
	mdev2 := makeMdevDir(t, root, "uuid-gi2")

	m := NewManager(nil)
	dev := newMIGDevice("0000:01:00.0")
	instances := []MIGInstance{
		{GIID: 1, MdevPath: mdev1, Profile: MIGProfile{Name: "1g.10gb", MemoryMiB: 10240}},
		{GIID: 2, MdevPath: mdev2, Profile: MIGProfile{Name: "1g.10gb", MemoryMiB: 10240}},
	}

	m.AddMIGInstances(dev, instances)

	assert.Equal(t, 2, m.TotalCount())
	assert.Equal(t, 2, m.Available())
}

func TestAddMIGInstances_EmptySlice_NoOp(t *testing.T) {
	m := NewManager(nil)
	m.AddMIGInstances(newMIGDevice("0000:01:00.0"), nil)
	assert.Equal(t, 0, m.TotalCount())
}

func TestAddMIGInstances_AppendsToExistingPool(t *testing.T) {
	root := t.TempDir()
	_, wholeGPU := buildManagerSysfs(t)

	mdev := makeMdevDir(t, root, "uuid-gi1")
	m := NewManager([]GPUDevice{wholeGPU})

	m.AddMIGInstances(newMIGDevice("0000:04:00.0"), []MIGInstance{
		{GIID: 1, MdevPath: mdev, Profile: MIGProfile{Name: "7g.80gb", MemoryMiB: 81920}},
	})

	assert.Equal(t, 2, m.TotalCount())
}

// --- Claim (MIG path) ---

func TestMIGClaim_Success(t *testing.T) {
	root := t.TempDir()
	mdev := makeMdevDir(t, root, "uuid-gi1")

	m := NewManager(nil)
	dev := newMIGDevice("0000:01:00.0")
	m.AddMIGInstances(dev, []MIGInstance{
		{GIID: 1, MdevPath: mdev, Profile: MIGProfile{Name: "1g.10gb", MemoryMiB: 10240}},
	})

	claimedDev, mig, err := m.Claim("i-001", "1g.10gb")
	require.NoError(t, err)
	require.NotNil(t, mig)
	assert.Equal(t, mdev, mig.MdevPath)
	assert.Equal(t, "1g.10gb", mig.Profile.Name)
	assert.Equal(t, dev.PCIAddress, claimedDev.PCIAddress)
	assert.Equal(t, 0, m.Available())
}

func TestMIGClaim_MdevPathMissing_ReturnsError(t *testing.T) {
	m := NewManager(nil)
	m.AddMIGInstances(newMIGDevice("0000:01:00.0"), []MIGInstance{
		{GIID: 1, MdevPath: "/nonexistent/mdev/uuid", Profile: MIGProfile{Name: "1g.10gb"}},
	})

	_, _, err := m.Claim("i-001", "1g.10gb")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not accessible")
	// The pool entry must still be available (no state change on failure).
	assert.Equal(t, 1, m.Available())
}

func TestMIGClaim_MultipleSlices_ClaimsInOrder(t *testing.T) {
	root := t.TempDir()
	mdev1 := makeMdevDir(t, root, "uuid-gi1")
	mdev2 := makeMdevDir(t, root, "uuid-gi2")

	m := NewManager(nil)
	m.AddMIGInstances(newMIGDevice("0000:01:00.0"), []MIGInstance{
		{GIID: 1, MdevPath: mdev1, Profile: MIGProfile{Name: "1g.10gb"}},
		{GIID: 2, MdevPath: mdev2, Profile: MIGProfile{Name: "1g.10gb"}},
	})

	_, mig1, err := m.Claim("i-001", "1g.10gb")
	require.NoError(t, err)
	_, mig2, err := m.Claim("i-002", "1g.10gb")
	require.NoError(t, err)
	_, _, err = m.Claim("i-003", "1g.10gb")
	require.Error(t, err, "pool exhausted after two claims")

	assert.NotEqual(t, mig1.MdevPath, mig2.MdevPath)
}

// --- Release (MIG path) ---

func TestMIGRelease_ClearsInstanceAndRestoresAvailability(t *testing.T) {
	root := t.TempDir()
	mdev := makeMdevDir(t, root, "uuid-gi1")

	m := NewManager(nil)
	m.AddMIGInstances(newMIGDevice("0000:01:00.0"), []MIGInstance{
		{GIID: 1, MdevPath: mdev, Profile: MIGProfile{Name: "1g.10gb"}},
	})

	_, _, err := m.Claim("i-001", "1g.10gb")
	require.NoError(t, err)
	assert.Equal(t, 0, m.Available())

	require.NoError(t, m.Release("i-001"))
	assert.Equal(t, 1, m.Available())
	assert.Equal(t, 0, m.AllocatedCount())
}

func TestMIGRelease_UnknownInstance_ReturnsError(t *testing.T) {
	m := NewManager(nil)
	err := m.Release("i-never-claimed")
	require.Error(t, err)
}

// --- MarkMIGFailed ---

func TestMarkMIGFailed_TargetsOnlyNamedSlice(t *testing.T) {
	root := t.TempDir()
	mdev1 := makeMdevDir(t, root, "uuid-gi1")
	mdev2 := makeMdevDir(t, root, "uuid-gi2")

	m := NewManager(nil)
	m.AddMIGInstances(newMIGDevice("0000:01:00.0"), []MIGInstance{
		{GIID: 1, MdevPath: mdev1, Profile: MIGProfile{Name: "1g.10gb"}},
		{GIID: 2, MdevPath: mdev2, Profile: MIGProfile{Name: "1g.10gb"}},
	})

	m.MarkMIGFailed(mdev1)

	// First slice is failed, second is still available.
	assert.Equal(t, 1, m.Available())

	_, mig, err := m.Claim("i-001", "1g.10gb")
	require.NoError(t, err)
	assert.Equal(t, mdev2, mig.MdevPath, "claim must skip failed slice and pick the healthy one")
}

func TestMarkMIGFailed_AlreadyClaimedSlice_NoChange(t *testing.T) {
	root := t.TempDir()
	mdev := makeMdevDir(t, root, "uuid-gi1")

	m := NewManager(nil)
	m.AddMIGInstances(newMIGDevice("0000:01:00.0"), []MIGInstance{
		{GIID: 1, MdevPath: mdev, Profile: MIGProfile{Name: "1g.10gb"}},
	})

	_, _, err := m.Claim("i-001", "1g.10gb")
	require.NoError(t, err)

	// MarkMIGFailed on a claimed slice must be a no-op (condition: InstanceID == "").
	m.MarkMIGFailed(mdev)
	require.NoError(t, m.Release("i-001"))
	// After release the slice must be available again (not failed).
	assert.Equal(t, 1, m.Available())
}

func TestMarkMIGFailed_UnknownPath_NoOp(t *testing.T) {
	m := NewManager(nil)
	// Must not panic.
	m.MarkMIGFailed("/nonexistent/mdev")
}

// --- MarkFailed (whole-GPU) does not affect MIG entries ---

func TestMarkFailed_DoesNotAffectMIGEntries(t *testing.T) {
	root := t.TempDir()
	mdev := makeMdevDir(t, root, "uuid-gi1")

	m := NewManager(nil)
	dev := newMIGDevice("0000:01:00.0")
	m.AddMIGInstances(dev, []MIGInstance{
		{GIID: 1, MdevPath: mdev, Profile: MIGProfile{Name: "1g.10gb"}},
	})

	// MarkFailed only targets whole-GPU entries (MIGInstance == nil).
	m.MarkFailed(dev.PCIAddress)
	assert.Equal(t, 1, m.Available(), "MIG slice must remain available after MarkFailed")
}

// --- ReclaimByMdev ---

func TestReclaimByMdev_Success(t *testing.T) {
	root := t.TempDir()
	mdev := makeMdevDir(t, root, "uuid-gi1")

	m := NewManager(nil)
	m.AddMIGInstances(newMIGDevice("0000:01:00.0"), []MIGInstance{
		{GIID: 1, MdevPath: mdev, Profile: MIGProfile{Name: "1g.10gb"}},
	})

	require.NoError(t, m.ReclaimByMdev(mdev, "i-001"))
	assert.Equal(t, 0, m.Available())
	assert.Equal(t, 1, m.AllocatedCount())
}

func TestReclaimByMdev_Idempotent_SameInstance(t *testing.T) {
	root := t.TempDir()
	mdev := makeMdevDir(t, root, "uuid-gi1")

	m := NewManager(nil)
	m.AddMIGInstances(newMIGDevice("0000:01:00.0"), []MIGInstance{
		{GIID: 1, MdevPath: mdev, Profile: MIGProfile{Name: "1g.10gb"}},
	})

	require.NoError(t, m.ReclaimByMdev(mdev, "i-001"))
	require.NoError(t, m.ReclaimByMdev(mdev, "i-001"), "same instance re-registering must not error")
	assert.Equal(t, 1, m.AllocatedCount())
}

func TestReclaimByMdev_ConflictsWithDifferentInstance(t *testing.T) {
	root := t.TempDir()
	mdev := makeMdevDir(t, root, "uuid-gi1")

	m := NewManager(nil)
	m.AddMIGInstances(newMIGDevice("0000:01:00.0"), []MIGInstance{
		{GIID: 1, MdevPath: mdev, Profile: MIGProfile{Name: "1g.10gb"}},
	})

	require.NoError(t, m.ReclaimByMdev(mdev, "i-001"))
	err := m.ReclaimByMdev(mdev, "i-002")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already claimed")
}

func TestReclaimByMdev_NotInPool_ReturnsError(t *testing.T) {
	m := NewManager(nil)
	err := m.ReclaimByMdev("/sys/bus/mdev/devices/unknown-uuid", "i-001")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found in pool")
}

func TestReclaimByMdev_MdevGone_ReturnsError(t *testing.T) {
	root := t.TempDir()
	mdev := filepath.Join(root, "uuid-gone")

	m := NewManager(nil)
	m.AddMIGInstances(newMIGDevice("0000:01:00.0"), []MIGInstance{
		{GIID: 1, MdevPath: mdev, Profile: MIGProfile{Name: "1g.10gb"}},
	})

	// The mdev directory was never created, simulating MIG mode being disabled.
	err := m.ReclaimByMdev(mdev, "i-001")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not accessible")
}
