//test:in-package — daemonGPUClaimer and its Daemon field are unexported;
package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/gpu"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newMIGClaimManager(t *testing.T, count int) *gpu.Manager {
	t.Helper()

	dev := gpu.GPUDevice{PCIAddress: "0000:03:00.0", Model: "NVIDIA A10"}
	instances := make([]gpu.MIGInstance, 0, count)
	for i := range count {
		mdevPath := filepath.Join(t.TempDir(), "mdev")
		require.NoError(t, os.WriteFile(mdevPath, []byte("mdev"), 0o600))
		instances = append(instances, gpu.MIGInstance{
			GIID:     i + 1,
			MdevPath: mdevPath,
			Profile:  gpu.MIGProfile{Name: "1g.10gb", MemoryMiB: 10 * 1024},
		})
	}
	mgr := gpu.NewManager(nil)
	mgr.AddMIGInstances(dev, instances)
	return mgr
}

func TestDaemonGPUClaimerClaimsRequestedCount(t *testing.T) {
	mgr := newMIGClaimManager(t, 2)
	claimer := &daemonGPUClaimer{d: &Daemon{gpuManager: mgr}}

	attachments, err := claimer.Claim("i-two-gpu", "1g.10gb", 2)
	require.NoError(t, err)
	require.Len(t, attachments, 2)
	assert.NotEqual(t, attachments[0].MdevPath, attachments[1].MdevPath)
	assert.Zero(t, mgr.AvailableSlices("1g.10gb"))

	require.NoError(t, claimer.Release("i-two-gpu"))
	assert.Equal(t, 2, mgr.AvailableSlices("1g.10gb"))
}

func TestDaemonGPUClaimerRollsBackPartialClaim(t *testing.T) {
	mgr := newMIGClaimManager(t, 1)
	claimer := &daemonGPUClaimer{d: &Daemon{gpuManager: mgr}}

	attachments, err := claimer.Claim("i-too-many", "1g.10gb", 2)
	require.Error(t, err)
	assert.Nil(t, attachments)
	assert.Equal(t, 1, mgr.AvailableSlices("1g.10gb"))
}
