package admin

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/ebs/metadata"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type staticInstanceIndex struct {
	ids map[string]bool
	err error
}

func (s staticInstanceIndex) InstanceIDs(context.Context) (map[string]bool, error) {
	return s.ids, s.err
}

type uefiOrphanFixture struct {
	objects  *objectstore.MemoryObjectStore
	metadata *ebsmetadata.Store
	stores   *uefistore.Store
	index    staticInstanceIndex
}

func newUEFIOrphanFixture(t *testing.T) *uefiOrphanFixture {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	stores := uefistore.NewStore(js)
	require.NoError(t, stores.Open(t.Context()))
	objects := objectstore.NewMemoryObjectStore()
	return &uefiOrphanFixture{
		objects:  objects,
		metadata: ebsmetadata.NewStore(objects, orphanTestBucket),
		stores:   stores,
		index:    staticInstanceIndex{ids: map[string]bool{}},
	}
}

func (f *uefiOrphanFixture) putStoreObject(t *testing.T, owner string) {
	t.Helper()
	_, err := f.objects.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String(orphanTestBucket),
		Key:    aws.String(uefistore.KeyPrefix(owner) + "0f1e2d3c"),
		Body:   bytes.NewReader([]byte("vars")),
	})
	require.NoError(t, err)
}

func (f *uefiOrphanFixture) record(t *testing.T, id string, deleting bool) {
	t.Helper()
	_, err := f.stores.Create(t.Context(), &uefistore.UEFIStoreRecord{InstanceID: id, AccountID: "111122223333", State: uefistore.StateLive})
	require.NoError(t, err)
	if deleting {
		_, err = f.stores.MarkDeleting(t.Context(), id)
		require.NoError(t, err)
	}
}

func (f *uefiOrphanFixture) scan(t *testing.T) (*OrphanUEFIStores, error) {
	t.Helper()
	return FindOrphanUEFIStores(t.Context(), f.objects, orphanTestBucket, f.metadata, f.stores, f.index)
}

// seedEveryCase plants one store of each shape the report must tell apart.
func (f *uefiOrphanFixture) seedEveryCase(t *testing.T) {
	t.Helper()
	// Owned: an instance record, a store record, and a registered image.
	f.index.ids["i-00000000000000live"] = true
	f.record(t, "i-00000000000000live", false)
	f.putStoreObject(t, "i-00000000000000live")
	f.record(t, "i-000000000recordonly", false)
	f.putStoreObject(t, "i-000000000recordonly")
	f.record(t, "i-0000000000deleting", true)
	f.putStoreObject(t, "i-0000000000deleting")
	require.NoError(t, f.metadata.PutAMI(t.Context(), ebsmetadata.AMI{ImageID: "ami-0000000000000live", Name: "img", State: "available"}))
	f.putStoreObject(t, "ami-0000000000000live")

	// Unowned.
	f.putStoreObject(t, "i-00000000000noowner")
	f.putStoreObject(t, "ami-000000000noimage")
	f.putStoreObject(t, "vol-00000000000other")
}

func TestFindOrphanUEFIStores_ListsEachUnownedShape(t *testing.T) {
	f := newUEFIOrphanFixture(t)
	f.seedEveryCase(t)

	orphans, err := f.scan(t)
	require.NoError(t, err)

	assert.Equal(t, []string{"uefi/i-00000000000noowner/"}, orphans.InstancePrefixes,
		"only a prefix with neither an instance record nor a store record is unowned")
	assert.Equal(t, []string{"uefi/ami-000000000noimage/"}, orphans.ImagePrefixes)
	assert.Equal(t, []string{"uefi/vol-00000000000other/"}, orphans.OtherPrefixes)
	assert.Equal(t, []OrphanUEFIStoreRecord{{InstanceID: "i-000000000recordonly", AccountID: "111122223333", State: uefistore.StateLive}},
		orphans.Records, "a deleting record is already on its way out, and the reaper owns it")
	assert.False(t, orphans.Empty())
}

// TestS7_UEFIOrphanReportDeletesNothing enforces ADR-0003:S7 for the report:
// an absent instance record is not a terminate, so a scan that lists every
// unowned store leaves every object and every record as it found them.
func TestS7_UEFIOrphanReportDeletesNothing(t *testing.T) {
	f := newUEFIOrphanFixture(t)
	f.seedEveryCase(t)
	objectsBefore := f.objects.Count()
	ids, err := f.stores.InstanceIDs(t.Context())
	require.NoError(t, err)
	revs := map[string]uint64{}
	for _, id := range ids {
		_, rev, err := f.stores.Get(t.Context(), id)
		require.NoError(t, err)
		revs[id] = rev
	}

	_, err = f.scan(t)
	require.NoError(t, err)

	assert.Equal(t, objectsBefore, f.objects.Count(), "ADR-0003:S7: the orphan report must delete no object")
	for id, rev := range revs {
		rec, got, err := f.stores.Get(t.Context(), id)
		require.NoError(t, err, "ADR-0003:S7: the orphan report removed the store record %s", id)
		assert.Equalf(t, rev, got, "ADR-0003:S7: the orphan report changed the store record %s", id)
		assert.NotEmpty(t, rec.State)
	}
}

func TestFindOrphanUEFIStores_EmptyClusterReportsNothing(t *testing.T) {
	f := newUEFIOrphanFixture(t)
	orphans, err := f.scan(t)
	require.NoError(t, err)
	assert.True(t, orphans.Empty())
}

// Reading an unreadable record or a failed index as absent would point an
// operator at live data, so either aborts the scan.
func TestFindOrphanUEFIStores_UnreadableInputsAbortTheScan(t *testing.T) {
	t.Run("store record", func(t *testing.T) {
		_, _, js := testutil.StartTestJetStream(t)
		stores := uefistore.NewStore(js)
		require.NoError(t, stores.Open(t.Context()))
		kv, err := js.KeyValue(t.Context(), uefistore.BucketName)
		require.NoError(t, err)
		_, err = kv.Put(t.Context(), "i-0123456789abcdef0", []byte(`{"schema_version":9,"instance_id":"i-0123456789abcdef0","state":"live"}`))
		require.NoError(t, err)
		objects := objectstore.NewMemoryObjectStore()

		_, err = FindOrphanUEFIStores(t.Context(), objects, orphanTestBucket, ebsmetadata.NewStore(objects, orphanTestBucket), stores, staticInstanceIndex{ids: map[string]bool{}})
		require.ErrorIs(t, err, uefistore.ErrInvalidRecord)
	})
	t.Run("instance index", func(t *testing.T) {
		f := newUEFIOrphanFixture(t)
		f.index.err = errors.New("kv unavailable")
		_, err := f.scan(t)
		require.Error(t, err)
	})
	t.Run("AMI document", func(t *testing.T) {
		f := newUEFIOrphanFixture(t)
		putRawAMIDocument(t, f.objects, "ami-corrupted000", []byte("{not json"))
		_, err := f.scan(t)
		require.Error(t, err)
	})
}
