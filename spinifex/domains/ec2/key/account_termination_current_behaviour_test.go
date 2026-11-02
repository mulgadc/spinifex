package key_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	ec2key "github.com/mulgadc/spinifex/spinifex/domains/ec2/key"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The TestCurrentBehaviour_AccountTermination_ tests pin how key pairs are
// stored and what Describe leaves behind today; a failure means it changed.
// The TestAccountTermination_ tests pin the corrected owner delete.

const (
	ctBucket  = "test-bucket"
	ctAccount = "111122223333"
	ctPubKey  = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
)

var errInjected = errors.New("injected store failure")

// faultyStore fails the operations its predicates select and passes the rest
// to an in-memory store, standing in for a partial object-store outage.
type faultyStore struct {
	*objectstore.MemoryObjectStore

	failGet    func(key string) bool
	failPut    func(key string) bool
	failDelete func(key string) bool
}

var _ objectstore.ObjectStore = (*faultyStore)(nil)

func newFaultyStore() *faultyStore {
	return &faultyStore{MemoryObjectStore: objectstore.NewMemoryObjectStore()}
}

func (f *faultyStore) GetObject(ctx context.Context, in *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	if f.failGet != nil && f.failGet(aws.StringValue(in.Key)) {
		return nil, errInjected
	}
	return f.MemoryObjectStore.GetObject(ctx, in)
}

func (f *faultyStore) PutObject(ctx context.Context, in *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
	if f.failPut != nil && f.failPut(aws.StringValue(in.Key)) {
		return nil, errInjected
	}
	return f.MemoryObjectStore.PutObject(ctx, in)
}

func (f *faultyStore) DeleteObject(ctx context.Context, in *s3.DeleteObjectInput) (*s3.DeleteObjectOutput, error) {
	if f.failDelete != nil && f.failDelete(aws.StringValue(in.Key)) {
		return nil, errInjected
	}
	return f.MemoryObjectStore.DeleteObject(ctx, in)
}

// ListObjectsV2 returns keys in S3's lexicographic order, so a test can place
// one record ahead of another in a scan.
func (f *faultyStore) ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
	out, err := f.MemoryObjectStore.ListObjectsV2(ctx, in)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out.Contents, func(a, b *s3.Object) int {
		return strings.Compare(aws.StringValue(a.Key), aws.StringValue(b.Key))
	})
	return out, nil
}

// accountObjects lists every object key under the account's key prefix.
func accountObjects(t *testing.T, store objectstore.ObjectStore, account string) []string {
	t.Helper()
	objects, _, err := objectstore.ListAll(context.Background(), store, &s3.ListObjectsV2Input{
		Bucket: aws.String(ctBucket),
		Prefix: aws.String("keys/" + account + "/"),
	})
	require.NoError(t, err)
	var keys []string
	for _, obj := range objects {
		keys = append(keys, aws.StringValue(obj.Key))
	}
	slices.Sort(keys)
	return keys
}

func readObject(t *testing.T, store objectstore.ObjectStore, key string) string {
	t.Helper()
	out, err := store.GetObject(context.Background(), &s3.GetObjectInput{Bucket: aws.String(ctBucket), Key: aws.String(key)})
	require.NoError(t, err)
	defer out.Body.Close()
	body, err := io.ReadAll(out.Body)
	require.NoError(t, err)
	return string(body)
}

func putObject(t *testing.T, store objectstore.ObjectStore, key, body string) {
	t.Helper()
	_, err := store.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(ctBucket), Key: aws.String(key), Body: strings.NewReader(body),
	})
	require.NoError(t, err)
}

func importKey(t *testing.T, svc *ec2key.KeyServiceImpl, account, name string) *ec2.ImportKeyPairOutput {
	t.Helper()
	out, err := svc.ImportKeyPair(context.Background(), &ec2.ImportKeyPairInput{
		KeyName: aws.String(name), PublicKeyMaterial: []byte(ctPubKey),
	}, account)
	require.NoError(t, err)
	return out
}

func describedKeyNames(t *testing.T, svc *ec2key.KeyServiceImpl, account string) []string {
	t.Helper()
	out, err := svc.DescribeKeyPairs(context.Background(), &ec2.DescribeKeyPairsInput{}, account)
	require.NoError(t, err)
	var names []string
	for _, kp := range out.KeyPairs {
		names = append(names, aws.StringValue(kp.KeyName))
	}
	slices.Sort(names)
	return names
}

