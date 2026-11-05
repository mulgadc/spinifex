package image

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	awss3 "github.com/aws/aws-sdk-go/service/s3"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	ebsmetadata "github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	uefiTestInstance = "i-0123456789abcdef0"
	uefiTestVarsSize = 4096
)

var uefiTestFirmware = uefistore.FirmwareIdentity{Arch: "x86_64", VarsSize: uefiTestVarsSize, CodeSHA256: "code-sha"}

func uefiRegion(tag string) []byte {
	b := make([]byte, uefiTestVarsSize)
	copy(b, tag)
	return b
}

// uefiImageFixture is an image service wired to a real store-record bucket,
// with a UEFI instance whose root volume CreateImage can snapshot.
type uefiImageFixture struct {
	svc     *ImageServiceImpl
	objects *objectstore.MemoryObjectStore
	records *uefistore.Store
	volume  ebsmetadata.Volume
}

func newUEFIImageFixture(t *testing.T) *uefiImageFixture {
	t.Helper()
	svc, objects, provider := setupProviderSnapshotImageService(t)
	_, _, js := testutil.StartTestJetStream(t)
	records := uefistore.NewStore(js)
	require.NoError(t, records.Open(t.Context()))
	svc.SetUEFIStores(records, "node-a")
	return &uefiImageFixture{
		svc: svc, objects: objects, records: records,
		volume: seedProviderVolume(t, svc, provider, "vol-uefi001", 8),
	}
}

// putInstanceStore puts vars at a fresh key under the instance and points a
// live store record at it.
func (f *uefiImageFixture) putInstanceStore(t *testing.T, vars []byte) string {
	t.Helper()
	key := f.putObject(t, uefiTestInstance, vars)
	_, err := f.records.Create(t.Context(), &uefistore.UEFIStoreRecord{
		InstanceID: uefiTestInstance, AccountID: testAccountID, State: uefistore.StateLive, Generation: 1,
		Pointer: &uefistore.UEFIVarStoreRef{Key: key, Firmware: uefiTestFirmware, WriterNode: "node-a", WrittenAt: time.Now().UTC()},
	})
	require.NoError(t, err)
	return key
}

func (f *uefiImageFixture) putObject(t *testing.T, owner string, vars []byte) string {
	t.Helper()
	key, err := uefistore.NewObjectKey(owner)
	require.NoError(t, err)
	require.NoError(t, uefistore.PutObject(t.Context(), f.objects, testBucket, key, vars, uefistore.ObjectMeta{
		OwnerID: owner, WriterNode: "node-a", Firmware: uefiTestFirmware, Source: uefistore.SourceWriteBack,
	}))
	return key
}

func (f *uefiImageFixture) createImage(t *testing.T, uefi bool) (string, error) {
	t.Helper()
	out, err := f.svc.CreateImageFromInstance(CreateImageParams{
		Input:        &ec2.CreateImageInput{InstanceId: aws.String(uefiTestInstance), Name: aws.String("uefi-image")},
		RootVolumeID: f.volume.VolumeID,
		UEFI:         uefi,
	}, testAccountID)
	if err != nil {
		return "", err
	}
	return aws.StringValue(out.ImageId), nil
}

func (f *uefiImageFixture) ami(t *testing.T, imageID string) ebsmetadata.AMI {
	t.Helper()
	ami, err := f.svc.MetadataStore().GetAMI(t.Context(), imageID)
	require.NoError(t, err)
	return ami
}

func (f *uefiImageFixture) region(t *testing.T, key string) []byte {
	t.Helper()
	out, err := f.objects.GetObject(t.Context(), &awss3.GetObjectInput{Bucket: aws.String(testBucket), Key: aws.String(key)})
	require.NoError(t, err)
	defer out.Body.Close()
	zr, err := gzip.NewReader(out.Body)
	require.NoError(t, err)
	b, err := io.ReadAll(zr)
	require.NoError(t, err)
	return b
}

func (f *uefiImageFixture) keys(t *testing.T, owner string) []string {
	t.Helper()
	keys, err := uefistore.ListObjects(t.Context(), f.objects, testBucket, owner)
	require.NoError(t, err)
	return keys
}

