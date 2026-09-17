package handlers_ec2_snapshot_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	handlers_ec2_snapshot "github.com/mulgadc/spinifex/spinifex/handlers/ec2/snapshot"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/testutil/recordingstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	byIDBucket    = "test-bucket"
	byIDAccount   = "111122223333"
	byIDOther     = "444455556666"
	byIDSnapshot  = "snap-target"
	byIDSnapshots = "spinifex/ebsmetadata/v2/snapshots/"
)

// newByIDService builds a snapshot service over store, with no EBS provider:
// every test here answers out of the metadata documents alone.
func newByIDService(store objectstore.ObjectStore) *handlers_ec2_snapshot.SnapshotServiceImpl {
	cfg := &config.Config{Predastore: config.PredastoreConfig{Bucket: byIDBucket}}
	return handlers_ec2_snapshot.NewSnapshotServiceImplWithStore(cfg, store, nil)
}

func putByIDSnapshot(t *testing.T, store objectstore.ObjectStore, snapshot ebsmetadata.Snapshot) {
	t.Helper()
	require.NoError(t, ebsmetadata.NewStore(store, byIDBucket).PutSnapshot(context.Background(), snapshot))
}

// byIDSnapshotDoc is a complete, readable snapshot document owned by ownerID.
func byIDSnapshotDoc(snapshotID, ownerID string) ebsmetadata.Snapshot {
	return ebsmetadata.Snapshot{
		SnapshotID: snapshotID, VolumeID: "vol-source", VolumeSize: 8, State: "completed",
		Progress: "100%", OwnerID: ownerID, StartTime: time.Now().UTC(),
		Tags: map[string]string{"Name": "nightly"},
	}
}

// seedByIDCatalog grows one account's catalog with documents no request names,
// so a by-ID describe's cost can be measured against a catalog it must ignore.
func seedByIDCatalog(t *testing.T, store objectstore.ObjectStore, ownerID string, n int) {
	t.Helper()
	for i := range n {
		putByIDSnapshot(t, store, byIDSnapshotDoc("snap-bulk"+strconv.Itoa(i), ownerID))
	}
}

func snapshotKey(t *testing.T, accountID, snapshotID string) string {
	t.Helper()
	key, err := ebsmetadata.SnapshotKey(accountID, snapshotID)
	require.NoError(t, err)
	return key
}

// A named snapshot costs one document read and no listing, whatever the account
// owns. The cost of the old path was the whole catalog, so the count is the
// assertion, not the answer.
func TestDescribeSnapshotsByID_ReadsOnlyTheNamedDocument(t *testing.T) {
	for _, catalog := range []int{1, 500} {
		t.Run("catalog"+strconv.Itoa(catalog), func(t *testing.T) {
			store := recordingstore.New()
			svc := newByIDService(store)
			putByIDSnapshot(t, store, byIDSnapshotDoc(byIDSnapshot, byIDAccount))
			seedByIDCatalog(t, store, byIDAccount, catalog)

			out, err := svc.DescribeSnapshots(t.Context(), &ec2.DescribeSnapshotsInput{
				SnapshotIds: aws.StringSlice([]string{byIDSnapshot}),
			}, byIDAccount)

			require.NoError(t, err)
			require.Len(t, out.Snapshots, 1)
			assert.Equal(t, byIDSnapshot, aws.StringValue(out.Snapshots[0].SnapshotId))
			assert.Empty(t, store.ListPrefixes(), "a named snapshot must not list a prefix")
			assert.Equal(t, []string{snapshotKey(t, byIDAccount, byIDSnapshot)}, store.Gets(),
				"a named snapshot must read its own document and nothing else")
		})
	}
}

// Naming the same snapshot twice is one read and one result: the listing path
// matched each document once however often the request named it.
func TestDescribeSnapshotsByID_MultipleAndDuplicateIDs(t *testing.T) {
	store := recordingstore.New()
	svc := newByIDService(store)
	putByIDSnapshot(t, store, byIDSnapshotDoc("snap-a", byIDAccount))
	putByIDSnapshot(t, store, byIDSnapshotDoc("snap-b", byIDAccount))

	out, err := svc.DescribeSnapshots(t.Context(), &ec2.DescribeSnapshotsInput{
		SnapshotIds: aws.StringSlice([]string{"snap-a", "snap-b", "snap-a"}),
	}, byIDAccount)

	require.NoError(t, err)
	require.Len(t, out.Snapshots, 2)
	assert.Equal(t, "snap-a", aws.StringValue(out.Snapshots[0].SnapshotId))
	assert.Equal(t, "snap-b", aws.StringValue(out.Snapshots[1].SnapshotId))
	assert.Len(t, store.Gets(), 2, "a duplicated ID must not be read twice")
	assert.Empty(t, store.ListPrefixes())
}

