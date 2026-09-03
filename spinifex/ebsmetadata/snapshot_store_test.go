package ebsmetadata_test

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	. "github.com/mulgadc/spinifex/spinifex/ebsmetadata"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	accountA = "000000000001"
	accountB = "000000000002"
)

func TestStoreRoundTripsSnapshot(t *testing.T) {
	store := NewStore(objectstore.NewMemoryObjectStore(), "control-plane")
	ctx := context.Background()

	snapshot := Snapshot{SnapshotID: "snap-1", VolumeID: "vol-1", OwnerID: accountA,
		VolumeSize: 8, State: "completed", ProviderHandle: "opaque"}
	require.NoError(t, store.PutSnapshot(ctx, snapshot))

	got, err := store.GetSnapshot(ctx, accountA, "snap-1")
	require.NoError(t, err)
	assert.Equal(t, snapshot.VolumeID, got.VolumeID)
	assert.Equal(t, snapshot.ProviderHandle, got.ProviderHandle)
	assert.Equal(t, SchemaVersion, got.SchemaVersion)

	require.NoError(t, store.DeleteSnapshot(ctx, accountA, "snap-1"))
	_, err = store.GetSnapshot(ctx, accountA, "snap-1")
	assert.True(t, objectstore.IsNoSuchKeyError(err))
}

// PutSnapshot takes the key segment from the document's own OwnerID, so an
// untenanted snapshot has nowhere to be written and cannot exist.
func TestPutSnapshot_RefusesUntenantedDocument(t *testing.T) {
	store := NewStore(objectstore.NewMemoryObjectStore(), "control-plane")
	err := store.PutSnapshot(context.Background(), Snapshot{SnapshotID: "snap-1", VolumeID: "vol-1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid EBS metadata account ID")
}

// A snapshot outside the caller's prefix is absent, not denied: the key is the
// ownership check.
func TestGetSnapshot_OtherAccountIsNotFound(t *testing.T) {
	store := NewStore(objectstore.NewMemoryObjectStore(), "control-plane")
	ctx := context.Background()
	require.NoError(t, store.PutSnapshot(ctx, Snapshot{SnapshotID: "snap-1", VolumeID: "vol-1", OwnerID: accountA}))

	_, err := store.GetSnapshot(ctx, accountB, "snap-1")
	assert.True(t, objectstore.IsNoSuchKeyError(err))
}

func TestListSnapshots_ScopedToOneAccount(t *testing.T) {
	store := NewStore(objectstore.NewMemoryObjectStore(), "control-plane")
	ctx := context.Background()
	for _, snapshot := range []Snapshot{
		{SnapshotID: "snap-a1", OwnerID: accountA},
		{SnapshotID: "snap-a2", OwnerID: accountA},
		{SnapshotID: "snap-b1", OwnerID: accountB},
	} {
		require.NoError(t, store.PutSnapshot(ctx, snapshot))
	}

	got, err := store.ListSnapshots(ctx, accountA)
	require.NoError(t, err)
	assert.Equal(t, []string{"snap-a1", "snap-a2"}, snapshotIDs(got))

	got, err = store.ListSnapshots(ctx, accountB)
	require.NoError(t, err)
	assert.Equal(t, []string{"snap-b1"}, snapshotIDs(got))
}

// The two verbs must partition and list the same way: a key builder that wrote
// one shape and listed another would show up here and nowhere else.
func TestListAllSnapshots_IsTheUnionOfEveryAccount(t *testing.T) {
	store := NewStore(objectstore.NewMemoryObjectStore(), "control-plane")
	ctx := context.Background()
	for _, snapshot := range []Snapshot{
		{SnapshotID: "snap-a1", OwnerID: accountA},
		{SnapshotID: "snap-b1", OwnerID: accountB},
		{SnapshotID: "snap-b2", OwnerID: accountB},
	} {
		require.NoError(t, store.PutSnapshot(ctx, snapshot))
	}

	all, err := store.ListAllSnapshots(ctx)
	require.NoError(t, err)

	var union []Snapshot
	for _, account := range []string{accountA, accountB} {
		scoped, err := store.ListSnapshots(ctx, account)
		require.NoError(t, err)
		union = append(union, scoped...)
	}
	assert.Equal(t, snapshotIDs(all), snapshotIDs(union))
}

// An empty account must not widen a listing to every account's documents.
func TestListSnapshots_RefusesANonAccountPrefix(t *testing.T) {
	store := NewStore(objectstore.NewMemoryObjectStore(), "control-plane")
	ctx := context.Background()
	require.NoError(t, store.PutSnapshot(ctx, Snapshot{SnapshotID: "snap-a1", OwnerID: accountA}))

	for _, account := range []string{"", "not-an-account", "snap-a1"} {
		_, err := store.ListSnapshots(ctx, account)
		require.Error(t, err, "account %q must not list", account)
		_, err = store.ListSnapshotsStrict(ctx, account)
		require.Error(t, err, "account %q must not list strictly", account)
	}
}

func TestListSnapshots_SkipsCorruptButStrictDoesNot(t *testing.T) {
	objects := objectstore.NewMemoryObjectStore()
	store := NewStore(objects, "control-plane")
	ctx := context.Background()
	require.NoError(t, store.PutSnapshot(ctx, Snapshot{SnapshotID: "snap-a1", OwnerID: accountA}))

	key, err := SnapshotKey(accountA, "snap-corrupt")
	require.NoError(t, err)
	_, err = objects.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("control-plane"), Key: aws.String(key),
		Body: strings.NewReader("{not json"),
	})
	require.NoError(t, err)

	tolerant, err := store.ListSnapshots(ctx, accountA)
	require.NoError(t, err)
	assert.Equal(t, []string{"snap-a1"}, snapshotIDs(tolerant))

	_, err = store.ListSnapshotsStrict(ctx, accountA)
	require.ErrorIs(t, err, ErrCorruptDocument)
}