func (f *uefiImageFixture) copyImage(t *testing.T, src, name string) string {
	t.Helper()
	out, err := f.svc.CopyImage(t.Context(), validCopyImageServiceInput(src, name), testAccountID)
	require.NoError(t, err)
	return aws.StringValue(out.ImageId)
}

func (f *uefiImageFixture) deregister(t *testing.T, imageID string) error {
	t.Helper()
	_, err := f.svc.DeregisterImage(t.Context(), &ec2.DeregisterImageInput{ImageId: aws.String(imageID)}, testAccountID)
	return err
}

// CreateImage copies the object the instance's record points at to the
// image's own prefix, and the AMI document names it with its firmware.
func TestCreateImage_CopiesTheInstancesUEFIStore(t *testing.T) {
	f := newUEFIImageFixture(t)
	instanceKey := f.putInstanceStore(t, uefiRegion("guest variables"))

	imageID, err := f.createImage(t, true)
	require.NoError(t, err)

	ami := f.ami(t, imageID)
	require.NotNil(t, ami.UEFIData)
	assert.Equal(t, []string{ami.UEFIData.Key}, f.keys(t, imageID), "the image has one object under its own prefix")
	assert.Equal(t, uefiRegion("guest variables"), f.region(t, ami.UEFIData.Key))
	assert.Equal(t, uefistore.AMIRef(ami.UEFIData.Key, uefiTestFirmware), ami.UEFIData)
	assert.Equal(t, []string{instanceKey}, f.keys(t, uefiTestInstance), "the instance's store is left as it was")

	store, err := uefistore.ImageVarStoreFromAMI(ami)
	require.NoError(t, err)
	assert.Equal(t, &uefistore.ImageVarStore{ImageID: imageID, Key: ami.UEFIData.Key, Firmware: uefiTestFirmware}, store,
		"the document decodes into the store a launch seeds from")
}

// A UEFI instance with no store record or no pointer, and a BIOS instance,
// give an image that launches from the template.
func TestCreateImage_AnInstanceWithoutAPointerContributesNothing(t *testing.T) {
	t.Run("no record", func(t *testing.T) {
		f := newUEFIImageFixture(t)
		imageID, err := f.createImage(t, true)
		require.NoError(t, err)
		assert.Nil(t, f.ami(t, imageID).UEFIData)
		assert.Empty(t, f.keys(t, imageID))
	})
	t.Run("no pointer", func(t *testing.T) {
		f := newUEFIImageFixture(t)
		_, err := f.records.Create(t.Context(), &uefistore.UEFIStoreRecord{
			InstanceID: uefiTestInstance, AccountID: testAccountID, State: uefistore.StateLive, Generation: 1,
		})
		require.NoError(t, err)
		imageID, err := f.createImage(t, true)
		require.NoError(t, err)
		assert.Nil(t, f.ami(t, imageID).UEFIData)
	})
	t.Run("BIOS instance", func(t *testing.T) {
		f := newUEFIImageFixture(t)
		f.putInstanceStore(t, uefiRegion("never read"))
		f.svc.SetUEFIStores(nil, "")
		imageID, err := f.createImage(t, false)
		require.NoError(t, err)
		assert.Nil(t, f.ami(t, imageID).UEFIData)
	})
}

// A UEFI instance's image is refused rather than made without its store when
// this node cannot read store records, and a terminating store is refused.
func TestCreateImage_RefusesWhenTheStoreCannotBeCarried(t *testing.T) {
	t.Run("no records wired", func(t *testing.T) {
		f := newUEFIImageFixture(t)
		f.svc.SetUEFIStores(nil, "")
		_, err := f.createImage(t, true)
		require.EqualError(t, err, awserrors.ErrorServerInternal)
	})
	t.Run("deleting", func(t *testing.T) {
		f := newUEFIImageFixture(t)
		f.putInstanceStore(t, uefiRegion("terminating"))
		_, err := f.records.MarkDeleting(t.Context(), uefiTestInstance)
		require.NoError(t, err)
		_, err = f.createImage(t, true)
		require.EqualError(t, err, awserrors.ErrorIncorrectInstanceState)
	})
	t.Run("object unreadable", func(t *testing.T) {
		f := newUEFIImageFixture(t)
		key := f.putInstanceStore(t, uefiRegion("x"))
		_, err := f.objects.PutObject(t.Context(), &awss3.PutObjectInput{
			Bucket: aws.String(testBucket), Key: aws.String(key), Body: bytes.NewReader([]byte("not gzip")),
		})
		require.NoError(t, err)
		_, err = f.createImage(t, true)
		require.EqualError(t, err, awserrors.ErrorServerInternal)
	})
}