// Another account's snapshot and a snapshot that was never created are the same
// answer, and neither reads outside the caller's prefix.
func TestDescribeSnapshotsByID_CrossTenantAndMissingAreIndistinguishable(t *testing.T) {
	store := recordingstore.New()
	svc := newByIDService(store)
	putByIDSnapshot(t, store, byIDSnapshotDoc("snap-theirs", byIDOther))

	crossTenant, crossErr := svc.DescribeSnapshots(t.Context(), &ec2.DescribeSnapshotsInput{
		SnapshotIds: aws.StringSlice([]string{"snap-theirs"}),
	}, byIDAccount)
	missing, missingErr := svc.DescribeSnapshots(t.Context(), &ec2.DescribeSnapshotsInput{
		SnapshotIds: aws.StringSlice([]string{"snap-never-created"}),
	}, byIDAccount)

	require.Error(t, crossErr)
	require.Error(t, missingErr)
	assert.Nil(t, crossTenant)
	assert.Nil(t, missing)
	assert.Equal(t, awserrors.ErrorInvalidSnapshotNotFound, crossErr.Error())
	assert.Equal(t, missingErr.Error(), crossErr.Error(),
		"another account's snapshot must be indistinguishable from one that does not exist")
	for _, key := range store.Gets() {
		assert.True(t, strings.HasPrefix(key, byIDSnapshots+byIDAccount+"/"),
			"a by-ID read must stay inside the caller's prefix, read %q", key)
	}
}

// OwnerIds selecting only another account names snapshots the caller's prefix
// cannot hold, so the named ID is absent without any read at all.
func TestDescribeSnapshotsByID_OwnerIDsFilter(t *testing.T) {
	tests := []struct {
		name     string
		ownerIDs []string
		wantErr  string
		wantGets int
	}{
		{name: "self", ownerIDs: []string{"self"}, wantGets: 1},
		{name: "own numeric account", ownerIDs: []string{byIDAccount}, wantGets: 1},
		{name: "another account", ownerIDs: []string{byIDOther},
			wantErr: awserrors.ErrorInvalidSnapshotNotFound, wantGets: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := recordingstore.New()
			svc := newByIDService(store)
			putByIDSnapshot(t, store, byIDSnapshotDoc(byIDSnapshot, byIDAccount))

			out, err := svc.DescribeSnapshots(t.Context(), &ec2.DescribeSnapshotsInput{
				SnapshotIds: aws.StringSlice([]string{byIDSnapshot}),
				OwnerIds:    aws.StringSlice(tt.ownerIDs),
			}, byIDAccount)

			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tt.wantErr, err.Error())
			} else {
				require.NoError(t, err)
				assert.Len(t, out.Snapshots, 1)
			}
			assert.Len(t, store.Gets(), tt.wantGets)
		})
	}
}

// A filter applies to a directly-fetched document exactly as it did to a listed
// one, and a named snapshot a filter excludes is not-found rather than an empty
// success.
func TestDescribeSnapshotsByID_AttributeFilters(t *testing.T) {
	tests := []struct {
		name        string
		filterName  string
		filterValue string
		wantErr     string
	}{
		{name: "matching status", filterName: "status", filterValue: "completed"},
		{name: "matching tag", filterName: "tag:Name", filterValue: "nightly"},
		{name: "non-matching status", filterName: "status", filterValue: "pending",
			wantErr: awserrors.ErrorInvalidSnapshotNotFound},
		{name: "non-matching volume", filterName: "volume-id", filterValue: "vol-other",
			wantErr: awserrors.ErrorInvalidSnapshotNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := recordingstore.New()
			svc := newByIDService(store)
			putByIDSnapshot(t, store, byIDSnapshotDoc(byIDSnapshot, byIDAccount))

			out, err := svc.DescribeSnapshots(t.Context(), &ec2.DescribeSnapshotsInput{
				SnapshotIds: aws.StringSlice([]string{byIDSnapshot}),
				Filters: []*ec2.Filter{{
					Name:   aws.String(tt.filterName),
					Values: aws.StringSlice([]string{tt.filterValue}),
				}},
			}, byIDAccount)

			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tt.wantErr, err.Error())
				return
			}
			require.NoError(t, err)
			assert.Len(t, out.Snapshots, 1)
		})
	}
}

