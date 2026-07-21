package handlers_ec2_instance

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/mulgadc/viperblock/viperblock"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// TestPrepareEFIVolume_BackendInitFailure_NoGoroutineLeak exercises the first
// of prepareEFIVolume's six error returns (Backend.Init failure) and asserts
// the chunk uploader and WAL syncer goroutines started by newViperblock do
// not survive the error return.
func TestPrepareEFIVolume_BackendInitFailure_NoGoroutineLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	// Swap in fake firmware files so the test doesn't depend on OVMF being
	// installed on the host; FirmwarePathCandidates is exported for exactly
	// this purpose.
	fwDir := t.TempDir()
	codePath := filepath.Join(fwDir, "CODE.fd")
	varsPath := filepath.Join(fwDir, "VARS.fd")
	require.NoError(t, os.WriteFile(codePath, []byte("fake-code"), 0o600))
	require.NoError(t, os.WriteFile(varsPath, []byte("fake-vars-template"), 0o600))

	origCandidates := vm.FirmwarePathCandidates
	//nolint:reassign // test-only override of vm's exported var, restored via t.Cleanup; same pattern vm/firmware_test.go uses within its own package.
	vm.FirmwarePathCandidates = map[string][]vm.FirmwareCandidate{
		"x86_64": {{Code: codePath, VarsTemplate: varsPath}},
	}
	t.Cleanup(func() {
		//nolint:reassign // restoring the original value swapped above
		vm.FirmwarePathCandidates = origCandidates
	})

	svc := &InstanceServiceImpl{
		config: &config.Config{
			WalDir: t.TempDir(),
			Predastore: config.PredastoreConfig{
				// Host/creds are unset on purpose: Backend.Init's ListObjectsV2
				// call fails fast on empty static credentials, no network needed.
				Host:   "127.0.0.1:1",
				Bucket: "test-bucket",
				Region: "us-east-1",
			},
		},
	}

	instance := &vm.VM{}
	err := svc.prepareEFIVolume(context.Background(), "test-image", viperblock.VolumeConfig{}, instance, "x86_64")
	require.Error(t, err, "Backend.Init must fail against an unreachable predastore host")
}