// laggingPointer answers the first read with a pointer whose object a prune
// has removed, and every later read with the record as stored.
type laggingPointer struct {
	*uefistore.Store

	stale *uefistore.UEFIStoreRecord
	reads int
}

func (l *laggingPointer) LeaderGet(ctx context.Context, id string) (*uefistore.UEFIStoreRecord, uint64, error) {
	l.reads++
	if l.reads == 1 {
		return l.stale, 1, nil
	}
	return l.Store.LeaderGet(ctx, id)
}

// A running instance's write-back can move the pointer and prune the old
// object between CreateImage's read and its get, so the pointer is read again.
func TestCreateImage_RereadsAPointerWhoseObjectWasPruned(t *testing.T) {
	f := newUEFIImageFixture(t)
	f.putInstanceStore(t, uefiRegion("newest"))
	stale, _, err := f.records.Get(t.Context(), uefiTestInstance)
	require.NoError(t, err)
	stale.Pointer.Key = uefistore.KeyPrefix(uefiTestInstance) + "pruned"
	lagging := &laggingPointer{Store: f.records, stale: stale}
	f.svc.SetUEFIStores(lagging, "node-a")

	imageID, err := f.createImage(t, true)
	require.NoError(t, err)

	assert.Equal(t, 2, lagging.reads)
	assert.Equal(t, uefiRegion("newest"), f.region(t, f.ami(t, imageID).UEFIData.Key))
}

// TestS7_AnInstancePruneNeverRemovesTheStoreItsImageBootsFrom enforces
// ADR-0003:S7 for the instance's automatic prune: the image's store is a copy
// under the image's prefix, so pruning the instance's prefix never reaches it.
func TestS7_AnInstancePruneNeverRemovesTheStoreItsImageBootsFrom(t *testing.T) {
	f := newUEFIImageFixture(t)
	f.putInstanceStore(t, uefiRegion("imaged"))
	imageID, err := f.createImage(t, true)
	require.NoError(t, err)
	newer := f.putObject(t, uefiTestInstance, uefiRegion("written back later"))

	_, err = uefistore.PruneObjects(t.Context(), f.objects, testBucket, uefiTestInstance, newer, time.Now().Add(time.Hour))
	require.NoError(t, err)

	assert.Equal(t, []string{newer}, f.keys(t, uefiTestInstance), "the prune ran")
	assert.Equal(t, uefiRegion("imaged"), f.region(t, f.ami(t, imageID).UEFIData.Key),
		"ADR-0003:S7: the instance's prune must not delete the store its image boots from")
}

// CopyImage gives the copy its own object holding the source's region.
func TestCopyImage_GivesTheCopyItsOwnUEFIStore(t *testing.T) {
	f := newUEFIImageFixture(t)
	f.putInstanceStore(t, uefiRegion("source variables"))
	src, err := f.createImage(t, true)
	require.NoError(t, err)

	cp := f.copyImage(t, src, "uefi-copy")

	srcRef, cpRef := f.ami(t, src).UEFIData, f.ami(t, cp).UEFIData
	require.NotNil(t, cpRef)
	assert.Equal(t, []string{cpRef.Key}, f.keys(t, cp))
	assert.Equal(t, srcRef.Firmware, cpRef.Firmware)
	assert.Equal(t, uefiRegion("source variables"), f.region(t, cpRef.Key))
	assert.Equal(t, []string{srcRef.Key}, f.keys(t, src), "the source keeps its own object")
}