// A key pair is two objects under keys/<account>/: the public key by name and
// the metadata by key pair ID. The generated private key is in neither.
func TestCurrentBehaviour_AccountTermination_KeyPairStorageLayout(t *testing.T) {
	store := newFaultyStore()
	svc := ec2key.NewKeyServiceImplWithStore(store, ctBucket)

	imported := importKey(t, svc, ctAccount, "imported")
	created, err := svc.CreateKeyPair(context.Background(), &ec2.CreateKeyPairInput{KeyName: aws.String("created")}, ctAccount)
	require.NoError(t, err)
	importKey(t, svc, "444455556666", "other-account")

	prefix := "keys/" + ctAccount + "/"
	want := []string{
		prefix + "created",
		prefix + "imported",
		prefix + aws.StringValue(created.KeyPairId) + ".json",
		prefix + aws.StringValue(imported.KeyPairId) + ".json",
	}
	slices.Sort(want)
	assert.Equal(t, want, accountObjects(t, store, ctAccount))

	assert.Contains(t, readObject(t, store, prefix+"imported"), ctPubKey)
	meta := readObject(t, store, prefix+aws.StringValue(created.KeyPairId)+".json")
	assert.Contains(t, meta, `"KeyName":"created"`)
	assert.NotContains(t, meta, "PRIVATE KEY")
	for _, key := range accountObjects(t, store, ctAccount) {
		assert.NotContains(t, readObject(t, store, key), aws.StringValue(created.KeyMaterial))
	}
}

// A key pair is deleted public key first and metadata last, so a failed step
// returns an error and leaves the key pair described and found by name; a
// retried delete then removes both objects and frees the name.
func TestAccountTermination_DeleteKeyPairKeepsTheKeyPairWhenAStepFails(t *testing.T) {
	prefix := "keys/" + ctAccount + "/"
	tests := []struct {
		name   string
		failOn func(keyPairID string) string
		left   func(keyPairID string) []string
	}{
		{"public key delete fails", func(string) string { return prefix + "deploy" },
			func(id string) []string { return sortedStrings(prefix+"deploy", prefix+id+".json") }},
		{"metadata delete fails", func(id string) string { return prefix + id + ".json" },
			func(id string) []string { return []string{prefix + id + ".json"} }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newFaultyStore()
			svc := ec2key.NewKeyServiceImplWithStore(store, ctBucket)
			id := aws.StringValue(importKey(t, svc, ctAccount, "deploy").KeyPairId)
			failing := tc.failOn(id)
			store.failDelete = func(key string) bool { return key == failing }

			_, err := svc.DeleteKeyPair(context.Background(), &ec2.DeleteKeyPairInput{KeyName: aws.String("deploy")}, ctAccount)
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
			assert.Equal(t, tc.left(id), accountObjects(t, store, ctAccount))
			assert.Equal(t, []string{"deploy"}, describedKeyNames(t, svc, ctAccount))

			store.failDelete = nil
			out, err := svc.DeleteKeyPair(context.Background(), &ec2.DeleteKeyPairInput{KeyName: aws.String("deploy")}, ctAccount)
			require.NoError(t, err)
			assert.Equal(t, id, aws.StringValue(out.KeyPairId))
			assert.Empty(t, accountObjects(t, store, ctAccount))
			assert.ErrorContains(t, svc.ValidateKeyPairExists(context.Background(), ctAccount, "deploy"), awserrors.ErrorInvalidKeyPairNotFound)

			_, err = svc.CreateKeyPair(context.Background(), &ec2.CreateKeyPairInput{KeyName: aws.String("deploy")}, ctAccount)
			assert.NoError(t, err, "the name is free again")
		})
	}
}

// A public key left with no metadata is that name's key: a delete by name
// removes it and reports success without a key pair ID, freeing the name.
func TestAccountTermination_DeleteKeyPairByNameRemovesALeftoverPublicKey(t *testing.T) {
	store := newFaultyStore()
	svc := ec2key.NewKeyServiceImplWithStore(store, ctBucket)
	putObject(t, store, "keys/"+ctAccount+"/orphan", ctPubKey+"\n")

	out, err := svc.DeleteKeyPair(context.Background(), &ec2.DeleteKeyPairInput{KeyName: aws.String("orphan")}, ctAccount)
	require.NoError(t, err)
	assert.True(t, aws.BoolValue(out.Return))
	assert.Nil(t, out.KeyPairId)
	assert.Empty(t, accountObjects(t, store, ctAccount))
	_, err = svc.CreateKeyPair(context.Background(), &ec2.CreateKeyPairInput{KeyName: aws.String("orphan")}, ctAccount)
	assert.NoError(t, err)
}

