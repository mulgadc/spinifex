package vpc

import (
	"context"
	"errors"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// noScanKV fails any listing of the ENI bucket, so a test proves a read reaches
// only the records it names.
type noScanKV struct {
	jetstream.KeyValue
	t *testing.T
}

func (k noScanKV) scan() error {
	k.t.Error("the ENI bucket was listed")
	return errors.New("bucket listing refused")
}

func (k noScanKV) Keys(context.Context, ...jetstream.WatchOpt) ([]string, error) {
	return nil, k.scan()
}

func (k noScanKV) ListKeys(context.Context, ...jetstream.WatchOpt) (jetstream.KeyLister, error) {
	return nil, k.scan()
}

func (k noScanKV) ListKeysFiltered(context.Context, ...string) (jetstream.KeyLister, error) {
	return nil, k.scan()
}

func (k noScanKV) WatchAll(context.Context, ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	return nil, k.scan()
}

func (k noScanKV) WatchFiltered(context.Context, []string, ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	return nil, k.scan()
}

// failGetKV fails the direct read of one key.
type failGetKV struct {
	jetstream.KeyValue
	key string
}

func (k failGetKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	if key == k.key {
		return nil, errors.New("replica unavailable")
	}
	return k.KeyValue.Get(ctx, key)
}

func attachedIDs(t *testing.T, svc *VPCServiceImpl, instanceID string, candidates ...string) []string {
	t.Helper()
	recs, err := svc.InstanceAttachedENIs(context.Background(), testAccountID, instanceID, candidates)
	require.NoError(t, err)
	ids := make([]string, 0, len(recs))
	for _, r := range recs {
		ids = append(ids, r.NetworkInterfaceId)
	}
	return ids
}

func newIndexFixture(t *testing.T) (*VPCServiceImpl, string) {
	t.Helper()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	return svc, createTestSubnet(t, svc, vpcID, "10.0.1.0/24")
}

func TestInstanceAttachedENIs_FollowsAttachAndDetachWithoutListingTheBucket(t *testing.T) {
	t.Parallel()
	svc, subnetID := newIndexFixture(t)
	first := createTestENI(t, svc, subnetID)
	second := createTestENI(t, svc, subnetID)
	neighbour := createTestENI(t, svc, subnetID)
	svc.WrapENIKVForTest(func(kv jetstream.KeyValue) jetstream.KeyValue { return noScanKV{KeyValue: kv, t: t} })

	for _, eni := range []string{first, second} {
		_, err := svc.AttachENI(testAccountID, eni, "i-one", 0)
		require.NoError(t, err)
	}
	_, err := svc.AttachENI(testAccountID, neighbour, "i-two", 0)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{first, second}, attachedIDs(t, svc, "i-one"))

	require.NoError(t, svc.DetachENI(context.Background(), testAccountID, second))
	assert.Equal(t, []string{first}, attachedIDs(t, svc, "i-one"), "a detached interface leaves the instance")

	_, err = svc.AttachENI(testAccountID, second, "i-two", 1)
	require.NoError(t, err)
	assert.Equal(t, []string{first}, attachedIDs(t, svc, "i-one"), "an interface moved to another instance leaves this one")
	assert.ElementsMatch(t, []string{neighbour, second}, attachedIDs(t, svc, "i-two"))
}

// An interface attached before the index existed has no entry; the instance
// record's own interfaces still find it.
func TestInstanceAttachedENIs_ReadsCandidatesTheIndexLacks(t *testing.T) {
	t.Parallel()
	svc, subnetID := newIndexFixture(t)
	eni := createTestENI(t, svc, subnetID)
	_, err := svc.AttachENI(testAccountID, eni, "i-one", 0)
	require.NoError(t, err)
	require.NoError(t, svc.eniAttachKV.Delete(context.Background(), eniAttachmentKey(testAccountID, "i-one", eni)))

	assert.Empty(t, attachedIDs(t, svc, "i-one"))
	assert.Equal(t, []string{eni}, attachedIDs(t, svc, "i-one", eni, "eni-gone"))
}

