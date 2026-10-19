package key_test

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/internal/testkit/readonlystore"
	ec2key "github.com/mulgadc/spinifex/spinifex/domains/ec2/key"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scanner returns a key service over a read-only view of store, so the scan
// fails the test on any write.
func scanner(t *testing.T, store objectstore.ObjectStore) (*ec2key.KeyServiceImpl, *readonlystore.ObjectStore) {
	t.Helper()
	ro := readonlystore.NewObjectStore(t, store)
	return ec2key.NewKeyServiceImplWithStore(ro, ctBucket), ro
}

// Every residue class the delete and create paths leave is classified, and
// a live key pair whose name ends in .json is not mistaken for metadata.
func TestScanAccountClassifiesEveryObjectUnderTheAccountPrefix(t *testing.T) {
	store := newFaultyStore()
	svc := ec2key.NewKeyServiceImplWithStore(store, ctBucket)
	prefix := "keys/" + ctAccount + "/"

	live := importKey(t, svc, ctAccount, "live")
	dotted := importKey(t, svc, ctAccount, "web.json")

	// A public key whose metadata is gone, as an interrupted delete leaves it.
	putObject(t, store, prefix+"deleted", ctPubKey+"\n")

	store.failPut = func(key string) bool { return strings.HasSuffix(key, ".json") }
	store.failDelete = func(string) bool { return true }
	_, err := svc.CreateKeyPair(context.Background(), &ec2.CreateKeyPairInput{KeyName: aws.String("half")}, ctAccount)
	require.Error(t, err)
	store.failPut, store.failDelete = nil, nil

	scanSvc, ro := scanner(t, store)
	scan, err := scanSvc.ScanAccount(context.Background(), ctAccount)
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{aws.StringValue(live.KeyPairId), aws.StringValue(dotted.KeyPairId)}, scan.LiveKeyPairIDs)
	assert.Equal(t, 2, scan.OrphanPublicKeys, "the interrupted-delete and failed-create public keys")
	assert.Empty(t, scan.UnreadableKeyPairIDs)
	assert.Zero(t, scan.UnresolvedPublicKeys)
	assert.Zero(t, scan.ReadFailures)

	for _, key := range ro.Gets() {
		assert.True(t, strings.HasPrefix(key, prefix+"key-") && strings.HasSuffix(key, ".json"), "the scan read %s, which is not metadata", key)
	}
}

// Metadata the store cannot return is unreadable and a read failure; metadata
// that does not decode, or names another ID, is unreadable. While any metadata
// is unreadable, an unmatched public key may be its own, so it is unresolved.
func TestScanAccountReportsUnreadableMetadata(t *testing.T) {
	store := newFaultyStore()
	svc := ec2key.NewKeyServiceImplWithStore(store, ctBucket)
	prefix := "keys/" + ctAccount + "/"
	importKey(t, svc, ctAccount, "live")
	putObject(t, store, prefix+"key-0badbadbadbadbad0.json", "{not json")
	putObject(t, store, prefix+"key-0mismatch00000000.json", `{"KeyName":"other","KeyPairId":"key-0different0000000"}`)
	putObject(t, store, prefix+"key-0unreadable000000.json", `{"KeyName":"hidden","KeyPairId":"key-0unreadable000000"}`)
	putObject(t, store, prefix+"hidden", ctPubKey)
	store.failGet = func(key string) bool { return key == prefix+"key-0unreadable000000.json" }

	scanSvc, _ := scanner(t, store)
	scan, err := scanSvc.ScanAccount(context.Background(), ctAccount)
	require.NoError(t, err)

	assert.Len(t, scan.LiveKeyPairIDs, 1)
	assert.ElementsMatch(t, []string{"key-0badbadbadbadbad0", "key-0mismatch00000000", "key-0unreadable000000"}, scan.UnreadableKeyPairIDs)
	assert.Equal(t, 1, scan.ReadFailures)
	assert.Equal(t, 1, scan.UnresolvedPublicKeys)
	assert.Zero(t, scan.OrphanPublicKeys)
}

// A metadata object removed between the listing and the read is gone, not unreadable.
func TestScanAccountSkipsMetadataRemovedAfterTheListing(t *testing.T) {
	store := &vanishingStore{faultyStore: newFaultyStore()}
	putObject(t, store, "keys/"+ctAccount+"/key-0gone000000000000.json", "{}")
	store.vanish = "keys/" + ctAccount + "/key-0gone000000000000.json"

	scan, err := ec2key.NewKeyServiceImplWithStore(store, ctBucket).ScanAccount(context.Background(), ctAccount)
	require.NoError(t, err)
	assert.Equal(t, &ec2key.AccountScan{}, scan)
}

// The account prefix ends at its slash, so account 1111 never sees 11112's objects.
func TestScanAccountStaysInsideTheAccountPrefix(t *testing.T) {
	store := newFaultyStore()
	svc := ec2key.NewKeyServiceImplWithStore(store, ctBucket)
	importKey(t, svc, "11112", "neighbour")
	putObject(t, store, "keys/11112/orphan", ctPubKey)

	scanSvc, _ := scanner(t, store)
	scan, err := scanSvc.ScanAccount(context.Background(), "1111")
	require.NoError(t, err)
	assert.Equal(t, &ec2key.AccountScan{}, scan)

	scan, err = scanSvc.ScanAccount(context.Background(), "11112")
	require.NoError(t, err)
	assert.Len(t, scan.LiveKeyPairIDs, 1)
	assert.Equal(t, 1, scan.OrphanPublicKeys)
}

// A listing failure is an error, never an empty scan.
func TestScanAccountReturnsAListingFailure(t *testing.T) {
	store := &listFailingStore{MemoryObjectStore: objectstore.NewMemoryObjectStore()}
	_, err := ec2key.NewKeyServiceImplWithStore(store, ctBucket).ScanAccount(context.Background(), ctAccount)
	require.ErrorIs(t, err, errInjected)
}

type listFailingStore struct {
	*objectstore.MemoryObjectStore
}

func (s *listFailingStore) ListObjectsV2(context.Context, *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
	return nil, errInjected
}

// vanishingStore deletes vanish just before it is read, as a concurrent delete would.
type vanishingStore struct {
	*faultyStore

	vanish string
}

func (s *vanishingStore) GetObject(ctx context.Context, in *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	if aws.StringValue(in.Key) == s.vanish {
		_, _ = s.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: in.Bucket, Key: in.Key})
	}
	return s.faultyStore.GetObject(ctx, in)
}