// A by-name delete that cannot read some metadata cannot prove the name is
// absent, so it fails rather than reporting an idempotent success.
func TestAccountTermination_DeleteKeyPairByNameFailsWhenMetadataIsUnreadable(t *testing.T) {
	store := newFaultyStore()
	svc := ec2key.NewKeyServiceImplWithStore(store, ctBucket)
	id := aws.StringValue(importKey(t, svc, ctAccount, "deploy").KeyPairId)
	store.failGet = func(key string) bool { return strings.HasSuffix(key, id+".json") }

	_, err := svc.DeleteKeyPair(context.Background(), &ec2.DeleteKeyPairInput{KeyName: aws.String("deploy")}, ctAccount)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
	assert.Len(t, accountObjects(t, store, ctAccount), 2, "nothing was deleted")
}

// Undecodable metadata may be the named key pair, so a by-name delete fails
// and leaves both the metadata and the same-name public key in place.
func TestAccountTermination_DeleteKeyPairByNameFailsOnUndecodableMetadata(t *testing.T) {
	store := newFaultyStore()
	svc := ec2key.NewKeyServiceImplWithStore(store, ctBucket)
	broken := "keys/" + ctAccount + "/key-0badbadbadbadbad0.json"
	pub := "keys/" + ctAccount + "/deploy"
	putObject(t, store, broken, "{not json")
	putObject(t, store, pub, ctPubKey+"\n")

	_, err := svc.DeleteKeyPair(context.Background(), &ec2.DeleteKeyPairInput{KeyName: aws.String("deploy")}, ctAccount)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
	assert.Equal(t, sortedStrings(broken, pub), accountObjects(t, store, ctAccount))
}

// An unreadable record listed ahead of the named key pair cannot hide it: the
// match proves the name present, so the delete goes ahead.
func TestAccountTermination_DeleteKeyPairByNameSucceedsPastUndecodableMetadata(t *testing.T) {
	store := newFaultyStore()
	svc := ec2key.NewKeyServiceImplWithStore(store, ctBucket)
	broken := "keys/" + ctAccount + "/key-000000000000000000.json"
	putObject(t, store, broken, "{not json")
	id := aws.StringValue(importKey(t, svc, ctAccount, "deploy").KeyPairId)
	require.Less(t, broken, "keys/"+ctAccount+"/"+id+".json", "the broken record must be scanned first")

	out, err := svc.DeleteKeyPair(context.Background(), &ec2.DeleteKeyPairInput{KeyName: aws.String("deploy")}, ctAccount)
	require.NoError(t, err)
	assert.Equal(t, id, aws.StringValue(out.KeyPairId))
	assert.Equal(t, []string{broken}, accountObjects(t, store, ctAccount))
}

func sortedStrings(s ...string) []string {
	slices.Sort(s)
	return s
}

// A create whose metadata write fails and whose cleanup delete also fails
// returns an error and leaves the public key object with no metadata.
func TestCurrentBehaviour_AccountTermination_FailedCreateCleanupLeavesPublicKey(t *testing.T) {
	store := newFaultyStore()
	svc := ec2key.NewKeyServiceImplWithStore(store, ctBucket)
	store.failPut = func(key string) bool { return strings.HasSuffix(key, ".json") }
	store.failDelete = func(string) bool { return true }

	_, err := svc.CreateKeyPair(context.Background(), &ec2.CreateKeyPairInput{KeyName: aws.String("half")}, ctAccount)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())

	assert.Equal(t, []string{"keys/" + ctAccount + "/half"}, accountObjects(t, store, ctAccount))
	assert.Empty(t, describedKeyNames(t, svc, ctAccount))
}

// DescribeKeyPairs reports only metadata it can read and decode, and never a
// public key object on its own. Each record class below is skipped silently
// and the call still succeeds.
func TestCurrentBehaviour_AccountTermination_DescribeKeyPairsHidesUnreadableRecords(t *testing.T) {
	prefix := "keys/" + ctAccount + "/"
	tests := []struct {
		name string
		seed func(t *testing.T, store *faultyStore)
	}{
		{"public key without metadata", func(t *testing.T, store *faultyStore) {
			putObject(t, store, prefix+"orphan", ctPubKey+"\n")
		}},
		{"undecodable metadata", func(t *testing.T, store *faultyStore) {
			putObject(t, store, prefix+"key-0badbadbadbadbad0.json", "{not json")
		}},
		{"metadata the store cannot read", func(t *testing.T, store *faultyStore) {
			putObject(t, store, prefix+"key-0unreadable000000.json", `{"KeyName":"hidden","KeyPairId":"key-0unreadable000000"}`)
			store.failGet = func(key string) bool { return key == prefix+"key-0unreadable000000.json" }
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newFaultyStore()
			svc := ec2key.NewKeyServiceImplWithStore(store, ctBucket)
			importKey(t, svc, ctAccount, "live")
			tc.seed(t, store)

			assert.Len(t, accountObjects(t, store, ctAccount), 3, "the seeded record is in the owner store")
			assert.Equal(t, []string{"live"}, describedKeyNames(t, svc, ctAccount))
		})
	}
}