// A snapshot document written by an older schema is corrupt, not absent: the
// two deserve different answers.
func TestUnmarshalSnapshot_RejectsUnknownSchemaVersion(t *testing.T) {
	data, err := MarshalSnapshot(Snapshot{SnapshotID: "snap-1", OwnerID: accountA})
	require.NoError(t, err)
	stale := strings.Replace(string(data), `"schema_version":2`, `"schema_version":1`, 1)
	require.NotEqual(t, string(data), stale)

	_, err = UnmarshalSnapshot([]byte(stale))
	require.ErrorIs(t, err, ErrCorruptDocument)
}

// A malformed account reaches the key builder only through a Spinifex bug, so
// it stays a plain error rather than a not-found the caller would report as a
// missing snapshot.
func TestSnapshotVerbs_RefuseAMalformedAccount(t *testing.T) {
	store := NewStore(objectstore.NewMemoryObjectStore(), "control-plane")
	ctx := context.Background()

	_, err := store.GetSnapshot(ctx, "not-an-account", "snap-1")
	require.Error(t, err)
	assert.False(t, objectstore.IsNoSuchKeyError(err))

	require.Error(t, store.DeleteSnapshot(ctx, "", "snap-1"))
}

func TestListAllSnapshotsStrict_FailsOnACorruptDocument(t *testing.T) {
	objects := objectstore.NewMemoryObjectStore()
	store := NewStore(objects, "control-plane")
	ctx := context.Background()
	require.NoError(t, store.PutSnapshot(ctx, Snapshot{SnapshotID: "snap-a1", OwnerID: accountA}))

	key, err := SnapshotKey(accountB, "snap-corrupt")
	require.NoError(t, err)
	_, err = objects.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("control-plane"), Key: aws.String(key),
		Body: strings.NewReader("{not json"),
	})
	require.NoError(t, err)

	tolerant, err := store.ListAllSnapshots(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"snap-a1"}, snapshotIDs(tolerant))

	_, err = store.ListAllSnapshotsStrict(ctx)
	require.ErrorIs(t, err, ErrCorruptDocument)
}

// An unconfigured store must say so rather than answer "no snapshots", which a
// caller would act on as authoritative absence.
func TestSnapshotListings_RefuseAnUnconfiguredStore(t *testing.T) {
	store := NewStore(nil, "control-plane")
	ctx := context.Background()

	for name, list := range map[string]func() ([]Snapshot, error){
		"ListSnapshots":          func() ([]Snapshot, error) { return store.ListSnapshots(ctx, accountA) },
		"ListSnapshotsStrict":    func() ([]Snapshot, error) { return store.ListSnapshotsStrict(ctx, accountA) },
		"ListAllSnapshots":       func() ([]Snapshot, error) { return store.ListAllSnapshots(ctx) },
		"ListAllSnapshotsStrict": func() ([]Snapshot, error) { return store.ListAllSnapshotsStrict(ctx) },
	} {
		_, err := list()
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "not configured", name)
	}
}

func snapshotIDs(snapshots []Snapshot) []string {
	ids := make([]string, 0, len(snapshots))
	for _, snapshot := range snapshots {
		ids = append(ids, snapshot.SnapshotID)
	}
	sort.Strings(ids)
	return ids
}