// A source AMI with no store gives a copy with none.
func TestCopyImage_SourceWithoutAUEFIStoreCopiesNone(t *testing.T) {
	f := newUEFIImageFixture(t)
	seedCopyableAMI(t, f.objects, "ami-src001", "source-ami", testAccountID, "snap-src001", "vol-src001", 8)

	cp := f.copyImage(t, "ami-src001", "copy")

	assert.Nil(t, f.ami(t, cp).UEFIData)
	assert.Empty(t, f.keys(t, cp))
}

// A source store that cannot be copied fails the copy and leaves no AMI.
func TestCopyImage_UnreadableSourceStoreFails(t *testing.T) {
	f := newUEFIImageFixture(t)
	seedCopyableAMI(t, f.objects, "ami-src001", "source-ami", testAccountID, "snap-src001", "vol-src001", 8)
	src := f.ami(t, "ami-src001")
	src.UEFIData = uefistore.AMIRef(uefistore.KeyPrefix("ami-src001")+"gone", uefiTestFirmware)
	require.NoError(t, f.svc.MetadataStore().PutAMI(t.Context(), src))

	_, err := f.svc.CopyImage(t.Context(), validCopyImageServiceInput("ami-src001", "copy"), testAccountID)

	require.EqualError(t, err, awserrors.ErrorServerInternal)
	amis, err := f.svc.MetadataStore().ListAMIs(t.Context())
	require.NoError(t, err)
	assert.Len(t, amis, 1, "no copy is registered without its store")
}

// TestS7_DeregisteringASourceImageLeavesItsCopysStore enforces ADR-0003:S7
// for DeregisterImage, which account termination also calls: it deletes only
// the deregistered image's prefix, and the copy's store is its own.
func TestS7_DeregisteringASourceImageLeavesItsCopysStore(t *testing.T) {
	f := newUEFIImageFixture(t)
	instanceKey := f.putInstanceStore(t, uefiRegion("source variables"))
	src, err := f.createImage(t, true)
	require.NoError(t, err)
	cp := f.copyImage(t, src, "uefi-copy")

	require.NoError(t, f.deregister(t, src))

	assert.Empty(t, f.keys(t, src), "the deregistered image's store is gone")
	assert.Equal(t, uefiRegion("source variables"), f.region(t, f.ami(t, cp).UEFIData.Key),
		"ADR-0003:S7: deregistering the source must not delete the store its copy boots from")
	assert.Equal(t, []string{instanceKey}, f.keys(t, uefiTestInstance), "the instance's store is left as it was")
}

// failingUEFIDelete fails every delete under the UEFI root.
type failingUEFIDelete struct{ *objectstore.MemoryObjectStore }

func (s failingUEFIDelete) DeleteObject(ctx context.Context, in *awss3.DeleteObjectInput) (*awss3.DeleteObjectOutput, error) {
	if strings.HasPrefix(aws.StringValue(in.Key), uefistore.ObjectRoot) {
		return nil, assert.AnError
	}
	return s.MemoryObjectStore.DeleteObject(ctx, in)
}

// DeregisterImage deletes the store before the document, so a failed delete
// keeps the image for a retry to finish.
func TestDeregisterImage_FailedUEFIStoreDeleteKeepsTheImage(t *testing.T) {
	f := newUEFIImageFixture(t)
	f.putInstanceStore(t, uefiRegion("x"))
	imageID, err := f.createImage(t, true)
	require.NoError(t, err)
	f.svc.store = failingUEFIDelete{f.objects}

	require.EqualError(t, f.deregister(t, imageID), awserrors.ErrorServerInternal)
	assert.NotNil(t, f.ami(t, imageID).UEFIData, "the document stays for the retry")

	f.svc.store = f.objects
	require.NoError(t, f.deregister(t, imageID))
	assert.Empty(t, f.keys(t, imageID))
	_, err = f.svc.MetadataStore().GetAMI(t.Context(), imageID)
	assert.True(t, objectstore.IsNoSuchKeyError(err), "the retry removes the document: %v", err)
}

var _ UEFIStoreRecords = (*laggingPointer)(nil)