// The tolerant variant's skip exists to stop one bad document hiding unrelated
// snapshots. A specifically-named snapshot is never unrelated, so an unreadable
// document for it fails both variants rather than reporting it as absent.
func TestDescribeSnapshotsByID_CorruptDocumentFailsBothVariants(t *testing.T) {
	store := recordingstore.New()
	svc := newByIDService(store)
	_, err := store.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String(byIDBucket),
		Key:    aws.String(snapshotKey(t, byIDAccount, byIDSnapshot)),
		Body:   strings.NewReader("not-json"),
	})
	require.NoError(t, err)
	input := &ec2.DescribeSnapshotsInput{SnapshotIds: aws.StringSlice([]string{byIDSnapshot})}

	_, tolerantErr := svc.DescribeSnapshots(t.Context(), input, byIDAccount)
	_, strictErr := svc.DescribeSnapshotsStrict(t.Context(), input, byIDAccount)

	require.Error(t, tolerantErr)
	require.Error(t, strictErr)
	assert.Equal(t, awserrors.ErrorServerInternal, tolerantErr.Error(),
		"a named snapshot that cannot be decoded must not report as not-found")
	assert.Equal(t, awserrors.ErrorServerInternal, strictErr.Error())
}

// getFailingStore fails the read of one key with an error that is not a missing
// key, standing in for a store that is reachable but unwell.
type getFailingStore struct {
	*objectstore.MemoryObjectStore

	failKey string
}

func (s *getFailingStore) GetObject(ctx context.Context, input *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	if aws.StringValue(input.Key) == s.failKey {
		return nil, errors.New("connection reset by peer")
	}
	return s.MemoryObjectStore.GetObject(ctx, input)
}

// A store failure is not absence: reporting not-found here would tell a caller
// its snapshot is gone on the strength of a broken connection.
func TestDescribeSnapshotsByID_StoreErrorIsInternal(t *testing.T) {
	memory := objectstore.NewMemoryObjectStore()
	store := &getFailingStore{MemoryObjectStore: memory, failKey: snapshotKey(t, byIDAccount, byIDSnapshot)}
	svc := newByIDService(store)
	putByIDSnapshot(t, memory, byIDSnapshotDoc(byIDSnapshot, byIDAccount))

	_, err := svc.DescribeSnapshots(t.Context(), &ec2.DescribeSnapshotsInput{
		SnapshotIds: aws.StringSlice([]string{byIDSnapshot}),
	}, byIDAccount)

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

// A caller that has already given up gets an error, never an empty result that
// reads as "this snapshot does not exist".
func TestDescribeSnapshotsByID_CancelledContextFails(t *testing.T) {
	store := recordingstore.New()
	svc := newByIDService(store)
	putByIDSnapshot(t, store, byIDSnapshotDoc(byIDSnapshot, byIDAccount))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	out, err := svc.DescribeSnapshots(ctx, &ec2.DescribeSnapshotsInput{
		SnapshotIds: aws.StringSlice([]string{byIDSnapshot}),
	}, byIDAccount)

	require.Error(t, err)
	assert.Nil(t, out)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

// A request whose only IDs are nil names nothing, which is an empty result
// rather than a missing snapshot, and reads nothing to find that out.
func TestDescribeSnapshotsByID_OnlyNilIDs(t *testing.T) {
	store := recordingstore.New()
	svc := newByIDService(store)
	putByIDSnapshot(t, store, byIDSnapshotDoc(byIDSnapshot, byIDAccount))

	out, err := svc.DescribeSnapshots(t.Context(), &ec2.DescribeSnapshotsInput{
		SnapshotIds: []*string{nil},
	}, byIDAccount)

	require.NoError(t, err)
	assert.Empty(t, out.Snapshots)
	assert.Empty(t, store.Gets())
	assert.Empty(t, store.ListPrefixes())
}
