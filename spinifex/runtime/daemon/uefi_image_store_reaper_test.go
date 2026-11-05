//test:in-package — the store reaper and the terminator adapter are unexported daemon types
package daemon

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	ebsmetadata "github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	ec2image "github.com/mulgadc/spinifex/spinifex/domains/ec2/image"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	ebsprovider "github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	imageTestInstance = "i-00000000000imaged"
	imageTestVarsSize = 4096
)

type imageTestTypes struct{}

func (imageTestTypes) Resolve(string) (vm.InstanceTypeSpec, bool) {
	return vm.InstanceTypeSpec{VCPUs: 1, MemoryMiB: 512, Architecture: "x86_64"}, true
}

// nodeFirmware points the node's x86_64 firmware at a test CODE and VARS
// template and returns the identity a store written here carries.
func nodeFirmware(t *testing.T) uefistore.FirmwareIdentity {
	t.Helper()
	dir := t.TempDir()
	code, vars := filepath.Join(dir, "OVMF_CODE.fd"), filepath.Join(dir, "OVMF_VARS.fd")
	require.NoError(t, os.WriteFile(code, []byte("ovmf code"), 0o600))
	require.NoError(t, os.WriteFile(vars, make([]byte, imageTestVarsSize), 0o600))
	orig, had := vm.FirmwarePathCandidates["x86_64"]
	vm.FirmwarePathCandidates["x86_64"] = []vm.FirmwareCandidate{{Code: code, VarsTemplate: vars}}
	t.Cleanup(func() {
		if had {
			vm.FirmwarePathCandidates["x86_64"] = orig
		} else {
			delete(vm.FirmwarePathCandidates, "x86_64")
		}
	})
	fw, err := vm.NodeFirmwareFor("x86_64")
	require.NoError(t, err)
	return fw.Identity
}

func varsRegion(tag string) []byte {
	b := make([]byte, imageTestVarsSize)
	copy(b, tag)
	return b
}

// putStore puts vars as a store object under owner and returns its key.
func (f *uefiReaperFixture) putStore(t *testing.T, owner string, vars []byte, fw uefistore.FirmwareIdentity) string {
	t.Helper()
	key, err := uefistore.NewObjectKey(owner)
	require.NoError(t, err)
	require.NoError(t, uefistore.PutObject(t.Context(), f.objects, uefiTestBucket, key, vars, uefistore.ObjectMeta{
		OwnerID: owner, WriterNode: "node-a", Firmware: fw, Source: uefistore.SourceWriteBack,
	}))
	return key
}

func (f *uefiReaperFixture) storedVars(t *testing.T, key string) []byte {
	t.Helper()
	out, err := f.objects.GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String(uefiTestBucket), Key: aws.String(key)})
	require.NoError(t, err, "ADR-0003:S7: the image's store object was deleted")
	defer out.Body.Close()
	zr, err := gzip.NewReader(out.Body)
	require.NoError(t, err)
	b, err := io.ReadAll(zr)
	require.NoError(t, err)
	return b
}

// createImage runs CreateImage for a running UEFI instance through the image
// service over the fixture's objects and store records, and returns the AMI.
func (f *uefiReaperFixture) createImage(t *testing.T, instanceID string) ebsmetadata.AMI {
	t.Helper()
	const volumeID = "vol-0123456789abcdef0"
	cfg := &config.Config{WalDir: t.TempDir(), AZ: "ap-southeast-2a",
		Predastore: config.PredastoreConfig{Host: "127.0.0.1:1", Bucket: uefiTestBucket, Region: "us-east-1"}}
	svc := ec2image.NewImageServiceImplWithConfig(cfg, f.objects, nil)
	provider := ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{})
	svc.SetEBSProvider(provider)
	svc.SetUEFIStores(f.m.UEFIStores(), "node-a")
	created, err := provider.CreateVolume(t.Context(), ebsprovider.CreateVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID, CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
	})
	require.NoError(t, err)
	require.NoError(t, svc.MetadataStore().PutVolume(t.Context(), ebsmetadata.Volume{
		VolumeID: volumeID, TenantID: "111122223333", CapacityGiB: 1, State: "in-use", CreatedAt: time.Now(),
		AvailabilityZone: cfg.AZ, VolumeType: "gp3", ProviderHandle: created.Handle,
	}))
	out, err := svc.CreateImageFromInstance(ec2image.CreateImageParams{
		Input:        &ec2.CreateImageInput{InstanceId: aws.String(instanceID), Name: aws.String("imaged")},
		RootVolumeID: volumeID, IsRunning: true, UEFI: true,
	}, "111122223333")
	require.NoError(t, err)
	ami, err := svc.MetadataStore().GetAMI(t.Context(), aws.StringValue(out.ImageId))
	require.NoError(t, err)
	require.NotNil(t, ami.UEFIData)
	return ami
}

