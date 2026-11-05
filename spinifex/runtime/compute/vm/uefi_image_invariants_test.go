package vm

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	ebsmetadata "github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	ec2image "github.com/mulgadc/spinifex/spinifex/domains/ec2/image"
	ebsprovider "github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createImage runs CreateImage for the fixture's instance through the image
// service over the fixture's object store and store records, and returns the
// key of the image's UEFI store object.
func (f *uefiFixture) createImage(t *testing.T) string {
	t.Helper()
	const volumeID = "vol-0123456789abcdef0"
	cfg := &config.Config{WalDir: t.TempDir(), AZ: "ap-southeast-2a",
		Predastore: config.PredastoreConfig{Host: "127.0.0.1:1", Bucket: testUEFIBucket, Region: "us-east-1"}}
	svc := ec2image.NewImageServiceImplWithConfig(cfg, f.objects, nil)
	provider := ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{})
	svc.SetEBSProvider(provider)
	svc.SetUEFIStores(f.records, "node-a")
	created, err := provider.CreateVolume(t.Context(), ebsprovider.CreateVolumeRequest{
		Versioned: ebsprovider.NewVersioned(), VolumeID: volumeID, CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30},
	})
	require.NoError(t, err)
	require.NoError(t, svc.MetadataStore().PutVolume(t.Context(), ebsmetadata.Volume{
		VolumeID: volumeID, TenantID: "111122223333", CapacityGiB: 1, State: "in-use", CreatedAt: time.Now(),
		AvailabilityZone: cfg.AZ, VolumeType: "gp3", ProviderHandle: created.Handle,
	}))

	out, err := svc.CreateImageFromInstance(ec2image.CreateImageParams{
		Input:        &ec2.CreateImageInput{InstanceId: aws.String(testUEFIInstance), Name: aws.String("imaged")},
		RootVolumeID: volumeID, IsRunning: true, UEFI: true,
	}, "111122223333")
	require.NoError(t, err)
	ami, err := svc.MetadataStore().GetAMI(t.Context(), aws.StringValue(out.ImageId))
	require.NoError(t, err)
	require.NotNil(t, ami.UEFIData)
	return ami.UEFIData.Key
}

// TestS7_AnInstancePruneNeverRemovesTheStoreItsImageBootsFrom enforces
// ADR-0003:S7 for the write-back reaper's automatic prune: CreateImage copies
// the instance's store under the image's prefix, which no instance prune reaches.
func TestS7_AnInstancePruneNeverRemovesTheStoreItsImageBootsFrom(t *testing.T) {
	f := newUEFIFixture(t)
	instance, clock := f.running(t)
	imaged := f.mustRecord(t).Pointer.Key
	imageKey := f.createImage(t)
	f.guestWrites(t, "written back later")
	f.m.finishUEFIWriteBack(instance)
	current := f.mustRecord(t).Pointer.Key
	require.NotEqual(t, imaged, current, "the write-back moved the pointer")
	old := clock.Now().Add(-24 * time.Hour)
	f.m.deps.UEFIVarStore.Objects = &agedObjectStore{MemoryObjectStore: f.objects, modified: map[string]time.Time{
		imaged: old, current: old, imageKey: old,
	}}
	f.m.uefi.queuePrune(testUEFIInstance)

	_, err := f.m.NewUEFIWriteBackReaper().Sweep(t.Context())
	require.NoError(t, err)

	assert.Equal(t, []string{current}, f.objectKeys(t), "the prune removed the instance's old object")
	assert.Equal(t, region(""), gunzip(t, f.objectBody(t, imageKey)),
		"ADR-0003:S7: the instance's prune must not delete the store its image boots from")
}