func TestInstanceAttachedENIs_FailsOnAnUnreadableRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("undecodable", func(t *testing.T) {
		t.Parallel()
		svc, subnetID := newIndexFixture(t)
		eni := createTestENI(t, svc, subnetID)
		_, err := svc.AttachENI(testAccountID, eni, "i-one", 0)
		require.NoError(t, err)
		_, err = svc.eniKV.Put(ctx, kvstore.AccountKey(testAccountID, eni), []byte("{not json"))
		require.NoError(t, err)

		_, err = svc.InstanceAttachedENIs(ctx, testAccountID, "i-one", nil)
		require.Error(t, err)
	})

	t.Run("read fails", func(t *testing.T) {
		t.Parallel()
		svc, subnetID := newIndexFixture(t)
		eni := createTestENI(t, svc, subnetID)
		_, err := svc.AttachENI(testAccountID, eni, "i-one", 0)
		require.NoError(t, err)
		svc.WrapENIKVForTest(func(kv jetstream.KeyValue) jetstream.KeyValue {
			return failGetKV{KeyValue: kv, key: kvstore.AccountKey(testAccountID, eni)}
		})

		_, err = svc.InstanceAttachedENIs(ctx, testAccountID, "i-one", nil)
		require.Error(t, err)
	})
}

// A cluster upgrading to the index backfills it from the attachments already
// recorded on the ENIs before any request is served.
func TestNewVPCService_BackfillsTheAttachmentIndex(t *testing.T) {
	t.Parallel()
	svc, nc := setupTestVPCServiceWithNC(t)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.0.1.0/24")
	attached := createTestENI(t, svc, subnetID)
	createTestENI(t, svc, subnetID)
	_, err := svc.AttachNetworkInterface(testAccountID, attached, "i-one", 1)
	require.NoError(t, err)

	js, err := jetstream.New(nc)
	require.NoError(t, err)
	require.NoError(t, js.DeleteKeyValue(t.Context(), KVBucketENIAttachments))

	upgraded, err := NewVPCServiceImplWithNATS(t.Context(), nil, nc)
	require.NoError(t, err)
	assert.Equal(t, []string{attached}, attachedIDs(t, upgraded, "i-one"))
}

func TestReconcileAttachmentIndex(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, subnetID := newIndexFixture(t)
	kept := createTestENI(t, svc, subnetID)
	detached := createTestENI(t, svc, subnetID)
	unindexed := createTestENI(t, svc, subnetID)
	for _, eni := range []string{kept, detached, unindexed} {
		_, err := svc.AttachENI(testAccountID, eni, "i-one", 0)
		require.NoError(t, err)
	}
	require.NoError(t, svc.DetachENI(ctx, testAccountID, detached))
	unindexedKey := eniAttachmentKey(testAccountID, "i-one", unindexed)
	require.NoError(t, svc.eniAttachKV.Delete(ctx, unindexedKey))

	indexed, pruned, err := svc.ReconcileAttachmentIndex(ctx, AttachmentIndexMinAge)
	require.NoError(t, err)
	assert.Equal(t, 1, indexed, "an attachment the index lacks is indexed")
	assert.Zero(t, pruned, "an entry younger than the minimum age may be an attach in flight")
	assert.ElementsMatch(t, []string{kept, unindexed}, attachedIDs(t, svc, "i-one"))

	indexed, pruned, err = svc.ReconcileAttachmentIndex(ctx, 0)
	require.NoError(t, err)
	assert.Zero(t, indexed)
	assert.Equal(t, 1, pruned, "only the detached interface's entry goes")
	_, err = svc.eniAttachKV.Get(ctx, eniAttachmentKey(testAccountID, "i-one", detached))
	require.ErrorIs(t, err, jetstream.ErrKeyNotFound)
	for _, eni := range []string{kept, unindexed} {
		_, err := svc.eniAttachKV.Get(ctx, eniAttachmentKey(testAccountID, "i-one", eni))
		require.NoError(t, err, "an attached interface keeps its entry")
	}
}
