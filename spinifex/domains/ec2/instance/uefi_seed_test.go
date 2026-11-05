package instance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	ebsprovider "github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSeedInstance = "i-0a1b2c3d4e5f60718"

type seedInstanceTypes map[string]vm.InstanceTypeSpec

func (r seedInstanceTypes) Resolve(name string) (vm.InstanceTypeSpec, bool) {
	spec, ok := r[name]
	return spec, ok
}

// uefiSeedingService is an instance service whose VM manager seeds stores into
// a real JetStream's store records and claims and an in-memory object store,
// under a stand-in x86_64 firmware.
func uefiSeedingService(t *testing.T) (*InstanceServiceImpl, *ebsprovider.MemoryProvider, *uefistore.Store, *objectstore.MemoryObjectStore) {
	t.Helper()
	fwDir := t.TempDir()
	code := filepath.Join(fwDir, "OVMF_CODE.fd")
	vars := filepath.Join(fwDir, "OVMF_VARS.fd")
	require.NoError(t, os.WriteFile(code, []byte("code"), 0o600))
	require.NoError(t, os.WriteFile(vars, make([]byte, 4096), 0o600))
	orig, had := vm.FirmwarePathCandidates["x86_64"]
	vm.FirmwarePathCandidates["x86_64"] = []vm.FirmwareCandidate{{Code: code, VarsTemplate: vars}}
	t.Cleanup(func() {
		if had {
			vm.FirmwarePathCandidates["x86_64"] = orig
		} else {
			delete(vm.FirmwarePathCandidates, "x86_64")
		}
	})

	_, _, js := testutil.StartTestJetStream(t)
	records := uefistore.NewStore(js)
	require.NoError(t, records.Open(t.Context()))

	provider := ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{CrashConsistentSnapshot: true, VolumeEnumeration: true})
	seedProviderSnapshot(t, provider, "vol-origin", "snap-source")
	objects := objectstore.NewMemoryObjectStore()
	svc := providerRootVolumeService(t, provider, rootVolumeAMILoader(), objects)
	svc.vmMgr = vm.NewManagerWithDeps(vm.Deps{
		NodeID:        "node-a",
		InstanceTypes: seedInstanceTypes{"t3.nano": {Architecture: "x86_64", VCPUs: 1, MemoryMiB: 512}},
		UEFIVarStore: vm.UEFIVarStoreDeps{
			Objects: objects, Bucket: testRootBucket, DataDir: t.TempDir(), Records: records,
			Claims: uefistore.NewClaims(kvstore.OpenSyncBucket(js), "node-a"),
		},
	})
	return svc, provider, records, objects
}

func uefiLaunch() (*ec2.RunInstancesInput, *vm.VM) {
	input := &ec2.RunInstancesInput{ImageId: aws.String("ami-1"), InstanceType: aws.String("t3.nano")}
	return input, &vm.VM{ID: testSeedInstance, AccountID: testRootAccount, InstanceType: "t3.nano", BootMode: "uefi"}
}

// A UEFI launch seeds its store as an object and a store record, and creates
// no <root>-efi volume and no EFI request.
func TestGenerateVolumes_UEFILaunchSeedsAStoreAndCreatesNoEFIVolume(t *testing.T) {
	svc, provider, records, objects := uefiSeedingService(t)
	input, instance := uefiLaunch()

	infos, err := svc.GenerateVolumes(t.Context(), input, instance)
	require.NoError(t, err)

	require.Len(t, infos, 1, "only the root volume is reported")
	instance.EBSRequests.Mu.Lock()
	for _, req := range instance.EBSRequests.Requests {
		assert.False(t, req.EFI, "no EFI request is appended")
		assert.False(t, strings.HasSuffix(req.Name, "-efi"), req.Name)
	}
	instance.EBSRequests.Mu.Unlock()
	listed, err := provider.ListVolumes(t.Context(), ebsprovider.ListVolumesRequest{Versioned: ebsprovider.NewVersioned()})
	require.NoError(t, err)
	for _, v := range listed.Volumes {
		assert.False(t, strings.HasSuffix(v.ID, "-efi"), "no EFI volume is created: %s", v.ID)
	}

	rec, _, err := records.Get(t.Context(), testSeedInstance)
	require.NoError(t, err)
	assert.Equal(t, uefistore.StateLive, rec.State)
	require.NotNil(t, rec.Pointer)
	keys, err := uefistore.ListObjects(t.Context(), objects, testRootBucket, testSeedInstance)
	require.NoError(t, err)
	assert.Equal(t, []string{rec.Pointer.Key}, keys, "the pointer names the one template object")
}

// A seed that cannot be written fails the launch as InternalError, and no EFI
// volume is created in its place.
func TestGenerateVolumes_FailedSeedIsInternalError(t *testing.T) {
	svc, provider, _, _ := uefiSeedingService(t)
	svc.vmMgr = vm.NewManager()
	input, instance := uefiLaunch()

	_, err := svc.GenerateVolumes(t.Context(), input, instance)

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
	listed, err := provider.ListVolumes(t.Context(), ebsprovider.ListVolumesRequest{Versioned: ebsprovider.NewVersioned()})
	require.NoError(t, err)
	for _, v := range listed.Volumes {
		assert.False(t, strings.HasSuffix(v.ID, "-efi"), v.ID)
	}
}