// Account cleanup deletes each readable key pair through the owner delete and
// removes a settled public key no metadata names. A younger orphan may be a
// create in flight and is left; unreadable metadata is never deleted.
func TestAccountCleanup_DeletesKeyPairsAndSettledOrphans(t *testing.T) {
	store := newFaultyStore()
	svc := ec2key.NewKeyServiceImplWithStore(store, ctBucket)
	importKey(t, svc, ctAccount, "deploy")
	importKey(t, svc, "999988887777", "theirs")
	putObject(t, store, "keys/"+ctAccount+"/old-orphan", ctPubKey)
	broken := "keys/" + ctAccount + "/key-0badbadbadbadbad0.json"
	putObject(t, store, broken, "{not json")

	out, err := svc.CleanupAccount(context.Background(), ctAccount, time.Now().Add(-time.Hour), nil)
	require.NoError(t, err)
	assert.Len(t, out.DeletedKeyPairIDs, 1)
	assert.Zero(t, out.OrphansRemoved, "orphans are left while a public key may belong to unreadable metadata")

	store.Clear()
	importKey(t, svc, ctAccount, "deploy")
	putObject(t, store, "keys/"+ctAccount+"/old-orphan", ctPubKey)
	out, err = svc.CleanupAccount(context.Background(), ctAccount, time.Now().Add(-time.Hour), nil)
	require.NoError(t, err)
	assert.Equal(t, 1, out.OrphansUnsettled, "an orphan written inside the settle window is kept")
	assert.Equal(t, []string{"keys/" + ctAccount + "/old-orphan"}, accountObjects(t, store, ctAccount))

	out, err = svc.CleanupAccount(context.Background(), ctAccount, time.Now().Add(time.Second), nil)
	require.NoError(t, err)
	assert.Equal(t, 1, out.OrphansRemoved)
	assert.Empty(t, accountObjects(t, store, ctAccount))
}

// A key pair, and an orphan public key, whose name the reference check names
// are kept with the given reason: IMDS serves the public key by name.
func TestAccountCleanup_KeepsReferencedKeyPairsAndPublicKeys(t *testing.T) {
	store := newFaultyStore()
	svc := ec2key.NewKeyServiceImplWithStore(store, ctBucket)
	kp := importKey(t, svc, ctAccount, "in-use")
	importKey(t, svc, ctAccount, "free")
	putObject(t, store, "keys/"+ctAccount+"/served-orphan", ctPubKey)
	referenced := func(name string) string {
		if name == "in-use" || name == "served-orphan" {
			return "instance-reference"
		}
		return ""
	}

	out, err := svc.CleanupAccount(context.Background(), ctAccount, time.Now().Add(time.Second), referenced)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{aws.StringValue(kp.KeyPairId): "instance-reference"}, out.Referenced)
	assert.Len(t, out.DeletedKeyPairIDs, 1)
	assert.Equal(t, map[string]int{"instance-reference": 1}, out.OrphansReferenced)
	assert.Zero(t, out.OrphansRemoved)
	assert.Equal(t, []string{"in-use"}, describedKeyNames(t, svc, ctAccount))
	assert.Contains(t, accountObjects(t, store, ctAccount), "keys/"+ctAccount+"/served-orphan")
}

// A key pair whose delete fails part way is reported failed with its metadata
// kept; the next run completes it.
func TestAccountCleanup_InterruptedKeyPairDeleteCompletesOnRerun(t *testing.T) {
	store := newFaultyStore()
	svc := ec2key.NewKeyServiceImplWithStore(store, ctBucket)
	kp := importKey(t, svc, ctAccount, "deploy")
	store.failDelete = func(key string) bool { return strings.HasSuffix(key, ".json") }

	out, err := svc.CleanupAccount(context.Background(), ctAccount, time.Now(), nil)
	require.NoError(t, err)
	assert.Equal(t, []string{aws.StringValue(kp.KeyPairId)}, out.FailedKeyPairIDs)
	assert.Equal(t, []string{"deploy"}, describedKeyNames(t, svc, ctAccount))

	store.failDelete = nil
	out, err = svc.CleanupAccount(context.Background(), ctAccount, time.Now(), nil)
	require.NoError(t, err)
	assert.Equal(t, []string{aws.StringValue(kp.KeyPairId)}, out.DeletedKeyPairIDs)
	assert.Empty(t, accountObjects(t, store, ctAccount))
}