// TestS7_TerminatingAnImagedInstanceLeavesItsImagesStore enforces ADR-0003:S7
// for the store reaper: terminating the instance an image was made from
// deletes the instance's store and never the copy the image boots from.
func TestS7_TerminatingAnImagedInstanceLeavesItsImagesStore(t *testing.T) {
	f := newUEFIReaperFixture(t)
	fw := uefistore.FirmwareIdentity{Arch: "x86_64", VarsSize: imageTestVarsSize, CodeSHA256: "code"}
	f.writeInstance(t, imageTestInstance, vm.StateRunning)
	key := f.putStore(t, imageTestInstance, varsRegion("imaged"), fw)
	_, err := f.m.UEFIStores().Create(t.Context(), &uefistore.UEFIStoreRecord{
		InstanceID: imageTestInstance, AccountID: "111122223333", State: uefistore.StateLive, Generation: 1,
		Pointer: &uefistore.UEFIVarStoreRef{Key: key, Firmware: fw, WriterNode: "node-a", WrittenAt: time.Now().UTC()},
	})
	require.NoError(t, err)
	ami := f.createImage(t, imageTestInstance)

	f.writeInstance(t, imageTestInstance, vm.StateShuttingDown)
	term := &uefiStoreTerminator{stores: f.m.UEFIStores(), dataDir: t.TempDir()}
	has, err := term.MarkUEFIStoreDeleting(imageTestInstance)
	require.NoError(t, err)
	require.True(t, has)
	f.writeTerminated(t, imageTestInstance, vm.TeardownPending)
	reaped, err := f.r.Sweep(t.Context())
	require.NoError(t, err)

	assert.Equal(t, 1, reaped)
	assert.Zero(t, f.objectCount(t, imageTestInstance), "the terminated instance's store is deleted")
	assert.Equal(t, varsRegion("imaged"), f.storedVars(t, ami.UEFIData.Key),
		"ADR-0003:S7: terminating the source instance must not delete the store its image boots from")
	assert.Equal(t, 1, f.objectCount(t, ami.ImageID))
}

// TestS7_AFailedRunInstancesFromAnImageLeavesTheImagesStore enforces
// ADR-0003:S7 for a launch seeded from an image: when it fails, the store
// reaper deletes the instance's seeded store and never the image's object.
func TestS7_AFailedRunInstancesFromAnImageLeavesTheImagesStore(t *testing.T) {
	f := newUEFIReaperFixture(t)
	fw := nodeFirmware(t)
	const imageID = "ami-0123456789abcdef0"
	imageKey := f.putStore(t, imageID, varsRegion("image variables"), fw)
	mgr := vm.NewManagerWithDeps(vm.Deps{
		NodeID:        "node-a",
		InstanceTypes: imageTestTypes{},
		UEFIVarStore: vm.UEFIVarStoreDeps{
			Objects: f.objects, Bucket: uefiTestBucket, DataDir: t.TempDir(),
			Records: f.m.UEFIStores(), Claims: uefistore.NewClaims(kvstore.OpenSyncBucket(f.m.js), "node-a"),
		},
	})
	instance := &vm.VM{ID: imageTestInstance, AccountID: "111122223333", InstanceType: "t3.nano", BootMode: "uefi",
		Status: vm.StatePending, FirstLaunch: true, Instance: &ec2.Instance{InstanceId: aws.String(imageTestInstance)}}
	require.NoError(t, mgr.SeedUEFIVarStore(t.Context(), instance,
		&uefistore.ImageVarStore{ImageID: imageID, Key: imageKey, Firmware: fw}))
	require.Equal(t, 1, f.objectCount(t, imageTestInstance), "the launch seeded its own object")

	term := &uefiStoreTerminator{stores: f.m.UEFIStores(), dataDir: t.TempDir()}
	has, err := term.MarkUEFIStoreDeleting(imageTestInstance)
	require.NoError(t, err)
	require.True(t, has)
	f.writeTerminated(t, imageTestInstance, vm.TeardownPending)
	reaped, err := f.r.Sweep(t.Context())
	require.NoError(t, err)

	assert.Equal(t, 1, reaped)
	assert.Zero(t, f.objectCount(t, imageTestInstance), "the failed launch's seeded store is deleted")
	assert.Equal(t, varsRegion("image variables"), f.storedVars(t, imageKey),
		"ADR-0003:S7: a failed launch must not delete the image store it was seeded from")
}
